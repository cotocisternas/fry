//go:build linux

package receiptstore

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"math"
	"sync"
	"time"

	"github.com/cotocisternas/fry/internal/hotstore"
	"github.com/cotocisternas/fry/internal/receiptstore/dbgen"

	"github.com/jackc/pgx/v5"
)

// memberOwner is the process-transport ownership prerequisite. It is not yet
// used by the in-process adapter. A lost database session fences admission and
// cancels work, but never silently closes the durable generation's obligations.
// Automatic recovery of an unclosed generation requires trusted external fencing
// and is deliberately absent. The endpoint must own the supplied member alone.
type memberOwner struct {
	member      *LocalMember
	epoch       int64
	instance    ID
	connection  *pgx.Conn
	ctx         context.Context
	cancel      context.CancelFunc
	monitorDone chan struct{}
	mu          sync.Mutex
	closing     bool
	operations  sync.WaitGroup
	closeOnce   sync.Once
	closeDone   chan struct{}
	closeErr    error
}

func ownMember(ctx context.Context, member *LocalMember) (_ *memberOwner, resultErr error) {
	if member == nil {
		return nil, ErrConflict
	}
	member.gate.Lock()
	defer member.gate.Unlock()
	unavailable := member.closed || member.isFrozen()
	if unavailable {
		return nil, ErrStale
	}
	if member.transportOwned {
		return nil, ErrBusy
	}
	connection, err := pgx.ConnectConfig(ctx, member.pool.Config().ConnConfig.Copy())
	if err != nil {
		return nil, err
	}
	keep := false
	defer closeUnkeptOwnerConnection(ctx, connection, &keep, &resultErr)
	if err = lockMemberOwner(ctx, connection, member.id); err != nil {
		return nil, err
	}
	tx, err := connection.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer rollback(ctx, tx)
	instance, epoch, err := registerMemberOwner(ctx, tx, member)
	if err != nil {
		return nil, err
	}
	work, cancel := context.WithCancel(context.WithoutCancel(ctx))
	o := &memberOwner{
		member:      member,
		epoch:       epoch,
		instance:    instance,
		connection:  connection,
		ctx:         work,
		cancel:      cancel,
		monitorDone: make(chan struct{}),
		closeDone:   make(chan struct{}),
	}
	member.transportOwned = true
	keep = true
	go o.monitor(work)
	return o, nil
}

func (o *memberOwner) monitor(ctx context.Context) {
	defer close(o.monitorDone)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-o.ctx.Done():
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(ctx, time.Second)
			err := o.connection.Ping(ctx)
			cancel()
			if err != nil {
				o.cancel()
				return
			}
		}
	}
}

// begin returns an operation context and a completion function. The endpoint
// must close its actual storage handles before completion; client disconnect is
// only cancellation. Close waits for all such completions before recording proof.
func (o *memberOwner) begin(ctx context.Context) (context.Context, func(), error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closing || o.ctx.Err() != nil {
		return nil, nil, ErrStale
	}
	o.operations.Add(1)
	work, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(o.ctx, cancel)
	var once sync.Once
	return work, func() { once.Do(func() { stop(); cancel(); o.operations.Done() }) }, nil
}

// guard follows attempt/receipt guards in transactions that publish bytes or
// confirm closure. The member row prevents replacement during that transaction.
func (o *memberOwner) guard(ctx context.Context, tx pgx.Tx) error {
	if o.ctx.Err() != nil {
		return ErrStale
	}
	var member string
	if err := func() error {
		sqlcResult, sqlcErr := dbgen.New(tx).MemberOwnerGuardReadMember(ctx, o.member.id.String())
		if sqlcErr != nil {
			return sqlcErr
		}
		member = sqlcResult
		return nil
	}(); err != nil {
		return err
	}
	var epoch int64
	var instance string
	var closed bool
	if err := func() error {
		sqlcResult, sqlcErr := dbgen.New(tx).MemberOwnerGuardReadMemberOwner(ctx, member)
		if sqlcErr != nil {
			return sqlcErr
		}
		epoch = sqlcResult.Epoch
		instance = sqlcResult.Instance
		closed = sqlcResult.Closed
		return nil
	}(); err != nil {
		return err
	}
	if epoch != o.epoch || instance != o.instance.String() || closed {
		return ErrStale
	}
	return nil
}

// Close first cancels work, then drains it and closes the actual member. A
// caller deadline can stop waiting, but cannot fabricate closure acknowledgment.
func (o *memberOwner) Close(ctx context.Context) error {
	o.closeOnce.Do(func() {
		o.mu.Lock()
		o.closing = true
		o.cancel()
		o.mu.Unlock()
		go func() {
			defer close(o.closeDone)
			o.operations.Wait()
			<-o.monitorDone
			err := o.member.Close()
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			if err == nil {
				var tagErr error
				tag, writeErr := dbgen.New(o.member.pool).
					MemberOwnerClose(cleanup, dbgen.MemberOwnerCloseParams{
						Member:   o.member.id.String(),
						Epoch:    o.epoch,
						Instance: o.instance.String(),
					})
				if writeErr == nil && tag.RowsAffected() != 1 {
					tagErr = ErrStale
				}
				err = errors.Join(writeErr, tagErr)
			}
			o.closeErr = errors.Join(err, o.connection.Close(cleanup))
		}()
	})
	select {
	case <-o.closeDone:
		return o.closeErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func closeUnkeptOwnerConnection(ctx context.Context, connection *pgx.Conn, keep *bool, resultErr *error) {
	if !*keep {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		*resultErr = errors.Join(*resultErr, connection.Close(closeCtx))
	}
}

func lockMemberOwner(ctx context.Context, connection *pgx.Conn, id ID) error {
	digest := sha256.Sum256(append([]byte("fry/member-owner/"), id[:]...))
	// Preserve all 64 hash bits as the signed PostgreSQL advisory key. The
	// masked conversion fits int64; adding MinInt64 sets the original high bit.
	key := int64(binary.BigEndian.Uint64(digest[:8]) & math.MaxInt64)
	if digest[0]&0x80 != 0 {
		key += math.MinInt64
	}
	var locked bool
	if err := func() error {
		sqlcResult, sqlcErr := dbgen.New(connection).OwnMemberReadAdvisoryLock(ctx, key)
		if sqlcErr != nil {
			return sqlcErr
		}
		locked = sqlcResult
		return nil
	}(); err != nil {
		return err
	}
	if !locked {
		return ErrBusy
	}
	return nil
}

func registerMemberOwner(ctx context.Context, tx pgx.Tx, member *LocalMember) (ID, int64, error) {
	if _, err := tx.Exec(ctx, `SET LOCAL synchronous_commit='on'`); err != nil {
		return ID{}, 0, err
	}
	var memberID string
	if err := func() error {
		sqlcResult, sqlcErr := dbgen.New(tx).OwnMemberReadMember(ctx, member.id.String())
		if sqlcErr != nil {
			return sqlcErr
		}
		memberID = sqlcResult
		return nil
	}(); err != nil {
		return ID{}, 0, err
	}
	previous, err := previousMemberOwner(ctx, tx, memberID)
	if err != nil {
		return ID{}, 0, err
	}
	instance, err := hotstore.NewID()
	if err != nil {
		return ID{}, 0, err
	}
	epoch := previous + 1
	if epoch <= 0 {
		return ID{}, 0, ErrConflict
	}
	if _, err = dbgen.New(tx).
		OwnMemberInsertMemberOwner(ctx, dbgen.OwnMemberInsertMemberOwnerParams{
			Member:   memberID,
			Epoch:    epoch,
			Instance: instance.String(),
		}); err != nil {
		return ID{}, 0, err
	}
	if err = commit(ctx, tx); err != nil {
		member.frozen = true
		return ID{}, 0, err
	} // Unknown registration remains unclosed debt.
	return instance, epoch, nil
}

func previousMemberOwner(ctx context.Context, tx pgx.Tx, member string) (int64, error) {
	stored, err := dbgen.New(tx).OwnMemberReadMemberOwner(ctx, member)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return 0, err
	}
	if err == nil && !stored.Closed {
		return 0, ErrBusy
	}
	return stored.Epoch, nil
}
