//go:build linux

package imapserver

import (
	"bytes"
	"context"
	"errors"
	"net/mail"
	"strings"

	"github.com/cotocisternas/fry/internal/receiptstore"
	"github.com/cotocisternas/fry/internal/telemetry"

	"github.com/emersion/go-imap/v2"
	wire "github.com/emersion/go-imap/v2/imapserver"
)

const maxHeaderBytes = receiptstore.MaxHeaderBytes

// Header metadata is receipt-scoped and erased by the receipt cleanup authority.
func (s *session) headers(ctx context.Context, m receiptstore.Mail) ([]byte, mail.Header, error) {
	raw, err := s.server.store.Header(ctx, s.account.org, m.Receipt)
	if err != nil {
		return nil, nil, err
	}
	parsed, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return nil, nil, err
	}
	return raw, parsed.Header, nil
}

// Report operation deadlines without claiming a header exceeded the parser
// bound. Never expose database or storage error details in a protocol response.
func headerFailure(ctx context.Context, err error) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return no("Header operation timed out")
	}
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		return no("Header operation canceled")
	}
	if errors.Is(err, receiptstore.ErrHeaderUnavailable) {
		return no("Header unavailable within parsing limit")
	}
	return safe(err)
}

func (s *session) Fetch(w *wire.FetchWriter, set imap.NumSet, o *imap.FetchOptions) (resultErr error) {
	plan, err := s.planFetch(o)
	if err != nil {
		return err
	}
	ctx, cancel := s.operation()
	defer cancel()
	ctx, finish := telemetry.FromContext(ctx).Start(ctx, "imap.fetch")
	defer func() { finish(resultErr) }()
	release, err := s.beginFetch(ctx, set, plan)
	if err != nil {
		return err
	}
	defer release()

	current, err := s.current()
	if err != nil {
		return err
	}
	for i, old := range s.view {
		if !s.matches(set, uint32(i+1), old.UID) {
			continue
		}
		message, exists := current[old.UID]
		if !exists {
			continue
		}
		data, err := s.prepareFetch(ctx, message, o, plan)
		if err != nil {
			return err
		}
		seq, err := wireCount(i + 1)
		if err != nil {
			return err
		}
		if err = s.writeFetch(ctx, w, seq, message, o, plan, data); err != nil {
			return err
		}
	}
	return nil
}

func foldContains(text, query string) bool {
	return strings.Contains(strings.ToLower(text), strings.ToLower(query))
}

// filterHeader preserves field order, repeated fields, original folding and line
// endings. The terminating blank line is always included, even for no matches.
func filterHeader(header []byte, include, exclude []string) []byte {
	names := map[string]bool{}
	for _, name := range include {
		names[strings.ToLower(name)] = true
	}
	for _, name := range exclude {
		names[strings.ToLower(name)] = true
	}
	result := make([]byte, 0, len(header))
	keep := false
	for len(header) > 0 {
		index := bytes.IndexByte(header, '\n')
		if index < 0 {
			index = len(header) - 1
		}
		line := header[:index+1]
		header = header[index+1:]
		if bytes.Equal(line, []byte("\r\n")) || bytes.Equal(line, []byte("\n")) {
			result = append(result, line...)
			break
		}
		if line[0] != ' ' && line[0] != '\t' {
			keep = keepHeaderLine(line, names, len(exclude) > 0)
		}
		if keep {
			result = append(result, line...)
		}
	}
	return result
}

func keepHeaderLine(line []byte, names map[string]bool, exclude bool) bool {
	colon := bytes.IndexByte(line, ':')
	name := ""
	if colon >= 0 {
		name = strings.ToLower(string(line[:colon]))
	}
	keep := names[name]
	if exclude {
		return !keep
	}
	return keep
}

func (s *session) beginFetch(ctx context.Context, set imap.NumSet, plan fetchPlan) (func(), error) {
	release := func() {}
	if plan.needsIndex {
		select {
		case s.server.mimeSlots <- struct{}{}:
			release = func() { <-s.server.mimeSlots }
		case <-ctx.Done():
			return nil, safe(ctx.Err())
		}
	}
	if plan.markSeen {
		if err := s.selection.ChangeFlags(ctx, s.selectedUIDs(set), "add", []string{"\\Seen"}); err != nil {
			release()
			return nil, safe(err)
		}
	}
	return release, nil
}
