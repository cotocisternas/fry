//go:build linux

package receiptstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cotocisternas/fry/internal/hotstore"
)

func rotationFixture(t *testing.T) *fixture {
	t.Helper()
	o := capacityOptions(16)
	o.MaxBytes += 4 * o.MaxFileBytes
	return setupWithPoolConfig(t, nil, o, Options{Lease: 10 * time.Second, RecoveryRetention: time.Second})
}

func TestRotateHotPreservesTargetsAndHistoricalFiles(t *testing.T) {
	f := rotationFixture(t)
	ctx := context.Background()
	m := f.members[0]
	first, err := capacityAccept(ctx, f, newID(t), f.envelope(t), "first sealed original")
	requireReceiptNoError(t, err)
	old := m.bytes.FileID()
	var targets string
	err = f.pool.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(t) ORDER BY member)::text FROM fry_target t WHERE `+
		`attempt=(SELECT attempt FROM fry_receipt WHERE id=$1)`, first.ID.String()).
		Scan(&targets)
	requireReceiptNoError(t, err)
	err = f.s.RotateHot(ctx, m.id)
	requireReceiptNoError(t, err)
	requireReceiptCondition(t, old != m.bytes.FileID(), "rotation retained current identity")
	requireReceiptCondition(
		t,
		scalar(
			t,
			f.pool,
			`SELECT count(*) FROM fry_member_file WHERE member=$1 AND file_id=$2 AND `+
				`state='sealed' AND sealed_records=1`,
			m.id.String(),
			old.String(),
		) == 1,
		"source not sealed",
	)
	var after string
	err = f.pool.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(t) ORDER BY member)::text FROM fry_target t WHERE `+
		`attempt=(SELECT attempt FROM fry_receipt WHERE id=$1)`, first.ID.String()).
		Scan(&after)
	requireReceiptNoError(t, err)
	requireReceiptCondition(t, after == targets, "rotation changed accepted targets")
	_, err = capacityAccept(ctx, f, newID(t), f.envelope(t), "second sealed original")
	requireReceiptNoError(t, err)
	err = f.s.RotateHot(ctx, m.id)
	requireReceiptNoError(t, err)
	requireReceiptCondition(
		t,
		scalar(
			t,
			f.pool,
			`SELECT count(*) FROM fry_member_file WHERE member=$1 AND state='sealed'`,
			m.id.String(),
		) == 2,
		"historical sealed file lost",
	)
	err = m.Close()
	requireReceiptNoError(t, err)
	reopened, err := OpenLocalMember(context.Background(), f.pool, m.id, m.domain, m.root, m.opts)
	requireReceiptCondition(t, err == nil, "historical rotation preflight", err)
	err = reopened.Close()
	requireReceiptNoError(t, err)
}

func TestRotateHotPreparingBlocksWithoutSuccessor(t *testing.T) {
	f := rotationFixture(t)
	ctx := context.Background()
	m := f.members[0]
	{
		_, err := capacityAccept(ctx, f, newID(t), f.envelope(t), "accepted original")
		requireReceiptCondition(t, err == nil, err)
	}
	prepare(t, f, newID(t), f.envelope(t))
	old := m.bytes.FileID()
	{
		err := f.s.RotateHot(ctx, m.id)
		requireReceiptCondition(t, err != nil, "preparing target did not block")
	}
	requireReceiptCondition(t, old == m.bytes.FileID(), "blocked census created successor")
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT count(*) FROM fry_hot_rewrite WHERE member=$1`, m.id.String()) == 0,
		"blocked census created successor",
	)
}

func TestRotateHotPublishedRecoveryNeverUnlinksSource(t *testing.T) {
	f := rotationFixture(t)
	ctx := context.Background()
	m := f.members[0]
	{
		_, err := capacityAccept(ctx, f, newID(t), f.envelope(t), "published source original")
		requireReceiptCondition(t, err == nil, err)
	}
	old := m.bytes.FileID()
	oldPath := filepath.Join(m.root, m.directory, "data.fry")
	before, err := storageReadFile(filepath.Dir(oldPath), filepath.Base(oldPath))
	requireReceiptNoError(t, err)
	cut := errors.New("synthetic publication reply loss")
	m.rewriteFault = func(at string) error {
		if at == "after_publish" {
			return cut
		}
		return nil
	}
	{
		err = f.s.RotateHot(ctx, m.id)
		requireReceiptCondition(t, errors.Is(err, cut), err)
	}
	requireReceiptCondition(t, m.isFrozen(), "ambiguous publication did not freeze")
	m.rewriteFault = nil
	err = m.Close()
	requireReceiptNoError(t, err)
	next, err := OpenLocalMember(context.Background(), f.pool, m.id, m.domain, m.root, m.opts)
	requireReceiptNoError(t, err)
	defer func() {
		if err := next.Close(); err != nil {
			t.Error(err)
		}
	}()
	after, err := storageReadFile(filepath.Dir(oldPath), filepath.Base(oldPath))
	requireReceiptCondition(t, err == nil, "published source modified/deleted", err)
	requireReceiptCondition(t, string(after) == string(before), "published source modified/deleted", err)
	requireReceiptCondition(t, next.bytes.FileID() != old, "recovery did not select published successor")
	requireReceiptCondition(
		t,
		scalar(
			t,
			f.pool,
			`SELECT count(*) FROM fry_hot_rewrite WHERE member=$1 AND kind='rotate' AND `+
				`state='complete'`,
			m.id.String(),
		) == 1,
		"rotation not completed",
	)
	requireReceiptCondition(
		t,
		scalar(
			t,
			f.pool,
			`SELECT count(*) FROM fry_target WHERE member=$1 AND incarnation=$2 AND NOT `+
				`bytes_removed`,
			m.id.String(),
			old.String(),
		) == 1,
		"sealed target debt changed",
	)
}

func TestRotateHotRequiresSpareCapacity(t *testing.T) {
	f := capacityFixture(t, 1)
	ctx := context.Background()
	m := f.members[0]
	{
		_, err := capacityAccept(ctx, f, newID(t), f.envelope(t), "original")
		requireReceiptCondition(t, err == nil, err)
	}
	old := m.bytes.FileID()
	{
		err := f.s.RotateHot(ctx, m.id)
		requireReceiptCondition(t, errors.Is(err, hotstore.ErrCapacity), "expected spare capacity rejection", err)
	}
	requireReceiptCondition(t, m.bytes.FileID() == old, "capacity rejection switched current")
}

func TestRemoteRotateHotOperationReplayAndKindBinding(t *testing.T) {
	f := rotationFixture(t)
	ctx := context.Background()
	s, members, _ := remoteMaintenance(t, f)
	accept(t, s, newID(t), f.envelope(t), "remote sealed original")
	operation := newID(t)
	old := members[0].identity().Incarnation
	requireReceiptNoError(t, members[0].RotateHot(ctx, operation))
	requireReceiptCondition(t, old != members[0].identity().Incarnation, "remote rotation did not refresh")
	{
		state, err := members[0].RotationStatus(ctx, operation)
		requireReceiptCondition(t, err == nil, state, err)
		requireReceiptCondition(t, state == "complete", state, err)
	}
	{
		err := members[0].RotateHot(ctx, operation)
		requireReceiptCondition(t, err == nil, "same operation replay", err)
	}
	requireReceiptCondition(
		t,
		scalar(
			t,
			f.pool,
			`SELECT count(*) FROM fry_hot_rewrite WHERE member=$1 AND kind='rotate'`,
			members[0].identity().Member.String(),
		) == 1,
		"replay rotated twice",
	)
	{
		err := members[0].ReclaimHot(ctx, operation)
		requireReceiptCondition(t, err != nil, "rotation id reused as rewrite")
	}
	{
		_, err := members[0].MaintenanceStatus(ctx, operation)
		requireReceiptCondition(t, err != nil, "rotation status accepted rewrite route")
	}
}

func TestRotateHotBuildingRecoveryRequiresEmptySuccessor(t *testing.T) {
	for _, unexpected := range []bool{false, true} {
		t.Run(map[bool]string{false: "empty", true: "unexpected-payload"}[unexpected], func(t *testing.T) {
			testRotationBuildingRecovery(t, unexpected)
		})
	}
}

func TestRotateHotConfirmedAbortPreservesReservationDebt(t *testing.T) {
	f := rotationFixture(t)
	ctx := context.Background()
	m := f.members[0]
	{
		_, err := capacityAccept(ctx, f, newID(t), f.envelope(t), "retained source")
		requireReceiptCondition(t, err == nil, err)
	}
	attempt := newID(t)
	claim := prepare(t, f, attempt, f.envelope(t))
	copies(t, f, claim, "aborted physical original")
	requireReceiptNoError(t, f.s.Abort(ctx, f.org, attempt))
	old := m.bytes.FileID()
	var before string
	requireReceiptNoError(
		t,
		f.pool.QueryRow(
			ctx,
			`SELECT to_jsonb(t)::text FROM fry_target t WHERE attempt=$1 AND member=$2`,
			attempt.String(),
			m.id.String(),
		).
			Scan(&before),
	)
	requireReceiptNoError(t, f.s.RotateHot(ctx, m.id))
	var after string
	requireReceiptNoError(
		t,
		f.pool.QueryRow(
			ctx,
			`SELECT to_jsonb(t)::text FROM fry_target t WHERE attempt=$1 AND member=$2`,
			attempt.String(),
			m.id.String(),
		).
			Scan(&after),
	)
	requireReceiptCondition(t, before == after, "rotation rewrote aborted target/debt")
	requireReceiptCondition(
		t,
		scalar(
			t,
			f.pool,
			`SELECT sealed_records FROM fry_member_file WHERE member=$1 AND file_id=$2`,
			m.id.String(),
			old.String(),
		) == 2,
		"sealed census omitted aborted bytes",
	)
}

func TestRemoteRotateHotStatusRejectsPreviousOwnerGeneration(t *testing.T) {
	f := rotationFixture(t)
	ctx := context.Background()
	s, members, servers := remoteMaintenance(t, f)
	accept(t, s, newID(t), f.envelope(t), "generation original")
	operation := newID(t)
	requireReceiptNoError(t, members[0].RotateHot(ctx, operation))
	oldEpoch := servers[0].owner.epoch
	m := f.members[0]
	requireReceiptNoError(t, servers[0].Close(ctx))
	next, err := OpenLocalMember(context.Background(), f.pool, m.id, m.domain, m.root, m.opts)
	requireReceiptNoError(t, err)
	copyFixture := *f
	copyFixture.members[0] = next
	server, certs, url := memberHTTP(t, &copyFixture)
	requireReceiptCondition(t, server.owner.epoch != oldEpoch, "owner epoch not advanced")
	metadata := memberRequest{Operation: operation, OwnerEpoch: server.owner.epoch, Incarnation: next.bytes.FileID()}
	for _, path := range []string{"/v1/rotate/status", "/v1/rotate"} {
		body, err := json.Marshal(metadata)
		requireReceiptNoError(t, err)
		response, err := receiptTestRequest(t, certs.allowed, http.MethodPost, url+path, bytes.NewReader(body))
		requireReceiptNoError(t, err)
		if err := response.Body.Close(); err != nil {
			t.Error(err)
		}
		requireReceiptCondition(
			t,
			path != "/v1/rotate/status" || response.StatusCode == http.StatusConflict,
			path,
			response.Status,
		)
		requireReceiptCondition(
			t,
			path != "/v1/rotate" || response.StatusCode == http.StatusServiceUnavailable,
			path,
			response.Status,
		)
	}
}

func TestRotateHotCensusRequiresHealthyOpenStore(t *testing.T) {
	f := rotationFixture(t)
	ctx := context.Background()
	m := f.members[0]
	{
		_, err := capacityAccept(ctx, f, newID(t), f.envelope(t), "health gate original")
		requireReceiptCondition(t, err == nil, err)
	}
	requireReceiptNoError(t, m.bytes.Close())
	{
		err := f.s.RotateHot(ctx, m.id)
		requireReceiptCondition(t, errors.Is(err, hotstore.ErrClosed), "closed byte store was sealed", err)
	}
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT count(*) FROM fry_hot_rewrite WHERE member=$1`, m.id.String()) == 0,
		"unhealthy census reserved successor",
	)
}

func testRotationBuildingRecovery(t *testing.T, unexpected bool) {
	t.Helper()

	f := rotationFixture(t)
	ctx := context.Background()
	m := f.members[0]
	{
		_, err := capacityAccept(ctx, f, newID(t), f.envelope(t), "source original")
		requireReceiptCondition(t, err == nil, err)
	}
	old := m.bytes.FileID()
	var path string
	cut := errors.New("registered successor cut")
	m.rewriteFault = func(at string) error {
		if at != "after_register" {
			return nil
		}
		var directory string
		if err := f.pool.QueryRow(
			ctx,
			`SELECT new_directory FROM fry_hot_rewrite WHERE member=$1 AND state='building'`,
			m.id.String(),
		).
			Scan(&directory); err != nil {
			return err
		}
		path = filepath.Join(m.root, directory, "data.fry")
		if err := appendUnexpectedRotationPayload(t, path, unexpected); err != nil {
			return err
		}
		return cut
	}
	{
		err := f.s.RotateHot(ctx, m.id)
		requireReceiptCondition(t, errors.Is(err, cut), err)
	}
	requireReceiptCondition(t, m.bytes.FileID() == old, "failed building rotated source")
	if unexpected {
		requireReceiptCondition(t, m.isFrozen(), "unexpected successor payload not frozen")
		{
			b, err := storageReadFile(filepath.Dir(path), filepath.Base(path))
			requireReceiptCondition(t, err == nil, "unexpected payload removed", err)
			requireReceiptCondition(t, len(b) > 64, "unexpected payload removed", err)
		}
	} else {
		{
			_, err := os.Stat(path)
			requireReceiptCondition(t, errors.Is(err, os.ErrNotExist), "empty successor not cleaned", err)
		}
		requireReceiptCondition(t, !m.isFrozen(), "known empty recovery froze")
	}
}

func appendUnexpectedRotationPayload(t *testing.T, path string, unexpected bool) error {
	t.Helper()
	if unexpected {
		file, err := splitStorageFile(path, os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			return err
		}
		_, err = file.Write([]byte("unexpected data"))
		if closeErr := file.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return err
		}
	}
	return nil
}
