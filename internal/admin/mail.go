//go:build linux

package admin

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/cotocisternas/fry/internal/admin/dbgen"
	"github.com/cotocisternas/fry/internal/receiptstore"

	"github.com/jackc/pgx/v5"
)

type Inbox struct {
	ID           string `json:"id"`
	Organization string `json:"organization"`
	Messages     int64  `json:"messages"`
}

func (s *Service) Inboxes(ctx context.Context, token, org string) ([]Inbox, error) {
	if !validID(org) {
		return nil, ErrInvalid
	}
	tx, err := s.begin(ctx, false)
	if err != nil {
		return nil, err
	}
	defer rollback(ctx, tx)
	who, err := authorize(ctx, tx, token)
	if err != nil {
		return nil, err
	}
	if !allowed(who, org, false) {
		return nil, ErrForbidden
	}
	rows, err := dbgen.New(tx).ListInboxes(ctx, org)
	if err != nil {
		return nil, err
	}
	result := []Inbox{}
	for _, row := range rows {
		var i Inbox
		i.ID = row.ID
		i.Organization = row.Organization
		i.Messages = row.Messages
		result = append(result, i)
	}
	return result, nil
}

func (s *Service) CreateInbox(ctx context.Context, token string, i Inbox) error {
	org, err := mailID(i.Organization)
	if err != nil {
		return err
	}
	id, err := mailID(i.ID)
	if err != nil {
		return err
	}
	tx, err := s.begin(ctx, false)
	if err != nil {
		return err
	}
	defer rollback(ctx, tx)
	who, err := authorize(ctx, tx, token)
	if err != nil {
		return err
	}
	if !allowed(who, i.Organization, false) {
		return ErrForbidden
	}
	var exists bool
	if exists, err = dbgen.New(tx).OrganizationExistsForInbox(ctx, i.Organization); err != nil {
		return err
	}
	if !exists {
		return ErrInvalid
	}
	if err = receiptstore.EnsureInboxInTransaction(ctx, tx, org, id); err != nil {
		return err
	}
	if err = audit(ctx, tx, who.ID, "inbox-create", i.Organization, i.ID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

type Message struct {
	ID       string    `json:"id"`
	Size     int64     `json:"size"`
	Received time.Time `json:"received"`
	State    string    `json:"state"`
}

// Messages deliberately returns operational metadata, never headers or bodies.
// Page using the last returned ID; concurrent inserts may appear on later scans.
func (s *Service) Messages(ctx context.Context, token, org, after string) ([]Message, error) {
	if !validID(org) || (after != "" && !validID(after)) {
		return nil, ErrInvalid
	}
	tx, err := s.begin(ctx, false)
	if err != nil {
		return nil, err
	}
	defer rollback(ctx, tx)
	who, err := authorize(ctx, tx, token)
	if err != nil {
		return nil, err
	}
	if !allowed(who, org, false) {
		return nil, ErrForbidden
	}
	rows, err := dbgen.New(tx).ListMessages(ctx, dbgen.ListMessagesParams{Organization: org, ID: after})
	if err != nil {
		return nil, err
	}
	result := []Message{}
	for _, row := range rows {
		var m Message
		m.ID = row.ID
		m.Size = row.RawSize
		m.Received = row.ReceivedAt
		m.State = row.Lifecycle
		result = append(result, m)
	}
	return result, nil
}

type Content struct {
	reader      *receiptstore.ReadHandle
	service     *Service
	auditID     int64
	Size        int64
	read        int64
	failed      bool
	verifiedEOF bool
	closed      bool
}

func (c *Content) Read(p []byte) (int, error) {
	n, err := c.reader.Read(p)
	c.read += int64(n)
	if err == io.EOF { //nolint:errorlint // Only exact EOF is successful; an integrity or protocol error may wrap EOF.
		c.verifiedEOF = true
	} else if err != nil {
		c.failed = true
	}
	return n, err
}

// WriteTo preserves destination failures for the audit recorded by Close. A
// failed write may follow a read that already consumed the complete original.
func (c *Content) WriteTo(dst io.Writer) (int64, error) {
	// Hide WriteTo from io.Copy to avoid recursively selecting this method.
	n, err := io.Copy(dst, struct{ io.Reader }{c})
	if err != nil {
		c.failed = true
	}
	return n, err
}

// Close records completion only after the caller observes verified EOF. This
// proves server-side transfer, not remote durable storage. Callers must close.
func (c *Content) Close() error {
	if c.closed {
		return nil
	}
	c.closed = true
	err := c.reader.Close()
	outcome := "interrupted"
	if err == nil && !c.failed && c.verifiedEOF && c.read == c.Size {
		outcome = "read-complete"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, auditErr := dbgen.New(c.service.db).
		SetAuditOutcome(ctx, dbgen.SetAuditOutcomeParams{ID: c.auditID, Outcome: outcome})
	return errors.Join(err, auditErr)
}

func (s *Service) OpenContent(ctx context.Context, token, org, receipt string) (*Content, error) {
	orgID, err := mailID(org)
	if err != nil {
		return nil, err
	}
	receiptID, err := mailID(receipt)
	if err != nil {
		return nil, err
	}
	tx, err := s.begin(ctx, false)
	if err != nil {
		return nil, err
	}
	defer rollback(ctx, tx)
	who, err := authorizeScope(ctx, tx, token, org, true)
	if err != nil {
		return nil, err
	}
	var size int64
	size, err = dbgen.New(tx).GetReceiptSize(ctx, dbgen.GetReceiptSizeParams{Organization: org, ID: receipt})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, receiptstore.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	var auditID int64
	auditID, err = dbgen.New(tx).
		AuthorizeContentDownload(
			ctx,
			dbgen.AuthorizeContentDownloadParams{
				Actor:        who.ID,
				Organization: org,
				Target:       receipt,
			},
		)
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	// Permission was current when the audited request started. Later revocation
	// prevents subsequent requests; it does not cancel this authorized transfer.
	reader, err := s.mail.Open(ctx, orgID, receiptID, 0, size)
	if err != nil {
		auditCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_, e := dbgen.New(s.db).MarkContentUnavailable(auditCtx, auditID)
		return nil, errors.Join(err, e)
	}
	return &Content{reader: reader, service: s, auditID: auditID, Size: size}, nil
}

type Audit struct {
	ID           int64     `json:"id"`
	At           time.Time `json:"at"`
	Actor        string    `json:"actor"`
	Action       string    `json:"action"`
	Organization string    `json:"organization"`
	Target       string    `json:"target"`
	Outcome      string    `json:"outcome"`
}

func (s *Service) Audits(ctx context.Context, token string, before int64) ([]Audit, error) {
	if before < 0 {
		return nil, ErrInvalid
	}
	tx, err := s.begin(ctx, false)
	if err != nil {
		return nil, err
	}
	defer rollback(ctx, tx)
	who, err := authorize(ctx, tx, token)
	if err != nil {
		return nil, err
	}
	rows, err := dbgen.New(tx).
		ListAudits(
			ctx,
			dbgen.ListAuditsParams{
				UserID:       who.ID,
				GlobalAccess: allowed(who, "*", false),
				BeforeID:     before,
			},
		)
	if err != nil {
		return nil, err
	}
	result := []Audit{}
	for _, row := range rows {
		var a Audit
		a.ID = row.ID
		a.At = row.OccurredAt
		a.Actor = row.Actor
		a.Action = row.Action
		a.Organization = row.Organization
		a.Target = row.Target
		a.Outcome = row.Outcome
		result = append(result, a)
	}
	return result, nil
}
