//go:build linux

package receiptstore

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync/atomic"
	"time"

	"github.com/cotocisternas/fry/internal/hotstore"
	"github.com/cotocisternas/fry/internal/objectstore"
	"github.com/cotocisternas/fry/internal/receiptstore/dbgen"

	"github.com/jackc/pgx/v5"
)

var ErrPackInUse = errors.New("receiptstore: pack still has current live contents")

type PackOptions struct {
	ScratchDirectory              string
	MaxPackBytes, MaxStagingBytes int64
	Lease                         time.Duration
}

// Packer performs bounded, explicitly selected batches. Scheduling/age-based
// closure remain scheduler work; Compact replaces mixed-live published packs.
type Packer struct {
	receipts     *Store
	objects      objectstore.Store
	opts         PackOptions
	slot         chan struct{}
	poisoned     atomic.Bool
	closed       atomic.Bool
	staging      *hotstore.Store
	afterUpload  func(ID) error
	afterPublish func() error
}
type packEntry struct {
	receipt Receipt
	version int64
	locator hotstore.Locator
}
type packJob struct {
	id, org ID
	source  ID
	key     string
	epoch   int64
	entries []packEntry
}

// NewPacker uses ctx for bounded initialization. Its successful result remains
// owned by the caller until Close, independently of initialization cancellation.
func NewPacker(ctx context.Context, receipts *Store, objects objectstore.Store, opts PackOptions) (*Packer, error) {
	if err := validatePackOptions(receipts, objects, opts); err != nil {
		return nil, err
	}
	var err error
	bindCtx, bindCancel := context.WithTimeout(ctx, 30*time.Second)
	defer bindCancel()
	if err = receipts.bindObjectNamespace(bindCtx, objects.Namespace()); err != nil {
		return nil, err
	}
	staging, err := hotstore.Open(
		opts.ScratchDirectory,
		hotstore.Options{MaxMessageBytes: 1, MaxBytes: 64, MaxConcurrentAppends: 1},
	)
	if err != nil {
		return nil, err
	}
	p := &Packer{receipts: receipts, objects: objects, opts: opts, slot: make(chan struct{}, 1), staging: staging}
	if err = p.reconcileStaging(bindCtx); err != nil {
		return nil, errors.Join(err, staging.Close())
	}
	if !receipts.cold.CompareAndSwap(nil, p) {
		return nil, errors.Join(ErrConflict, staging.Close())
	}
	return p, nil
}

// Pack returns the registered job ID even if later work fails. Status/Retire
// resolve interrupted jobs; never reuse their object keys. One build runs per
// Packer instance, with an explicit per-instance scratch budget and deadline.
// Copies acquire bounded read ownership before any source can be reclaimed.
func (p *Packer) Pack(ctx context.Context, organization ID, receipts []ID) (id ID, resultErr error) {
	if p.closed.Load() {
		return id, hotstore.ErrClosed
	}
	if p.poisoned.Load() {
		return id, fmt.Errorf("receiptstore: staging cleanup failed; operator reconciliation required")
	}
	select {
	case p.slot <- struct{}{}:
		defer func() { <-p.slot }()
	default:
		return id, ErrBusy
	}
	if p.closed.Load() {
		return id, hotstore.ErrClosed
	}
	job, err := p.register(ctx, organization, receipts)
	if err != nil {
		return job.id, err
	}
	return p.build(ctx, job)
}

func (p *Packer) build(ctx context.Context, job packJob) (id ID, resultErr error) {
	id = job.id
	workCtx, cancel := context.WithTimeout(ctx, p.opts.Lease)
	defer cancel()
	dir := filepath.Join(p.opts.ScratchDirectory, "fry-pack-"+id.String())
	if err := os.Mkdir(dir, 0o700); err != nil {
		return id, err
	}
	if err := syncMemberRoot(p.opts.ScratchDirectory); err != nil {
		return id, err
	}
	defer func() { resultErr = errors.Join(resultErr, p.cleanupPackBuild(ctx, id, dir)) }()
	builder, err := hotstore.Open(
		dir,
		hotstore.Options{
			MaxMessageBytes:      p.opts.MaxPackBytes,
			MaxBytes:             p.opts.MaxStagingBytes,
			MaxConcurrentAppends: 1,
		},
	)
	if err != nil {
		return id, err
	}
	defer func() {
		if closeErr := builder.Close(); closeErr != nil {
			p.poisoned.Store(true)
			resultErr = errors.Join(resultErr, closeErr)
		}
	}()
	if err = p.copyPackEntries(workCtx, &job, builder); err != nil {
		return id, err
	}
	image, err := builder.Snapshot()
	if err != nil {
		return id, err
	}
	defer func() { resultErr = errors.Join(resultErr, image.Close()) }()
	info, err := p.uploadPackImage(workCtx, job, image)
	if err != nil {
		return id, err
	}
	return id, p.publishBuiltPack(workCtx, job, info)
}

type contextStream struct {
	ctx context.Context
	r   io.Reader
}

func (r *contextStream) Read(b []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(b)
}

func (p *Packer) register(ctx context.Context, org ID, ids []ID) (packJob, error) {
	job := packJob{org: org, epoch: 1}
	sorted, err := sortedPackSelection(org, ids)
	if err != nil {
		return job, err
	}
	tx, err := p.receipts.db.begin(ctx)
	if err != nil {
		return job, err
	}
	defer rollback(ctx, tx)
	sizes, err := loadPackSelection(ctx, tx, &job, sorted)
	if err != nil {
		return job, err
	}
	imageSize, err := hotstore.ImageSize(sizes...)
	if err != nil {
		return job, err
	}
	if imageSize > p.opts.MaxPackBytes {
		return job, hotstore.ErrTooLarge
	}
	if err = p.recordPackRegistration(ctx, tx, &job); err != nil {
		return job, err
	}
	return job, commit(ctx, tx)
}

func packGuard(ctx context.Context, tx pgx.Tx, job packJob) error {
	var state string
	var epoch int64
	var deadline time.Time
	err := func() error {
		sqlcResult, sqlcErr := dbgen.New(tx).
			PackGuardReadPack(ctx, dbgen.PackGuardReadPackParams{ID: job.id.String(), Organization: job.org.String()})
		if sqlcErr != nil {
			return sqlcErr
		}
		state = sqlcResult.State
		epoch = sqlcResult.Epoch
		deadline = sqlcResult.LeaseUntil
		return nil
	}()
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	var now time.Time
	if err = func() error {
		sqlcResult, sqlcErr := dbgen.New(tx).PackGuardReadClock(ctx)
		if sqlcErr != nil {
			return sqlcErr
		}
		now = sqlcResult
		return nil
	}(); err != nil {
		return err
	}
	if state != "building" || epoch != job.epoch || !deadline.After(now) {
		return ErrStale
	}
	return nil
}

func (p *Packer) recordImage(ctx context.Context, job packJob, size int64, digest [32]byte) error {
	tx, err := p.receipts.db.begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(ctx, tx)
	if err = packGuard(ctx, tx, job); err != nil {
		return err
	}
	if _, err = dbgen.New(tx).
		PackerRecordImageUpdatePack(ctx, dbgen.PackerRecordImageUpdatePackParams{
			ID:          job.id.String(),
			ImageSize:   sqlcPointer(size),
			ImageSha256: digest[:],
		}); err != nil {
		return err
	}
	for _, entry := range job.entries {
		b, e := json.Marshal(entry.locator)
		if e != nil {
			return e
		}
		if _, err = dbgen.New(tx).
			PackerRecordImageUpdatePackMember(ctx, dbgen.PackerRecordImageUpdatePackMemberParams{
				Pack:    job.id.String(),
				Receipt: entry.receipt.ID.String(),
				Locator: b,
			}); err != nil {
			return err
		}
	}
	return commit(ctx, tx)
}

func (p *Packer) publish(ctx context.Context, job packJob, info objectstore.Info) error {
	if job.source != (ID{}) {
		return p.publishRewrite(ctx, job, info)
	}
	tx, err := p.receipts.db.begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(ctx, tx)
	if err = lockPackPublicationReceipts(ctx, tx, job); err != nil {
		return err
	}
	if err = packGuard(ctx, tx, job); err != nil {
		return err
	}
	if err = livePackMembers(ctx, tx, job); err != nil {
		return err
	}
	if err = verifyPublishedPackImage(ctx, tx, job, info); err != nil {
		return err
	}
	if err = publishPackLocations(ctx, tx, job, info); err != nil {
		return err
	}
	return commit(ctx, tx)
}

func (p *Packer) Status(ctx context.Context, organization, id ID) (string, error) {
	var state string
	err := func() error {
		sqlcResult, sqlcErr := dbgen.New(p.receipts.db.pool).
			PackerStatus(ctx, dbgen.PackerStatusParams{ID: id.String(), Organization: organization.String()})
		if sqlcErr != nil {
			return sqlcErr
		}
		state = sqlcResult
		return nil
	}()
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	return state, err
}

// Retire revokes publication rights before replacing an unpublished object's
// body with a permanent empty marker. Compact must first retire a published
// pack, and every actual reader must acknowledge closure before erasure.
func (p *Packer) Retire(ctx context.Context, organization, id ID) error {
	key, needed, err := p.beginPackRetirement(ctx, organization, id)
	if err != nil || !needed {
		return err
	}
	if err = p.objects.Retire(ctx, key); err != nil {
		return err
	}
	return p.finishPackRetirement(ctx, id)
}

func (p *Packer) beginPackRetirement(ctx context.Context, organization, id ID) (string, bool, error) {
	tx, err := p.receipts.db.begin(ctx)
	if err != nil {
		return "", false, err
	}
	defer rollback(ctx, tx)
	row, err := dbgen.New(tx).
		PackerRetireReadPack(ctx, dbgen.PackerRetireReadPackParams{
			ID:           id.String(),
			Organization: organization.String(),
		})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, ErrNotFound
	}
	if err != nil {
		return "", false, err
	}
	if row.State == "published" {
		return "", false, ErrPackInUse
	}
	if row.State == "retired" {
		return "", false, nil
	}
	if err = revokePackPublication(ctx, tx, id); err != nil {
		return "", false, err
	}
	return row.ObjectKey, true, commit(ctx, tx)
}

func revokePackPublication(ctx context.Context, tx pgx.Tx, id ID) error {
	readers, err := dbgen.New(tx).PackerRetireReadReadPin(ctx, sqlcPointer(id.String()))
	if err != nil {
		return err
	}
	if readers != 0 {
		return ErrBusy
	}
	_, err = dbgen.New(tx).PackerRetireUpdatePack(ctx, id.String())
	return err
}

func (p *Packer) readLocation(ctx context.Context, loc coldLocation, offset, length int64, dst io.Writer) error {
	return hotstore.ReadImage(ctx, func(ctx context.Context, off, n int64) (io.ReadCloser, error) {
		return p.objects.Range(ctx, loc.Key, loc.ETag, off, n)
	}, loc.Locator, offset, length, dst)
}

func livePackMembers(ctx context.Context, tx pgx.Tx, job packJob) error {
	var live int
	err := func() error {
		sqlcResult, sqlcErr := dbgen.New(tx).LivePackMembers(ctx, job.id.String())
		if sqlcErr != nil {
			return sqlcErr
		}
		live = int(sqlcResult)
		return nil
	}()
	if err != nil {
		return err
	}
	if live != len(job.entries) {
		return ErrStale
	}
	return nil
}

func validatePackOptions(receipts *Store, objects objectstore.Store, opts PackOptions) error {
	if receipts == nil || objects == nil || !validPackBounds(opts) {
		return fmt.Errorf("receiptstore: invalid pack options")
	}
	largest, err := hotstore.ImageSize(opts.MaxPackBytes)
	if err != nil {
		return err
	}
	if opts.MaxStagingBytes < opts.MaxPackBytes+2*largest {
		return fmt.Errorf("receiptstore: staging budget must cover image and two maximum record reservations")
	}
	info, err := os.Stat(opts.ScratchDirectory)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("receiptstore: scratch directory required")
	}
	return nil
}

func (p *Packer) cleanupPackBuild(ctx context.Context, id ID, dir string) error {
	if e := errors.Join(os.RemoveAll(dir), syncMemberRoot(p.opts.ScratchDirectory)); e != nil {
		p.poisoned.Store(true)
		return e
	}
	cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cleanupCancel()
	tx, e := p.receipts.db.begin(cleanupCtx)
	if e == nil {
		_, e = dbgen.New(tx).PackerBuild(cleanupCtx, id.String())
		if e == nil {
			e = commit(cleanupCtx, tx)
		} else {
			rollback(cleanupCtx, tx)
		}
	}
	return e
}

func (p *Packer) copyPackEntries(workCtx context.Context, job *packJob, builder *hotstore.Store) error {
	for i, entry := range job.entries {
		reader, writer := io.Pipe()
		done := make(chan error, 1)
		go func() {
			e := p.receipts.Read(workCtx, job.org, entry.receipt.ID, 0, entry.receipt.Size, writer)
			finishPipe(writer, e)
			done <- e
		}()
		loc, e := builder.Append(
			workCtx,
			hotstore.Key{Receipt: entry.receipt.ID, Attempt: entry.receipt.Attempt},
			reader,
		)
		finishPipe(reader, e)
		readErr := <-done
		if e != nil || readErr != nil {
			return errors.Join(e, readErr)
		}
		if loc.Size != entry.receipt.Size || loc.SHA256 != entry.receipt.SHA256 {
			return objectstore.ErrIntegrity
		}
		job.entries[i].locator = loc
	}
	return nil
}

func (p *Packer) uploadPackImage(ctx context.Context, job packJob, image *hotstore.Image) (objectstore.Info, error) {
	if image.Size() > p.opts.MaxPackBytes {
		return objectstore.Info{}, hotstore.ErrTooLarge
	}
	hash := sha256.New()
	if _, err := io.CopyBuffer(hash, &contextStream{ctx: ctx, r: image}, make([]byte, 64*1024)); err != nil {
		return objectstore.Info{}, err
	}
	var digest [32]byte
	copy(digest[:], hash.Sum(nil))
	if err := p.recordImage(ctx, job, image.Size(), digest); err != nil {
		return objectstore.Info{}, err
	}
	if _, err := image.Seek(0, io.SeekStart); err != nil {
		return objectstore.Info{}, err
	}
	putErr := p.objects.Create(ctx, job.key, image, image.Size(), digest)
	// HEAD must provide a service-verified full-object checksum, not user metadata
	// or an assumed interpretation of ETag. This also resolves a lost PUT reply.
	info, headErr := p.objects.Stat(ctx, job.key)
	if headErr != nil {
		return objectstore.Info{}, errors.Join(putErr, headErr)
	}
	if info.Retired || info.Size != image.Size() || info.SHA256 != digest || info.ETag == "" {
		return objectstore.Info{}, errors.Join(putErr, objectstore.ErrIntegrity)
	}
	return info, nil
}

func (p *Packer) publishBuiltPack(ctx context.Context, job packJob, info objectstore.Info) error {
	if p.afterUpload != nil {
		if err := p.afterUpload(job.id); err != nil {
			return err
		}
	}
	if err := p.publish(ctx, job, info); err != nil {
		return err
	}
	if p.afterPublish != nil {
		if err := p.afterPublish(); err != nil {
			return fmt.Errorf("%w: %w", ErrUncertain, err)
		}
	}
	return nil
}

func sortedPackSelection(org ID, ids []ID) ([]ID, error) {
	if org == (ID{}) || len(ids) == 0 || len(ids) > 1024 {
		return nil, fmt.Errorf("receiptstore: invalid pack selection")
	}
	sorted := append([]ID(nil), ids...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].String() < sorted[j].String() })
	for i, id := range sorted {
		if id == (ID{}) || (i > 0 && id == sorted[i-1]) {
			return nil, ErrConflict
		}
	}
	return sorted, nil
}

func loadPackSelection(ctx context.Context, tx pgx.Tx, job *packJob, sorted []ID) ([]int64, error) {
	var sizes []int64
	var policy int64
	for i, id := range sorted {
		version, retention, err := unpackedReceiptVersion(ctx, tx, job.org, id)
		if err != nil {
			return nil, err
		}
		if i > 0 && retention != policy {
			return nil, ErrConflict
		}
		policy = retention
		receipt, err := loadReceipt(ctx, tx, job.org, id, false)
		if err != nil {
			return nil, err
		}
		job.entries = append(job.entries, packEntry{receipt: receipt, version: version})
		sizes = append(sizes, receipt.Size)
	}
	return sizes, nil
}

func unpackedReceiptVersion(ctx context.Context, tx pgx.Tx, org, id ID) (int64, int64, error) {
	row, err := dbgen.New(tx).
		PackerRegisterReadReference(ctx, dbgen.PackerRegisterReadReferenceParams{
			ID:           id.String(),
			Organization: org.String(),
		})
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, ErrNotFound
	}
	if err != nil {
		return 0, 0, err
	}
	// Fresh statement after waiting for the receipt lock sees current references.
	live, err := dbgen.New(tx).PackerRegisterCheckReference(ctx, id.String())
	if err != nil {
		return 0, 0, err
	}
	if !row.Column2 || !live {
		return 0, 0, ErrConflict
	}
	return row.ManifestVersion, row.RecoverySeconds, nil
}

func (p *Packer) recordPackRegistration(ctx context.Context, tx pgx.Tx, job *packJob) error {
	var err error
	job.id, err = hotstore.NewID()
	if err != nil {
		return err
	}
	job.key = "packs/v1/" + job.id.String()[:2] + "/" + job.org.String() + "/" + job.id.String()
	if _, err = dbgen.New(tx).
		PackerRegisterInsertPack(ctx, dbgen.PackerRegisterInsertPackParams{
			ID:           job.id.String(),
			Organization: job.org.String(),
			ObjectKey:    job.key,
			Column4:      p.opts.Lease.Microseconds(),
			StagingOwner: sqlcPointer(p.staging.FileID().String()),
		}); err != nil {
		return err
	}
	for _, entry := range job.entries {
		if _, err = dbgen.New(tx).
			PackerRegisterInsertPackMember(ctx, dbgen.PackerRegisterInsertPackMemberParams{
				Pack:            job.id.String(),
				Receipt:         entry.receipt.ID.String(),
				ManifestVersion: entry.version,
			}); err != nil {
			return err
		}
	}
	return nil
}

func lockPackPublicationReceipts(ctx context.Context, tx pgx.Tx, job packJob) error {
	// Shared total order: receipt guards first, then the pack guard. Retirement
	// takes only the pack guard and never waits for receipt guards afterward.
	for _, entry := range job.entries {
		var version int64
		var unpacked, live bool
		err := func() error {
			sqlcResult, sqlcErr := dbgen.New(tx).
				PackerPublishReadReference(ctx, dbgen.PackerPublishReadReferenceParams{
					ID:           entry.receipt.ID.String(),
					Organization: job.org.String(),
				})
			if sqlcErr != nil {
				return sqlcErr
			}
			version = sqlcResult.ManifestVersion
			unpacked = sqlcResult.Column2
			live = sqlcResult.Exists
			return nil
		}()
		if err != nil {
			return err
		}
		if version != entry.version || !unpacked || !live {
			return ErrStale
		}
	}
	return nil
}

func verifyPublishedPackImage(ctx context.Context, tx pgx.Tx, job packJob, info objectstore.Info) error {
	var size int64
	var digest []byte
	if err := func() error {
		sqlcResult, sqlcErr := dbgen.New(tx).PackerPublishReadPack(ctx, job.id.String())
		if sqlcErr != nil {
			return sqlcErr
		}
		if sqlcResult.ImageSize == nil {
			return fmt.Errorf("receiptstore: unexpected NULL in PackerPublishReadPack.ImageSize")
		}
		size = *sqlcResult.ImageSize
		digest = sqlcResult.ImageSha256
		return nil
	}(); err != nil {
		return err
	}
	if info.Retired || info.Size != size || string(info.SHA256[:]) != string(digest) {
		return objectstore.ErrIntegrity
	}
	return nil
}

func (p *Packer) finishPackRetirement(ctx context.Context, id ID) error {
	tx, err := p.receipts.db.begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(ctx, tx)
	if _, err = dbgen.New(tx).PackerRetireUpdatePackRetired(ctx, id.String()); err != nil {
		return err
	}
	return commit(ctx, tx)
}

func validPackBounds(opts PackOptions) bool {
	return opts.MaxPackBytes >= 1024 && opts.MaxPackBytes <= 256<<20 && opts.Lease >= time.Second &&
		opts.Lease <= 24*time.Hour
}

func publishPackLocations(ctx context.Context, tx pgx.Tx, job packJob, info objectstore.Info) error {
	for _, entry := range job.entries {
		if _, err := dbgen.New(tx).
			PackerPublishUpdateReceipt(ctx, dbgen.PackerPublishUpdateReceiptParams{
				ID:     entry.receipt.ID.String(),
				PackID: sqlcPointer(job.id.String()),
			}); err != nil {
			return err
		}
	}
	if err := packGuard(ctx, tx, job); err != nil {
		return err
	}
	if err := livePackMembers(ctx, tx, job); err != nil {
		return err
	}
	if _, err := dbgen.New(tx).
		PackerPublishUpdatePack(ctx, dbgen.PackerPublishUpdatePackParams{
			ID:   job.id.String(),
			Etag: sqlcPointer(info.ETag),
		}); err != nil {
		return err
	}
	return nil
}
