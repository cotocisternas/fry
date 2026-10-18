//go:build linux

package receiptstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cotocisternas/fry/internal/hotstore"

	"github.com/jackc/pgx/v5"
)

func sealedReclaimPair(t *testing.T) (*fixture, Receipt, Receipt, ID) {
	t.Helper()
	f := rotationFixture(t)
	ctx := context.Background()
	dead := accept(t, f.s, newID(t), f.envelope(t), "dead sealed original")
	live := accept(t, f.s, newID(t), f.envelope(t), "live sealed original")
	old := f.members[0].bytes.FileID()
	for _, m := range f.members {
		requireReceiptNoError(t, f.s.RotateHot(ctx, m.id))
	}
	expireReceipt(t, f, dead)
	return f, dead, live, old
}

func TestSealedReclaimSurvivorsPreserveCurrentAndLanes(t *testing.T) {
	f, dead, live, old := sealedReclaimPair(t)
	ctx := context.Background()
	m := f.members[0]
	current, dir := m.bytes.FileID(), m.directory
	var oldLane int64
	requireReceiptNoError(
		t,
		f.pool.QueryRow(
			ctx,
			`SELECT capacity_lane FROM fry_target WHERE member=$1 AND attempt=$2`,
			m.id.String(),
			live.Attempt.String(),
		).
			Scan(&oldLane),
	)
	requireReceiptNoError(t, f.s.ReclaimSealed(ctx, m.id))
	requireReceiptCondition(t, m.bytes.FileID() == current, "sealed reclaim changed current")
	requireReceiptCondition(t, m.directory == dir, "sealed reclaim changed current")
	requireSealedReclaimCatalog(t, f, m, old, oldLane, dead, live)
	var got bytes.Buffer
	{
		err := f.s.Read(ctx, f.org, live.ID, 0, live.Size, &got)
		requireReceiptCondition(t, err == nil, got.String(), err)
		requireReceiptCondition(t, got.String() == "live sealed original", got.String(), err)
	}
	{
		_, err := f.s.Accept(ctx, newID(t), f.envelope(t), io.NopCloser(bytes.NewBufferString("new current original")))
		requireReceiptCondition(t, err == nil, err)
	}
	requireReceiptNoError(t, m.Close())
	next, err := OpenLocalMember(context.Background(), f.pool, m.id, m.domain, m.root, m.opts)
	requireReceiptCondition(t, err == nil, "reopen survivor provenance", err)
	defer func() {
		if err := next.Close(); err != nil {
			t.Error(err)
		}
	}()
}

func TestSealedReclaimDeleteOnlyCreatesNoReplacement(t *testing.T) {
	f := rotationFixture(t)
	ctx := context.Background()
	m := f.members[0]
	dead := accept(t, f.s, newID(t), f.envelope(t), "all dead original")
	old := m.bytes.FileID()
	oldPath := filepath.Join(m.root, m.directory, "data.fry")
	requireReceiptNoError(t, f.s.RotateHot(ctx, m.id))
	expireReceipt(t, f, dead)
	current := m.bytes.FileID()
	// A delete-only operation must not reserve C: a real budget-row writer
	// remains held throughout the public operation.
	budgetTx, err := f.pool.Begin(ctx)
	requireReceiptNoError(t, err)
	defer func() {
		if err := budgetTx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			t.Error(err)
		}
	}()
	_, err = budgetTx.Exec(ctx, `SELECT member FROM fry_member_profile WHERE member=$1 FOR UPDATE`, m.id.String())
	requireReceiptNoError(t, err)
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	requireReceiptNoError(t, f.s.ReclaimSealed(bounded, m.id))
	requireReceiptCondition(t, m.bytes.FileID() == current, "delete-only changed current")
	{
		_, err := os.Stat(oldPath)
		requireReceiptCondition(t, errors.Is(err, os.ErrNotExist), "old file survived", err)
	}
	requireReceiptCondition(
		t,
		scalar(
			t,
			f.pool,
			`SELECT count(*) FROM fry_hot_rewrite WHERE member=$1 AND kind='sealed-reclaim' AND `+
				`kept_records=0 AND new_incarnation IS NULL AND reserved_bytes=0 AND state='removed'`,
			m.id.String(),
		) == 1,
		"delete-only proof missing",
	)
	requireReceiptCondition(
		t,
		scalar(
			t,
			f.pool,
			`SELECT count(*) FROM fry_member_file WHERE member=$1 AND state<>'removed'`,
			m.id.String(),
		) == 1,
		"delete-only created extra active file",
	)
	requireReceiptCondition(
		t,
		scalar(
			t,
			f.pool,
			`SELECT count(*) FROM fry_target WHERE member=$1 AND incarnation=$2 AND bytes_removed`,
			m.id.String(),
			old.String(),
		) == 1,
		"delete-only debt not cleared",
	)
}

func TestSealedReclaimFailureCutsPreserveAuthority(t *testing.T) {
	for _, cut := range []string{"after_register", "after_copy", "after_publish", "after_unlink"} {
		t.Run(cut, func(t *testing.T) {
			f, _, live, old := sealedReclaimPair(t)
			ctx := context.Background()
			m := f.members[0]
			current := m.bytes.FileID()
			_, sourcePath, _ := sealedReclaimSourcePath(t, f, m, live)
			marker := errors.New("synthetic sealed reclaim cut")
			m.rewriteFault = func(at string) error {
				if at == cut {
					return marker
				}
				return nil
			}
			{
				err := f.s.ReclaimSealed(ctx, m.id)
				requireReceiptCondition(t, errors.Is(err, marker), err)
			}
			requireReceiptCondition(t, m.bytes.FileID() == current, "failure changed current")
			if cut == "after_register" || cut == "after_copy" {
				requireReceiptCondition(t, !m.isFrozen(), "confirmed unpublished recovery froze")
				requireUnpublishedSealedTarget(t, f, m, live, old)
			} else {
				requireReceiptCondition(t, m.isFrozen(), "published uncertainty not frozen")
				m.rewriteFault = nil
				requireReceiptNoError(t, m.Close())
				next, err := OpenLocalMember(context.Background(), f.pool, m.id, m.domain, m.root, m.opts)
				requireReceiptCondition(t, err == nil, "recover", err)
				defer func() {
					if err := next.Close(); err != nil {
						t.Error(err)
					}
				}()
				requireReceiptCondition(t, next.bytes.FileID() == current, "recovery changed current")
				requireReceiptCondition(
					t,
					scalar(
						t,
						f.pool,
						`SELECT count(*) FROM fry_hot_rewrite WHERE member=$1 AND kind='sealed-reclaim' AND `+
							`state='removed'`,
						m.id.String(),
					) == 1,
					"published cleanup not completed",
				)
				requireSealedSourceReclaimed(t, f, next, old, sourcePath, live, []byte("live sealed original"))
			}
		})
	}
}

func TestSealedReclaimFiniteSetProgressPastLiveNoop(t *testing.T) {
	f := rotationFixture(t)
	ctx := context.Background()
	m := f.members[0]
	// Build two real sealed sources, then use catalog order to expire the later
	// one. Selection starts before every key, so the earlier live file is a no-op.
	first := accept(t, f.s, newID(t), f.envelope(t), "first original")
	requireReceiptNoError(t, f.s.RotateHot(ctx, m.id))
	second := accept(t, f.s, newID(t), f.envelope(t), "second original")
	requireReceiptNoError(t, f.s.RotateHot(ctx, m.id))
	var earliest string
	requireReceiptNoError(
		t,
		f.pool.QueryRow(ctx, `SELECT file_id FROM fry_member_file WHERE member=$1 AND state='sealed' ORDER BY `+
			`file_id LIMIT 1`, m.id.String()).
			Scan(&earliest),
	)
	var firstFile string
	requireReceiptNoError(
		t,
		f.pool.QueryRow(
			ctx,
			`SELECT incarnation FROM fry_target WHERE member=$1 AND attempt=$2`,
			m.id.String(),
			first.Attempt.String(),
		).
			Scan(&firstFile),
	)
	dead := first
	if firstFile == earliest {
		dead = second
	}
	expireReceipt(t, f, dead)
	requireReceiptNoError(t, f.s.ReclaimSealed(ctx, m.id))
	requireReceiptCondition(
		t,
		scalar(
			t,
			f.pool,
			`SELECT count(*) FROM fry_hot_rewrite WHERE member=$1 AND kind='sealed-reclaim'`,
			m.id.String(),
		) == 0,
		"live no-op rewrote",
	)
	requireReceiptNoError(t, f.s.ReclaimSealed(ctx, m.id))
	requireReceiptCondition(
		t,
		scalar(
			t,
			f.pool,
			`SELECT count(*) FROM fry_target WHERE member=$1 AND attempt=$2 AND bytes_removed`,
			m.id.String(),
			dead.Attempt.String(),
		) == 1,
		"no-op starved later disposable source",
	)
}

func TestSealedReclaimRemoteReplayAndStatus(t *testing.T) {
	f := rotationFixture(t)
	ctx := context.Background()
	s, members, _ := remoteMaintenance(t, f)
	dead := accept(t, s, newID(t), f.envelope(t), "dead remote original")
	live := accept(t, s, newID(t), f.envelope(t), "live remote original")
	requireReceiptNoError(t, s.RotateHot(ctx, members[0].identity().Member))
	current := members[0].identity().Incarnation
	expireReceipt(t, f, dead)
	operation := newID(t)
	requireReceiptNoError(t, members[0].ReclaimSealed(ctx, operation))
	{
		state, err := members[0].SealedReclaimStatus(ctx, operation)
		requireReceiptCondition(t, err == nil, state, err)
		requireReceiptCondition(t, state == "complete", state, err)
	}
	requireReceiptNoError(t, members[0].ReclaimSealed(ctx, operation))
	requireReceiptCondition(t, members[0].identity().Incarnation == current, "remote current changed")
	requireReceiptCondition(
		t,
		scalar(
			t,
			f.pool,
			`SELECT count(*) FROM fry_hot_rewrite WHERE member=$1 AND kind='sealed-reclaim'`,
			members[0].identity().Member.String(),
		) == 1,
		"replay selected another source",
	)
	{
		_, err := members[0].RotationStatus(ctx, operation)
		requireReceiptCondition(t, err != nil, "wrong operation kind accepted")
	}
	var got bytes.Buffer
	{
		err := s.Read(ctx, f.org, live.ID, 0, live.Size, &got)
		requireReceiptCondition(t, err == nil, got.String(), err)
		requireReceiptCondition(t, got.String() == "live remote original", got.String(), err)
	}
}

func TestSealedReclaimDeleteOnlyUnexpectedPathPreserved(t *testing.T) {
	f := rotationFixture(t)
	ctx := context.Background()
	m := f.members[0]
	dead := accept(t, f.s, newID(t), f.envelope(t), "delete-only original")
	requireReceiptNoError(t, f.s.RotateHot(ctx, m.id))
	expireReceipt(t, f, dead)
	var unexpected string
	cut := errors.New("delete-only prepublication cut")
	m.rewriteFault = func(at string) error {
		if at != "after_copy" {
			return nil
		}
		var directory string
		if err := f.pool.QueryRow(
			ctx,
			`SELECT new_directory FROM fry_hot_rewrite WHERE member=$1 AND kind='sealed-reclaim' `+
				`AND state='building'`,
			m.id.String(),
		).
			Scan(&directory); err != nil {
			return err
		}
		unexpected = filepath.Join(m.root, directory)
		if err := os.Mkdir(unexpected, 0o700); err != nil {
			return err
		}
		return cut
	}
	{
		err := f.s.ReclaimSealed(ctx, m.id)
		requireReceiptCondition(t, errors.Is(err, cut), err)
	}
	requireReceiptCondition(t, m.isFrozen(), "unexpected delete-only directory did not freeze")
	{
		_, err := os.Stat(unexpected)
		requireReceiptCondition(t, err == nil, "unexpected path removed", err)
	}
	requireReceiptCondition(
		t,
		scalar(
			t,
			f.pool,
			`SELECT count(*) FROM fry_hot_rewrite WHERE member=$1 AND kind='sealed-reclaim' AND `+
				`state='building' AND kept_records=0`,
			m.id.String(),
		) == 1,
		"unresolved delete-only authority lost",
	)
}

func TestSealedReclaimRejectsCatalogPhysicalMismatch(t *testing.T) {
	for _, kind := range []string{"count", "length"} {
		t.Run(kind, func(t *testing.T) {
			f, _, _, old := sealedReclaimPair(t)
			ctx := context.Background()
			m := f.members[0]
			var directory string
			requireReceiptNoError(
				t,
				f.pool.QueryRow(
					ctx,
					`SELECT directory FROM fry_member_file WHERE member=$1 AND file_id=$2`,
					m.id.String(),
					old.String(),
				).
					Scan(&directory),
			)
			path := filepath.Join(m.root, directory, "data.fry")
			before, err := storageReadFile(filepath.Dir(path), filepath.Base(path))
			requireReceiptNoError(t, err)
			if kind == "count" {
				execSQL(
					t,
					f.pool,
					`UPDATE fry_member_file SET sealed_records=sealed_records+1 WHERE member=$1 AND `+
						`file_id=$2`,
					m.id.String(),
					old.String(),
				)
			} else {
				err = os.Truncate(path, int64(len(before)-1))
				requireReceiptNoError(t, err)
				before = before[:len(before)-1]
			}
			{
				err = f.s.ReclaimSealed(ctx, m.id)
				requireReceiptCondition(t, err != nil, "mismatch allowed reclamation")
			}
			after, err := storageReadFile(filepath.Dir(path), filepath.Base(path))
			requireReceiptCondition(t, err == nil, "mismatched source modified", err)
			requireReceiptCondition(t, bytes.Equal(before, after), "mismatched source modified", err)
			requireReceiptCondition(
				t,
				scalar(
					t,
					f.pool,
					`SELECT count(*) FROM fry_hot_rewrite WHERE member=$1 AND kind='sealed-reclaim'`,
					m.id.String(),
				) == 0,
				"mismatched inventory created job",
			)
		})
	}
}

func TestSealedReclaimConfirmedAbortLostLocatorOrAbsent(t *testing.T) {
	for _, physical := range []bool{false, true} {
		t.Run(fmt.Sprint(physical), func(t *testing.T) {
			f := rotationFixture(t)
			ctx := context.Background()
			m := f.members[0]
			live := accept(t, f.s, newID(t), f.envelope(t), "live with aborted debt")
			attempt := newID(t)
			claim := prepare(t, f, attempt, f.envelope(t))
			if physical {
				{
					_, err := m.bytes.Append(
						ctx,
						hotstore.Key{Receipt: claim.Receipt, Attempt: claim.Attempt},
						bytes.NewBufferString("physical without recordCopy"),
					)
					requireReceiptCondition(t, err == nil, err)
				}
			}
			requireReceiptNoError(t, f.s.Abort(ctx, f.org, attempt))
			old := m.bytes.FileID()
			requireReceiptNoError(t, f.s.RotateHot(ctx, m.id))
			requireReceiptNoError(t, f.s.ReclaimSealed(ctx, m.id))
			requireReceiptCondition(
				t,
				scalar(
					t,
					f.pool,
					`SELECT count(*) FROM fry_target WHERE member=$1 AND attempt=$2 AND incarnation=$3 AND `+
						`bytes_removed AND locator IS NULL`,
					m.id.String(),
					attempt.String(),
					old.String(),
				) == 1,
				"closed aborted debt not removed",
			)
			requireReceiptCondition(
				t,
				scalar(
					t,
					f.pool,
					`SELECT count(*) FROM fry_target WHERE member=$1 AND attempt=$2 AND incarnation<>$3 `+
						`AND NOT bytes_removed`,
					m.id.String(),
					live.Attempt.String(),
					old.String(),
				) == 1,
				"survivor not retained",
			)
		})
	}
}

func TestSealedReclaimStatusRejectsNewOwnerEpoch(t *testing.T) {
	f := rotationFixture(t)
	ctx := context.Background()
	s, members, servers := remoteMaintenance(t, f)
	dead := accept(t, s, newID(t), f.envelope(t), "old owner dead original")
	requireReceiptNoError(t, s.RotateHot(ctx, members[0].identity().Member))
	expireReceipt(t, f, dead)
	operation := newID(t)
	requireReceiptNoError(t, members[0].ReclaimSealed(ctx, operation))
	oldEpoch := servers[0].owner.epoch
	m := f.members[0]
	requireReceiptNoError(t, servers[0].Close(ctx))
	next, err := OpenLocalMember(context.Background(), f.pool, m.id, m.domain, m.root, m.opts)
	requireReceiptNoError(t, err)
	copyFixture := *f
	copyFixture.members[0] = next
	server, certs, url := memberHTTP(t, &copyFixture)
	requireReceiptCondition(t, server.owner.epoch != oldEpoch, "epoch not advanced")
	data, receiptErr1 := json.Marshal(
		memberRequest{Operation: operation, OwnerEpoch: server.owner.epoch, Incarnation: next.bytes.FileID()},
	)
	requireReceiptNoError(t, receiptErr1)
	for _, path := range []string{"/v1/reclaim-sealed/status", "/v1/reclaim-sealed"} {
		response, err := receiptTestRequest(t, certs.allowed, http.MethodPost, url+path, bytes.NewReader(data))
		requireReceiptNoError(t, err)
		if err := response.Body.Close(); err != nil {
			t.Error(err)
		}
		expected := http.StatusConflict
		if path == "/v1/reclaim-sealed" {
			expected = http.StatusServiceUnavailable
		}
		requireReceiptCondition(t, response.StatusCode == expected, path, response.Status)
	}
}

func TestSealedReclaimDeleteOnlyRecoveryCuts(t *testing.T) {
	for _, cut := range []string{"after_copy", "after_publish", "after_unlink"} {
		t.Run(cut, func(t *testing.T) {
			f := rotationFixture(t)
			ctx := context.Background()
			m := f.members[0]
			dead := accept(t, f.s, newID(t), f.envelope(t), "delete-only crash original")
			old, path := m.bytes.FileID(), filepath.Join(m.root, m.directory, "data.fry")
			requireReceiptNoError(t, f.s.RotateHot(ctx, m.id))
			current := m.bytes.FileID()
			expireReceipt(t, f, dead)
			marker := errors.New("synthetic delete-only cut")
			m.rewriteFault = func(at string) error {
				if at == cut {
					return marker
				}
				return nil
			}
			{
				err := f.s.ReclaimSealed(ctx, m.id)
				requireReceiptCondition(t, errors.Is(err, marker), err)
			}
			m.rewriteFault = nil
			if cut == "after_copy" {
				requireUnpublishedDeleteOnlyDebt(t, f, m, path, dead, old)
			} else {
				requireReceiptCondition(t, m.isFrozen(), "published delete-only not frozen")
				requireReceiptNoError(t, m.Close())
				next, err := OpenLocalMember(context.Background(), f.pool, m.id, m.domain, m.root, m.opts)
				requireReceiptNoError(t, err)
				defer func() {
					if err := next.Close(); err != nil {
						t.Error(err)
					}
				}()
				requireRecoveredDeleteOnlyDebt(t, f, m, next, current, path, dead, old)
			}
			requireReceiptCondition(
				t,
				scalar(
					t,
					f.pool,
					`SELECT count(*) FROM fry_hot_rewrite WHERE member=$1 AND kind='sealed-reclaim' AND `+
						`new_incarnation IS NOT NULL`,
					m.id.String(),
				) == 0,
				"delete-only recovery invented successor",
			)
		})
	}
}

func requireSealedReclaimCatalog(t *testing.T, f *fixture, m *LocalMember, old ID, oldLane int64, dead, live Receipt) {
	t.Helper()
	requireReceiptCondition(
		t,
		scalar(
			t,
			f.pool,
			`SELECT count(*) FROM fry_member_file WHERE member=$1 AND file_id=$2 AND `+
				`state='removed' AND charged_bytes=0`,
			m.id.String(),
			old.String(),
		) == 1,
		"predecessor not removed",
	)
	requireReceiptCondition(
		t,
		scalar(
			t,
			f.pool,
			`SELECT count(*) FROM fry_target WHERE member=$1 AND attempt=$2 AND incarnation<>$3 `+
				`AND capacity_lane=$4 AND NOT bytes_removed`,
			m.id.String(),
			live.Attempt.String(),
			old.String(),
			oldLane,
		) == 1,
		"survivor lane/target not preserved",
	)
	requireReceiptCondition(
		t,
		scalar(
			t,
			f.pool,
			`SELECT count(*) FROM fry_target WHERE member=$1 AND attempt=$2 AND bytes_removed`,
			m.id.String(),
			dead.Attempt.String(),
		) == 1,
		"dead bytes not removed",
	)
}

func requireUnpublishedSealedTarget(t *testing.T, f *fixture, m *LocalMember, live Receipt, old ID) {
	t.Helper()
	requireReceiptCondition(
		t,
		scalar(
			t,
			f.pool,
			`SELECT count(*) FROM fry_target WHERE member=$1 AND attempt=$2 AND incarnation=$3 AND `+
				`NOT bytes_removed`,
			m.id.String(),
			live.Attempt.String(),
			old.String(),
		) == 1,
		"unpublished target changed",
	)
}

func requireUnpublishedDeleteOnlyDebt(t *testing.T, f *fixture, m *LocalMember, path string, dead Receipt, old ID) {
	t.Helper()
	requireReceiptCondition(t, !m.isFrozen(), "known delete-only rollback frozen")
	{
		_, err := os.Stat(path)
		requireReceiptCondition(t, err == nil, "unpublished delete-only erased source", err)
	}
	requireReceiptCondition(
		t,
		scalar(
			t,
			f.pool,
			`SELECT count(*) FROM fry_target WHERE attempt=$1 AND member=$2 AND incarnation=$3 AND `+
				`NOT bytes_removed`,
			dead.Attempt.String(),
			m.id.String(),
			old.String(),
		) == 1,
		"unpublished delete-only cleared debt",
	)
}

func requireRecoveredDeleteOnlyDebt(
	t *testing.T,
	f *fixture,
	m, next *LocalMember,
	current ID,
	path string,
	dead Receipt,
	old ID,
) {
	t.Helper()
	requireReceiptCondition(t, next.bytes.FileID() == current, "delete-only recovery changed current")
	{
		_, err := os.Stat(path)
		requireReceiptCondition(
			t,
			errors.Is(err, os.ErrNotExist),
			"recovered delete-only source exists",
			err,
		)
	}
	requireReceiptCondition(
		t,
		scalar(
			t,
			f.pool,
			`SELECT count(*) FROM fry_target WHERE attempt=$1 AND member=$2 AND incarnation=$3 AND `+
				`bytes_removed`,
			dead.Attempt.String(),
			m.id.String(),
			old.String(),
		) == 1,
		"recovered delete-only debt unresolved",
	)
}
