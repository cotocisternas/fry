// Package imapsource provides a single-session, read-only IMAP history reader.
// Methods must not run concurrently. The Dial context bounds the whole session.
// Finite searches bound honest-server results; the underlying parser still
// allocates LIST names and SEARCH response tokens before application validation.
package imapsource

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

const (
	MaxFolders = 4096
	MaxWindow  = 1024
)

type Config struct {
	Address, Username, Password string
	LoopbackPlaintext           bool
	OmitSubscriptions           bool
	MaxMessageBytes             int64
}
type Folder struct {
	Name       string
	Delimiter  rune
	Subscribed bool
}
type (
	Epoch    struct{ UIDValidity, UIDNext uint32 }
	Metadata struct {
		UID          uint32
		Size         int64
		InternalDate time.Time
		Flags        []string
	}
)

type Reader struct {
	client            *imapclient.Client
	conn              net.Conn
	stop              func() bool
	maxBytes          int64
	unexpected        atomic.Bool
	omitSubscriptions bool
}

func Dial(ctx context.Context, cfg Config) (*Reader, error) {
	host, err := cfg.validate()
	if err != nil {
		return nil, err
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	var conn net.Conn
	if cfg.LoopbackPlaintext {
		conn, err = dialer.DialContext(ctx, "tcp", cfg.Address)
	} else {
		conn, err = (&tls.Dialer{
			NetDialer: dialer,
			Config:    &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12},
		}).DialContext(
			ctx,
			"tcp",
			cfg.Address,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("imapsource: connection failed")
	}
	r := &Reader{conn: conn, maxBytes: cfg.MaxMessageBytes, omitSubscriptions: cfg.OmitSubscriptions}
	r.stop = context.AfterFunc(ctx, func() { closeAfterFailure(conn) })
	r.client = imapclient.New(
		conn,
		&imapclient.Options{
			StrictFetchRouting:           true,
			RejectDuplicateSearchResults: true,
			UnilateralDataHandler:        &imapclient.UnilateralDataHandler{Fetch: r.rejectUnexpectedFetch},
		},
	)
	if err = r.client.Login(cfg.Username, cfg.Password).Wait(); err != nil {
		closeAfterFailure(r)
		return nil, fmt.Errorf("imapsource: authentication failed")
	}
	return r, nil
}

func (cfg Config) validate() (string, error) {
	host, _, err := net.SplitHostPort(cfg.Address)
	if err != nil || cfg.Username == "" || cfg.Password == "" || cfg.MaxMessageBytes < 1 ||
		cfg.MaxMessageBytes > 256<<20 {
		return "", fmt.Errorf("imapsource: invalid configuration")
	}
	if cfg.LoopbackPlaintext && !net.ParseIP(host).IsLoopback() {
		return "", fmt.Errorf("imapsource: plaintext requires numeric loopback IP")
	}
	return host, nil
}

func (r *Reader) rejectUnexpectedFetch(msg *imapclient.FetchMessageData) {
	r.unexpected.Store(true)
	closeAfterFailure(r.conn)
	for item := msg.Next(); item != nil; item = msg.Next() {
		if b, ok := item.(imapclient.FetchItemDataBodySection); ok && b.Literal != nil {
			if _, err := io.Copy(io.Discard, b.Literal); err != nil {
				return
			}
		}
	}
}

func (r *Reader) Close() error { r.stop(); return r.conn.Close() }
func (r *Reader) Discover() ([]Folder, error) {
	cmd := r.client.List("", "*", nil)
	var out []Folder
	seen := map[string]bool{}
	count := 0
	for f := cmd.Next(); f != nil; f = cmd.Next() {
		count++
		if count > MaxFolders {
			closeAfterFailure(r)
			closeAfterFailure(cmd)
			return nil, fmt.Errorf("imapsource: folder limit exceeded")
		}
		if slices.Contains(f.Attrs, imap.MailboxAttrNoSelect) {
			continue
		}
		name := f.Mailbox
		key := folderKey(name)
		if name == "" || seen[key] {
			closeAfterFailure(r)
			closeAfterFailure(cmd)
			return nil, fmt.Errorf("imapsource: invalid or duplicate folder")
		}
		seen[key] = true
		subscribed := slices.Contains(f.Attrs, imap.MailboxAttrSubscribed)
		out = append(out, Folder{Name: name, Delimiter: f.Delim, Subscribed: subscribed})
	}
	if err := cmd.Close(); err != nil {
		return nil, err
	}

	if !r.omitSubscriptions {
		if err := r.applySubscriptions(out); err != nil {
			return nil, err
		}
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func folderKey(name string) string {
	if strings.EqualFold(name, "INBOX") {
		return "INBOX"
	}
	return name
}

// LSUB may contain nonexistent subscribed names and synthesized parents;
// only selectable folders returned by LIST can receive subscriptions.
func (r *Reader) applySubscriptions(out []Folder) error {
	cmd := r.client.LSub("", "*")
	subscribed := map[string]bool{}
	count := 0
	for f := cmd.Next(); f != nil; f = cmd.Next() {
		count++
		if count > MaxFolders {
			closeAfterFailure(r)
			closeAfterFailure(cmd)
			return fmt.Errorf("imapsource: subscription limit exceeded")
		}
		subscribed[folderKey(f.Mailbox)] = true
	}
	if err := cmd.Close(); err != nil {
		return err
	}
	for i := range out {
		out[i].Subscribed = subscribed[folderKey(out[i].Name)]
	}
	return nil
}

func (r *Reader) Select(name string) (Epoch, error) {
	s, err := r.client.Select(name, &imap.SelectOptions{ReadOnly: true}).Wait()
	if err != nil {
		return Epoch{}, err
	}
	if s.UIDValidity == 0 || s.UIDNext == 0 {
		return Epoch{}, fmt.Errorf("imapsource: invalid source epoch")
	}
	return Epoch{s.UIDValidity, uint32(s.UIDNext)}, nil
}

func (r *Reader) SearchWindow(first, last uint32) ([]uint32, error) {
	if first == 0 || last < first || uint64(last)-uint64(first)+1 > MaxWindow {
		return nil, fmt.Errorf("imapsource: invalid finite UID window")
	}
	set := imap.UIDSet{{Start: imap.UID(first), Stop: imap.UID(last)}}
	data, err := r.client.UIDSearch(&imap.SearchCriteria{UID: []imap.UIDSet{set}}, nil).Wait()
	if err != nil {
		return nil, err
	}
	result, ok := data.All.(imap.UIDSet)
	if !ok {
		return nil, fmt.Errorf("imapsource: expected UID results")
	}
	return expandUIDs(result, first, last)
}

func expandUIDs(result imap.UIDSet, first, last uint32) ([]uint32, error) {
	out := []uint32{}
	seen := map[uint32]bool{}
	for _, span := range result {
		a, b := uint32(span.Start), uint32(span.Stop)
		if a < first || b > last || a > b {
			return nil, fmt.Errorf("imapsource: out-of-range UID result")
		}
		for u := a; ; u++ {
			if seen[u] {
				return nil, fmt.Errorf("imapsource: duplicate UID result")
			}
			seen[u] = true
			out = append(out, u)
			if u == b {
				break
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

func (r *Reader) Metadata(uid uint32) (Metadata, bool, error) {
	var m Metadata
	if uid == 0 {
		return m, false, fmt.Errorf("imapsource: zero UID")
	}
	cmd := r.client.Fetch(
		imap.UIDSetNum(imap.UID(uid)),
		&imap.FetchOptions{UID: true, Flags: true, InternalDate: true, RFC822Size: true},
	)
	msg := cmd.Next()
	if msg == nil {
		err := cmd.Close()
		if r.unexpected.Load() {
			err = fmt.Errorf("imapsource: unsolicited FETCH")
		}
		return m, false, err
	}
	seen, invalid := readMetadata(msg, &m)

	if cmd.Next() != nil {
		invalid = true
		closeAfterFailure(r)
	}
	err := cmd.Close()
	if err != nil {
		return m, false, err
	}
	if invalid || r.unexpected.Load() || len(seen) != 4 || !m.valid(uid, r.maxBytes) {
		return m, false, fmt.Errorf("imapsource: invalid metadata")
	}
	return m, true, nil
}

func (m Metadata) valid(uid uint32, maxBytes int64) bool {
	return m.UID == uid && !m.InternalDate.IsZero() && m.Size >= 0 && m.Size <= maxBytes
}

func readMetadata(msg *imapclient.FetchMessageData, m *Metadata) (map[string]bool, bool) {
	seen := map[string]bool{}
	invalid := false
	for item := msg.Next(); item != nil; item = msg.Next() {
		key, valid := m.readItem(item)
		if !valid || seen[key] {
			invalid = true
		}
		seen[key] = true
	}
	return seen, invalid
}

func (m *Metadata) readItem(item imapclient.FetchItemData) (string, bool) {
	switch v := item.(type) {
	case imapclient.FetchItemDataUID:
		m.UID = uint32(v.UID)
		return "uid", true
	case imapclient.FetchItemDataFlags:
		flags, valid := metadataFlags(v.Flags)
		m.Flags = append(m.Flags, flags...)
		return "flags", valid
	case imapclient.FetchItemDataInternalDate:
		m.InternalDate = v.Time
		return "date", true
	case imapclient.FetchItemDataRFC822Size:
		m.Size = v.Size
		return "size", true
	default:
		return "", false
	}
}

func metadataFlags(flags []imap.Flag) ([]string, bool) {
	var out []string
	valid := true
	for _, f := range flags {
		flag := string(f)
		if flag == "" || strings.ContainsAny(flag, " (){}\r\n\t\x00") {
			valid = false
		}
		if flag == "\\Recent" {
			continue
		}
		if strings.HasPrefix(flag, "\\") &&
			!slices.Contains(
				[]imap.Flag{imap.FlagSeen, imap.FlagAnswered, imap.FlagFlagged, imap.FlagDeleted, imap.FlagDraft},
				f,
			) {
			valid = false
		}
		out = append(out, flag)
	}
	return out, valid
}

type verifiedLiteral struct {
	reader    io.Reader
	remaining int64
	conn      net.Conn
	finished  bool
	finalize  func() error
}

func (v *verifiedLiteral) Read(p []byte) (int, error) {
	if v.finished {
		return 0, io.EOF
	}
	n, err := v.reader.Read(p)
	v.remaining -= int64(n)
	if v.remaining < 0 {
		return n, fmt.Errorf("imapsource: oversized literal")
	}
	// Only the exact EOF sentinel proves framing; wrapped EOF can carry a protocol failure.
	if err == io.EOF { //nolint:errorlint // Integrity requires an unadorned EOF sentinel.
		if v.remaining != 0 {
			return n, io.ErrUnexpectedEOF
		}
		if err = v.finalize(); err != nil {
			closeAfterFailure(v.conn)
			return n, err
		}
		v.finished = true
	}
	return n, err
}

func (v *verifiedLiteral) Close() error {
	if !v.finished {
		return v.conn.Close()
	}
	return nil
}

// Fetch calls consume synchronously. Success requires consume to read through
// verified EOF. Closing an unfinished body cancels the entire session.
func (r *Reader) Fetch(uid uint32, size int64, consume func(io.ReadCloser) error) (bool, error) {
	if uid == 0 || size < 0 || size > r.maxBytes || consume == nil {
		return false, fmt.Errorf("imapsource: invalid fetch")
	}
	section := &imap.FetchItemBodySection{Peek: true}
	cmd := r.client.Fetch(
		imap.UIDSetNum(imap.UID(uid)),
		&imap.FetchOptions{UID: true, BodySection: []*imap.FetchItemBodySection{section}},
	)
	msg := cmd.Next()
	if msg == nil {
		err := cmd.Close()
		if r.unexpected.Load() {
			err = fmt.Errorf("imapsource: unsolicited FETCH")
		}
		return false, err
	}
	state := fetchState{reader: r, cmd: cmd, msg: msg, uid: uid}
	if err := state.consume(size, consume); err != nil {
		return false, errors.Join(err, r.Close(), cmd.Close())
	}
	return true, nil
}

func (s *fetchState) consume(size int64, consume func(io.ReadCloser) error) error {
	for item := s.msg.Next(); item != nil; item = s.msg.Next() {
		switch v := item.(type) {
		case imapclient.FetchItemDataUID:
			if s.uidSeen {
				return fmt.Errorf("imapsource: duplicate UID")
			}
			s.uidSeen = true
			s.observed = uint32(v.UID)
		case imapclient.FetchItemDataBodySection:
			return s.consumeLiteral(v, size, consume)
		default:
			return fmt.Errorf("imapsource: unexpected FETCH item")
		}
	}
	return fmt.Errorf("imapsource: missing literal")
}

func (s *fetchState) consumeLiteral(
	v imapclient.FetchItemDataBodySection,
	size int64,
	consume func(io.ReadCloser) error,
) error {
	if v.Literal == nil || !v.MatchCommand(&imap.FetchItemBodySection{Peek: true}) || v.Literal.Size() != size {
		return fmt.Errorf("imapsource: invalid literal")
	}
	body := &verifiedLiteral{reader: v.Literal, remaining: size, conn: s.reader.conn}
	body.finalize = s.finalize
	if err := consume(body); err != nil {
		return err
	}
	if !body.finished {
		return fmt.Errorf("imapsource: body not fully consumed")
	}
	return nil
}

type fetchState struct {
	reader        *Reader
	cmd           *imapclient.FetchCommand
	msg           *imapclient.FetchMessageData
	uid, observed uint32
	uidSeen       bool
}

func (s *fetchState) finalize() error {
	for tail := s.msg.Next(); tail != nil; tail = s.msg.Next() {
		u, ok := tail.(imapclient.FetchItemDataUID)
		if !ok || s.uidSeen {
			return fmt.Errorf("imapsource: unexpected trailing FETCH item")
		}
		s.uidSeen = true
		s.observed = uint32(u.UID)
	}
	if !s.uidSeen || s.observed != s.uid {
		return fmt.Errorf("imapsource: UID mismatch")
	}
	if s.cmd.Next() != nil {
		return fmt.Errorf("imapsource: extra FETCH message")
	}
	if err := s.cmd.Close(); err != nil {
		return err
	}
	if s.reader.unexpected.Load() {
		return fmt.Errorf("imapsource: unsolicited FETCH")
	}
	return nil
}
