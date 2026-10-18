package smtpserver

import (
	"errors"
	"io"
	"net"
	mailsmtp "net/smtp"
	"net/textproto"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-smtp"
)

const (
	originalDataBody = "Subject: original\r\n\r\noriginal body\r\n"
	laterDataBody    = "Subject: later\r\n\r\nlater body\r\n"
)

// go-smtp rejects invalid wire DATA before calling the backend. Capture the
// session through Backend.NewSession to exercise the public backend contract,
// including a second Data call before the caller has reset a completed one.
// No private session state is constructed or changed by these tests.
func TestSMTPBackendRejectsInvalidDataWithoutAcceptance(t *testing.T) {
	for _, state := range []string{"new", "reset", "missing-recipient", "duplicate"} {
		t.Run(state, func(t *testing.T) {
			f := setup(t, 1024)
			f.config.MaxInFlight = 1
			sessions := make(chan *observedBackendSession, 2)
			f.startObservingSessions(t, sessions)
			client := f.client(t)
			{
				err := client.Hello("backend-fixture")
				requireTestCondition(t, err == nil, err)
			}
			var backend *observedBackendSession
			select {
			case backend = <-sessions:
			case <-time.After(time.Second):
				t.Fatal("backend session was not created")
			}
			accepted := prepareBackendState(t, backend, state)
			body := strings.NewReader("Subject: invalid\r\n\r\nmust not be accepted\r\n")
			unread := body.Len()
			err := backend.Data(body)
			var rejection *smtp.SMTPError
			requireTestConditionf(
				t,
				(errors.As(err, &rejection) && rejection.Code == 451),
				"invalid backend DATA: want temporary rejection, got %T: %v",
				err,
				err,
			)
			requireTestCondition(t, body.Len() == unread, "invalid backend DATA consumed the message")
			assertBackendDisconnected(t, client, backend)
			{
				got := f.count(t, "fry_receipt")
				requireTestConditionf(
					t,
					got == accepted,
					"invalid backend DATA changed accepted receipts: got %d want %d",
					got,
					accepted,
				)
			}
			requireLaterBackendTransaction(t, f, accepted)
		})
	}
}

func TestSMTPWireRejectsInvalidDataAndPreservesNextTransaction(t *testing.T) {
	for _, state := range []string{"new", "reset", "missing-recipient", "duplicate"} {
		t.Run(state, func(t *testing.T) {
			f := setup(t, 1024)
			f.config.MaxInFlight = 1
			f.start(t)
			client := f.client(t)
			{
				err := client.Hello("wire-fixture")
				requireTestCondition(t, err == nil, err)
			}
			accepted := 0
			switch state {
			case "reset", "missing-recipient":
				{
					err := client.Mail("sender@partner.test")
					requireTestCondition(t, err == nil, err)
				}
				if state == "reset" {
					{
						err := client.Rcpt("billing@example.test")
						requireTestCondition(t, err == nil, err)
					}
					{
						err := client.Reset()
						requireTestCondition(t, err == nil, err)
					}
				}
			case "duplicate":
				{
					err := send(t, client, []string{"billing@example.test"}, originalDataBody)
					requireTestCondition(t, err == nil, err)
				}
				accepted = 1
			}
			assertWireDataRejected(t, client)
			{
				got := f.count(t, "fry_receipt")
				requireTestConditionf(
					t,
					got == accepted,
					"invalid wire DATA changed receipts: got %d want %d",
					got,
					accepted,
				)
			}
			requireLaterWireTransaction(t, f, client, accepted)
		})
	}
}

func assertWireDataRejected(t *testing.T, client *mailsmtp.Client) {
	t.Helper()
	writer, err := client.Data()
	if writer != nil {
		// Close the transport before the DATA writer so a regression cannot commit
		// an unintended empty message while this assertion is failing.
		testCleanup(t)(client.Close())
		if closeErr := writer.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) &&
			!errors.Is(closeErr, io.ErrClosedPipe) {
			t.Logf("unexpected DATA writer cleanup (%T)", closeErr)
		}
	}
	var rejection *textproto.Error
	requireTestConditionf(
		t,
		(errors.As(err, &rejection) && rejection.Code == 502),
		"invalid wire DATA: want 502, got %T: %v",
		err,
		err,
	)
}

// observedBackendSession joins the protocol-owned Logout without inspecting
// FRY session internals or changing its transaction lifecycle.
type observedBackendSession struct {
	smtp.Session
	loggedOut chan struct{}
}

func (s *observedBackendSession) Logout() error {
	err := s.Session.Logout()
	close(s.loggedOut)
	return err
}

func prepareBackendState(t *testing.T, backend *observedBackendSession, state string) int {
	t.Helper()
	if state != "new" {
		{
			err := backend.Mail("sender@partner.test", nil)
			requireTestCondition(t, err == nil, err)
		}
	}
	if state == "reset" || state == "duplicate" {
		{
			err := backend.Rcpt("billing@example.test", nil)
			requireTestCondition(t, err == nil, err)
		}
	}
	accepted := 0
	switch state {
	case "reset":
		backend.Reset()
	case "duplicate":
		{
			err := backend.Data(strings.NewReader(originalDataBody))
			requireTestCondition(t, err == nil, err)
		}
		accepted = 1
	}
	return accepted
}

func assertBackendDisconnected(t *testing.T, client *mailsmtp.Client, backend *observedBackendSession) {
	t.Helper()
	if err := client.Noop(); err == nil {
		t.Fatal("invalid backend DATA left its transport usable")
	} else {
		var networkErr net.Error
		requireTestCondition(
			t,
			(!errors.As(err, &networkErr) || !networkErr.Timeout()),
			"invalid backend DATA did not close its transport",
		)
	}
	// EOF alone does not prove that the protocol goroutine has run Logout.
	// Join that public callback before testing release of the sole ingest slot.
	select {
	case <-backend.loggedOut:
	case <-time.After(time.Second):
		t.Fatal("invalid backend DATA left its session active")
	}
}

func requireLaterBackendTransaction(t *testing.T, f *fixture, accepted int) {
	t.Helper()
	// Rejection/Reset must release the one ingest slot. A fresh protocol session
	// must still accept exactly one independent, correctly framed transaction.
	{
		err := send(t, f.client(t), []string{"billing@example.test"}, laterDataBody)
		requireTestCondition(t, err == nil, "later transaction failed", err)
	}
	{
		got := f.count(t, "fry_receipt")
		requireTestConditionf(
			t,
			got == accepted+1,
			"later transaction receipt count: got %d want %d",
			got,
			accepted+1,
		)
	}
	wantBody := laterDataBody
	if accepted != 0 {
		wantBody = originalDataBody
	}
	{
		got := f.original(t)
		requireTestCondition(t, got == wantBody, "invalid DATA changed stored original bytes")
	}
}

func requireLaterWireTransaction(t *testing.T, f *fixture, client *mailsmtp.Client, accepted int) {
	t.Helper()
	{
		err := send(t, client, []string{"billing@example.test"}, laterDataBody)
		requireTestCondition(t, err == nil, "later transaction failed", err)
	}
	{
		got := f.count(t, "fry_receipt")
		requireTestConditionf(
			t,
			got == accepted+1,
			"later transaction receipt count: got %d want %d",
			got,
			accepted+1,
		)
	}
	wantBody := laterDataBody
	if accepted != 0 {
		wantBody = originalDataBody
	}
	{
		got := f.original(t)
		requireTestCondition(t, got == wantBody, "invalid DATA changed stored original bytes")
	}
}
