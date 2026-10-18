//go:build linux

package receiptstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cotocisternas/fry/internal/hotstore"
)

func sealedReadFixture(t *testing.T) *fixture {
	t.Helper()
	e, err := hotstore.EncodedRecordBytes(256 << 10)
	requireReceiptNoError(t, err)
	c := 64 + 4*e
	return setupWithPoolConfig(
		t,
		nil,
		hotstore.Options{
			MaxMessageBytes:      256 << 10,
			MaxConcurrentAppends: 2,
			MaxRecords:           4,
			MaxFileBytes:         c,
			MaxBytes:             3*c + 4*e,
		},
		Options{Lease: 10 * time.Second, RecoveryRetention: time.Hour},
	)
}

func sealedReadBody() []byte {
	return append(
		append(bytes.Repeat([]byte{0xA1}, 64<<10), bytes.Repeat([]byte{0xB2}, 64<<10)...),
		bytes.Repeat([]byte{0xC3}, 128)...)
}

func requireSealedRead(t *testing.T, s *Store, org ID, r Receipt, body []byte) {
	t.Helper()
	for _, part := range [][2]int64{{0, r.Size}, {65530, 23}, {r.Size, 0}} {
		var out bytes.Buffer
		{
			err := s.Read(context.Background(), org, r.ID, part[0], part[1], &out)
			requireReceiptCondition(t, err == nil, "original full/range read differs", part, err, out.Len())
			requireReceiptCondition(
				t,
				bytes.Equal(out.Bytes(), body[part[0]:part[0]+part[1]]),
				"original full/range read differs",
				part,
				err,
				out.Len(),
			)
		}
	}
}

func TestSealedLocalReadAfterActualRotationUsesSoleReplica(t *testing.T) {
	f := sealedReadFixture(t)
	ctx := context.Background()
	body := sealedReadBody()
	r := accept(t, f.s, newID(t), f.envelope(t), string(body))
	var old [2]ID
	for i, m := range f.members {
		old[i] = m.bytes.FileID()
		requireReceiptNoError(t, f.s.RotateHot(ctx, m.id))
		requireReceiptCondition(t, m.bytes.FileID() != old[i], "rotation kept active file")
	}
	fresh := accept(t, f.s, newID(t), f.envelope(t), "new current original")
	requireReceiptNoError(t, f.members[1].Close())
	requireSealedRead(t, f.s, f.org, r, body)
	var current bytes.Buffer
	{
		err := f.s.Read(ctx, f.org, fresh.ID, 0, fresh.Size, &current)
		requireReceiptCondition(t, err == nil, "active read regressed", err)
		requireReceiptCondition(t, current.String() == "new current original", "active read regressed", err)
	}
	var forbidden bytes.Buffer
	{
		err := f.s.Read(ctx, newID(t), r.ID, 0, r.Size, &forbidden)
		requireReceiptCondition(t, errors.Is(err, ErrNotFound), "cross-organization original exposed", err)
		requireReceiptCondition(t, forbidden.Len() == 0, "cross-organization original exposed", err)
	}
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT count(*) FROM fry_read_pin`) == 0,
		"completed local read retained parent pins",
	)
}

func TestSealedRemoteReadRefreshesAuthoritativeTargetForStaleCoordinator(t *testing.T) {
	f := sealedReadFixture(t)
	ctx := context.Background()
	s, remote, servers := remoteMaintenance(t, f)
	stale := cachedCoordinator(t, f, remote)
	oldBody := sealedReadBody()
	r := accept(t, s, newID(t), f.envelope(t), string(oldBody))
	before := remote[0].identity()
	for _, m := range f.members {
		requireReceiptNoError(t, s.RotateHot(ctx, m.id))
	}
	fresh := accept(t, s, newID(t), f.envelope(t), "new remote current original")
	requireReceiptCondition(
		t,
		remote[0].identity().OwnerEpoch == before.OwnerEpoch,
		"physical rotation replaced owner generation",
	)
	requireReceiptNoError(t, servers[1].Close(ctx))
	requireSealedRead(t, stale, f.org, r, oldBody)
	var out bytes.Buffer
	{
		err := stale.Read(ctx, f.org, fresh.ID, 0, fresh.Size, &out)
		requireReceiptCondition(t, err == nil, "stale physical hint replaced target authority", err)
		requireReceiptCondition(
			t,
			out.String() == "new remote current original",
			"stale physical hint replaced target authority",
			err,
		)
	}
	requireReceiptCondition(
		t,
		scalar(
			t,
			f.pool,
			`SELECT count(*) FROM fry_member_read WHERE receipt=$1 AND member=$2 AND `+
				`incarnation=$3 AND closed`,
			r.ID.String(),
			f.members[0].id.String(),
			before.Incarnation.String(),
		) >= 1,
		"sealed member pin did not bind actual original file",
	)
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT count(*) FROM fry_member_read WHERE NOT closed`) == 0,
		"completed remote read retained pin debt",
	)
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT count(*) FROM fry_read_pin`) == 0,
		"completed remote read retained pin debt",
	)
}

func TestSealedReadDoesNotFallbackAfterVerifiedPrefix(t *testing.T) {
	f := sealedReadFixture(t)
	ctx := context.Background()
	body := sealedReadBody()
	r := accept(t, f.s, newID(t), f.envelope(t), string(body))
	for _, m := range f.members {
		requireReceiptNoError(t, f.s.RotateHot(ctx, m.id))
	}
	var dir string
	requireReceiptNoError(
		t,
		f.pool.QueryRow(ctx, `SELECT f.directory FROM fry_target t JOIN fry_member_file f ON f.member=t.member AND `+
			`f.file_id=t.incarnation WHERE t.attempt=$1 ORDER BY t.member LIMIT 1`, r.Attempt.String()).
			Scan(&dir),
	)
	first := f.members[0]
	if f.members[1].id.String() < first.id.String() {
		first = f.members[1]
	}
	path := filepath.Join(first.root, dir, "data.fry")
	data, err := storageReadFile(filepath.Dir(path), filepath.Base(path))
	requireReceiptNoError(t, err)
	at := bytes.Index(data, bytes.Repeat([]byte{0xB2}, 32))
	requireReceiptCondition(t, at >= 0, "missing independently identifiable payload frame")
	data[at] ^= 1
	err = os.WriteFile(path, data, 0o600)
	requireReceiptNoError(t, err)
	var out bytes.Buffer
	err = f.s.Read(ctx, f.org, r.ID, 0, r.Size, &out)
	requireReceiptCondition(
		t,
		errors.Is(err, hotstore.ErrCorrupt),
		"partial corrupt read fell back or emitted unchecked bytes",
		err,
		out.Len(),
	)
	requireReceiptCondition(
		t,
		out.Len() == 64<<10,
		"partial corrupt read fell back or emitted unchecked bytes",
		err,
		out.Len(),
	)
	requireReceiptCondition(
		t,
		bytes.Equal(out.Bytes(), body[:64<<10]),
		"partial corrupt read fell back or emitted unchecked bytes",
		err,
		out.Len(),
	)
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT count(*) FROM fry_read_pin`) == 0,
		"ordinary corruption with successful descriptor close left debt",
	)
}

func TestSealedReadPermitsWaitForContextAndDrain(t *testing.T) {
	f := sealedReadFixture(t)
	ctx := context.Background()
	body := sealedReadBody()
	r := accept(t, f.s, newID(t), f.envelope(t), string(body))
	for _, m := range f.members {
		requireReceiptNoError(t, f.s.RotateHot(ctx, m.id))
	}
	first := f.members[0]
	if f.members[1].id.String() < first.id.String() {
		first = f.members[1]
	}
	var handles []*ReadHandle
	defer func() {
		for _, h := range handles {
			if err := h.Close(); err != nil {
				t.Error(err)
			}
		}
	}()
	for range 32 {
		h, err := f.s.Open(ctx, f.org, r.ID, 0, r.Size)
		requireReceiptNoError(t, err)
		handles = append(handles, h)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(first.readPermits) != 32 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	requireReceiptCondition(
		t,
		len(first.readPermits) == 32,
		"real sealed readers failed to occupy bounded permits",
		len(first.readPermits),
	)
	waiting, stop := context.WithTimeout(ctx, 50*time.Millisecond)
	defer stop()
	h, err := f.s.Open(waiting, f.org, r.ID, 0, r.Size)
	requireReceiptNoError(t, err)
	_, err = io.Copy(io.Discard, h)
	if err := h.Close(); err != nil {
		t.Error(err)
	}
	requireReceiptCondition(t, errors.Is(err, context.DeadlineExceeded), "queued reader ignored context", err)
	requireReceiptCondition(t, len(first.readPermits) == 32, "queued reader exceeded permit bound")
	for _, h := range handles {
		err = h.Close()
		requireReceiptNoError(t, err)
	}
	requireReceiptCondition(
		t,
		len(first.readPermits) == 0,
		"canceled actual readers did not close descriptors and pins",
	)
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT count(*) FROM fry_read_pin`) == 0,
		"canceled actual readers did not close descriptors and pins",
	)
}

func TestSealedReadClosureProofLossRetainsDebtAndFreezesMember(t *testing.T) {
	for _, remoteRead := range []bool{false, true} {
		for _, zero := range []bool{false, true} {
			t.Run(fmt.Sprintf("remote=%t/zero=%t", remoteRead, zero), func(t *testing.T) {
				testSealedReadClosureProofLoss(t, remoteRead, zero)
			})
		}
	}
}

func TestReadClosureUncertaintyDoesNotCloseOwnerGeneration(t *testing.T) {
	f := sealedReadFixture(t)
	ctx := context.Background()
	m := f.members[0]
	owner, err := ownMember(ctx, m)
	requireReceiptNoError(t, err)
	// No descriptor is uncertain in this policy-only case; this explicit state
	// injection checks the shutdown authority guard independently of read I/O.
	m.readClosureUncertain.Store(true)
	{
		err = owner.Close(ctx)
		requireReceiptCondition(
			t,
			errors.Is(err, ErrUncertain),
			"owner published closure despite unresolved descriptor",
			err,
		)
	}
	requireReceiptCondition(
		t,
		scalar(
			t,
			f.pool,
			`SELECT count(*) FROM fry_member_owner WHERE member=$1 AND epoch=$2 AND NOT closed`,
			m.id.String(),
			owner.epoch,
		) == 1,
		"uncertain owner generation marked closed",
	)
	m.readClosureUncertain.Store(false)
	err = m.Close()
	requireReceiptNoError(t, err)
}

func testSealedReadClosureProofLoss(t *testing.T, remoteRead, zero bool) {
	t.Helper()

	f := sealedReadFixture(t)
	ctx := context.Background()
	s := f.s
	if remoteRead {
		s, _, _ = remoteMaintenance(t, f)
	}
	body := sealedReadBody()
	r := accept(t, s, newID(t), f.envelope(t), string(body))
	for _, m := range f.members {
		requireReceiptNoError(t, s.RotateHot(ctx, m.id))
	}
	first, other := f.members[0], f.members[1]
	if other.id.String() < first.id.String() {
		first, other = other, first
	}
	reallyClosed, fallbackCloses := injectSealedDescriptorClosureLoss(t, first, other)
	defer func() {
		if !reallyClosed.Load() {
			t.Error("actual image descriptor was not closed")
		}
		first.readClosureUncertain.Store(false)
	}()
	length := r.Size
	if zero {
		length = 0
	}
	var out bytes.Buffer
	{
		err := s.Read(ctx, f.org, r.ID, 0, length, &out)
		requireReceiptCondition(t, errors.Is(err, errReadClosureUncertain), "closure proof loss not propagated", err)
	}
	requireSealedClosureDebt(t, f, first, remoteRead, r, fallbackCloses)
}

func injectSealedDescriptorClosureLoss(t *testing.T, first, other *LocalMember) (*atomic.Bool, *atomic.Int32) {
	t.Helper()
	var reallyClosed atomic.Bool
	var fallbackCloses atomic.Int32
	// Conservative proof-loss simulation: the real descriptor is closed, then
	// the test returns a synthetic lost-ack error. This is not an OS close fault.
	first.imageClose = func(image *hotstore.Image) error {
		if err := image.Close(); err != nil {
			return err
		}
		reallyClosed.Store(true)
		return errors.New("synthetic lost descriptor-close proof")
	}
	other.imageClose = func(image *hotstore.Image) error { fallbackCloses.Add(1); return image.Close() }
	return &reallyClosed, &fallbackCloses
}

func requireSealedClosureDebt(
	t *testing.T,
	f *fixture,
	first *LocalMember,
	remoteRead bool,
	r Receipt,
	fallbackCloses *atomic.Int32,
) {
	t.Helper()
	requireReceiptCondition(t, fallbackCloses.Load() == 0, "replica fallback followed uncertain descriptor closure")
	requireReceiptCondition(t, first.isFrozen(), "closure uncertainty did not freeze admission")
	requireReceiptCondition(t, !first.available(), "closure uncertainty did not freeze admission")
	{
		err := first.Close()
		requireReceiptCondition(t, errors.Is(err, ErrUncertain), "member Close fabricated clean storage closure", err)
	}
	if next, err := OpenLocalMember(
		context.Background(),
		f.pool,
		first.id,
		first.domain,
		first.root,
		first.opts,
	); err == nil {
		if err := next.Close(); err != nil {
			t.Error(err)
		}
		t.Fatal("uncertain member released root ownership")
	}
	wantPins := int64(1)
	if remoteRead {
		wantPins = 2
		requireReceiptCondition(
			t,
			scalar(
				t,
				f.pool,
				`SELECT count(*) FROM fry_member_read WHERE member=$1 AND NOT closed`,
				first.id.String(),
			) == 1,
			"remote read published false closure proof",
		)
	}
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT count(*) FROM fry_read_pin WHERE receipt=$1`, r.ID.String()) == wantPins,
		"uncertain closure discarded read debt",
	)
}
