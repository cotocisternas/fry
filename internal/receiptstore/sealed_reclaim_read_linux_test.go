//go:build linux

package receiptstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cotocisternas/fry/internal/hotstore"
)

func sealedReclaimReaderFixture(t *testing.T) (*fixture, Receipt, []byte) {
	t.Helper()
	encoded, err := hotstore.EncodedRecordBytes(256 << 10)
	requireReceiptNoError(t, err)
	fileBytes := 64 + 4*encoded
	f := setupWithPoolConfig(
		t,
		nil,
		hotstore.Options{
			MaxMessageBytes:      256 << 10,
			MaxConcurrentAppends: 2,
			MaxRecords:           4,
			MaxFileBytes:         fileBytes,
			MaxBytes:             3*fileBytes + 4*encoded,
		},
		Options{Lease: 10 * time.Second, RecoveryRetention: time.Second},
	)
	ctx := context.Background()
	env := f.envelope(t)
	dead := accept(t, f.s, newID(t), env, "expired sealed original")
	body := sealedReadBody()
	live := accept(t, f.s, newID(t), f.envelope(t), string(body))
	err = f.s.ReleaseReference(
		ctx,
		f.org,
		dead.ID,
		Reference{Kind: "pending-mailbox", Key: env.Destinations[0].Inbox.String()},
	)
	requireReceiptNoError(t, err)
	time.Sleep(1100 * time.Millisecond)
	_, err = f.s.SweepExpiry(ctx, 10)
	requireReceiptNoError(t, err)
	for _, m := range f.members {
		err = f.s.RotateHot(ctx, m.id)
		requireReceiptNoError(t, err)
	}
	return f, live, body
}

func sealedReclaimSourcePath(t *testing.T, f *fixture, m *LocalMember, r Receipt) (ID, string, int64) {
	t.Helper()
	var file, dir string
	var charge int64
	requireReceiptNoError(
		t,
		f.pool.QueryRow(
			context.Background(),
			`SELECT f.file_id,f.directory,f.charged_bytes FROM fry_target t JOIN fry_member_file f `+
				`ON f.member=t.member AND f.file_id=t.incarnation WHERE t.attempt=$1 AND t.member=$2 `+
				`AND f.state='sealed'`,
			r.Attempt.String(),
			m.id.String(),
		).
			Scan(&file, &dir, &charge),
	)
	id, err := idFrom(file)
	requireReceiptNoError(t, err)
	return id, filepath.Join(m.root, dir, "data.fry"), charge
}

func requireSealedSourceReclaimed(
	t *testing.T,
	f *fixture,
	m *LocalMember,
	old ID,
	path string,
	live Receipt,
	body []byte,
) {
	t.Helper()
	{
		_, err := os.Stat(path)
		requireReceiptCondition(t, errors.Is(err, os.ErrNotExist), "old sealed file remains or unlink unproven", err)
	}
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
		"old physical charge released without removed transition",
	)
	var raw []byte
	var directory string
	var length int64
	requireReceiptNoError(
		t,
		f.pool.QueryRow(
			context.Background(),
			`SELECT t.locator,f.directory,f.charged_bytes FROM fry_target t JOIN fry_member_file f `+
				`ON f.member=t.member AND f.file_id=t.incarnation WHERE t.attempt=$1 AND t.member=$2 `+
				`AND f.state='sealed' AND NOT t.bytes_removed`,
			live.Attempt.String(),
			m.id.String(),
		).
			Scan(&raw, &directory, &length),
	)
	var locator hotstore.Locator
	requireReceiptNoError(t, json.Unmarshal(raw, &locator))
	requireReceiptCondition(t, locator.File != old, "survivor retained removed source locator")
	image, err := hotstore.OpenFileImage(filepath.Join(m.root, directory), locator.File, length)
	requireReceiptNoError(t, err)
	var out bytes.Buffer
	readErr := image.ReadRecord(context.Background(), locator, 0, live.Size, &out)
	closeErr := image.Close()
	requireReceiptCondition(t, readErr == nil, "direct sealed successor original differs", readErr, closeErr)
	requireReceiptCondition(t, closeErr == nil, "direct sealed successor original differs", readErr, closeErr)
	requireReceiptCondition(
		t,
		bytes.Equal(out.Bytes(), body),
		"direct sealed successor original differs",
		readErr,
		closeErr,
	)
}

func TestSealedReclaimWaitsForLocalDescriptorClosure(t *testing.T) {
	f, live, body := sealedReclaimReaderFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	m := f.members[0]
	if f.members[1].id.String() < m.id.String() {
		m = f.members[1]
	}
	old, path, _ := sealedReclaimSourcePath(t, f, m, live)
	current := m.bytes.FileID()
	h, err := f.s.Open(ctx, f.org, live.ID, 0, live.Size)
	requireReceiptNoError(t, err)
	defer func() {
		if err := h.Close(); err != nil {
			t.Error(err)
		}
	}()
	prefix := make([]byte, 16)
	{
		_, err = io.ReadFull(h, prefix)
		requireReceiptCondition(t, err == nil, err)
		requireReceiptCondition(t, bytes.Equal(prefix, body[:16]), err)
	}
	requireReceiptCondition(t, len(m.readPermits) == 1, "actual descriptor did not hold a read permit")
	done := make(chan error, 1)
	started := make(chan struct{})
	go func() { close(started); done <- f.s.ReclaimSealed(ctx, m.id) }()
	<-started
	select {
	case err = <-done:
		t.Fatal("sealed reclaim passed a live descriptor", err)
	case <-time.After(50 * time.Millisecond):
	}
	{
		_, err = os.Stat(path)
		requireReceiptCondition(t, err == nil, "source removed while descriptor active", err)
	}
	err = h.Close()
	requireReceiptNoError(t, err)
	select {
	case err = <-done:
	case <-ctx.Done():
		t.Fatal("reclaim did not resume after actual descriptor closed", ctx.Err())
	}
	requireReceiptNoError(t, err)
	requireReceiptCondition(t, m.bytes.FileID() == current, "sealed reclaim changed active writer")
	requireSealedSourceReclaimed(t, f, m, old, path, live, body)
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT count(*) FROM fry_read_pin`) == 0,
		"closed local reader retained pins",
	)
}

func TestSealedReclaimWaitsForRemoteMemberPinAfterParentCloses(t *testing.T) {
	f, live, body := sealedReclaimReaderFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	s, _, servers := remoteMaintenance(t, f)
	m := f.members[0]
	old, path, _ := sealedReclaimSourcePath(t, f, m, live)
	parent, err := s.Open(ctx, f.org, live.ID, 0, live.Size)
	requireReceiptNoError(t, err)
	defer func() {
		if err := parent.Close(); err != nil {
			t.Error(err)
		}
	}()
	var parentRaw string
	err = f.pool.QueryRow(
		ctx,
		`SELECT id FROM fry_read_pin WHERE receipt=$1 AND owner_member IS NULL`,
		live.ID.String(),
	).
		Scan(&parentRaw)
	requireReceiptNoError(t, err)
	parentID, err := idFrom(parentRaw)
	requireReceiptNoError(t, err)
	held := &heldReadWriter{entered: make(chan struct{}), release: make(chan struct{})}
	var once sync.Once
	defer once.Do(func() { close(held.release) })
	operation := newID(t)
	reading := make(chan error, 1)
	go func() {
		reading <- servers[0].owner.read(ctx, operation, parentID, f.org, live.ID, m.bytes.FileID(), 0, live.Size, held)
	}()
	select {
	case <-held.entered:
	case <-ctx.Done():
		t.Fatal("remote actual descriptor did not start")
	}
	err = parent.Close()
	requireReceiptNoError(t, err)
	requireRemoteSealedReadPin(t, f, operation, m)
	done := make(chan error, 1)
	go func() { done <- s.ReclaimSealed(ctx, m.id) }()
	select {
	case err = <-done:
		t.Fatal("remote reclaim passed active member descriptor", err)
	case <-time.After(50 * time.Millisecond):
	}
	{
		_, err = os.Stat(path)
		requireReceiptCondition(t, err == nil, "old remote file removed while member pin active", err)
	}
	joinRemoteSealedReclaim(t, ctx, &once, held, reading, done, body)
	requireSealedSourceReclaimed(t, f, m, old, path, live, body)
	requireRemoteReclaimClosureProof(t, f, operation)
}

func TestSealedReclaimExclusiveGateDoesNotWaitForReaderPermits(t *testing.T) {
	f, live, body := sealedReclaimReaderFixture(t)
	m := f.members[0]
	old, path, _ := sealedReclaimSourcePath(t, f, m, live)
	// These permits model remote readers that reserved capacity but are queued
	// behind the exclusive maintenance gate. They own no descriptor yet.
	for range cap(m.readPermits) {
		m.readPermits <- struct{}{}
	}
	defer func() {
		for len(m.readPermits) > 0 {
			<-m.readPermits
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	{
		err := f.s.ReclaimSealed(ctx, m.id)
		requireReceiptCondition(t, err == nil, "exclusive maintenance waited for queued-reader permits", err)
	}
	requireReceiptCondition(t, len(m.readPermits) == cap(m.readPermits), "maintenance consumed a normal-reader permit")
	for len(m.readPermits) > 0 {
		<-m.readPermits
	}
	requireSealedSourceReclaimed(t, f, m, old, path, live, body)
}

func TestSealedReclaimInventoryClosureProofLossCannotPublishOrUnlink(t *testing.T) {
	f, live, _ := sealedReclaimReaderFixture(t)
	ctx := context.Background()
	m := f.members[0]
	old, path, charge := sealedReclaimSourcePath(t, f, m, live)
	before := memberFilesTree(t, m.root)
	var closed atomic.Bool
	// The actual source descriptor closes successfully. The synthetic returned
	// error models lost closure proof, not a real operating-system close fault.
	m.imageClose = func(image *hotstore.Image) error {
		if err := image.Close(); err != nil {
			return err
		}
		closed.Store(true)
		return errors.New("synthetic lost sealed-inventory close proof")
	}
	defer func() {
		if !closed.Load() {
			t.Error("actual source descriptor not closed")
		}
		m.readClosureUncertain.Store(false)
		m.frozen = false
	}()
	{
		err := f.s.ReclaimSealed(ctx, m.id)
		requireReceiptCondition(t, errors.Is(err, ErrUncertain), "uncertain inventory close was not fail-closed", err)
	}
	requireReceiptCondition(t, m.isFrozen(), "close uncertainty did not freeze maintenance")
	{
		err := m.Close()
		requireReceiptCondition(t, errors.Is(err, ErrUncertain), "uncertain inventory allowed clean root closure", err)
	}
	{
		_, err := os.Stat(path)
		requireReceiptCondition(t, err == nil, "source unlinked before close proof", err)
	}
	requireUnclosedSealedInventoryAuthority(t, f, m, old, charge, live)
	requirePreexistingMemberFilesPreserved(t, m, before)
	requireReceiptCondition(
		t,
		scalar(
			t,
			f.pool,
			`SELECT count(*) FROM fry_hot_rewrite WHERE member=$1 AND kind='sealed-reclaim' AND `+
				`state='published'`,
			m.id.String(),
		) == 0,
		"uncertain source close published rewrite",
	)
}

func joinRemoteSealedReclaim(
	t *testing.T,
	ctx context.Context,
	once *sync.Once,
	held *heldReadWriter,
	reading, done <-chan error,
	body []byte,
) {
	t.Helper()
	var err error
	once.Do(func() { close(held.release) })
	select {
	case err = <-reading:
	case <-ctx.Done():
		t.Fatal("member reader failed to join")
	}
	requireReceiptCondition(t, err == nil, "member original changed", err)
	requireReceiptCondition(t, bytes.Equal(held.Bytes(), body), "member original changed", err)
	select {
	case err = <-done:
	case <-ctx.Done():
		t.Fatal("remote reclaim did not resume")
	}
	requireReceiptNoError(t, err)
}

func requireRemoteReclaimClosureProof(t *testing.T, f *fixture, operation ID) {
	t.Helper()
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT count(*) FROM fry_member_read WHERE id=$1 AND closed`, operation.String()) == 1,
		"member closure proof missing after reclaim",
	)
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT count(*) FROM fry_read_pin`) == 0,
		"member closure proof missing after reclaim",
	)
}

func requirePreexistingMemberFilesPreserved(t *testing.T, m *LocalMember, before map[string]string) {
	t.Helper()
	// The source's exact bytes remain; an unpublished, tracked successor may also
	// remain charged. It is debt to reconcile, never evidence permitting deletion.
	after := memberFilesTree(t, m.root)
	for name, data := range before {
		got, ok := after[name]
		if strings.HasPrefix(data, "d") {
			// A tracked successor legitimately adds a child to the root.
			// Preserve directory type/permissions, not its entry-size metadata.
			beforeMode, _, _ := strings.Cut(data, ":")
			afterMode, _, _ := strings.Cut(got, ":")
			requireReceiptCondition(t, ok, "preexisting directory type or permissions changed", name)
			requireReceiptCondition(
				t,
				beforeMode == afterMode,
				"preexisting directory type or permissions changed",
				name,
			)
		} else {
			requireReceiptCondition(t, ok, "preexisting source bytes/type/mode changed on failed close", name)
			requireReceiptCondition(t, got == data, "preexisting source bytes/type/mode changed on failed close", name)
		}
	}
}

func requireRemoteSealedReadPin(t *testing.T, f *fixture, operation ID, m *LocalMember) {
	t.Helper()
	requireReceiptCondition(
		t,
		scalar(
			t,
			f.pool,
			`SELECT count(*) FROM fry_read_pin WHERE id=$1 AND owner_member=$2`,
			operation.String(),
			m.id.String(),
		) == 1,
		"parent closure erased actual member protection",
	)
}

func requireUnclosedSealedInventoryAuthority(
	t *testing.T,
	f *fixture,
	m *LocalMember,
	old ID,
	charge int64,
	live Receipt,
) {
	t.Helper()
	requireReceiptCondition(
		t,
		scalar(
			t,
			f.pool,
			`SELECT count(*) FROM fry_member_file WHERE member=$1 AND file_id=$2 AND `+
				`state='sealed' AND charged_bytes=$3`,
			m.id.String(),
			old.String(),
			charge,
		) == 1,
		"source state/charge changed before close proof",
	)
	requireReceiptCondition(
		t,
		scalar(
			t,
			f.pool,
			`SELECT count(*) FROM fry_target WHERE attempt=$1 AND member=$2 AND incarnation=$3 AND `+
				`NOT bytes_removed`,
			live.Attempt.String(),
			m.id.String(),
			old.String(),
		) == 1,
		"survivor locator published before source closure",
	)
}
