//go:build linux

package imapserver

import (
	"context"
	"mime"
	"net/mail"
	"net/textproto"
	"strings"
	"time"

	"github.com/cotocisternas/fry/internal/receiptstore"
	"github.com/cotocisternas/fry/internal/telemetry"

	"github.com/emersion/go-imap/v2"
	wire "github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-message/charset"
)

// Search compares the human-readable header text. Decode only fields actually
// queried, so an unrelated unknown charset cannot break a date/flag search.
func decodeSearchHeaders(h mail.Header, criteria *imap.SearchCriteria) error {
	keys := map[string]bool{}
	var visit func(*imap.SearchCriteria)
	visit = func(c *imap.SearchCriteria) {
		for _, field := range c.Header {
			if field.Value != "" {
				keys[textproto.CanonicalMIMEHeaderKey(field.Key)] = true
			}
		}
		for i := range c.Not {
			visit(&c.Not[i])
		}
		for i := range c.Or {
			visit(&c.Or[i][0])
			visit(&c.Or[i][1])
		}
	}
	visit(criteria)
	decoder := mime.WordDecoder{CharsetReader: charset.Reader}
	for key := range keys {
		for i, value := range h[key] {
			decoded, err := decoder.DecodeHeader(value)
			if err != nil {
				return err
			}
			h[key][i] = decoded
		}
	}
	return nil
}

func searchSupported(c *imap.SearchCriteria) bool {
	if len(c.Body) > 0 || len(c.Text) > 0 || c.ModSeq != nil {
		return false
	}
	for i := range c.Not {
		if !searchSupported(&c.Not[i]) {
			return false
		}
	}
	for _, pair := range c.Or {
		if !searchSupported(&pair[0]) || !searchSupported(&pair[1]) {
			return false
		}
	}
	return true
}

func requiresHeader(c *imap.SearchCriteria) bool {
	if len(c.Header) > 0 || !c.SentSince.IsZero() || !c.SentBefore.IsZero() {
		return true
	}
	for i := range c.Not {
		if requiresHeader(&c.Not[i]) {
			return true
		}
	}
	for _, pair := range c.Or {
		if requiresHeader(&pair[0]) || requiresHeader(&pair[1]) {
			return true
		}
	}
	return false
}

func day(t time.Time) time.Time { y, m, d := t.Date(); return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }

func dateMatches(date, since, before time.Time) bool {
	d := day(date)
	return (since.IsZero() || !d.Before(day(since))) && (before.IsZero() || d.Before(day(before)))
}

func (s *session) searchMatch(c *imap.SearchCriteria, seq uint32, m receiptstore.Mail, h mail.Header) bool {
	if !s.searchNumbers(c, seq, m) {
		return false
	}

	if !searchMessageCriteria(c, m, h) {
		return false
	}

	for i := range c.Not {
		if s.searchMatch(&c.Not[i], seq, m, h) {
			return false
		}
	}
	for _, pair := range c.Or {
		if !s.searchMatch(&pair[0], seq, m, h) && !s.searchMatch(&pair[1], seq, m, h) {
			return false
		}
	}
	return true
}

func (s *session) Search(
	kind wire.NumKind,
	c *imap.SearchCriteria,
	o *imap.SearchOptions,
) (_ *imap.SearchData, resultErr error) {
	if !searchSupported(c) || o.ReturnSave {
		return nil, unsupported()
	}
	current, err := s.current()
	if err != nil {
		return nil, err
	}
	ctx, cancel := s.operation()
	defer cancel()
	ctx, finish := telemetry.FromContext(ctx).Start(ctx, "imap.search")
	defer func() { finish(resultErr) }()
	needsHeader := requiresHeader(c)
	var seqs imap.SeqSet
	var uids imap.UIDSet
	for i, old := range s.view {
		m, ok := current[old.UID]
		if !ok {
			continue
		}
		h, err := s.searchHeader(ctx, m, c, needsHeader)
		if err != nil {
			return nil, err
		}

		if s.searchMatch(c, uint32(i+1), m, h) {
			seqs.AddNum(uint32(i + 1))
			uids.AddNum(imap.UID(m.UID))
		}
	}
	if kind == wire.NumKindUID {
		return &imap.SearchData{All: uids}, nil
	}
	return &imap.SearchData{All: seqs}, nil
}

func searchMessageCriteria(c *imap.SearchCriteria, m receiptstore.Mail, h mail.Header) bool {
	return searchDates(c, m, h) && searchFlags(c, m) && searchSize(c, m) && searchHeaders(c, h)
}

func searchDates(c *imap.SearchCriteria, m receiptstore.Mail, h mail.Header) bool {
	if !dateMatches(m.Date, c.Since, c.Before) {
		return false
	}
	if !c.SentSince.IsZero() || !c.SentBefore.IsZero() {
		date, err := h.Date()
		if err != nil || !dateMatches(date, c.SentSince, c.SentBefore) {
			return false
		}
	}
	return true
}

func searchFlags(c *imap.SearchCriteria, m receiptstore.Mail) bool {
	for _, flag := range c.Flag {
		if !hasSearchFlag(m, string(flag)) {
			return false
		}
	}
	for _, flag := range c.NotFlag {
		if hasSearchFlag(m, string(flag)) {
			return false
		}
	}
	return true
}

func hasSearchFlag(m receiptstore.Mail, flag string) bool {
	for _, candidate := range m.Flags {
		if strings.EqualFold(candidate, flag) {
			return true
		}
	}
	return false
}

func searchSize(c *imap.SearchCriteria, m receiptstore.Mail) bool {
	return (c.Larger <= 0 || m.Size > c.Larger) && (c.Smaller <= 0 || m.Size < c.Smaller)
}

func searchHeaders(c *imap.SearchCriteria, h mail.Header) bool {
	for _, field := range c.Header {
		if !searchHeaderContains(h, field.Key, field.Value) {
			return false
		}
	}
	return true
}

func searchHeaderContains(h mail.Header, key, query string) bool {
	for name, values := range h {
		if !strings.EqualFold(name, key) {
			continue
		}
		for _, value := range values {
			if foldContains(value, query) {
				return true
			}
		}
	}
	return false
}

func (s *session) searchHeader(
	ctx context.Context,
	m receiptstore.Mail,
	c *imap.SearchCriteria,
	needed bool,
) (mail.Header, error) {
	if !needed {
		return mail.Header{}, nil
	}
	_, h, err := s.headers(ctx, m)
	if err != nil {
		return nil, headerFailure(ctx, err)
	}
	if err = decodeSearchHeaders(h, c); err != nil {
		return nil, no("Search header charset unavailable")
	}
	return h, nil
}

func (s *session) searchNumbers(c *imap.SearchCriteria, seq uint32, m receiptstore.Mail) bool {
	for _, set := range c.SeqNum {
		if !s.matches(set, seq, m.UID) {
			return false
		}
	}
	for _, set := range c.UID {
		if !s.matches(set, seq, m.UID) {
			return false
		}
	}
	return true
}
