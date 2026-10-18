//go:build linux

package receiptstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/cotocisternas/fry/internal/hotstore"
)

// Preserve every file byte and directory mode when asserting fail-closed open.
// A stage sentinel makes accidental hotstore recovery cleanup observable.
func memberFilesTree(t *testing.T, root string) map[string]string {
	t.Helper()
	result := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		value := fmt.Sprintf("%s:%d", info.Mode(), info.Size())
		if info.Mode().IsRegular() {
			raw, err := storageReadFile(root, relative)
			if err != nil {
				return err
			}
			value = fmt.Sprintf("%s:%x", value, sha256.Sum256(raw))
		}
		result[relative] = value
		return nil
	})
	requireReceiptNoError(t, err)
	return result
}

func memberFilesUnchanged(t *testing.T, root string, before map[string]string) {
	t.Helper()
	after := memberFilesTree(t, root)
	requireReceiptConditionf(
		t,
		reflect.DeepEqual(before, after),
		"refused open mutated member files: before=%v after=%v",
		before,
		after,
	)
}

func TestMemberFilesInitialBindingAndRefusedOpenPreservesBytes(t *testing.T) {
	for _, fault := range []string{
		"missing-schema",
		"missing-binding",
		"wrong-file",
		"wrong-directory",
		"wrong-header",
	} {
		t.Run(fault, func(t *testing.T) {
			f := setup(t)
			ctx := context.Background()
			m := f.members[0]
			requireReceiptCondition(
				t,
				scalar(
					t,
					f.pool,
					`SELECT count(*) FROM fry_member_file WHERE member=$1 AND file_id=$2 AND directory='.' `+
						`AND state='current' AND rewrite_job IS NULL`,
					m.id.String(),
					m.bytes.FileID().String(),
				) == 1,
				"initial immutable file binding missing",
			)
			accept(t, f.s, newID(t), f.envelope(t), "original must survive refused startup")
			requireReceiptNoError(t, m.Close())
			requireReceiptNoError(
				t,
				os.WriteFile(
					filepath.Join(m.root, "stage-catalog-sentinel"),
					[]byte("recovery must not delete before catalog validation"),
					0o600,
				),
			)
			injectMemberCatalogFault(t, ctx, f, m, fault)
			before := memberFilesTree(t, m.root)
			reopened, err := OpenLocalMember(context.Background(), f.pool, m.id, m.domain, m.root, hotOptions())
			if err == nil {
				if err := reopened.Close(); err != nil {
					t.Error(err)
				}
				t.Fatal("incompatible catalog/file accepted", fault)
			}
			memberFilesUnchanged(t, m.root, before)
		})
	}
}

func TestMemberFilesRewritePublishesCatalogAndReplayKeepsOwnerEpoch(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	store, _, servers := remoteMaintenance(t, f)
	envelope := f.envelope(t)
	attempt := newID(t)
	dead := accept(t, store, newID(t), f.envelope(t), "expired original")
	live := accept(t, store, attempt, envelope, "exact live original")
	expireReceipt(t, f, dead)
	m := f.members[0]
	oldFile := m.bytes.FileID()
	var epoch int64
	requireReceiptNoError(
		t,
		f.pool.QueryRow(ctx, `SELECT epoch FROM fry_member_owner WHERE member=$1 AND NOT closed`, m.id.String()).
			Scan(&epoch),
	)
	requireReceiptNoError(t, store.ReclaimHot(ctx, m.id))
	requireMemberRewriteCatalogPublication(t, f, m, oldFile, epoch)
	var raw []byte
	var inc string
	requireReceiptNoError(
		t,
		f.pool.QueryRow(ctx, `SELECT locator,incarnation FROM fry_target WHERE attempt=$1 AND member=$2 AND NOT `+
			`bytes_removed`, live.Attempt.String(), m.id.String()).
			Scan(&raw, &inc),
	)
	var loc hotstore.Locator
	requireReceiptNoError(t, json.Unmarshal(raw, &loc))
	requireReceiptCondition(t, loc.File == m.bytes.FileID(), "catalog and exact target disagree")
	requireReceiptCondition(t, inc == loc.File.String(), "catalog and exact target disagree")
	requireReceiptCondition(t, loc.Key.Receipt == live.ID, "catalog and exact target disagree")
	var out bytes.Buffer
	{
		err := m.bytes.Read(ctx, loc, 0, live.Size, &out)
		requireReceiptCondition(t, err == nil, "direct successor read", err)
		requireReceiptCondition(t, out.String() == "exact live original", "direct successor read", err)
	}
	// Accepted replay cannot depend on either remote endpoint or a caller's new body.
	for _, server := range servers {
		requireReceiptNoError(t, server.Close(ctx))
	}
	replay, err := store.Accept(
		ctx,
		attempt,
		envelope,
		io.NopCloser(bytes.NewBufferString("replacement must not be used")),
	)
	requireReceiptCondition(t, err == nil, "accepted replay changed after rewrite/endpoints stopped", err)
	requireReceiptCondition(t, replay == live, "accepted replay changed after rewrite/endpoints stopped", err)
}

func TestMemberFilesPreparingTargetCannotCreateSuccessor(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	m := f.members[0]
	old := m.bytes.FileID()
	c := prepare(t, f, newID(t), f.envelope(t))
	copies(t, f, c, "durable prepared bytes")
	before := memberFilesTree(t, m.root)
	{
		err := f.s.ReclaimHot(ctx, m.id)
		requireReceiptCondition(t, errors.Is(err, ErrBusy), "unresolved write obligation permitted rewrite", err)
	}
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT count(*) FROM fry_member_file WHERE member=$1`, m.id.String()) == 1,
		"preparing target changed file catalog",
	)
	requireReceiptCondition(t, m.bytes.FileID() == old, "preparing target changed file catalog")
	memberFilesUnchanged(t, m.root, before)
}

func TestMemberFilesCopiedFileCannotBindAnotherMember(t *testing.T) {
	f := setup(t)
	m := f.members[0]
	accept(t, f.s, newID(t), f.envelope(t), "original belongs to exactly one member")
	originalFile := m.bytes.FileID()
	requireReceiptNoError(t, m.Close())
	copied := t.TempDir()
	requireReceiptNoError(t, os.CopyFS(copied, os.DirFS(m.root)))
	before := memberFilesTree(t, copied)
	other := newID(t)
	duplicate, err := OpenLocalMember(context.Background(), f.pool, other, m.domain, copied, hotOptions())
	if err == nil {
		if err := duplicate.Close(); err != nil {
			t.Error(err)
		}
		t.Fatal("same physical file registered as another member")
	}
	memberFilesUnchanged(t, copied, before)
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT count(*) FROM fry_member_file WHERE file_id=$1`, originalFile.String()) == 1,
		"global file binding changed",
	)
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT count(*) FROM fry_member WHERE id=$1`, other.String()) == 0,
		"failed clone registration left new member",
	)
}

func TestMemberFilesRewriteFailureCutsKeepAuthoritativeFile(t *testing.T) {
	for _, point := range []string{
		"before_register",
		"before_register_symlink",
		"after_copy",
		"after_publish",
		"after_unlink",
	} {
		t.Run(point, func(t *testing.T) {
			testMemberRewriteFailureCut(t, point)
		})
	}
}

func TestMemberFilesBuildingCleanupRequiresExactCatalogAuthority(t *testing.T) {
	for _, damage := range []string{
		"incomplete-tail",
		"absent-file",
		"absent-file-unknown",
		"absent-directory",
		"wrong-header",
		"orphan-job",
	} {
		t.Run(damage, func(t *testing.T) {
			testBuildingCleanupAuthority(t, damage)
		})
	}
}

func TestMemberFilesFreshIncompleteSchemaDoesNotCreateOrCleanFiles(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	root := t.TempDir()
	requireReceiptNoError(
		t,
		os.WriteFile(filepath.Join(root, "stage-schema-sentinel"), []byte("preserve staged source"), 0o600),
	)
	{
		_, err := f.pool.Exec(
			ctx,
			`ALTER TABLE fry_hot_rewrite RENAME COLUMN new_incarnation TO missing_new_incarnation`,
		)
		requireReceiptCondition(t, err == nil, err)
	}
	before := memberFilesTree(t, root)
	member, err := OpenLocalMember(context.Background(), f.pool, newID(t), "fresh", root, hotOptions())
	if err == nil {
		if err := member.Close(); err != nil {
			t.Error(err)
		}
		t.Fatal("fresh open accepted incompatible catalog schema")
	}
	memberFilesUnchanged(t, root, before)
}

func TestMemberFilesFreshCatalogInsertFailureRollsBackBinding(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	id := newID(t)
	root := t.TempDir()
	_, err := f.pool.Exec(
		ctx,
		fmt.Sprintf(
			`CREATE FUNCTION reject_fresh_file() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN RAISE `+
				`EXCEPTION 'synthetic catalog insert failure'; END$$; CREATE TRIGGER reject_fresh_file `+
				`BEFORE INSERT ON fry_member_file FOR EACH ROW WHEN (NEW.member='%s') EXECUTE FUNCTION `+
				`reject_fresh_file()`,
			id.String(),
		),
	)
	requireReceiptNoError(t, err)
	member, err := OpenLocalMember(context.Background(), f.pool, id, "fresh", root, hotOptions())
	if err == nil {
		if err := member.Close(); err != nil {
			t.Error(err)
		}
		t.Fatal("catalog insertion failure acknowledged registration")
	}
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT count(*) FROM fry_member WHERE id=$1`, id.String()) == 0,
		"member/catalog registration did not roll back atomically",
	)
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT count(*) FROM fry_member_file WHERE member=$1`, id.String()) == 0,
		"member/catalog registration did not roll back atomically",
	)
	{
		_, err = os.Stat(filepath.Join(root, "data.fry"))
		requireReceiptCondition(t, err == nil, "failed registration lost its unregistered file evidence", err)
	}
	_, err = f.pool.Exec(ctx, `DROP TRIGGER reject_fresh_file ON fry_member_file`)
	requireReceiptNoError(t, err)
	err = os.WriteFile(
		filepath.Join(root, "stage-unregistered-sentinel"),
		[]byte("must survive next refused open"),
		0o600,
	)
	requireReceiptNoError(t, err)
	before := memberFilesTree(t, root)
	member, err = OpenLocalMember(context.Background(), f.pool, id, "fresh", root, hotOptions())
	if err == nil {
		if err := member.Close(); err != nil {
			t.Error(err)
		}
		t.Fatal("unregistered persisted file silently adopted on retry")
	}
	memberFilesUnchanged(t, root, before)
}

func testMemberRewriteFailureCut(t *testing.T, point string) {
	t.Helper()

	f := setup(t)
	ctx := context.Background()
	m := f.members[0]
	dead := accept(t, f.s, newID(t), f.envelope(t), "expired cut original")
	live := accept(t, f.s, newID(t), f.envelope(t), "live cut original")
	expireReceipt(t, f, dead)
	old := m.bytes.FileID()
	m.rewriteFault = func(at string) error {
		if at == point || point == "before_register_symlink" && at == "before_register" {
			return io.ErrUnexpectedEOF
		}
		return nil
	}
	{
		err := f.s.ReclaimHot(ctx, m.id)
		requireReceiptCondition(t, err != nil, "fault cut acknowledged rewrite", point)
	}
	var job, oldDirectory, newDirectory string
	requireReceiptNoError(
		t,
		f.pool.QueryRow(
			ctx,
			`SELECT id,old_directory,new_directory FROM fry_hot_rewrite WHERE member=$1`,
			m.id.String(),
		).
			Scan(&job, &oldDirectory, &newDirectory),
	)
	requirePublishedRewriteFiles(t, f, m, point, old, job, oldDirectory, newDirectory)
	requireReceiptNoError(t, m.Close())
	if point == "before_register_symlink" {
		path := filepath.Join(m.root, newDirectory)
		requireReceiptNoError(t, os.RemoveAll(path))
		requireReceiptNoError(t, os.Symlink(t.TempDir(), path))
	}
	before := memberFilesTree(t, m.root)
	next, err := OpenLocalMember(context.Background(), f.pool, m.id, m.domain, m.root, hotOptions())
	if point == "before_register" || point == "before_register_symlink" {
		if err == nil {
			if err := next.Close(); err != nil {
				t.Error(err)
			}
			t.Fatal("unregistered successor authorized automatic deletion")
		}
		memberFilesUnchanged(t, m.root, before)
		return
	}
	requireReceiptCondition(t, err == nil, "registered recovery failed", point, err)
	defer func() {
		if err := next.Close(); err != nil {
			t.Error(err)
		}
	}()
	requireRecoveredMemberOriginal(t, ctx, f, m, next, point, old, live)
}

func testBuildingCleanupAuthority(t *testing.T, damage string) {
	t.Helper()

	f := setup(t)
	ctx := context.Background()
	m := f.members[0]
	dead := accept(t, f.s, newID(t), f.envelope(t), "dead tail original")
	accept(t, f.s, newID(t), f.envelope(t), "live tail original")
	expireReceipt(t, f, dead)
	m.rewriteFault = func(at string) error {
		if at == "after_copy" {
			m.frozen = true
			return io.ErrUnexpectedEOF
		}
		return nil
	}
	err := f.s.ReclaimHot(ctx, m.id)
	requireReceiptCondition(t, err != nil, "copy cut ignored")
	requireReceiptNoError(t, m.Close())
	var directory, job string
	requireReceiptNoError(
		t,
		f.pool.QueryRow(
			ctx,
			`SELECT new_directory,id FROM fry_hot_rewrite WHERE member=$1 AND state='building'`,
			m.id.String(),
		).
			Scan(&directory, &job),
	)
	path := filepath.Join(m.root, directory, "data.fry")
	damageBuildingCleanupFile(t, ctx, f, damage, path, job)
	before := memberFilesTree(t, m.root)
	next, err := OpenLocalMember(context.Background(), f.pool, m.id, m.domain, m.root, hotOptions())
	if damage == "wrong-header" || damage == "orphan-job" || damage == "absent-file-unknown" {
		if err == nil {
			if err := next.Close(); err != nil {
				t.Error(err)
			}
			t.Fatal("untrusted successor authorized deletion", damage)
		}
		memberFilesUnchanged(t, m.root, before)
		return
	}
	requireReceiptCondition(t, err == nil, "registered unpublished cleanup could not resume", damage, err)
	defer func() {
		if err := next.Close(); err != nil {
			t.Error(err)
		}
	}()
	_, err = os.Stat(filepath.Join(m.root, directory))
	requireReceiptCondition(t, errors.Is(err, os.ErrNotExist), "unpublished generation remains", err)
	requireReceiptCondition(
		t,
		scalar(
			t,
			f.pool,
			`SELECT count(*) FROM fry_member_file WHERE member=$1 AND state='building'`,
			m.id.String(),
		) == 0,
		"resumed cleanup left building catalog",
	)
}

func requireRecoveredMemberOriginal(
	t *testing.T,
	ctx context.Context,
	f *fixture,
	m, next *LocalMember,
	point string,
	old ID,
	live Receipt,
) {
	t.Helper()
	var err error
	f.members[0] = next
	requireReceiptCondition(
		t,
		point != "after_copy" || next.bytes.FileID() == old,
		"unpublished successor became current",
	)
	requireReceiptCondition(t, point == "after_copy" || next.bytes.FileID() != old, "published successor not recovered")
	requireReceiptCondition(
		t,
		scalarContext(
			ctx,
			t,
			f.pool,
			`SELECT count(*) FROM fry_member_file WHERE member=$1 AND state='current'`,
			m.id.String(),
		) == 1,
		"recovery current binding ambiguous",
	)
	requireReceiptCondition(
		t,
		scalarContext(
			ctx,
			t,
			f.pool,
			`SELECT count(*) FROM fry_member_file WHERE member=$1 AND state IN `+
				`('building','retiring')`,
			m.id.String(),
		) == 0,
		"recovery left catalog debt",
	)
	var rawLocator []byte
	err = f.pool.QueryRow(
		ctx,
		`SELECT locator FROM fry_target WHERE attempt=$1 AND member=$2 AND NOT bytes_removed`,
		live.Attempt.String(),
		next.id.String(),
	).
		Scan(&rawLocator)
	requireReceiptNoError(t, err)
	var locator hotstore.Locator
	err = json.Unmarshal(rawLocator, &locator)
	requireReceiptNoError(t, err)
	requireReceiptCondition(t, locator.File == next.bytes.FileID(), "recovered SQL locator selects wrong file")
	var out bytes.Buffer
	{
		err = next.bytes.Read(ctx, locator, 0, live.Size, &out)
		requireReceiptCondition(t, err == nil, "recovered exact member original", err)
		requireReceiptCondition(t, out.String() == "live cut original", "recovered exact member original", err)
	}
}

func requirePublishedRewriteFiles(
	t *testing.T,
	f *fixture,
	m *LocalMember,
	point string,
	old ID,
	job, oldDirectory, newDirectory string,
) {
	t.Helper()
	if point == "after_publish" {
		requireReceiptCondition(
			t,
			scalar(
				t,
				f.pool,
				`SELECT count(*) FROM fry_member_file WHERE member=$1 AND state='retiring' AND `+
					`file_id=$2 AND rewrite_job=$3`,
				m.id.String(),
				old.String(),
				job,
			) == 1,
			"published predecessor debt lost",
		)
		requireReceiptCondition(
			t,
			scalar(
				t,
				f.pool,
				`SELECT count(*) FROM fry_member_file WHERE member=$1 AND state='current' AND `+
					`file_id!=$2`,
				m.id.String(),
				old.String(),
			) == 1,
			"published successor missing",
		)
		{
			_, err := os.Stat(filepath.Join(m.root, oldDirectory, "data.fry"))
			requireReceiptCondition(t, err == nil, "ambiguous publication deleted predecessor", err)
		}
		{
			_, err := os.Stat(filepath.Join(m.root, newDirectory, "data.fry"))
			requireReceiptCondition(t, err == nil, "ambiguous publication lost successor", err)
		}
	}
}

func damageBuildingCleanupFile(t *testing.T, ctx context.Context, f *fixture, damage, path, job string) {
	t.Helper()
	switch damage {
	case "absent-file", "absent-file-unknown":
		requireReceiptNoError(t, os.Remove(path))
		if damage == "absent-file-unknown" {
			requireReceiptNoError(
				t,
				os.WriteFile(filepath.Join(filepath.Dir(path), "untracked-original"), []byte("do not delete"), 0o600),
			)
		}
	case "absent-directory":
		requireReceiptNoError(t, os.RemoveAll(filepath.Dir(path)))
	case "orphan-job":
		{
			_, err := f.pool.Exec(ctx, `UPDATE fry_hot_rewrite SET state='abandoned' WHERE id=$1`, job)
			requireReceiptCondition(t, err == nil, err)
		}
	default:
		flags := os.O_WRONLY
		if damage == "incomplete-tail" {
			flags |= os.O_APPEND
		}
		file, err := splitStorageFile(path, flags, 0)
		requireReceiptNoError(t, err)
		if damage == "incomplete-tail" {
			_, err = file.Write([]byte("incomplete-unpublished-record"))
		} else {
			_, err = file.WriteAt([]byte("bad!"), 0)
		}
		closeErr := file.Close()
		err = errors.Join(err, closeErr)
		requireReceiptNoError(t, err)
	}
}

func injectMemberCatalogFault(t *testing.T, ctx context.Context, f *fixture, m *LocalMember, fault string) {
	t.Helper()
	switch fault {
	case "missing-schema":
		{
			_, err := f.pool.Exec(ctx, `ALTER TABLE fry_member_file RENAME TO absent_member_file`)
			requireReceiptCondition(t, err == nil, err)
		}
	case "missing-binding":
		{
			_, err := f.pool.Exec(ctx, `DELETE FROM fry_member_file WHERE member=$1`, m.id.String())
			requireReceiptCondition(t, err == nil, err)
		}
	case "wrong-file":
		{
			_, err := f.pool.Exec(
				ctx,
				`UPDATE fry_member_file SET file_id=$2 WHERE member=$1`,
				m.id.String(),
				newID(t).String(),
			)
			requireReceiptCondition(t, err == nil, err)
		}
	case "wrong-directory":
		{
			_, err := f.pool.Exec(
				ctx,
				`UPDATE fry_member_file SET directory=$2 WHERE member=$1`,
				m.id.String(),
				"generation-"+newID(t).String(),
			)
			requireReceiptCondition(t, err == nil, err)
		}
	case "wrong-header":
		path := filepath.Join(m.root, "data.fry")
		file, err := splitStorageFile(path, os.O_RDWR, 0)
		requireReceiptNoError(t, err)
		_, writeErr := file.WriteAt([]byte("bad!"), 0)
		closeErr := file.Close()
		err = errors.Join(writeErr, closeErr)
		requireReceiptNoError(t, err)
	}
}

func requireMemberRewriteCatalogPublication(t *testing.T, f *fixture, m *LocalMember, oldFile ID, epoch int64) {
	t.Helper()
	requireReceiptCondition(t, m.bytes.FileID() != oldFile, "rewrite reused physical identity")
	requireReceiptCondition(
		t,
		scalar(
			t,
			f.pool,
			`SELECT count(*) FROM fry_member_owner WHERE member=$1 AND epoch=$2 AND NOT closed`,
			m.id.String(),
			epoch,
		) == 1,
		"physical rewrite replaced process owner",
	)
	requireReceiptCondition(
		t,
		scalar(
			t,
			f.pool,
			`SELECT count(*) FROM fry_member_file WHERE member=$1 AND file_id=$2 AND `+
				`state='removed'`,
			m.id.String(),
			oldFile.String(),
		) == 1,
		"old file not recorded removed",
	)
	requireReceiptCondition(
		t,
		scalar(
			t,
			f.pool,
			`SELECT count(*) FROM fry_member_file WHERE member=$1 AND file_id=$2 AND `+
				`state='current' AND rewrite_job IS NULL`,
			m.id.String(),
			m.bytes.FileID().String(),
		) == 1,
		"successor current binding missing",
	)
	requireReceiptCondition(
		t,
		scalar(
			t,
			f.pool,
			`SELECT count(*) FROM fry_member_file WHERE member=$1 AND state='current'`,
			m.id.String(),
		) == 1,
		"ambiguous current file",
	)
	{
		_, err := os.Stat(filepath.Join(m.root, "data.fry"))
		requireReceiptCondition(t, errors.Is(err, os.ErrNotExist), "predecessor bytes remain", err)
	}
}
