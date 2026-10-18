//go:build linux

package admin

import (
	"context"
	"errors"
	"net"
	"net/smtp"
	"testing"
	"time"

	"github.com/cotocisternas/fry/internal/imapserver"
	"github.com/cotocisternas/fry/internal/maildirectory"
	"github.com/cotocisternas/fry/internal/smtpserver"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

func TestManagedDirectoryIsolationAndPersistence(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	manager := f.manager(t, false)
	directory := maildirectory.New(f.db)
	domain := maildirectory.Domain{Name: "customer.test", Organization: f.org, Enabled: true}
	check(t, f.service.PutDomain(ctx, manager.Token, domain))
	{
		_, err := directory.Resolve(ctx, "unknown@customer.test")
		requireErrorIs(t, err, maildirectory.ErrNotFound)
	}
	otherInbox := newID(t)
	check(t, f.service.CreateInbox(ctx, f.root.Token, Inbox{ID: otherInbox, Organization: f.other}))
	alias := maildirectory.Alias{
		Address:      "invoices@customer.test",
		Organization: f.org,
		Inboxes:      []string{f.inbox, f.inbox},
		Enabled:      true,
	}
	check(t, f.service.PutAlias(ctx, manager.Token, alias))
	resolved, err := directory.Resolve(ctx, "invoices@CUSTOMER.TEST")
	check(t, err)
	if len(resolved.Inboxes) != 1 || resolved.Inboxes[0].String() != f.inbox {
		t.Fatal("alias target deduplication failed")
	}
	bad := alias
	bad.Inboxes = []string{otherInbox}
	requireErrorIs(t, f.service.PutAlias(ctx, manager.Token, bad), maildirectory.ErrConflict)
	badDomain := domain
	badDomain.Organization = f.other
	requireErrorIs(t, f.service.PutDomain(ctx, f.root.Token, badDomain), maildirectory.ErrConflict)
	domain.CatchAll = f.inbox
	check(t, f.service.PutDomain(ctx, manager.Token, domain))
	_, err = directory.Resolve(ctx, "unknown@customer.test")
	check(t, err)
	alias.Enabled = false
	check(t, f.service.PutAlias(ctx, manager.Token, alias))
	{
		_, err := directory.Resolve(ctx, alias.Address)
		requireErrorIs(t, err, maildirectory.ErrNotFound)
	}
	checkManagedMailCredentials(t, f, manager, directory, domain, alias)
}

func TestLiveSMTPRouteAndIMAPCredentialChanges(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	directory := maildirectory.New(f.db)
	listener, imapListener := startLiveMailProtocols(t, f, directory)
	configureLiveMailDirectory(t, f, ctx)
	connection, err := smtp.Dial(listener.Addr().String())
	check(t, err)
	defer func() { check(t, connection.Close()) }()
	check(t, connection.Mail("sender@example.test"))
	check(t, connection.Rcpt("invoices@live.test"))
	writer, err := connection.Data()
	check(t, err)
	_, err = writer.Write([]byte("Subject: live directory\r\n\r\noriginal bytes\r\n"))
	check(t, err)
	check(t, writer.Close())
	_, err = f.mail.PublishMailboxes(ctx, 32)
	check(t, err)
	reader, err := imapclient.DialInsecure(imapListener.Addr().String(), nil)
	check(t, err)
	defer func() { check(t, reader.Close()) }()
	check(t, reader.Login("live-reader", "live-mail-password").Wait())
	selected, err := reader.Select("INBOX", nil).Wait()
	check(t, err)
	if selected.NumMessages != 1 {
		t.Fatal("accepted message not published")
	}
	check(t, f.service.SetMailAccess(ctx, f.root.Token, f.org, "live-reader", false))
	if _, err = reader.Fetch(
		imap.UIDSetNum(1),
		&imap.FetchOptions{BodySection: []*imap.FetchItemBodySection{{Peek: true}}},
	).
		Collect(); err == nil {
		t.Fatal("already authenticated disabled account fetched mail")
	}
	check(
		t,
		f.service.PutDomain(
			ctx,
			f.root.Token,
			maildirectory.Domain{Name: "live.test", Organization: f.org, Enabled: false},
		),
	)
	check(t, connection.Mail("sender@example.test"))
	if err = connection.Rcpt("invoices@live.test"); err == nil {
		t.Fatal("disabled domain remained routable")
	}
	checkSMTPDirectoryOutage(t, f, connection)
}

func checkManagedMailCredentials(
	t *testing.T,
	f *fixture,
	manager Session,
	directory *maildirectory.Directory,
	domain maildirectory.Domain,
	alias maildirectory.Alias,
) {
	t.Helper()
	ctx := context.Background()
	var err error
	account := maildirectory.Account{Username: "mail-reader", Organization: f.org, Inbox: f.inbox, Enabled: true}
	requireErrorIs(t, f.service.PutMailAccount(ctx, manager.Token, account, "mail-only-password"), ErrForbidden)
	check(t, f.service.PutMailAccount(ctx, f.root.Token, account, "mail-only-password"))
	{
		_, err := f.service.Login(ctx, account.Username, "mail-only-password")
		requireErrorIs(t, err, ErrUnauthorized)
	}
	principal, err := directory.Authenticate(ctx, account.Username, "mail-only-password")
	check(t, err)
	// Starting from stale bootstrap entries cannot re-enable an alias or rotate a password.
	seededAlias := alias
	seededAlias.Enabled = true
	check(
		t,
		directory.Seed(
			ctx,
			[]maildirectory.Domain{domain},
			[]maildirectory.Alias{seededAlias},
			[]maildirectory.Account{account},
			[]string{"old-bootstrap-password"},
		),
	)
	{
		_, err := directory.Resolve(ctx, alias.Address)
		requireErrorIs(t, err, maildirectory.ErrNotFound)
	}
	{
		_, err := directory.Authenticate(ctx, account.Username, "old-bootstrap-password")
		requireErrorIs(t, err, maildirectory.ErrNotFound)
	}
	check(t, directory.Validate(ctx, principal))
	check(t, f.service.SetMailAccess(ctx, manager.Token, f.org, account.Username, false))
	requireErrorIs(t, directory.Validate(ctx, principal), maildirectory.ErrNotFound)
}

func startLiveMailProtocols(t *testing.T, f *fixture, directory *maildirectory.Directory) (net.Listener, net.Listener) {
	t.Helper()
	ctx := context.Background()
	server, err := smtpserver.New(
		f.mail,
		smtpserver.Config{
			Domain:          "fry.test",
			Directory:       directory,
			MaxConnections:  8,
			MaxInFlight:     4,
			MaxRecipients:   32,
			MaxMessageBytes: 1 << 20,
			IdleTimeout:     time.Second * 5,
			IngestTimeout:   time.Second * 10,
		},
	)
	check(t, err)
	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(ctx, "tcp", "127.0.0.1:0")
	check(t, err)
	smtpDone := make(chan error, 1)
	go func() { smtpDone <- server.Serve(listener) }()
	t.Cleanup(func() {
		deadline, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := server.Shutdown(deadline); err != nil {
			t.Error(err)
		}
		if err := <-smtpDone; err != nil {
			t.Error(err)
		}
	})
	imapServer, err := imapserver.NewWithDirectory(f.mail, directory)
	check(t, err)
	imapListener, err := listenConfig.Listen(ctx, "tcp", "127.0.0.1:0")
	check(t, err)
	imapDone := make(chan error, 1)
	go func() { imapDone <- imapServer.Serve(imapListener) }()
	t.Cleanup(func() {
		if err := imapServer.Close(); err != nil {
			t.Error(err)
		}
		if err := <-imapDone; err != nil && !errors.Is(err, net.ErrClosed) {
			t.Error(err)
		}
	})
	return listener, imapListener
}

func checkSMTPDirectoryOutage(t *testing.T, f *fixture, connection *smtp.Client) {
	t.Helper()
	ctx := context.Background()
	// Catalog failure is temporary, not a permanent unknown-recipient rejection.
	_, err := f.db.Exec(ctx, `ALTER TABLE fry_domain RENAME TO unavailable_domain`)
	check(t, err)
	check(t, connection.Reset())
	check(t, connection.Mail("sender@example.test"))
	err = connection.Rcpt("invoices@live.test")
	if err == nil || err.Error()[:3] != "451" {
		t.Fatalf("directory outage: %v", err)
	}
}

func configureLiveMailDirectory(t *testing.T, f *fixture, ctx context.Context) {
	t.Helper()
	// Both listeners are already serving before any domain or credential exists.
	check(
		t,
		f.service.PutDomain(
			ctx,
			f.root.Token,
			maildirectory.Domain{Name: "live.test", Organization: f.org, Enabled: true},
		),
	)
	check(
		t,
		f.service.PutAlias(
			ctx,
			f.root.Token,
			maildirectory.Alias{
				Address:      "invoices@live.test",
				Organization: f.org,
				Inboxes:      []string{f.inbox},
				Enabled:      true,
			},
		),
	)
	check(
		t,
		f.service.PutMailAccount(
			ctx,
			f.root.Token,
			maildirectory.Account{Username: "live-reader", Organization: f.org, Inbox: f.inbox, Enabled: true},
			"live-mail-password",
		),
	)
}
