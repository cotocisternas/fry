//go:build linux

package imapcollect

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/cotocisternas/fry/internal/hotstore"
	"github.com/cotocisternas/fry/internal/imapsource"
	"github.com/cotocisternas/fry/internal/receiptstore"

	"github.com/emersion/go-imap/v2"
)

type collectionRun struct {
	collector     *Collector
	reader        *imapsource.CollectionReader
	sourceCleanup *imapsource.Cleanup
	result        Result
}

func (r *collectionRun) collectMappings(ctx context.Context) error {
	cfg := r.collector.config
	folders, err := r.collector.store.Folders(ctx, cfg.Organization, cfg.Inbox)
	if err != nil {
		return err
	}
	targets := map[string]receiptstore.ID{}
	for _, folder := range folders {
		targets[folder.Name] = folder.ID
	}
	for _, mapping := range cfg.Mappings {
		target, exists := targets[canonicalInbox(mapping.DestinationFolder)]
		if !exists {
			return fmt.Errorf("imapcollect: destination folder must already exist")
		}
		selected, err := r.reader.Select(mapping.SourceFolder)
		if err != nil {
			return fmt.Errorf("imapcollect: cannot select source folder")
		}
		if selected.UIDValidity == 0 {
			return fmt.Errorf("imapcollect: source returned zero UIDVALIDITY")
		}
		source := receiptstore.Source{
			Connection:  cfg.Connection,
			Folder:      canonicalInbox(mapping.SourceFolder),
			UIDValidity: selected.UIDValidity,
		}
		exhausted, err := r.collectFolder(ctx, source, target)
		if err != nil || exhausted {
			return err
		}
	}
	return nil
}

func (r *collectionRun) collectFolder(
	ctx context.Context,
	source receiptstore.Source,
	target receiptstore.ID,
) (bool, error) {
	uids, err := r.reader.Search()
	if err != nil {
		return false, err
	}
	cfg := r.collector.config
	known := map[uint32]bool{}
	for index, uid := range uids {
		if !cfg.DeleteSource && index%1024 == 0 {
			known, err = r.acceptedSources(ctx, source, uids[index:min(index+1024, len(uids))])
			if err != nil {
				return false, err
			}
		}
		if known[uint32(uid)] {
			r.result.AlreadyPresent++
			continue
		}
		if r.result.Copied >= cfg.MaxMessages {
			return true, nil
		}
		source.UID = uint32(uid)
		if err = r.collectMessage(ctx, source, target, uid); err != nil {
			return false, err
		}
	}
	return false, nil
}

func (r *collectionRun) acceptedSources(
	ctx context.Context,
	source receiptstore.Source,
	batch []imap.UID,
) (map[uint32]bool, error) {
	numbers := make([]uint32, len(batch))
	for i, uid := range batch {
		numbers[i] = uint32(uid)
	}
	source.UID = 0
	return r.collector.store.AcceptedSources(ctx, r.collector.config.Organization, source, numbers)
}

func (r *collectionRun) collectMessage(
	ctx context.Context,
	source receiptstore.Source,
	target receiptstore.ID,
	uid imap.UID,
) error {
	c := r.collector
	attempt, envelope, accepted, err := r.sourceAttempt(ctx, source)
	if err != nil {
		return err
	}
	if accepted {
		r.result.AlreadyPresent++
		return r.cleanup(ctx, source, attempt)
	}

	metadata, found, err := r.reader.Metadata(uint32(uid))
	if err != nil {
		return err
	}
	if !found {
		r.result.Vanished++
		return nil
	}
	if attempt == (receiptstore.ID{}) {
		attempt, err = hotstore.NewID()
		if err != nil {
			return err
		}
		envelope = r.envelope(source, target, metadata)
	}
	copied, err := r.reader.Fetch(
		uint32(uid),
		metadata.Size,
		func(body io.ReadCloser) error { return c.acceptLiteral(ctx, attempt, envelope, body) },
	)
	if err != nil {
		return err
	}
	if !copied {
		r.result.Vanished++
		return nil
	}
	r.result.Copied++
	return r.cleanup(ctx, source, attempt)
}

func (r *collectionRun) envelope(
	source receiptstore.Source,
	target receiptstore.ID,
	m imapsource.Metadata,
) receiptstore.Envelope {
	cfg := r.collector.config
	flags := []string{}
	for _, flag := range m.Flags {
		if collectionFlag(flag) {
			flags = append(flags, flag)
		}
	}
	return receiptstore.Envelope{
		Organization: cfg.Organization,
		Source:       &source,
		Destinations: []receiptstore.Destination{
			{
				Inbox:        cfg.Inbox,
				Folder:       target,
				Recipients:   []string{"imap-source:" + cfg.Connection.String()},
				Flags:        flags,
				InternalDate: m.InternalDate,
			},
		},
	}
}

func collectionFlag(value string) bool {
	return !strings.HasPrefix(value, "\\") || value == string(imap.FlagSeen) || value == string(imap.FlagAnswered) ||
		value == string(imap.FlagFlagged) ||
		value == string(imap.FlagDraft) ||
		value == string(imap.FlagDeleted)
}

func (r *collectionRun) cleanup(ctx context.Context, source receiptstore.Source, attempt receiptstore.ID) error {
	if !r.collector.config.DeleteSource {
		return nil
	}
	if err := r.collector.cleanup(ctx, r.sourceCleanup, source, attempt); err != nil {
		return err
	}
	r.result.Cleaned++
	return nil
}

func (r *collectionRun) sourceAttempt(
	ctx context.Context,
	source receiptstore.Source,
) (receiptstore.ID, receiptstore.Envelope, bool, error) {
	attempt, envelope, status, err := r.collector.store.SourceAttempt(ctx, r.collector.config.Organization, source)
	if err != nil && !errors.Is(err, receiptstore.ErrNotFound) {
		return attempt, envelope, false, err
	}
	return attempt, envelope, err == nil && status.State == "accepted", nil
}
