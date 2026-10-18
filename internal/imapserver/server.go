//go:build linux

// Package imapserver provides a deliberately limited IMAP adapter with explicit secure transports.
package imapserver

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/cotocisternas/fry/internal/credentials"
	"github.com/cotocisternas/fry/internal/maildirectory"
	"github.com/cotocisternas/fry/internal/receiptstore"
	"github.com/cotocisternas/fry/internal/telemetry"

	"github.com/emersion/go-imap/v2"
	wire "github.com/emersion/go-imap/v2/imapserver"
)

type Account struct {
	Username, Password  string
	Organization, Inbox receiptstore.ID
}
type credential struct {
	org, inbox receiptstore.ID
}
type MailboxDirectory interface {
	Authenticate(context.Context, string, string) (maildirectory.Principal, error)
	Validate(context.Context, maildirectory.Principal) error
}

type Server struct {
	closeOnce    sync.Once
	closeErr     error
	transport    Options
	connections  map[*transportConn]struct{}
	handshakes   sync.WaitGroup
	serveMu      sync.Mutex
	serveStarted bool
	appendLimit  uint32
	mimeSlots    chan struct{}
	directory    MailboxDirectory
	store        *receiptstore.Store
	wire         *wire.Server
	users        map[string]credential
	passwords    *credentials.Verifier
	ctx          context.Context
	cancel       context.CancelFunc
	mu           sync.Mutex
	sessions     int
	closing      bool
	wg           sync.WaitGroup
}

func New(store *receiptstore.Store, accounts []Account) (*Server, error) {
	return NewWithOptions(store, accounts, Options{})
}

func NewWithDirectory(store *receiptstore.Store, directory MailboxDirectory) (*Server, error) {
	if directory == nil {
		return nil, fmt.Errorf("imap: directory required")
	}
	return NewWithDirectoryOptions(store, directory, Options{})
}

func NewWithOptions(store *receiptstore.Store, accounts []Account, opts Options) (*Server, error) {
	return newServer(store, accounts, nil, opts)
}

func NewWithDirectoryOptions(store *receiptstore.Store, directory MailboxDirectory, opts Options) (*Server, error) {
	if directory == nil {
		return nil, fmt.Errorf("imap: directory required")
	}
	return newServer(store, nil, directory, opts)
}

func newServer(
	store *receiptstore.Store,
	accounts []Account,
	directory MailboxDirectory,
	opts Options,
) (*Server, error) {
	opts, err := normalizeTransport(opts)
	if err != nil {
		return nil, err
	}

	if store == nil || (len(accounts) == 0 && directory == nil) {
		return nil, fmt.Errorf("imap: store and accounts required")
	}
	ctx, cancel := context.WithCancel(telemetry.WithRuntime(context.Background(), opts.Telemetry))
	s := &Server{
		transport:   opts,
		connections: map[*transportConn]struct{}{},
		mimeSlots:   make(chan struct{}, 4),
		appendLimit: 256 << 20,
		directory:   directory,
		store:       store,
		users:       map[string]credential{},
		ctx:         ctx,
		cancel:      cancel,
	}
	if err = s.configureAccounts(accounts); err != nil {
		cancel()
		return nil, err
	}

	caps := make(imap.CapSet)
	for _, capability := range []imap.Cap{imap.CapIMAP4rev1, imap.CapMove, imap.CapUIDPlus} {
		caps[capability] = struct{}{}
	}
	s.wire = wire.New(
		&wire.Options{Caps: caps, InsecureAuth: opts.Transport == transportLoopback, NewSession: s.newSession},
	)

	return s, nil
}

func (s *Server) Serve(l net.Listener) error {
	address, ok := l.Addr().(*net.TCPAddr)
	if !ok || (s.transport.Transport == transportLoopback && !address.IP.IsLoopback()) {
		return fmt.Errorf("imap: plaintext transport requires numeric loopback listener")
	}
	s.serveMu.Lock()
	if s.serveStarted {
		s.serveMu.Unlock()
		return fmt.Errorf("imap: server already serving")
	}
	s.serveStarted = true
	s.serveMu.Unlock()
	tracked := &admissionListener{Listener: l, server: s}
	var listener net.Listener = tracked
	if s.transport.Transport == transportImplicitTLS {
		listener = newHandshakeListener(s, tls.NewListener(tracked, s.transport.TLS))
	}
	defer closeAfterFailure(listener)
	return s.wire.Serve(listener)
}

func (s *Server) Close() error {
	// Every caller joins the same complete shutdown, including handshakes that
	// may start while the wire server is closing its registered listener.
	s.closeOnce.Do(func() { s.closeErr = s.close() })
	return s.closeErr
}

func (s *Server) close() error {
	s.mu.Lock()
	s.closing = true
	for connection := range s.connections {
		closeAfterFailure(connection.Conn)
	}
	s.mu.Unlock()
	s.cancel()
	err := s.wire.Close()
	s.handshakes.Wait()
	s.wg.Wait()
	return err
}
func no(text string) error { return &imap.Error{Type: imap.StatusResponseTypeNo, Text: text} }
func safe(err error) error {
	if err != nil {
		return no("Mailbox operation unavailable")
	}
	return nil
}
func unsupported() error { return no("Operation not implemented by this prototype") }

type session struct {
	principal maildirectory.Principal
	server    *Server
	conn      *wire.Conn
	account   credential
	logged    bool
	selection *receiptstore.MailboxSelection
	validity  uint32
	keywords  []string
	readOnly  bool
	view      []receiptstore.Mail
}

func (s *session) operation() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(s.server.ctx, 30*time.Second)
	if s.server.directory != nil && s.logged {
		if err := s.server.directory.Validate(ctx, s.principal); err != nil {
			cancel()
		}
	}
	return ctx, cancel
}

func (s *session) Close() error {
	s.server.mu.Lock()
	s.server.sessions--
	s.server.mu.Unlock()
	s.server.wg.Done()
	return nil
}

func (s *session) Login(user, password string) error {
	if s.server.directory != nil {
		ctx, cancel := context.WithTimeout(s.server.ctx, 30*time.Second)
		defer cancel()
		p, err := s.server.directory.Authenticate(ctx, user, password)
		if err != nil {
			return wire.ErrAuthFailed
		}
		s.principal = p
		s.account = credential{org: p.Organization, inbox: p.Inbox}
		s.logged = true
		return nil
	}
	if !s.server.passwords.Verify(user, password) {
		return wire.ErrAuthFailed
	}
	c := s.server.users[user]
	s.account = c
	s.logged = true
	return nil
}

var systemFlags = []imap.Flag{imap.FlagSeen, imap.FlagAnswered, imap.FlagFlagged, imap.FlagDeleted, imap.FlagDraft}

func (s *session) Select(name string, o *imap.SelectOptions) (*imap.SelectData, error) {
	ctx, cancel := s.operation()
	defer cancel()
	selection, f, m, err := s.server.store.SelectMailbox(ctx, s.account.org, s.account.inbox, name)
	if err != nil {
		return nil, safe(err)
	}
	if f.UIDNext > 4294967295 {
		return nil, no("Mailbox UID space exhausted")
	}
	s.selection = selection
	s.validity = f.UIDValidity
	s.view = m
	s.keywords = f.Keywords
	s.readOnly = o.ReadOnly
	count, err := wireCount(len(m))
	if err != nil {
		return nil, err
	}
	result := &imap.SelectData{
		Flags:          mailboxFlags(f.Keywords),
		PermanentFlags: append(append([]imap.Flag(nil), systemFlags...), imap.FlagWildcard),
		NumMessages:    count,
		UIDValidity:    f.UIDValidity,
		UIDNext:        imap.UID(f.UIDNext),
	}
	if o.ReadOnly {
		result.PermanentFlags = nil
	}
	for i, msg := range m {
		if !hasFlag(msg, "\\Seen") {
			result.FirstUnseenSeqNum = uint32(i + 1)
			break
		}
	}
	return result, nil
}
func (s *session) Unselect() error { s.selection = nil; s.view = nil; s.keywords = nil; return nil }
func (s *session) Create(name string, _ *imap.CreateOptions) error {
	ctx, c := s.operation()
	defer c()
	return safe(s.server.store.CreateFolder(ctx, s.account.org, s.account.inbox, name))
}

func (s *session) Delete(name string) error {
	ctx, c := s.operation()
	defer c()
	return safe(s.server.store.DeleteFolder(ctx, s.account.org, s.account.inbox, name))
}

func (s *session) Rename(name, newName string, _ *imap.RenameOptions) error {
	ctx, c := s.operation()
	defer c()
	err := s.server.store.RenameFolder(ctx, s.account.org, s.account.inbox, name, newName)
	return safe(err)
}
func (s *session) Subscribe(name string) error   { return s.subscribe(name, true) }
func (s *session) Unsubscribe(name string) error { return s.subscribe(name, false) }
func (s *session) subscribe(name string, value bool) error {
	ctx, c := s.operation()
	defer c()
	return safe(s.server.store.SubscribeFolder(ctx, s.account.org, s.account.inbox, name, value))
}

func (s *session) List(w *wire.ListWriter, ref string, patterns []string, o *imap.ListOptions) error {
	ctx, c := s.operation()
	defer c()
	if !listSupported(o) {
		return unsupported()
	}

	if len(patterns) == 0 {
		if w.IsLSUB() {
			return nil
		}
		return w.WriteList(&imap.ListData{Mailbox: "", Delim: '/', Attrs: []imap.MailboxAttr{imap.MailboxAttrNoSelect}})
	}
	folders, err := s.server.store.ListFolders(ctx, s.account.org, s.account.inbox, o.SelectSubscribed)
	if err != nil {
		return safe(err)
	}
	entries := listEntries(folders, ref, patterns, w.IsLSUB())
	return writeListEntries(w, ref, patterns, o, entries)
}

func (s *session) Status(name string, o *imap.StatusOptions) (*imap.StatusData, error) {
	ctx, c := s.operation()
	defer c()
	f, m, err := s.server.store.Mailbox(ctx, s.account.org, s.account.inbox, name)
	if err != nil {
		return nil, safe(err)
	}
	result := &imap.StatusData{Mailbox: f.Name}
	count, err := wireCount(len(m))
	if err != nil {
		return nil, err
	}
	unseen := unseenMessages(m)
	var recent uint32

	if o.NumMessages {
		result.NumMessages = &count
	}
	if o.NumRecent {
		result.NumRecent = &recent
	}
	if o.NumUnseen {
		result.NumUnseen = &unseen
	}
	if o.UIDNext {
		if f.UIDNext > 4294967295 {
			return nil, no("Mailbox UID space exhausted")
		}
		result.UIDNext = imap.UID(f.UIDNext)
	}
	if o.UIDValidity {
		result.UIDValidity = f.UIDValidity
	}
	return result, nil
}

func (s *session) Copy(set imap.NumSet, destination string) (*imap.CopyData, error) {
	ctx, c := s.operation()
	defer c()
	result, err := s.selection.CopyTo(ctx, destination, s.selectedUIDs(set), false)
	if err != nil {
		return nil, safe(err)
	}
	return copyData(result), nil
}

func copyData(result receiptstore.CopyResult) *imap.CopyData {
	if len(result.Source) == 0 {
		return nil
	}
	data := &imap.CopyData{UIDValidity: result.UIDValidity}
	for _, u := range result.Source {
		data.SourceUIDs.AddNum(imap.UID(u))
	}
	for _, u := range result.Destination {
		data.DestUIDs.AddNum(imap.UID(u))
	}
	return data
}

func (s *session) Move(w *wire.MoveWriter, set imap.NumSet, destination string) error {
	if s.readOnly {
		return no("Mailbox is read-only")
	}
	ctx, c := s.operation()
	defer c()
	result, err := s.selection.CopyTo(ctx, destination, s.selectedUIDs(set), true)
	if err != nil {
		return safe(err)
	}
	if len(result.Source) > 0 {
		if err = w.WriteCopyData(copyData(result)); err != nil {
			return err
		}
	}
	current, err := s.current()
	if err != nil {
		return err
	}
	s.view, err = compactView(s.view, func(seq uint32, m receiptstore.Mail) (receiptstore.Mail, bool, error) {
		if _, exists := current[m.UID]; exists {
			return m, false, nil
		}
		return m, true, w.WriteExpunge(seq)
	})
	return err
}

func mailboxFlags(keywords []string) []imap.Flag {
	flags := append([]imap.Flag(nil), systemFlags...)
	for _, keyword := range keywords {
		flags = append(flags, imap.Flag(keyword))
	}
	return flags
}

// wireCount rejects counts which cannot be represented by IMAP sequence numbers.
func wireCount(n int) (uint32, error) {
	if n < 0 || n > 4294967295 {
		return 0, no("Mailbox message count exceeds wire range")
	}
	return uint32(n), nil
}

func (s *Server) configureAccounts(accounts []Account) error {
	passwords := map[string]string{}
	for _, a := range accounts {
		if a.Username == "" || a.Password == "" || a.Organization == (receiptstore.ID{}) ||
			a.Inbox == (receiptstore.ID{}) {
			return fmt.Errorf("imap: incomplete account")
		}
		if _, ok := s.users[a.Username]; ok {
			return fmt.Errorf("imap: duplicate account")
		}
		passwords[a.Username] = a.Password
		s.users[a.Username] = credential{org: a.Organization, inbox: a.Inbox}
	}
	if s.directory == nil {
		var err error
		s.passwords, err = credentials.New(passwords)
		return err
	}
	return nil
}

func (s *Server) newSession(c *wire.Conn) (wire.Session, *wire.GreetingData, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing || s.sessions >= s.transport.MaxConnections {
		return nil, nil, no("Connection capacity reached")
	}
	s.sessions++
	s.wg.Add(1)
	return &session{server: s, conn: c}, nil, nil
}

func unseenMessages(messages []receiptstore.Mail) uint32 {
	var count uint32
	for _, message := range messages {
		if !hasFlag(message, "\\Seen") {
			count++
		}
	}
	return count
}
