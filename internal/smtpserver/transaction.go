package smtpserver

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/cotocisternas/fry/internal/hotstore"
	"github.com/cotocisternas/fry/internal/maildirectory"
	"github.com/cotocisternas/fry/internal/receiptstore"

	"github.com/emersion/go-smtp"
)

func (s *session) authorizeMail(from string) (*principal, *receiptstore.SenderAuthorization, error) {
	var account *principal
	var proof *receiptstore.SenderAuthorization
	if s.server.config.Submission != nil {
		s.mu.Lock()
		account = s.account
		s.mu.Unlock()
		if account == nil {
			return nil, nil, smtp.ErrAuthRequired
		}
		if err := s.checkMailCredential(account); err != nil {
			return nil, nil, err
		}

		key, domain, err := addressKey(from)
		if err != nil || (!account.senders[key] && !account.domains[domain]) {
			return nil, nil, senderUnauthorized()
		}

		if authority := s.server.config.Submission.SenderAuthority; authority != nil {
			proof, err = s.authorizeMailSender(account, from, authority)
			if err != nil {
				return nil, nil, err
			}
		}

	}
	return account, proof, nil
}

func (s *session) authorizeMailSender(
	account *principal,
	from string,
	authority SenderAuthority,
) (*receiptstore.SenderAuthorization, error) {
	ctx, cancel := context.WithTimeout(s.server.ctx, 10*time.Second)
	defer cancel()
	proof, err := authority.AuthorizeSender(ctx, account.org, from)
	if errors.Is(err, receiptstore.ErrSenderUnauthorized) {
		return nil, senderUnauthorized()
	}
	if err != nil || proof == nil {
		return nil, temporary()
	}
	return proof, nil
}

func (s *session) submissionRecipient(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current == nil || s.current.started || s.current.envelope.Outbound == nil {
		return temporary()
	}
	s.current.envelope.Outbound.Recipients = append(s.current.envelope.Outbound.Recipients, key)
	return nil
}

func (s *Server) resolveRecipient(key string) (resolved, error) {
	if s.config.Directory != nil {
		ctx, cancel := context.WithTimeout(s.ctx, 10*time.Second)
		defer cancel()
		found, err := s.config.Directory.Resolve(ctx, key)
		if errors.Is(err, maildirectory.ErrNotFound) {
			return resolved{}, unknownRecipient()
		}
		if err != nil {
			return resolved{}, temporary()
		}
		return resolved{found.Organization, found.Inboxes}, nil
	}
	route, ok := s.routes[key]
	if !ok {
		return resolved{}, unknownRecipient()
	}
	return route, nil
}

func (s *session) beginData() (*transaction, error) {
	s.mu.Lock()
	t := s.current
	if t == nil || t.started ||
		(len(t.envelope.Destinations) == 0 &&
			(t.envelope.Outbound == nil || len(t.envelope.Outbound.Recipients) == 0)) {
		s.mu.Unlock()
		closeAfterFailure(s.conn)
		return nil, temporary()
	}
	t.started = true
	s.mu.Unlock()
	return t, nil
}

func (s *session) registerData() error {
	s.server.mu.Lock()
	if s.server.stopping {
		s.server.mu.Unlock()
		closeAfterFailure(s.conn)
		return temporary()
	}
	s.server.active.Add(1)
	s.server.mu.Unlock()
	return nil
}

func (s *session) validateDataAuthority(work context.Context, t *transaction, reader io.Reader) error {
	if err := s.validateDataCredential(work, t, reader); err != nil {
		return err
	}
	return s.validateDataSender(work, t, reader)
}

func (s *session) validateDataCredential(work context.Context, t *transaction, reader io.Reader) error {
	if t.envelope.Outbound != nil && s.server.config.Submission.Credentials != nil {
		proof := t.envelope.Outbound.SubmissionAuthorization
		var p *maildirectory.SubmissionPrincipal
		if proof != nil {
			p = &maildirectory.SubmissionPrincipal{
				Username:     proof.Username,
				Organization: t.envelope.Organization,
				Version:      proof.Version,
			}
		}
		credentialCtx, cancel := context.WithTimeout(work, 10*time.Second)
		credentialErr := s.validateCredential(credentialCtx, p)
		cancel()
		if credentialErr != nil {
			if _, drainErr := io.Copy(io.Discard, reader); drainErr != nil {
				closeAfterFailure(s.conn)
				return temporary()
			}
			if errors.Is(credentialErr, receiptstore.ErrSubmissionUnauthorized) {
				return submissionUnauthorized()
			}
			return temporary()
		}
	}
	return nil
}

func (s *session) validateDataSender(work context.Context, t *transaction, reader io.Reader) error {
	// Recheck before streaming so a grant revoked since MAIL can reject DATA
	// after consuming its framing and leave the authenticated session usable.
	if t.envelope.Outbound != nil && s.server.config.Submission.SenderAuthority != nil {
		authorityCtx, cancel := context.WithTimeout(work, 10*time.Second)
		current, authorityErr := s.server.config.Submission.SenderAuthority.AuthorizeSender(
			authorityCtx,
			t.envelope.Organization,
			t.envelope.MailFrom,
		)
		cancel()
		original := t.envelope.Outbound.SenderAuthorization
		denied := errors.Is(authorityErr, receiptstore.ErrSenderUnauthorized) ||
			(authorityErr == nil && senderProofChanged(current, original))
		if authorityErr != nil || current == nil || denied {
			if _, drainErr := io.Copy(io.Discard, reader); drainErr != nil {
				closeAfterFailure(s.conn)
				return temporary()
			}
			if denied {
				return senderUnauthorized()
			}
			return temporary()
		}
	}
	return nil
}

func (s *session) resolveDataFailure(work context.Context, t *transaction, source *messageSource, err error) error {
	// Never emit a definite rejection for an indeterminate committed receipt.
	credentialDenied := errors.Is(err, receiptstore.ErrSubmissionUnauthorized)
	senderDenied := errors.Is(err, receiptstore.ErrSenderUnauthorized)
	authorityDenied := senderDenied || credentialDenied
	if errors.Is(err, receiptstore.ErrUncertain) || authorityDenied {
		accepted, resolveErr := s.resolveDataOutcome(work, t, authorityDenied)
		if resolveErr != nil {
			return resolveErr
		}
		if accepted {
			return nil
		}
	}

	return s.rejectData(work, t, source, err)
}

func (s *session) rejectData(work context.Context, t *transaction, source *messageSource, err error) error {
	// A completed failed attempt is terminal before any final negative reply.
	// If abort cannot be confirmed, closing leaves the result explicitly uncertain.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(work), 5*time.Second)
	abortErr := s.server.store.Abort(ctx, t.envelope.Organization, t.attempt)
	cancel()
	if abortErr != nil && !errors.Is(abortErr, receiptstore.ErrNotFound) {
		closeAfterFailure(s.conn)
		return temporary()
	}

	// Only confirmed terminal non-acceptance may drain a deferred, untouched DATA
	// source. Keep t.ctx and its socket-cancellation callback live until EOF.
	if drainErr := source.drainUntouched(work, s.server.config.MaxMessageBytes); drainErr != nil {
		closeAfterFailure(s.conn)
		return temporary()
	}
	return dataFailureReply(err)
}

func dataFailureReply(err error) error {
	if errors.Is(err, receiptstore.ErrSubmissionUnauthorized) {
		return submissionUnauthorized()
	}
	if errors.Is(err, receiptstore.ErrSenderUnauthorized) {
		return senderUnauthorized()
	}
	if errors.Is(err, smtp.ErrDataTooLarge) || errors.Is(err, hotstore.ErrTooLarge) {
		return smtp.ErrDataTooLarge
	}
	return temporary()
}

func (s *session) resolveDataOutcome(work context.Context, t *transaction, authorityDenied bool) (bool, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(work), 5*time.Second)
	defer cancel()
	status, err := s.server.status(ctx, t.envelope.Organization, t.attempt)
	if err == nil && status.State == "accepted" {
		return true, nil
	}
	if err != nil && (!authorityDenied || !errors.Is(err, receiptstore.ErrNotFound)) {
		closeAfterFailure(s.conn)
		return false, temporary()
	}
	return false, nil
}

func (s *session) checkMailCredential(account *principal) error {
	credentialCtx, cancel := context.WithTimeout(s.server.ctx, 10*time.Second)
	credentialErr := s.validateCredential(credentialCtx, account.credential)
	cancel()
	if errors.Is(credentialErr, receiptstore.ErrSubmissionUnauthorized) {
		return submissionUnauthorized()
	}
	if credentialErr != nil {
		return temporary()
	}
	return nil
}

func senderProofChanged(current, original *receiptstore.SenderAuthorization) bool {
	return current != nil && original != nil && *current != *original
}
