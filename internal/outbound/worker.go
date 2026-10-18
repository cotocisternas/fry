//go:build linux

// Package outbound hands durable recipient jobs to explicitly selected SMTP
// relays. It never resolves recipient MX records or switches to another relay.
package outbound

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/cotocisternas/fry/internal/receiptstore"

	"github.com/emersion/go-sasl"
	"github.com/emersion/go-smtp"
)

type Relay struct {
	ID                          receiptstore.ID
	Address, Username, Password string
	// Transport is starttls (normally 587), tls (normally 465), or loopback.
	Transport string
	Roots     *x509.CertPool
}
type Worker struct {
	store    *receiptstore.Store
	relays   map[receiptstore.ID]Relay
	hostname string
}

// New requires the installation's complete relay catalog, including destinations
// still needed by queued mail and pending failure notices. Binding protects each
// ID's destination, not distributed catalog completeness: deployments must keep
// concurrently running workers configured for all destinations they may claim.
func New(store *receiptstore.Store, hostname string, relays []Relay) (*Worker, error) {
	if store == nil || hostname == "" || strings.ContainsAny(hostname, "\r\n\x00") || len(relays) == 0 {
		return nil, fmt.Errorf("outbound: store, hostname and relays required")
	}
	w := &Worker{store: store, hostname: hostname, relays: map[receiptstore.ID]Relay{}}
	var destinations []receiptstore.RelayDestination
	for _, r := range relays {
		destination, err := (receiptstore.RelayDestination{
			ID:        r.ID,
			Address:   r.Address,
			Transport: r.Transport,
			Username:  r.Username,
		}).Canonical()
		if err != nil {
			return nil, err
		}
		r.Address = destination.Address
		destinations = append(destinations, destination)
		if _, exists := w.relays[r.ID]; exists {
			return nil, fmt.Errorf("outbound: duplicate relay")
		}
		if err := r.validateTransport(); err != nil {
			return nil, err
		}
		w.relays[r.ID] = r
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := store.BindRelayDestinations(ctx, destinations); err != nil {
		return nil, err
	}
	return w, nil
}

func (r Relay) validateTransport() error {
	host, _, err := net.SplitHostPort(r.Address)
	if err != nil || host == "" || r.ID == (receiptstore.ID{}) {
		return fmt.Errorf("outbound: invalid relay")
	}
	switch r.Transport {
	case "starttls", "tls":
		if r.Username == "" || r.Password == "" {
			return fmt.Errorf("outbound: authenticated relay required")
		}
	case "loopback":
		if !net.ParseIP(host).IsLoopback() {
			return fmt.Errorf("outbound: plaintext relay requires loopback IP")
		}
	default:
		return fmt.Errorf("outbound: invalid transport")
	}
	return nil
}

// RunOnce returns whether a due recipient was claimed. Relay failure is recorded
// durably as a retry/failure outcome; a non-nil error means queue progress itself
// could not be recorded. An unrecorded SMTP success can be sent again after its
// lease expires, which is the unavoidable ambiguous-handoff case.
func (w *Worker) RunOnce(ctx context.Context) (bool, error) {
	claim, err := w.store.ClaimOutbound(ctx, 3*time.Minute)
	if errors.Is(err, receiptstore.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	result := receiptstore.OutboundResult{State: "retry", RetryAfter: retryDelay(claim.Attempts)}
	if !claim.Deadline.After(time.Now()) {
		result.State = "failed"
		result.ResponseCode = 554
		result.Diagnostic = "Outbound retry period expired"
	} else if relay, ok := w.relays[claim.Relay]; !ok {
		result.Diagnostic = "Selected relay configuration unavailable"
	} else {
		sendCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		// Never let a transfer outlive the queue claim or configured retry deadline.
		bounded, cancelBounded := context.WithDeadline(
			sendCtx,
			minTime(claim.Deadline, claim.LeaseUntil.Add(-5*time.Second)),
		)
		err = w.send(bounded, relay, claim)
		cancelBounded()
		cancel()
		result = classifyHandoff(result, err)
	}
	return true, w.store.CompleteOutbound(ctx, claim, result)
}

func classifyHandoff(result receiptstore.OutboundResult, err error) receiptstore.OutboundResult {
	if err == nil {
		result.State = "handed-off"
		result.ResponseCode = 250
		result.Diagnostic = "Accepted by selected relay"
	} else {
		result.Diagnostic = "Relay connection or transfer interrupted"
		var smtpError *smtp.SMTPError
		if errors.As(err, &smtpError) {
			result.ResponseCode = smtpError.Code
			result.Diagnostic = "Selected relay rejected SMTP operation"
			if smtpError.Code >= 500 && smtpError.Code <= 599 {
				result.State = "failed"
			}
		}
	}
	return result
}

func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func retryDelay(attempt int64) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	if attempt > 10 {
		attempt = 10
	}
	return min(time.Duration(1<<uint(attempt-1))*5*time.Second, 30*time.Minute)
}

func (w *Worker) send(ctx context.Context, relay Relay, claim receiptstore.OutboundClaim) error {
	raw, tlsConfig, err := relay.dial(ctx)
	if err != nil {
		return err
	}
	defer closeConnection(raw)
	stop := context.AfterFunc(ctx, func() { closeConnection(raw) })
	defer stop()
	var client *smtp.Client
	if relay.Transport == "starttls" {
		client, err = smtp.NewClientStartTLS(raw, tlsConfig)
	} else {
		client = smtp.NewClient(raw)
	}
	if err != nil {
		return fmt.Errorf("outbound: relay TLS negotiation unavailable")
	}
	defer func() {
		if err := client.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			slog.WarnContext(ctx, "outbound SMTP client cleanup failed")
		}
	}()
	if err := w.greetRelay(client, relay); err != nil {
		return err
	}
	return w.transfer(ctx, raw, client, claim)
}

func (w *Worker) greetRelay(client *smtp.Client, relay Relay) error {
	client.CommandTimeout = 30 * time.Second
	client.SubmissionTimeout = time.Minute
	if err := client.Hello(w.hostname); err != nil {
		return fmt.Errorf("outbound: relay greeting unavailable")
	}
	if relay.Username != "" {
		if err := client.Auth(sasl.NewPlainClient("", relay.Username, relay.Password)); err != nil {
			return fmt.Errorf("outbound: relay authentication unavailable")
		}
	}
	return nil
}

// transfer closes DATA only after a complete original read and successful close.
func (w *Worker) transfer(
	ctx context.Context,
	raw net.Conn,
	client *smtp.Client,
	claim receiptstore.OutboundClaim,
) error {
	if err := client.Mail(claim.Sender, &smtp.MailOptions{Size: claim.Size}); err != nil {
		return err
	}
	if err := client.Rcpt(claim.Recipient, nil); err != nil {
		return err
	}
	reader, err := w.store.Open(ctx, claim.Organization, claim.Receipt, 0, claim.Size)
	if err != nil {
		return err
	}
	defer func() {
		if err := reader.Close(); err != nil {
			slog.ErrorContext(ctx, "outbound original cleanup failed")
		}
	}()
	data, err := client.Data()
	if err != nil {
		return err
	}
	n, err := io.Copy(data, reader)
	if err != nil || n != claim.Size {
		// Never close DATA normally after a partial storage read: that would emit
		// the terminator and could cause the relay to accept a truncated message.
		closeConnection(raw)
		if err != nil {
			return err
		}
		return io.ErrUnexpectedEOF
	}
	if err = reader.Close(); err != nil {
		closeConnection(raw)
		return err
	}
	// Only a successful final DATA reply establishes downstream acceptance.
	_, err = data.CloseWithResponse()
	return err
}

func (r Relay) dial(ctx context.Context) (net.Conn, *tls.Config, error) {
	host, _, err := net.SplitHostPort(r.Address)
	if err != nil {
		return nil, nil, err
	}
	dialer := net.Dialer{Timeout: 10 * time.Second}
	var raw net.Conn
	tlsConfig := &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12, RootCAs: r.Roots}
	if r.Transport == "tls" {
		d := tls.Dialer{NetDialer: &dialer, Config: tlsConfig}
		raw, err = d.DialContext(ctx, "tcp", r.Address)
	} else {
		raw, err = dialer.DialContext(ctx, "tcp", r.Address)
	}
	if err != nil {
		return nil, nil, err
	}
	return raw, tlsConfig, nil
}

// A socket cleanup failure does not negate the final SMTP acknowledgment.
func closeConnection(conn net.Conn) {
	if err := conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		slog.Warn("outbound SMTP connection cleanup failed")
	}
}
