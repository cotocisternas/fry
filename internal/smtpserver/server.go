// Package smtpserver serves bounded incoming SMTP and authenticated submission.
// Configured recipients and sender authorization are enforced before acceptance.
package smtpserver

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/cotocisternas/fry/internal/credentials"
	"github.com/cotocisternas/fry/internal/hotstore"
	"github.com/cotocisternas/fry/internal/maildirectory"
	"github.com/cotocisternas/fry/internal/receiptstore"
	"github.com/cotocisternas/fry/internal/telemetry"

	"github.com/emersion/go-smtp"
)

type Route struct {
	Address      string
	Organization receiptstore.ID
	Inboxes      []receiptstore.ID
}
type RecipientDirectory interface {
	Resolve(context.Context, string) (maildirectory.Route, error)
}

type Config struct {
	Telemetry *telemetry.Runtime
	// Transport is loopback (default), starttls (incoming),
	// starttls-required (submission), or tls (implicit TLS). Serve takes raw TCP.
	Transport                                  string
	TLSConfig                                  *tls.Config
	HandshakeTimeout                           time.Duration
	Directory                                  RecipientDirectory
	Submission                                 *SubmissionConfig
	Domain                                     string
	Routes                                     []Route
	MaxConnections, MaxInFlight, MaxRecipients int
	MaxMessageBytes                            int64
	IdleTimeout, IngestTimeout                 time.Duration
}
type resolved struct {
	org     receiptstore.ID
	inboxes []receiptstore.ID
}
type Server struct {
	passwords         *credentials.Verifier
	accounts          map[string]principal
	retryWindow       time.Duration
	smtp              *smtp.Server
	store             *receiptstore.Store
	config            Config
	routes            map[string]resolved
	slots             chan struct{}
	ctx               context.Context
	cancel            context.CancelFunc
	mu                sync.Mutex
	connections       map[*trackedConn]struct{}
	stopping, serving bool
	active            sync.WaitGroup
	accept            func(
		context.Context,
		receiptstore.ID,
		receiptstore.Envelope,
		io.ReadCloser,
	) (
		receiptstore.Receipt,
		error,
	)
	status func(context.Context, receiptstore.ID, receiptstore.ID) (receiptstore.Status, error)
}

func addressKey(address string) (string, string, error) { return maildirectory.AddressKey(address) }
func domainValid(domain string) bool                    { return maildirectory.DomainValid(domain) }
func New(store *receiptstore.Store, config Config) (*Server, error) {
	if store == nil || !validServiceRole(config) || !validServiceBounds(config) {
		return nil, fmt.Errorf("smtpserver: invalid configuration")
	}
	var err error
	config, err = normalizeTransport(config)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(telemetry.WithRuntime(context.Background(), config.Telemetry))
	s := &Server{
		store:       store,
		config:      config,
		routes:      map[string]resolved{},
		slots:       make(chan struct{}, config.MaxInFlight),
		ctx:         ctx,
		cancel:      cancel,
		connections: map[*trackedConn]struct{}{},
		accept:      store.Accept,
		status:      store.Status,
	}
	if err = s.configureRoutes(config.Routes); err != nil {
		cancel()
		return nil, err
	}

	if config.Submission != nil {
		if err := s.configureSubmission(*config.Submission); err != nil {
			cancel()
			return nil, err
		}
	}
	s.configureWireServer()

	return s, nil
}

// Serve takes a raw TCP listener. Implicit TLS is layered after admission so
// the protocol library sees *tls.Conn while shutdown tracks the raw socket.
func (s *Server) Serve(listener net.Listener) error {
	address, ok := listener.Addr().(*net.TCPAddr)
	if !ok || (s.config.Transport == transportLoopback && !address.IP.IsLoopback()) {
		return fmt.Errorf("smtpserver: raw TCP listener required; plaintext must bind loopback")
	}
	s.mu.Lock()
	if s.stopping || s.serving {
		s.mu.Unlock()
		return fmt.Errorf("smtpserver: already serving or stopped")
	}
	s.serving = true
	s.mu.Unlock()
	var admitted net.Listener = &limitedListener{Listener: listener, server: s}
	if s.config.Transport == transportImplicitTLS {
		admitted = tls.NewListener(admitted, s.config.TLSConfig)
	}
	return s.smtp.Serve(admitted)
}

// Shutdown stops admission, drains until ctx expires, then closes tracked raw
// transports and cancels receipt work. It waits for started storage calls to
// return, including their cancellation/rollback, before releasing the caller.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.stopping = true
	s.mu.Unlock()
	err := s.smtp.Shutdown(ctx)
	s.cancel()
	s.mu.Lock()
	connections := make([]*trackedConn, 0, len(s.connections))
	for c := range s.connections {
		connections = append(connections, c)
	}
	s.mu.Unlock()
	for _, c := range connections {
		closeAfterFailure(c)
	}
	s.active.Wait()
	return err
}

type limitedListener struct {
	net.Listener
	server *Server
}

func (l *limitedListener) Accept() (net.Conn, error) {
	for {
		raw, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		s := l.server
		s.mu.Lock()
		if s.stopping || len(s.connections) >= s.config.MaxConnections {
			s.mu.Unlock()
			s.rejectConnection(raw)
			continue
		}
		c := &trackedConn{Conn: raw, server: s}
		s.connections[c] = struct{}{}
		s.mu.Unlock()
		return c, nil
	}
}

type trackedConn struct {
	net.Conn
	server *Server
	once   sync.Once
	err    error
}

func (c *trackedConn) Close() error {
	c.once.Do(func() {
		c.err = c.Conn.Close()
		c.server.mu.Lock()
		delete(c.server.connections, c)
		c.server.mu.Unlock()
	})
	return c.err
}

type session struct {
	account *principal
	server  *Server
	conn    net.Conn
	mu      sync.Mutex
	current *transaction
}
type transaction struct {
	ctx      context.Context
	cancel   context.CancelFunc
	stop     func() bool
	release  sync.Once
	attempt  receiptstore.ID
	envelope receiptstore.Envelope
	started  bool
}

func (t *transaction) finish(s *Server) { t.release.Do(func() { t.stop(); t.cancel(); <-s.slots }) }
func (s *session) Reset() {
	s.mu.Lock()
	t := s.current
	s.current = nil
	if t != nil {
		t.stop()
		t.cancel()
		if !t.started {
			t.finish(s.server)
		}
	}
	s.mu.Unlock()
}
func (s *session) Logout() error { s.Reset(); return nil }
func (s *session) Mail(from string, _ *smtp.MailOptions) error {
	s.Reset()
	account, proof, err := s.authorizeMail(from)
	if err != nil {
		return err
	}

	s.server.mu.Lock()
	stopping := s.server.stopping
	s.server.mu.Unlock()
	if stopping {
		return temporary()
	}
	select {
	case s.server.slots <- struct{}{}:
	default:
		return temporary()
	}
	id, err := hotstore.NewID()
	if err != nil {
		<-s.server.slots
		return temporary()
	}
	ctx, cancel := context.WithTimeout(s.server.ctx, s.server.config.IngestTimeout)
	t := &transaction{ctx: ctx, cancel: cancel, attempt: id, envelope: receiptstore.Envelope{MailFrom: from}}
	if account != nil {
		t.envelope.Organization = account.org
		t.envelope.Outbound = &receiptstore.Outbound{
			Relay:               account.relay,
			RetryWindow:         s.server.retryWindow,
			SenderAuthorization: proof,
		}
		if account.credential != nil {
			t.envelope.Outbound.SubmissionAuthorization = &receiptstore.SubmissionAuthorization{
				Username: account.credential.Username,
				Version:  account.credential.Version,
			}
		}
	}
	t.stop = context.AfterFunc(ctx, func() { closeAfterFailure(s.conn) })
	s.mu.Lock()
	s.current = t
	s.mu.Unlock()
	return nil
}

func (s *session) Rcpt(to string, _ *smtp.RcptOptions) error {
	key, _, err := addressKey(to)
	if err != nil {
		return unknownRecipient()
	}
	if s.server.config.Submission != nil {
		return s.submissionRecipient(key)
	}
	route, err := s.server.resolveRecipient(key)
	if err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.current
	if t == nil || t.started {
		return temporary()
	}
	// The current receipt transaction has one organization. Defer an additional
	// organization's recipient so the sender can retry it in another transaction.
	if t.envelope.Organization != (receiptstore.ID{}) && t.envelope.Organization != route.org {
		return &smtp.SMTPError{
			Code:         451,
			EnhancedCode: smtp.EnhancedCode{4, 3, 0},
			Message:      "Use a separate transaction for this recipient",
		}
	}
	count := len(t.envelope.Destinations) + len(route.inboxes)
	if count > 1024 {
		return temporary()
	}
	t.envelope.Organization = route.org
	for _, inbox := range route.inboxes {
		t.envelope.Destinations = append(
			t.envelope.Destinations,
			receiptstore.Destination{Inbox: inbox, Recipients: []string{to}},
		)
	}
	return nil
}

func (s *session) Data(reader io.Reader) (result error) {
	t, err := s.beginData()
	if err != nil {
		return err
	}

	defer t.finish(s.server)
	operation := "smtp.receive"
	if s.server.config.Submission != nil {
		operation = "smtp.submit"
	}
	work, finish := telemetry.FromContext(t.ctx).Start(t.ctx, operation)
	defer func() { finish(result) }()
	if err = s.registerData(); err != nil {
		return err
	}
	defer s.server.active.Done()
	if err = s.validateDataAuthority(work, t, reader); err != nil {
		return err
	}

	source := &messageSource{Reader: reader, conn: s.conn}
	// A source closed before its first DATA read may keep the socket only until
	// this method proves framing EOF or chooses an indeterminate disconnect.
	defer source.finishDeferred()
	_, err = s.server.accept(work, t.attempt, t.envelope, source)
	if err == nil {
		return nil
	}
	return s.resolveDataFailure(work, t, source, err)
}

func senderUnauthorized() error {
	return &smtp.SMTPError{
		Code:         553,
		EnhancedCode: smtp.EnhancedCode{5, 7, 1},
		Message:      "Sender is not authorized for this organization",
	}
}

func temporary() error {
	return &smtp.SMTPError{
		Code:         451,
		EnhancedCode: smtp.EnhancedCode{4, 3, 0},
		Message:      "Temporary receipt failure; retry later",
	}
}

func unknownRecipient() error {
	return &smtp.SMTPError{Code: 550, EnhancedCode: smtp.EnhancedCode{5, 1, 1}, Message: "Recipient not configured"}
}

// Receipt acceptance owns Close. A DATA Close before its first Read defers raw
// socket closure for adapter-owned rejection handling. The same mutex prevents
// any later source Read from starting. Once a read has begun, Close still closes
// the socket immediately to interrupt it. BDAT pipes always close immediately.
type messageSource struct {
	io.Reader
	conn                           net.Conn
	mu                             sync.Mutex
	once                           sync.Once
	eof, started, closed, deferred bool
}

func (r *messageSource) Read(b []byte) (int, error) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return 0, io.ErrClosedPipe
	}
	r.started = true
	r.mu.Unlock()
	n, err := r.Reader.Read(b)
	if err == io.EOF {
		r.mu.Lock()
		r.eof = true
		r.mu.Unlock()
	}
	return n, err
}

func (r *messageSource) Close() error {
	r.once.Do(func() {
		r.mu.Lock()
		r.closed = true
		if r.eof {
			r.mu.Unlock()
			return
		}
		closer, canClose := r.Reader.(io.Closer)
		if !canClose && !r.started {
			r.deferred = true
			r.mu.Unlock()
			return
		}
		r.mu.Unlock()
		if canClose {
			closeAfterFailure(closer)
		} else {
			closeAfterFailure(r.conn)
		}
	})
	return nil
}

// finishDeferred runs before the ingest context is released. Accepted replay,
// unresolved authority, and every other exit without EOF remain disconnects.
func (r *messageSource) finishDeferred() {
	r.mu.Lock()
	closeSocket := r.deferred && !r.eof
	r.deferred = false
	r.mu.Unlock()
	if closeSocket {
		closeAfterFailure(r.conn)
	}
}

func (r *messageSource) drainUntouched(ctx context.Context, limit int64) error {
	r.mu.Lock()
	eof, deferred := r.eof, r.deferred
	r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if eof {
		return nil
	}
	if !deferred {
		// BDAT owns a closable pipe; its handler drains the advertised chunk.
		if _, ok := r.Reader.(io.Closer); ok {
			return nil
		}
		return io.ErrClosedPipe
	}
	return r.drainFraming(ctx, limit)
}

func (r *messageSource) drainFraming(ctx context.Context, limit int64) error {
	// The source wrapper is already closed, so read its original protocol reader.
	// The extra byte detects oversize; only the reader's own EOF proves framing.
	var buffer [32 << 10]byte
	var count int64
	emptyReads := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		remaining := limit + 1 - count
		n, err := r.Reader.Read(buffer[:min(int64(len(buffer)), remaining)])
		count += int64(n)
		if count > limit {
			return hotstore.ErrTooLarge
		}
		if err == io.EOF {
			return r.recordFramingEOF(ctx)
		}

		if err != nil {
			return err
		}
		if n == 0 {
			emptyReads++
			if emptyReads >= 100 {
				return io.ErrNoProgress
			}
		} else {
			emptyReads = 0
		}
	}
}

func (r *messageSource) recordFramingEOF(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	r.eof = true
	r.mu.Unlock()
	return nil
}
