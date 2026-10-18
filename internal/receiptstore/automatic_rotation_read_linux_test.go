//go:build linux

package receiptstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/cotocisternas/fry/internal/hotstore"
)

func automaticReadFixture(t *testing.T, records int64, fileRecords int64) *fixture {
	t.Helper()
	e, err := hotstore.EncodedRecordBytes(256 << 10)
	requireReceiptNoError(t, err)
	c := 64 + fileRecords*e
	return setupWithPoolConfig(
		t,
		nil,
		hotstore.Options{
			MaxMessageBytes:      256 << 10,
			MaxConcurrentAppends: 2,
			MaxRecords:           records,
			MaxFileBytes:         c,
			MaxBytes:             3*c + 4*e,
		},
		Options{Lease: 10 * time.Second, RecoveryRetention: time.Hour},
	)
}

func TestAutomaticRotationBusyReaderAndQueuedWriterReturnPromptly(t *testing.T) {
	for _, remoteRead := range []bool{false, true} {
		t.Run(fmt.Sprintf("remote=%t", remoteRead), func(t *testing.T) {
			testAutomaticRotationBusyReader(t, remoteRead)
		})
	}
}

func TestAutomaticRotationRefundRestoresFitBeforeEligibilityCheck(t *testing.T) {
	f := automaticReadFixture(t, 3, 2)
	ctx := context.Background()
	envA, envB := f.envelope(t), f.envelope(t)
	a, b := prepare(t, f, newID(t), envA), prepare(t, f, newID(t), envB)
	var old [2]ID
	for i, m := range f.members {
		old[i] = m.bytes.FileID()
	}
	{
		_, err := capacityAccept(ctx, f, newID(t), f.envelope(t), "temporarily no max reservation fits")
		requireReceiptCondition(
			t,
			errors.Is(err, hotstore.ErrCapacity),
			"fixture did not exhaust byte reservations",
			err,
		)
	}
	// Resume the actual prepared attempts through public acceptance. Both refunds
	// leave a record slot and enough committed bytes for another maximum message.
	expire(t, f, a.Attempt)
	{
		_, err := capacityAccept(ctx, f, a.Attempt, envA, "small A")
		requireReceiptCondition(t, err == nil, err)
	}
	expire(t, f, b.Attempt)
	{
		_, err := capacityAccept(ctx, f, b.Attempt, envB, "small B")
		requireReceiptCondition(t, err == nil, err)
	}
	for i, m := range f.members {
		requireReceiptNoError(t, f.s.RotateHotIfNeeded(ctx, m.id))
		requireReceiptCondition(
			t,
			m.bytes.FileID() == old[i],
			"automatic rotation sealed a file after refunds restored fit",
		)
	}
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT count(*) FROM fry_member_file WHERE state='sealed'`) == 0,
		"conditional no-op created sealed history",
	)
	{
		_, err := capacityAccept(ctx, f, newID(t), f.envelope(t), "third current original")
		requireReceiptCondition(t, err == nil, "refund capacity not usable without rotation", err)
	}
	for i, m := range f.members {
		requireReceiptCondition(t, m.bytes.FileID() == old[i], "current identity changed on no-op")
	}
}

func TestAutomaticRotationNoopReplayCannotLaterRotateFullFile(t *testing.T) {
	f := automaticReadFixture(t, 1, 1)
	ctx := context.Background()
	s, members, _ := remoteMaintenance(t, f)
	operation := newID(t)
	before := f.members[0].bytes.FileID()
	requireReceiptNoError(t, members[0].RotateHotIfNeeded(ctx, operation))
	{
		state, err := members[0].AutomaticRotationStatus(ctx, operation)
		requireReceiptCondition(t, err == nil, "no-op status not durable", state, err)
		requireReceiptCondition(t, state == "complete", "no-op status not durable", state, err)
	}
	accept(t, s, newID(t), f.envelope(t), string(sealedReadBody()))
	{
		err := members[0].RotateHotIfNeeded(ctx, operation)
		requireReceiptCondition(t, err == nil, "durable no-op replay failed", err)
	}
	requireReceiptCondition(t, f.members[0].bytes.FileID() == before, "same completed no-op later rotated a full file")
	requireReceiptCondition(
		t,
		scalar(
			t,
			f.pool,
			`SELECT count(*) FROM fry_member_file WHERE member=$1 AND state='sealed'`,
			f.members[0].id.String(),
		) == 0,
		"same completed no-op later rotated a full file",
	)
	requireReceiptCondition(
		t,
		scalar(
			t,
			f.pool,
			`SELECT count(*) FROM fry_member_maintenance WHERE id=$1 AND kind='auto-rotate' AND `+
				`state='complete'`,
			operation.String(),
		) == 1,
		"conditional replay lost operation identity",
	)
	{
		err := members[0].RotateHotIfNeeded(ctx, newID(t))
		requireReceiptCondition(t, err == nil, "new operation did not rotate genuinely full file", err)
	}
	requireReceiptCondition(t, f.members[0].bytes.FileID() != before, "full file remained active")
}

func TestAutomaticRotationResumesAcceptanceAndPreservesSoleHistoricalReads(t *testing.T) {
	for _, remoteRead := range []bool{false, true} {
		t.Run(fmt.Sprintf("remote=%t", remoteRead), func(t *testing.T) {
			f := automaticReadFixture(t, 1, 1)
			ctx := context.Background()
			s := f.s
			var servers [2]*MemberServer
			if remoteRead {
				s, _, servers = remoteMaintenance(t, f)
			}
			body := sealedReadBody()
			original := accept(t, s, newID(t), f.envelope(t), string(body))
			attempt := newID(t)
			env := f.envelope(t)
			{
				_, err := s.Accept(ctx, attempt, env, io.NopCloser(bytes.NewBufferString("new current original")))
				requireReceiptCondition(t, errors.Is(err, hotstore.ErrCapacity), "full file did not defer", err)
			}

			var old [2]ID
			for i, m := range f.members {
				old[i] = m.bytes.FileID()
				requireReceiptNoError(t, s.RotateHotIfNeeded(ctx, m.id))
				requireReceiptCondition(t, m.bytes.FileID() != old[i], "full bounded file did not rotate")
			}
			fresh := accept(t, s, attempt, env, "new current original")
			if remoteRead {
				requireReceiptNoError(t, servers[1].Close(ctx))
			} else {
				requireReceiptNoError(t, f.members[1].Close())
			}
			requireSealedRead(t, s, f.org, original, body)
			var out bytes.Buffer
			{
				err := s.Read(ctx, f.org, fresh.ID, 0, fresh.Size, &out)
				requireReceiptCondition(t, err == nil, "acceptance did not resume on new active file", err)
				requireReceiptCondition(
					t,
					out.String() == "new current original",
					"acceptance did not resume on new active file",
					err,
				)
			}
		})
	}
}

func testAutomaticRotationBusyReader(t *testing.T, remoteRead bool) {
	t.Helper()

	f := automaticReadFixture(t, 1, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	s := f.s
	var servers [2]*MemberServer
	if remoteRead {
		s, _, servers = remoteMaintenance(t, f)
	}
	body := sealedReadBody()
	r := accept(t, s, newID(t), f.envelope(t), string(body))
	m := f.members[0]
	if !remoteRead && f.members[1].id.String() < m.id.String() {
		m = f.members[1]
	}
	parent, err := s.Open(ctx, f.org, r.ID, 0, r.Size)
	requireReceiptNoError(t, err)
	defer func() {
		if err := parent.Close(); err != nil {
			t.Error(err)
		}
	}()
	var releaseReader func() error
	if remoteRead {
		release, entered := startAutomaticRemoteReader(t, ctx, f, servers, m, r)
		releaseReader = release
		defer func() {
			if err := releaseReader(); err != nil {
				t.Error(err)
			}
		}()
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("remote descriptor did not start")
		}
		err = parent.Close()
		requireReceiptNoError(t, err)
	} else {
		prefix := make([]byte, 16)
		{
			_, err = io.ReadFull(parent, prefix)
			requireReceiptCondition(t, err == nil, "actual local descriptor did not start", err)
			requireReceiptCondition(t, bytes.Equal(prefix, body[:16]), "actual local descriptor did not start", err)
		}
		releaseReader = parent.Close
	}
	manual := make(chan error, 1)
	go func() { manual <- s.RotateHot(ctx, m.id) }()
	requireAutomaticWriterQueued(t, ctx, m, manual, releaseReader)
	requirePromptConditionalRotation(t, ctx, s, m, manual, releaseReader)
}

func startAutomaticRemoteReader(
	t *testing.T,
	ctx context.Context,
	f *fixture,
	servers [2]*MemberServer,
	m *LocalMember,
	r Receipt,
) (func() error, <-chan struct{}) {
	t.Helper()
	var err error
	var releaseReader func() error
	var raw string
	err = f.pool.QueryRow(ctx, `SELECT id FROM fry_read_pin WHERE receipt=$1 AND owner_member IS NULL`, r.ID.String()).
		Scan(&raw)
	requireReceiptNoError(t, err)
	pin, e := idFrom(raw)
	requireReceiptNoError(t, e)
	held := &heldReadWriter{entered: make(chan struct{}), release: make(chan struct{})}
	done := make(chan error, 1)
	var once sync.Once
	var readErr error
	go func() {
		done <- servers[0].owner.read(ctx, newID(t), pin, f.org, r.ID, m.bytes.FileID(), 0, r.Size, held)
	}()
	releaseReader = func() error {
		once.Do(func() {
			close(held.release)
			select {
			case readErr = <-done:
			case <-ctx.Done():
				readErr = ctx.Err()
			}
		})
		return readErr
	}
	return releaseReader, held.entered
}

func automaticWriterQueued(t *testing.T, m *LocalMember) bool {
	t.Helper()
	queued := false
	until := time.Now().Add(3 * time.Second)
	for time.Now().Before(until) {
		if m.gate.TryRLock() {
			m.gate.RUnlock()
			time.Sleep(time.Millisecond)
		} else {
			queued = true
			break
		}
	}
	return queued
}

func requirePromptConditionalRotation(
	t *testing.T,
	ctx context.Context,
	s *Store,
	m *LocalMember,
	manual <-chan error,
	releaseReader func() error,
) {
	t.Helper()
	conditional := make(chan error, 1)
	go func() { conditional <- s.RotateHotIfNeeded(ctx, m.id) }()
	var autoErr error
	prompt := false
	select {
	case autoErr = <-conditional:
		prompt = true
	case <-time.After(time.Second):
	}
	// Always release and join before reporting a lock-order regression.
	readErr := releaseReader()
	var manualErr error
	select {
	case manualErr = <-manual:
	case <-ctx.Done():
		manualErr = ctx.Err()
	}
	if !prompt {
		select {
		case autoErr = <-conditional:
		case <-ctx.Done():
			autoErr = ctx.Err()
		}
	}
	requireReceiptCondition(t, readErr == nil, "reader/manual writer failed to join", readErr, manualErr)
	requireReceiptCondition(t, manualErr == nil, "reader/manual writer failed to join", readErr, manualErr)
	requireReceiptCondition(t, prompt, "conditional rotation waited behind reader/queued writer", prompt, autoErr)
	requireReceiptCondition(
		t,
		errors.Is(autoErr, ErrBusy),
		"conditional rotation waited behind reader/queued writer",
		prompt,
		autoErr,
	)
}

func requireAutomaticWriterQueued(
	t *testing.T,
	ctx context.Context,
	m *LocalMember,
	manual <-chan error,
	releaseReader func() error,
) {
	t.Helper()
	// A failed TryRLock while an actual reader still owns the shared gate proves
	// a writer is queued, without depending on guessed HTTP or fsync timings.
	queued := automaticWriterQueued(t, m)
	if !queued {
		if err := releaseReader(); err != nil {
			t.Error(err)
		}
		select {
		case drainErr := <-manual:
			if drainErr != nil {
				t.Error(drainErr)
			}
		case <-ctx.Done():
		}
		t.Fatal("manual writer did not reach the gate")
	}
}
