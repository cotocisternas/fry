//go:build linux

package receiptstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/cotocisternas/fry/internal/receiptstore/dbgen"

	"github.com/jackc/pgx/v5"
)

// QueueFailureNotices renders bounded multipart/report messages once and submits
// them through the same durable outbound path. Their null reverse-path prevents
// recursive bounces. Retried rendering/acceptance uses the persisted snapshot.
func (s *Store) QueueFailureNotices(
	ctx context.Context,
	hostname string,
	retryWindow time.Duration,
	limit int,
) (int, error) {
	queryLimit, err := validateNoticeRequest(hostname, retryWindow, limit)
	if err != nil {
		return 0, err
	}
	rows, err := dbgen.New(s.db.pool).StoreQueueFailureNoticesReadFailureNotice(ctx, queryLimit)
	if err != nil {
		return 0, err
	}
	var ids []ID
	for _, sqlcRow := range rows {
		raw := sqlcRow
		id, e := idFrom(raw)
		if e != nil {
			err = e
			break
		}
		ids = append(ids, id)
	}
	if err != nil {
		return 0, err
	}
	count := 0
	for _, id := range ids {
		if err := s.queueFailureNotice(ctx, id, hostname, retryWindow); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}

func (s *Store) prepareNotice(
	ctx context.Context,
	id ID,
	hostname string,
	retryWindow time.Duration,
) (string, Envelope, error) {
	var envelope Envelope
	tx, err := s.db.begin(ctx)
	if err != nil {
		return "", envelope, err
	}
	defer rollback(ctx, tx)
	notice, err := dbgen.New(tx).StorePrepareNoticeReadFailureNotice(ctx, id.String())
	if err != nil {
		return "", envelope, err
	}
	hostname, retryWindow, err = noticeRendering(ctx, tx, id, notice, hostname, retryWindow)
	if err != nil {
		return "", envelope, err
	}
	envelope, err = failureNoticeEnvelope(notice, retryWindow)
	if err != nil {
		return "", envelope, err
	}
	rendered := renderFailureNotice(id, hostname, notice)

	return rendered, envelope, commit(ctx, tx)
}

func validateNoticeRequest(hostname string, retryWindow time.Duration, limit int) (int32, error) {
	if hostname == "" || strings.ContainsAny(hostname, "\r\n\x00 <>@") || !outboundAddress("postmaster@"+hostname) ||
		retryWindow < time.Second ||
		retryWindow > 30*24*time.Hour ||
		retryWindow%time.Second != 0 ||
		limit < 1 ||
		limit > 1024 {
		return 0, ErrConflict
	}
	return int32(limit), nil
}

func (s *Store) queueFailureNotice(ctx context.Context, id ID, hostname string, retryWindow time.Duration) error {
	body, envelope, e := s.prepareNotice(ctx, id, hostname, retryWindow)
	if e != nil {
		return e
	}
	receipt, e := s.Accept(ctx, id, envelope, io.NopCloser(strings.NewReader(body)))
	if errors.Is(e, ErrUncertain) {
		status, statusErr := s.Status(ctx, envelope.Organization, id)
		if statusErr == nil && status.State == "accepted" && status.Receipt != nil {
			receipt = *status.Receipt
			e = nil
		}
	}
	if e != nil {
		return e
	}
	if _, e = dbgen.New(s.db.pool).
		StoreQueueFailureNoticesUpdateFailureNotice(ctx, dbgen.StoreQueueFailureNoticesUpdateFailureNoticeParams{
			ID:      id.String(),
			Receipt: new(receipt.ID.String()),
		}); e != nil {
		return e
	}
	return nil
}

func noticeRendering(
	ctx context.Context,
	tx pgx.Tx,
	id ID,
	notice dbgen.StorePrepareNoticeReadFailureNoticeRow,
	hostname string,
	retryWindow time.Duration,
) (string, time.Duration, error) {
	if notice.RenderingHost != nil {
		if notice.NoticeRetrySeconds == nil {
			return "", 0, ErrConflict
		}
		hostname = *notice.RenderingHost
		retryWindow = time.Duration(*notice.NoticeRetrySeconds) * time.Second
	} else {
		if _, err := dbgen.New(tx).
			StorePrepareNoticeUpdateFailureNotice(ctx, dbgen.StorePrepareNoticeUpdateFailureNoticeParams{
				ID:                 id.String(),
				RenderingHost:      new(hostname),
				NoticeRetrySeconds: new(int64(retryWindow / time.Second)),
			}); err != nil {
			return "", 0, err
		}
	}
	return hostname, retryWindow, nil
}

func failureNoticeEnvelope(
	notice dbgen.StorePrepareNoticeReadFailureNoticeRow,
	retryWindow time.Duration,
) (Envelope, error) {
	var envelope Envelope
	organization, err := idFrom(notice.Organization)
	if err != nil {
		return envelope, err
	}
	relayID, err := idFrom(notice.Relay)
	if err != nil {
		return envelope, err
	}
	envelope = Envelope{
		Organization: organization,
		MailFrom:     "",
		Outbound:     &Outbound{Relay: relayID, Recipients: []string{notice.Recipient}, RetryWindow: retryWindow},
	}
	envelope, _, err = normalize(envelope)
	if err != nil {
		return envelope, err
	}
	return envelope, nil
}

func renderFailureNotice(id ID, hostname string, notice dbgen.StorePrepareNoticeReadFailureNoticeRow) string {
	boundary := "FRY-" + id.String()
	diagnostic := strings.Map(func(r rune) rune {
		if r < 32 || r > 126 {
			return ' '
		}
		return r
	}, notice.Diagnostic)
	rendered := fmt.Sprintf(
		"From: Mail Delivery Subsystem <postmaster@%s>\r\nTo: %s\r\nDate: "+
			"%s\r\nMessage-ID: <%s@%s>\r\nSubject: Delivery failure\r\n"+
			"Auto-Submitted: auto-replied\r\nMIME-Version: 1.0\r\nContent-Type: "+
			"multipart/report; report-type=delivery-status; boundary=\"%s\"\r\n\r\n"+
			"--%s\r\nContent-Type: text/plain; charset=us-ascii\r\n\r\nFRY could "+
			"not hand your message to the selected relay for %s.\r\n%s\r\n\r\n"+
			"--%s\r\nContent-Type: message/delivery-status\r\n\r\nReporting-MTA: "+
			"dns; %s\r\n\r\nFinal-Recipient: rfc822; %s\r\nAction: failed\r\n"+
			"Status: 5.0.0\r\nDiagnostic-Code: smtp; %d %s\r\n\r\n--%s--\r\n",
		hostname,
		notice.Recipient,
		notice.CreatedAt.UTC().Format(time.RFC1123Z),
		id.String(),
		hostname,
		boundary,
		boundary,
		notice.FailedRecipient,
		diagnostic,
		boundary,
		hostname,
		notice.FailedRecipient,
		notice.ResponseCode,
		diagnostic,
		boundary,
	)

	return rendered
}
