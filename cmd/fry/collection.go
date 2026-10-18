//go:build linux

package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/cotocisternas/fry/internal/imapcollect"
	"github.com/cotocisternas/fry/internal/receiptstore"
)

type collectionConfig struct {
	ID                  string `json:"id"`
	Organization        string `json:"organization"`
	Inbox               string `json:"inbox"`
	Address             string `json:"address"`
	Username            string `json:"username"`
	PasswordEnvironment string `json:"password_environment"`
	LoopbackPlaintext   bool   `json:"loopback_plaintext"`
	DeleteSource        bool   `json:"delete_source"`
	Mappings            []struct {
		Source      string `json:"source"`
		Destination string `json:"destination"`
	} `json:"mappings"`
}

func startCollectors(
	parent context.Context,
	store *receiptstore.Store,
	configs []collectionConfig,
	maxMessageBytes int64,
) (func(), error) {
	ctx, cancel := context.WithCancel(parent)
	var wg sync.WaitGroup
	stop := func() { cancel(); wg.Wait() }
	var collectors []*imapcollect.Collector
	seen := map[receiptstore.ID]bool{}
	for _, config := range configs {
		id, err := parseID(config.ID)
		if err != nil {
			stop()
			return nil, err
		}
		if seen[id] {
			stop()
			return nil, fmt.Errorf("duplicate collection connection ID")
		}
		seen[id] = true
		collector, err := config.prepareCollector(ctx, store, id, maxMessageBytes)
		if err != nil {
			stop()
			return nil, err
		}
		collectors = append(collectors, collector)
	}
	for _, collector := range collectors {
		wg.Go(func() { runCollector(ctx, collector) })
	}
	return stop, nil
}

func (config collectionConfig) prepareCollector(
	ctx context.Context,
	store *receiptstore.Store,
	id receiptstore.ID,
	maxMessageBytes int64,
) (*imapcollect.Collector, error) {
	org, err := parseID(config.Organization)
	if err != nil {
		return nil, err
	}
	inbox, err := parseID(config.Inbox)
	if err != nil {
		return nil, err
	}
	if err = store.EnsureInbox(ctx, org, inbox); err != nil {
		return nil, err
	}
	folders, err := store.Folders(ctx, org, inbox)
	if err != nil {
		return nil, err
	}
	cfg := imapcollect.Config{
		Connection:        id,
		Organization:      org,
		Inbox:             inbox,
		Address:           config.Address,
		Username:          config.Username,
		Password:          os.Getenv(config.PasswordEnvironment),
		LoopbackPlaintext: config.LoopbackPlaintext,
		DeleteSource:      config.DeleteSource,
		MaxMessages:       128,
		MaxMessageBytes:   maxMessageBytes,
		Timeout:           5 * time.Minute,
	}
	cfg.Mappings, err = config.prepareMappings(ctx, store, org, inbox, folders)
	if err != nil {
		return nil, err
	}
	collector, err := imapcollect.New(store, cfg)
	if err != nil {
		return nil, err
	}
	return collector, nil
}

func (config collectionConfig) prepareMappings(
	ctx context.Context,
	store *receiptstore.Store,
	org, inbox receiptstore.ID,
	folders []receiptstore.Folder,
) ([]imapcollect.Mapping, error) {
	var mappings []imapcollect.Mapping
	var err error
	existing := map[string]bool{}
	for _, f := range folders {
		existing[f.Name] = true
	}
	for _, mapping := range config.Mappings {
		name := mapping.Destination
		if strings.EqualFold(name, "INBOX") {
			name = "INBOX"
		}
		if !existing[name] {
			if err = store.CreateFolder(ctx, org, inbox, name); err != nil {
				return nil, err
			}
			existing[name] = true
		}
		mappings = append(mappings, imapcollect.Mapping{SourceFolder: mapping.Source, DestinationFolder: name})
	}
	return mappings, nil
}

func runCollector(ctx context.Context, collector *imapcollect.Collector) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		result, err := collector.RunOnce(ctx)
		if err != nil && ctx.Err() == nil {
			log.Print("IMAP collection will retry after a connection or storage error")
		}
		if err == nil && result.Copied == 128 {
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
