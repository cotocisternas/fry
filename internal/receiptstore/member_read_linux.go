//go:build linux

package receiptstore

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/cotocisternas/fry/internal/hotstore"
	"github.com/cotocisternas/fry/internal/receiptstore/dbgen"

	"github.com/jackc/pgx/v5"
)

// read creates a member-owned pin before opening bytes. The caller must make
// dst's writes interruptible by ctx. An HTTP connection closing only cancels this
// call; proof is recorded after the actual byte reader has returned. The explicit
// operation ID is never reused, even after its pin is released.
func (o *memberOwner) read(
	ctx context.Context,
	operation, parentPin, org, receipt, _ ID,
	offset, length int64,
	dst io.Writer,
) error {
	if operation == (ID{}) || parentPin == (ID{}) || dst == nil {
		return ErrConflict
	}
	started := time.Now()
	work, finished, err := o.begin(ctx)
	if err != nil {
		return err
	}
	defer finished()
	m := o.member
	release, err := m.acquireRead(work)
	if err != nil {
		return err
	}
	defer release()
	m.gate.RLock()
	defer m.gate.RUnlock()
	if m.closed || m.isFrozen() {
		return ErrStale
	}
	a := &authority{pool: m.pool}
	tx, err := a.begin(work)
	if err != nil {
		return err
	}
	defer rollback(work, tx)
	request := pinnedReadRequest{
		organization: org,
		receipt:      receipt,
		pin:          parentPin,
		offset:       offset,
		length:       length,
		dst:          dst,
	}
	source, remaining, err := o.registerReadPin(work, tx, operation, request)
	if err != nil {
		return err
	}
	return o.readWithClosureProof(ctx, work, started, operation, request, source, remaining)
}

func (o *memberOwner) readWithClosureProof(
	ctx, work context.Context,
	started time.Time,
	operation ID,
	request pinnedReadRequest,
	source memberReadSource,
	remaining float64,
) error {
	readCtx, cancel := context.WithDeadline(work, started.Add(time.Duration(remaining*float64(time.Second))))
	defer cancel()
	readErr := o.member.readSource(readCtx, source, request.offset, request.length, request.dst)
	if errors.Is(readErr, errReadClosureUncertain) {
		return readErr
	}
	// Only this owner may attest that its storage call ended. Failure to record
	// that proof leaves the pin intact, including after a client disconnect.
	cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer stop()
	return errors.Join(readErr, o.confirmReadClosure(cleanup, operation))
}

func (o *memberOwner) registerReadPin(
	work context.Context,
	tx pgx.Tx,
	operation ID,
	request pinnedReadRequest,
) (memberReadSource, float64, error) {
	m := o.member
	if _, err := lockReceipt(work, tx, request.organization, request.receipt); err != nil {
		return memberReadSource{}, 0, err
	}
	remaining, err := parentReadLease(work, tx, request.pin, request.receipt)
	if err != nil {
		return memberReadSource{}, 0, err
	}
	if err = o.guard(work, tx); err != nil {
		return memberReadSource{}, 0, err
	}
	source, err := resolveMemberRead(work, tx, m, request.pin, request.organization, request.receipt)
	if err != nil {
		return memberReadSource{}, 0, err
	}
	loc := source.locator
	incarnation := loc.File
	if !validOwnedReadRange(loc, request) {
		return memberReadSource{}, 0, hotstore.ErrInvalidLocator
	}
	if _, err = dbgen.New(tx).
		MemberOwnerReadInsertMemberRead(work, dbgen.MemberOwnerReadInsertMemberReadParams{
			ID:          operation.String(),
			Member:      m.id.String(),
			OwnerEpoch:  o.epoch,
			Receipt:     request.receipt.String(),
			ParentPin:   request.pin.String(),
			Incarnation: incarnation.String(),
		}); err != nil {
		return memberReadSource{}, 0, err
	}
	if _, err = dbgen.New(tx).
		MemberOwnerReadInsertReadPin(work, dbgen.MemberOwnerReadInsertReadPinParams{
			ID:          operation.String(),
			Receipt:     request.receipt.String(),
			OwnerMember: new(m.id.String()),
			OwnerEpoch:  new(o.epoch),
			Incarnation: new(incarnation.String()),
			ID_2:        request.pin.String(),
		}); err != nil {
		return memberReadSource{}, 0, err
	}
	if err = commitReadRegistration(work, tx); err != nil {
		return memberReadSource{}, 0, err
	} // Unknown pin registration remains debt.
	return source, remaining, nil
}

// Cancellation can refuse a registration only before COMMIT is sent. Once sent,
// cancelling the wait would turn a visible pin into unknown debt although no
// storage call began, so the outcome is awaited and closure can still be proven.
func commitReadRegistration(work context.Context, tx pgx.Tx) error {
	if err := work.Err(); err != nil {
		return err
	}
	settle, cancel := context.WithTimeout(context.WithoutCancel(work), 5*time.Second)
	defer cancel()
	return commit(settle, tx)
}

func parentReadLease(ctx context.Context, tx pgx.Tx, parentPin, receipt ID) (float64, error) {
	value, err := dbgen.New(tx).
		MemberOwnerReadReadReadPin(ctx, dbgen.MemberOwnerReadReadReadPinParams{
			ID:      parentPin.String(),
			Receipt: receipt.String(),
		})
	if err != nil {
		return 0, err
	}
	number, err := value.Float64Value()
	return number.Float64, err
}

func validOwnedReadRange(loc hotstore.Locator, request pinnedReadRequest) bool {
	return request.offset >= 0 && request.length >= 0 && request.offset <= loc.Size &&
		request.length <= loc.Size-request.offset &&
		loc.Key.Receipt == request.receipt
}

func (o *memberOwner) confirmReadClosure(cleanup context.Context, operation ID) error {
	m := o.member
	closeTx, err := (&authority{pool: o.member.pool}).begin(cleanup)
	if err != nil {
		return err
	}
	defer rollback(cleanup, closeTx)
	tag, err := dbgen.New(closeTx).
		MemberOwnerReadUpdateMemberRead(cleanup, dbgen.MemberOwnerReadUpdateMemberReadParams{
			ID:         operation.String(),
			Member:     m.id.String(),
			OwnerEpoch: o.epoch,
		})
	if err == nil && tag.RowsAffected() != 1 {
		err = ErrStale
	}
	if err == nil {
		_, err = dbgen.New(closeTx).
			MemberOwnerReadDeleteReadPin(cleanup, dbgen.MemberOwnerReadDeleteReadPinParams{
				ID:          operation.String(),
				OwnerMember: new(m.id.String()),
				OwnerEpoch:  new(o.epoch),
			})
	}
	if err == nil {
		err = commit(cleanup, closeTx)
	}
	return err
}
