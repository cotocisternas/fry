//go:build linux

package receiptstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/cotocisternas/fry/internal/hotstore"
	"github.com/cotocisternas/fry/internal/receiptstore/dbgen"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Preflight precedes every mutating hotstore open. Missing catalogs are never
// inferred from bytes left by an older schema or an uncertain registration.
func preflightMemberFiles(ctx context.Context, pool *pgxpool.Pool, id ID, root string, opts hotstore.Options) error {
	p := memberFilePreflight{pool: pool, member: id, root: root, opts: opts}
	if err := p.checkSchema(ctx); err != nil {
		return err
	}
	fresh, err := p.loadRegistration(ctx)
	if err != nil || fresh {
		return err
	}
	if err = p.checkCurrentFile(ctx); err != nil {
		return err
	}
	jobs, err := p.rewriteJobs(ctx)
	if err != nil {
		return err
	}
	for _, job := range jobs {
		if err = p.checkRewriteJob(ctx, job); err != nil {
			return err
		}
	}
	return nil
}

type memberFilePreflight struct {
	pool                   *pgxpool.Pool
	member                 ID
	root                   string
	opts                   hotstore.Options
	incarnation, directory string
}

func (p *memberFilePreflight) checkSchema(ctx context.Context) error {
	if err := preflightMemberProfile(ctx, p.pool, p.member, p.opts); err != nil {
		return err
	}
	// Query the complete required shape even when no member is registered.
	probe, err := p.pool.Query(ctx, dbgen.PreflightMemberFilesReadMemberFile)
	if err != nil {
		return err
	}
	probe.Close()
	if err = probe.Err(); err != nil {
		return err
	}
	return preflightSealedFiles(ctx, p.pool, p.member, p.root)
}

func (p *memberFilePreflight) loadRegistration(ctx context.Context) (bool, error) {
	row, err := dbgen.New(p.pool).PreflightMemberFilesReadMember(ctx, p.member.String())
	fresh := errors.Is(err, pgx.ErrNoRows)
	if err != nil && !fresh {
		return false, err
	}
	p.incarnation, p.directory = row.Incarnation, row.Directory
	// Also require the new schema for a fresh member.
	anyFile, err := dbgen.New(p.pool).PreflightMemberFilesCheckMemberFile(ctx, p.member.String())
	if err != nil {
		return false, err
	}
	if fresh {
		return true, preflightFreshMemberRoot(p.root, anyFile)
	}
	return false, nil
}

func preflightFreshMemberRoot(root string, anyFile bool) error {
	if anyFile {
		return ErrConflict
	}
	_, err := storageLstat(root, "data.fry")
	if err == nil {
		return ErrConflict
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (p *memberFilePreflight) checkCurrentFile(ctx context.Context) error {
	if !validMemberDirectory(p.directory) {
		return ErrConflict
	}
	row, err := dbgen.New(p.pool).PreflightMemberFilesReadMemberFileLane(ctx, p.member.String())
	if err != nil {
		return err
	}
	if row.FileID != p.incarnation || row.Directory != p.directory {
		return ErrConflict
	}
	actual, err := hotstore.InspectFileIdentity(filepath.Join(p.root, p.directory))
	if err != nil {
		return err
	}
	if actual.String() != p.incarnation {
		return ErrConflict
	}
	return p.checkCurrentFileSize(row.Column3)
}

func (p *memberFilePreflight) checkCurrentFileSize(reservedBytes int64) error {
	if p.opts.MaxFileBytes > 0 {
		info, err := storageStat(p.root, filepath.Join(p.directory, "data.fry"))
		if err != nil {
			return err
		}
		if info.Size() > p.opts.MaxFileBytes {
			return hotstore.ErrCapacity
		}
		if info.Size() > reservedBytes {
			return ErrConflict
		}
	}
	return nil
}

type memberRewriteJob struct {
	id, old, oldDir, newDir, state, kind string
	next                                 *string
}

func (p *memberFilePreflight) rewriteJobs(ctx context.Context) ([]memberRewriteJob, error) {
	inconsistent, err := dbgen.New(p.pool).PreflightMemberFilesCheckMemberFileBuilding(ctx, p.member.String())
	if err != nil {
		return nil, err
	}
	if inconsistent {
		return nil, ErrConflict
	}
	rows, err := p.pool.Query(ctx, dbgen.PreflightMemberFilesReadHotRewrite, p.member.String())
	if err != nil {
		return nil, err
	}
	var jobs []memberRewriteJob
	for rows.Next() {
		var job memberRewriteJob
		if err = rows.Scan(&job.id, &job.old, &job.oldDir, &job.newDir, &job.state, &job.next, &job.kind); err != nil {
			break
		}
		jobs = append(jobs, job)
	}
	rowErr := rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if rowErr != nil {
		return nil, rowErr
	}
	return jobs, nil
}

func (p *memberFilePreflight) checkRewriteJob(ctx context.Context, job memberRewriteJob) error {
	if job.kind == "sealed-reclaim" {
		return preflightSealedReclaimJob(ctx, p.pool, p.member, p.root, p.incarnation, job.id)
	}
	if job.kind != "rewrite" && job.kind != "rotate" {
		return ErrConflict
	}
	if !validMemberDirectory(job.oldDir) || !validMemberDirectory(job.newDir) || job.oldDir == job.newDir {
		return ErrConflict
	}
	if job.state == "building" {
		return p.checkBuildingJob(ctx, job)
	}
	return p.checkPublishedJob(ctx, job)
}

func (p *memberFilePreflight) checkBuildingJob(ctx context.Context, job memberRewriteJob) error {
	if p.incarnation != job.old || p.directory != job.oldDir {
		return ErrConflict
	}
	if err := verifyCatalogPath(
		ctx,
		p.pool,
		p.member,
		p.root,
		job.id,
		job.newDir,
		job.next,
		"building",
		true,
	); err != nil {
		return err
	}
	if job.kind == "rotate" && job.next != nil {
		return checkEmptyRotationFile(p.root, job.newDir)
	}
	return nil
}

func checkEmptyRotationFile(root, directory string) error {
	info, err := storageStat(root, filepath.Join(directory, "data.fry"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil && info.Size() != 64 {
		return ErrConflict
	}
	return nil
}

func (p *memberFilePreflight) checkPublishedJob(ctx context.Context, job memberRewriteJob) error {
	if job.next == nil || p.incarnation != *job.next || p.directory != job.newDir {
		return ErrConflict
	}
	state, allowAbsent := "retiring", true
	if job.kind == "rotate" {
		state, allowAbsent = "sealed", false
	}
	return verifyCatalogPath(ctx, p.pool, p.member, p.root, job.id, job.oldDir, &job.old, state, allowAbsent)
}

func verifyCatalogPath(
	ctx context.Context,
	pool *pgxpool.Pool,
	member ID,
	root, job, dir string,
	file *string,
	state string,
	allowAbsent bool,
) error {
	if !validMemberDirectory(dir) {
		return ErrConflict
	}
	if file == nil {
		return verifyUnclaimedDirectory(ctx, pool, member, root, dir)
	}
	return verifyRegisteredFile(ctx, pool, member, root, job, dir, *file, state, allowAbsent)
}

func verifyUnclaimedDirectory(ctx context.Context, pool *pgxpool.Pool, member ID, root, dir string) error {
	used, err := dbgen.New(pool).
		VerifyCatalogPathCheckMemberFile(ctx, dbgen.VerifyCatalogPathCheckMemberFileParams{
			Member:    member.String(),
			Directory: dir,
		})
	if err != nil {
		return err
	}
	if used == nil {
		return fmt.Errorf("receiptstore: unexpected NULL in VerifyCatalogPathCheckMemberFile.column_1")
	}
	if *used {
		return ErrConflict
	}
	info, err := storageLstat(root, filepath.Clean(dir))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return ErrConflict
	}
	entries, err := os.ReadDir(filepath.Join(root, dir))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return ErrConflict
	}
	return nil
}

func verifyRegisteredFile(
	ctx context.Context,
	pool *pgxpool.Pool,
	member ID,
	root, job, dir, file, state string,
	allowAbsent bool,
) error {
	row, err := dbgen.New(pool).
		VerifyCatalogPathReadMemberFile(ctx, dbgen.VerifyCatalogPathReadMemberFileParams{
			Member:     member.String(),
			FileID:     file,
			Directory:  dir,
			State:      state,
			RewriteJob: sqlcPointer(job),
		})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrConflict
		}
		return err
	}
	actual, err := hotstore.InspectFileIdentity(filepath.Join(root, dir))
	if allowAbsent && errors.Is(err, os.ErrNotExist) {
		return verifyAbsentGenerationRoot(root, dir)
	}
	if err != nil {
		return err
	}
	if actual.String() != file {
		return ErrConflict
	}
	if row.Enabled && state == "retiring" {
		return verifyRetiringFileSize(root, dir, row.ChargedBytes)
	}
	return nil
}

func verifyAbsentGenerationRoot(root, dir string) error {
	// Only a removed generation directory or the original root file is allowed.
	if dir == "." {
		return nil
	}
	info, err := storageLstat(root, filepath.Clean(dir))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return ErrConflict
	}
	return verifyGenerationScaffolding(root, dir)
}

func verifyGenerationScaffolding(root, dir string) error {
	// Interrupted removal may leave only regular lock/staging scaffolding.
	entries, err := os.ReadDir(filepath.Join(root, dir))
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() != "writer.lock" && !strings.HasPrefix(entry.Name(), "stage-") {
			return ErrConflict
		}
		metadata, err := entry.Info()
		if err != nil {
			return err
		}
		if !metadata.Mode().IsRegular() {
			return ErrConflict
		}
	}
	return nil
}

func verifyRetiringFileSize(root, dir string, charged int64) error {
	info, err := storageStat(root, filepath.Join(dir, "data.fry"))
	if err != nil {
		return err
	}
	if info.Size() != charged {
		return ErrConflict
	}
	return nil
}

func (m *LocalMember) registerSuccessor(ctx context.Context, job ID, dir string, file ID) error {
	tx, err := (&authority{pool: m.pool}).begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(ctx, tx)
	tag, err := dbgen.New(tx).
		LocalMemberRegisterSuccessorUpdateHotRewrite(ctx, dbgen.LocalMemberRegisterSuccessorUpdateHotRewriteParams{
			ID:             job.String(),
			Member:         m.id.String(),
			NewIncarnation: sqlcPointer(file.String()),
			NewDirectory:   dir,
		})
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrConflict
	}
	tag, err = dbgen.New(tx).
		LocalMemberRegisterSuccessorInsertMemberFile(ctx, dbgen.LocalMemberRegisterSuccessorInsertMemberFileParams{
			Member:     m.id.String(),
			FileID:     file.String(),
			Directory:  dir,
			RewriteJob: sqlcPointer(job.String()),
		})
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrConflict
	}
	if err = insertCapacityLanes(ctx, tx, m.id, file, m.opts); err != nil {
		return err
	}
	if _, err = dbgen.New(tx).LocalMemberRegisterSuccessorUpdateHotRewrite2(ctx, job.String()); err != nil {
		return err
	}
	return commit(ctx, tx)
}
