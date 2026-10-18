//go:build linux

package imapserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cotocisternas/fry/internal/hotstore"
	"github.com/cotocisternas/fry/internal/receiptstore"
	"github.com/cotocisternas/fry/internal/telemetry"

	"github.com/emersion/go-imap/v2"
)

// SetAppendLimit configures the wire admission limit before Serve is called.
// Storage admission remains authoritative. The process uses its message limit.
func (s *Server) SetAppendLimit(bytes uint32) error {
	if bytes == 0 || bytes > 256<<20 {
		return fmt.Errorf("imap: invalid APPEND limit")
	}
	s.appendLimit = bytes
	return nil
}
func (s *session) AppendLimit() uint32 { return s.server.appendLimit }

func (s *session) Append(
	name string,
	literal imap.LiteralReader,
	options *imap.AppendOptions,
) (_ *imap.AppendData, resultErr error) {
	ctx, cancel := s.operation()
	defer cancel()
	ctx, finish := telemetry.FromContext(ctx).Start(ctx, "imap.append")
	defer func() { finish(resultErr) }()
	folder, err := s.server.store.Folder(ctx, s.account.org, s.account.inbox, name)
	if errors.Is(err, receiptstore.ErrNotFound) {
		return nil, &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Code: imap.ResponseCodeTryCreate,
			Text: "Create the destination mailbox first",
		}
	}
	if err != nil {
		return nil, safe(err)
	}
	if literal.Size() < 0 || literal.Size() > int64(s.server.appendLimit) {
		return nil, &imap.Error{
			Type: imap.StatusResponseTypeNo,
			Code: imap.ResponseCodeTooBig,
			Text: "Message exceeds configured limit",
		}
	}
	flags := make([]string, len(options.Flags))
	for i, flag := range options.Flags {
		flags[i] = string(flag)
	}
	attempt, err := hotstore.NewID()
	if err != nil {
		return nil, safe(err)
	}
	envelope := receiptstore.Envelope{
		Organization:  s.account.org,
		MailboxAppend: true,
		Destinations: []receiptstore.Destination{
			{Inbox: s.account.inbox, Folder: folder.ID, Flags: flags, InternalDate: options.Time},
		},
	}
	source := &appendSource{Reader: literal, conn: s.conn.NetConn(), remaining: literal.Size()}
	receipt, err := s.acceptAppend(ctx, attempt, envelope, source)
	if err != nil {
		return nil, err
	}

	// From here a negative response would falsely imply no mailbox change. Close
	// the connection if the stable acknowledgment cannot be read after commit.
	placement, err := s.server.store.Appended(ctx, s.account.org, receipt.ID)
	if err != nil {
		closeAfterFailure(s.conn.NetConn())
		return nil, safe(err)
	}
	return &imap.AppendData{UID: imap.UID(placement.UID), UIDValidity: placement.UIDValidity}, nil
}

func (s *session) acceptAppend(
	ctx context.Context,
	attempt receiptstore.ID,
	envelope receiptstore.Envelope,
	source *appendSource,
) (receiptstore.Receipt, error) {
	receipt, err := s.server.store.Accept(ctx, attempt, envelope, source)
	if errors.Is(err, receiptstore.ErrUncertain) {
		resolve, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		status, e := s.server.store.Status(resolve, s.account.org, attempt)
		stop()
		if e != nil {
			closeAfterFailure(s.conn.NetConn())
			return receiptstore.Receipt{}, safe(e)
		}
		if status.State == "accepted" {
			receipt = *status.Receipt
			err = nil
		}
	}
	if err != nil {
		abort, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		abortErr := s.server.store.Abort(abort, s.account.org, attempt)
		stop()
		if abortErr != nil && !errors.Is(abortErr, receiptstore.ErrNotFound) {
			closeAfterFailure(s.conn.NetConn())
		}
		return receiptstore.Receipt{}, safe(err)
	}
	return receipt, nil
}

type appendSource struct {
	io.Reader
	conn      net.Conn
	remaining int64
	eof       atomic.Bool
	once      sync.Once
}

func (r *appendSource) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.remaining -= int64(n)
	if r.remaining < 0 {
		return 0, fmt.Errorf("imap: literal exceeded declared size")
	}
	if err == io.EOF {
		if r.remaining != 0 {
			return n, io.ErrUnexpectedEOF
		}
		r.eof.Store(true)
	}
	return n, err
}

func (r *appendSource) Close() error {
	r.once.Do(func() {
		if !r.eof.Load() {
			closeAfterFailure(r.conn)
		}
	})
	return nil
}
