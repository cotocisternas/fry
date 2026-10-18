package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strconv"

	"github.com/cotocisternas/fry/internal/replicaconfig"
)

type config struct {
	replicaconfig.MemberConfig
	TLS replicaconfig.TLS `json:"tls"`
}

func configuration(args []string) (config, string, error) {
	flags := flag.NewFlagSet("member-maintenance", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	path := flags.String("config", "", "private synthetic member/client configuration")
	if err := flags.Parse(args); err != nil {
		return config{}, "", err
	}
	if *path == "" || flags.NArg() != 1 {
		return config{}, "", fmt.Errorf("usage: member-maintenance -config FILE rotate|reclaim-sealed")
	}
	action := flags.Arg(0)
	if action != "rotate" && action != "reclaim-sealed" {
		return config{}, "", fmt.Errorf("unsupported maintenance action")
	}
	cfg, err := loadConfig(*path)
	if err != nil {
		return config{}, "", err
	}
	return cfg, action, validateConfig(cfg)
}

func loadConfig(path string) (cfg config, resultErr error) {
	//nolint:gosec // G304: explicit local fixture configuration; owner FRY maintainers, review 2027-01-18.
	file, err := os.Open(path)
	if err != nil {
		return cfg, err
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	data, err := io.ReadAll(io.LimitReader(file, (64<<10)+1))
	if err != nil {
		return cfg, err
	}
	if len(data) > 64<<10 {
		return cfg, fmt.Errorf("configuration exceeds 64 KiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return cfg, err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return cfg, fmt.Errorf("configuration must contain one JSON value")
	}
	return cfg, nil
}

func validateConfig(cfg config) error {
	if err := loopback(cfg.URL); err != nil {
		return err
	}
	if _, err := replicaconfig.ParseID(cfg.ID); err != nil {
		return err
	}
	if cfg.Domain == "" {
		return fmt.Errorf("failure domain required")
	}
	_, err := replicaconfig.Key(cfg.ServerKey)
	return err
}

func loopback(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || !validFixtureURL(u) || !loopbackHost(u.Host) {
		return fmt.Errorf("numeric loopback HTTPS endpoint required")
	}
	return nil
}

func validFixtureURL(u *url.URL) bool {
	return u.Scheme == "https" && u.User == nil && u.Path == "" && u.RawQuery == "" && !u.ForceQuery &&
		u.Fragment == "" &&
		u.Opaque == ""
}

func loopbackHost(raw string) bool {
	host, port, err := net.SplitHostPort(raw)
	number, portErr := strconv.Atoi(port)
	ip := net.ParseIP(host)
	return err == nil && portErr == nil && ip != nil && ip.IsLoopback() && number >= 1 && number <= 65535
}
