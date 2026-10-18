// Command fry-admin uses the same authorization and audits as the web interface.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
)

type client struct {
	origin string
	http   *http.Client
	csrf   string
}

func (c *client) request(ctx context.Context, method, path string, data any) (*http.Response, error) {
	var body io.Reader
	if data != nil {
		raw, err := json.Marshal(data)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(raw)
	}
	// signIn validates the operator-selected origin as HTTPS or numeric loopback and refuses redirects.
	// Paths are constructed from local command selectors and escaped identifiers, never email content.
	// #nosec G704 -- Intentional administration destination; owner: admin maintainers; review by 2027-01-18.
	request, err := http.NewRequestWithContext(ctx, method, c.origin+"/api/"+path, body)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Origin", c.origin)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-FRY-CSRF", c.csrf)
	// #nosec G704 -- Same validated origin and redirect policy above; owner: admin maintainers; review by 2027-01-18.
	response, err := c.http.Do(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != 200 {
		defer warnClose(response.Body, "administration response")
		var problem struct {
			Error string `json:"error"`
		}
		if err := json.NewDecoder(io.LimitReader(response.Body, 65536)).Decode(&problem); err != nil {
			problem.Error = response.Status
		}
		if problem.Error == "" {
			problem.Error = response.Status
		}
		return nil, fmt.Errorf("%s", problem.Error)
	}
	return response, nil
}

func submissionPolicyList(raw string) []string {
	result := []string{}
	for entry := range strings.SplitSeq(raw, ",") {
		if entry = strings.TrimSpace(entry); entry != "" {
			result = append(result, entry)
		}
	}
	return result
}

func submissionProvisioning(
	command, org, username, senders, domains string,
	enabled bool,
) (string, map[string]any, error) {
	data := map[string]any{
		"organization":       org,
		"username":           username,
		"authorized_senders": submissionPolicyList(senders),
		"authorized_domains": submissionPolicyList(domains),
	}
	if command == "submission-policy-set" {
		return "submission-policy", data, nil
	}
	password := os.Getenv("FRY_NEW_SUBMISSION_PASSWORD")
	if password == "" {
		return "", nil, fmt.Errorf("FRY_NEW_SUBMISSION_PASSWORD is required")
	}
	data["password"], data["enabled"] = password, enabled
	return "submission-accounts", data, nil
}

func storageQuotaPayload(org, raw string, disable bool) (map[string]any, error) {
	if disable == (raw != "") {
		return nil, fmt.Errorf("choose exactly one of -limit-bytes or -disable-limit")
	}
	var limit any
	if !disable {
		for _, c := range raw {
			if c < '0' || c > '9' {
				return nil, fmt.Errorf("invalid -limit-bytes")
			}
		}
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("invalid -limit-bytes")
		}
		limit = raw
	}
	return map[string]any{"organization": org, "limit_bytes": limit}, nil
}

func memberStoragePath(member, after string, limit int) (string, error) {
	if !validMemberID(member) {
		return "", fmt.Errorf("a valid -member ID is required")
	}
	if after != "" && !validMemberID(after) {
		return "", fmt.Errorf("invalid -after file ID")
	}
	if limit < 1 || limit > 64 {
		return "", fmt.Errorf("-limit must be between 1 and 64")
	}
	query := url.Values{"member": {member}, "limit": {strconv.Itoa(limit)}}
	if after != "" {
		query.Set("after", after)
	}
	return "member-storage?" + query.Encode(), nil
}

func run(ctx context.Context, args []string) error {
	o, err := parseCommand(args, os.Stderr)
	if err != nil {
		return err
	}
	var plan commandPlan
	if o.command == "download" {
		if o.output == "" {
			return &commandUsageError{cause: fmt.Errorf("-out is required for a message download")}
		}
	} else {
		plan, err = o.plan()
		if err != nil {
			return &commandUsageError{cause: err}
		}
	}
	c, err := o.signIn(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if _, err := c.call(ctx, http.MethodPost, "logout", map[string]bool{}); err != nil {
			log.Print("administration logout incomplete")
		}
	}()
	if o.command == "download" {
		return o.download(ctx, c)
	}
	raw, err := c.call(ctx, plan.method, plan.path, plan.data)
	if err != nil {
		return err
	}
	return printResult(raw, plan.method)
}

func printResult(raw []byte, method string) error {
	var formatted bytes.Buffer
	if err := json.Indent(&formatted, raw, "", "  "); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(os.Stdout, strings.TrimSpace(formatted.String())); err != nil {
		if method == http.MethodPost {
			return fmt.Errorf("administration command completed but output failed; inspect state before retrying")
		}
		return fmt.Errorf("could not write administration output")
	}
	return nil
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:])
	cancel()
	code := commandExitCode(err)
	if code != 0 {
		if _, writeErr := fmt.Fprintln(os.Stderr, "fry-admin:", err); writeErr != nil {
			os.Exit(code)
		}
		os.Exit(code)
	}
}

// Usage errors never reach the server; other errors may follow a submitted
// mutation and must not imply that blindly retrying it is safe.
func commandExitCode(err error) int {
	if err == nil || errors.Is(err, flag.ErrHelp) {
		return 0
	}
	if _, ok := errors.AsType[*commandUsageError](err); ok {
		return 2
	}
	return 1
}

// A response close or logout failure cannot roll back an acknowledged mutation.
func warnClose(c io.Closer, operation string) {
	if err := c.Close(); err != nil {
		log.Printf("could not close %s", operation)
	}
}

func validMemberID(id string) bool {
	if len(id) != 32 || id == strings.Repeat("0", 32) {
		return false
	}
	for _, c := range id {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
