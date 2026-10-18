package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"time"
)

func (o commandOptions) signIn(ctx context.Context) (*client, error) {
	if err := validOrigin(o.origin); err != nil {
		return nil, err
	}
	password := os.Getenv("FRY_ADMIN_PASSWORD")
	if password == "" {
		return nil, fmt.Errorf("FRY_ADMIN_PASSWORD is required")
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	c := &client{
		origin: o.origin,
		http: &http.Client{
			Jar:     jar,
			Timeout: 5 * time.Minute,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return fmt.Errorf("unexpected administration redirect")
			},
		},
	}
	raw, err := c.call(ctx, http.MethodPost, "login", map[string]string{"username": o.username, "password": password})
	if err != nil {
		return nil, err
	}
	var who struct {
		CSRF string `json:"csrf"`
	}
	if err = json.Unmarshal(raw, &who); err != nil || who.CSRF == "" {
		return nil, fmt.Errorf("invalid session response")
	}
	c.csrf = who.CSRF
	return c, nil
}

func validOrigin(origin string) error {
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" ||
		parsed.Fragment != "" {
		return fmt.Errorf("invalid origin")
	}
	if parsed.Scheme == "https" {
		return nil
	}
	return validLoopbackOrigin(parsed)
}

func validLoopbackOrigin(parsed *url.URL) error {
	ip := net.ParseIP(parsed.Hostname())
	if parsed.Scheme != "http" || ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("HTTP is restricted to numeric loopback addresses")
	}

	return nil
}
