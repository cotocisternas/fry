//go:build linux

package receiptstore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/cotocisternas/fry/internal/hotstore"
)

// Every injected failure follows a successful real close. These tests model
// lost closure proof, rather than claiming to reproduce an OS close failure.
func requireCurrentClosureDebt(t *testing.T, f *fixture, m *LocalMember) {
	t.Helper()
	requireReceiptCondition(t, m.isFrozen(), "unproven close did not retain atomic debt")
	requireReceiptCondition(t, m.readClosureUncertain.Load(), "unproven close did not retain atomic debt")
	for range 2 {
		{
			err := m.Close()
			requireReceiptCondition(t, errors.Is(err, ErrUncertain), "repeat close manufactured closure proof", err)
		}
	}
	duplicate, err := OpenLocalMember(context.Background(), f.pool, m.id, m.domain, m.root, m.opts)
	if duplicate != nil {
		if err := duplicate.Close(); err != nil {
			t.Error(err)
		}
		t.Fatal("second member opened despite retained root ownership")
	}
	requireReceiptCondition(t, errors.Is(err, hotstore.ErrBusy), "root ownership not retained", err)
}

func clearSyntheticCurrentDebt(t *testing.T, m *LocalMember) {
	t.Helper()
	t.Cleanup(func() { m.storeClose = nil; m.imageClose = nil; m.readClosureUncertain.Store(false); m.frozen = false })
}

func TestCurrentReclaimSnapshotClosureDebtRetainsSource(t *testing.T) {
	f := capacityFixture(t, 4)
	_ = capacityDeadAndLive(t, f)
	m := f.members[0]
	ctx := context.Background()
	clearSyntheticCurrentDebt(t, m)
	source := m.bytes.FileID()
	before, err := os.ReadFile(filepath.Join(m.root, m.directory, "data.fry"))
	requireReceiptNoError(t, err)
	marker := errors.New("lost current snapshot close proof")
	m.imageClose = func(image *hotstore.Image) error {
		if err := image.Close(); err != nil {
			return err
		}
		return marker
	}
	{
		err = f.s.ReclaimHot(ctx, m.id)
		requireReceiptCondition(t, errors.Is(err, ErrUncertain), err)
		requireReceiptCondition(t, errors.Is(err, marker), err)
	}
	requireCurrentClosureDebt(t, f, m)
	after, err := os.ReadFile(filepath.Join(m.root, m.directory, "data.fry"))
	requireReceiptCondition(t, err == nil, "snapshot close failure changed source", err)
	requireReceiptCondition(t, string(before) == string(after), "snapshot close failure changed source", err)
	requireReceiptCondition(t, m.bytes.FileID() == source, "snapshot close failure started rewrite")
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT count(*) FROM fry_hot_rewrite WHERE member=$1`, m.id.String()) == 0,
		"snapshot close failure started rewrite",
	)
}

func TestCurrentReclaimStoreClosureDebtAtEveryPhase(t *testing.T) {
	for _, phase := range []string{
		"unpublished-successor",
		"published-source",
		"faulted-publication-source",
		"commit-error-source",
		"budget-reopen",
	} {
		t.Run(phase, func(t *testing.T) {
			testCurrentReclaimClosurePhase(t, phase)
		})
	}
}

func testCurrentReclaimClosurePhase(t *testing.T, phase string) {
	t.Helper()

	f := capacityFixture(t, 4)
	live := capacityDeadAndLive(t, f)
	ctx := context.Background()
	m := f.members[0]
	clearSyntheticCurrentDebt(t, m)
	old := m.bytes.FileID()
	oldPath := filepath.Join(m.root, m.directory, "data.fry")
	marker := errors.New("synthetic lost current-store close proof")
	cut := errors.New("synthetic current rewrite cut")
	failed := injectCurrentStoreClosureLoss(t, m, old, phase, marker)
	injectCurrentReclaimCut(t, f, m, phase, cut)
	err := f.s.ReclaimHot(ctx, m.id)
	requireReceiptCondition(t, errors.Is(err, ErrUncertain), "close failure not surfaced", err, failed)
	requireReceiptCondition(t, errors.Is(err, marker), "close failure not surfaced", err, failed)
	requireReceiptCondition(t, *failed, "close failure not surfaced", err, *failed)
	requireReceiptCondition(
		t,
		(phase != "unpublished-successor" && phase != "faulted-publication-source") || errors.Is(err, cut),
		"primary failure lost",
		err,
	)
	m.rewriteFault = nil
	requireCurrentClosureDebt(t, f, m)
	requireCurrentReclaimClosureAuthority(t, f, m, phase, old, oldPath, live)
}

func injectCurrentStoreClosureLoss(t *testing.T, m *LocalMember, old ID, phase string, marker error) *bool {
	t.Helper()
	var failed bool
	m.storeClose = func(store *hotstore.Store) error {
		if err := store.Close(); err != nil {
			return err
		}
		chosen := false
		switch phase {
		case "unpublished-successor":
			chosen = store.FileID() != old
		case "published-source", "faulted-publication-source", "commit-error-source":
			chosen = store.FileID() == old
		case "budget-reopen":
			chosen = store.FileID() != old
		}
		if chosen && !failed {
			failed = true
			return marker
		}
		return nil
	}
	return &failed
}

func injectCurrentReclaimCut(t *testing.T, f *fixture, m *LocalMember, phase string, cut error) {
	t.Helper()
	if phase == "unpublished-successor" {
		m.rewriteFault = func(at string) error {
			if at == "after_copy" {
				return cut
			}
			return nil
		}
	}
	if phase == "faulted-publication-source" {
		m.rewriteFault = func(at string) error {
			if at == "after_publish" {
				return cut
			}
			return nil
		}
	}
	if phase == "commit-error-source" {
		execSQL(
			t,
			f.pool,
			`CREATE FUNCTION reject_current_publication_commit() RETURNS trigger LANGUAGE plpgsql `+
				`AS $$ BEGIN IF NEW.incarnation<>OLD.incarnation THEN RAISE EXCEPTION 'synthetic `+
				`deferred publication failure'; END IF; RETURN NEW; END $$`,
		)
		execSQL(
			t,
			f.pool,
			`CREATE CONSTRAINT TRIGGER reject_current_publication_commit AFTER UPDATE ON `+
				`fry_member DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION `+
				`reject_current_publication_commit()`,
		)
	}
}

func requireCurrentReclaimClosureAuthority(
	t *testing.T,
	f *fixture,
	m *LocalMember,
	phase string,
	old ID,
	oldPath string,
	live Receipt,
) {
	t.Helper()
	requireCurrentReclaimSourceState(t, f, m, phase, old, oldPath)
	if phase == "unpublished-successor" || phase == "commit-error-source" {
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
			"unpublished close debt changed survivor target",
		)
		requireReceiptCondition(
			t,
			scalar(
				t,
				f.pool,
				`SELECT count(*) FROM fry_member_file WHERE member=$1 AND state='building' AND `+
					`charged_bytes=$2`,
				m.id.String(),
				m.opts.MaxFileBytes,
			) == 1,
			"unpublished successor charge lost",
		)
	} else {
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
			"published survivor not retained",
		)
	}
}

func requireCurrentReclaimSourceState(t *testing.T, f *fixture, m *LocalMember, phase string, old ID, oldPath string) {
	t.Helper()
	if phase == "budget-reopen" {
		{
			_, err := os.Stat(oldPath)
			requireReceiptCondition(
				t,
				errors.Is(err, os.ErrNotExist),
				"old source not durably removed before budget reopen",
				err,
			)
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
			"old removal accounting incorrect",
		)
	} else {
		{
			_, err := os.Stat(oldPath)
			requireReceiptCondition(t, err == nil, "close debt erased source", err)
		}
	}
}
