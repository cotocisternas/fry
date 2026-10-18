package receiptstore

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"time"

	"github.com/cotocisternas/fry/db"
	"github.com/cotocisternas/fry/internal/dbschema"
	"github.com/cotocisternas/fry/internal/hotstore"
	"github.com/cotocisternas/fry/internal/receiptstore/dbgen"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SchemaDefinition returns only this module's SQL for atomic installation.
func SchemaDefinition() dbschema.Component {
	return dbschema.Component{Name: "receiptstore", SQL: db.ReceiptStoreSchema()}
}

// Migrate initializes a fresh, dedicated prototype schema. It intentionally
// fails on existing tables; this is not an upgrade/migration framework.
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(ctx, tx)
	if _, err = tx.Exec(ctx, db.ReceiptStoreSchema()); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func rollback(ctx context.Context, tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	// Rollback is cleanup only: pgx closes the connection on rollback failure.
	// Never replace a committed receipt outcome or authorize a retry from here.
	if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		return
	}
}

type authority struct {
	shards       []shardBinding
	pool         *pgxpool.Pool
	opts         Options
	afterCommit  func() error
	beforeCommit func(pgx.Tx) error
}
type claim struct {
	Attempt, Receipt, Organization ID
	Epoch, Profile                 int64
	Envelope                       Envelope
}
type attemptRow struct {
	receipt, organization     string
	envelope                  []byte
	state                     string
	epoch, profile, retention int64
	deadline                  time.Time
	shard                     string
	placementVersion          int
}

func (a *authority) begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := a.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, "SET LOCAL synchronous_commit = 'on'"); err != nil {
		rollback(ctx, tx)
		return nil, err
	}
	return tx, nil
}

func profileLock(ctx context.Context, tx pgx.Tx) (int64, error) {
	var epoch int64
	err := func() error {
		sqlcResult, sqlcErr := dbgen.New(tx).ProfileLock(ctx)
		if sqlcErr != nil {
			return sqlcErr
		}
		epoch = sqlcResult
		return nil
	}()
	return epoch, err
}

func attemptLock(ctx context.Context, tx pgx.Tx, id ID) (r attemptRow, err error) {
	err = func() error {
		sqlcResult, sqlcErr := dbgen.New(tx).AttemptLock(ctx, id.String())
		if sqlcErr != nil {
			return sqlcErr
		}
		r.receipt = sqlcResult.ReceiptID
		r.organization = sqlcResult.Organization
		r.envelope = sqlcResult.Envelope
		r.state = sqlcResult.State
		r.epoch = sqlcResult.Epoch
		r.profile = sqlcResult.ProfileEpoch
		r.deadline = sqlcResult.LeaseUntil
		r.retention = sqlcResult.RecoverySeconds
		r.shard = sqlcResult.IngestShard
		r.placementVersion = int(sqlcResult.PlacementVersion)
		return nil
	}()
	if errors.Is(err, pgx.ErrNoRows) {
		err = ErrNotFound
	}
	return
}

func idFrom(s string) (ID, error) {
	var id ID
	b, e := hex.DecodeString(s)
	if e != nil || len(b) != len(id) {
		return id, fmt.Errorf("receiptstore: invalid stored ID")
	}
	copy(id[:], b)
	return id, nil
}

func live(ctx context.Context, tx pgx.Tx, r attemptRow, c claim, profile int64) error {
	if r.state == "aborted" {
		return ErrAborted
	}
	if r.state != "preparing" || r.epoch != c.Epoch || r.profile != c.Profile || profile != c.Profile {
		return ErrStale
	}
	var now time.Time
	if err := func() error {
		sqlcResult, sqlcErr := dbgen.New(tx).Live(ctx)
		if sqlcErr != nil {
			return sqlcErr
		}
		now = sqlcResult
		return nil
	}(); err != nil {
		return err
	}
	if !r.deadline.After(now) {
		return ErrStale
	}
	return nil
}

func commit(ctx context.Context, tx pgx.Tx) error {
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("%w: %w", ErrUncertain, err)
	}
	return nil
}

func (a *authority) prepare(
	ctx context.Context,
	attempt ID,
	envelope Envelope,
	data []byte,
	members [2]*LocalMember,
) (claim, *Receipt, error) {
	return a.prepareMembers(
		ctx,
		attempt,
		envelope,
		data,
		[2]MemberIdentity{members[0].identity(), members[1].identity()},
	)
}

func (a *authority) prepareMembers(
	ctx context.Context,
	attempt ID,
	envelope Envelope,
	data []byte,
	members [2]MemberIdentity,
) (claim, *Receipt, error) {
	for retry := range capacityPreparationRetries {
		if err := ctx.Err(); err != nil {
			return claim{}, nil, err
		}
		c, r, err := a.prepareMembersOnce(ctx, attempt, envelope, data, members, retry)
		if !errors.Is(err, errCapacityRace) {
			return c, r, err
		}
	}
	return claim{}, nil, ErrBusy
}

func (a *authority) prepareMembersOnce(
	ctx context.Context,
	attempt ID,
	envelope Envelope,
	data []byte,
	members [2]MemberIdentity,
	retry int,
) (claim, *Receipt, error) {
	tx, err := a.begin(ctx)
	if err != nil {
		return claim{}, nil, err
	}
	return a.prepareMembersTx(ctx, tx, attempt, envelope, data, members, retry)
}

// prepareMembersTx owns rollback, including confirmed rollback before a capacity
// retry. Each step below runs in the original lock and validation order.
func (a *authority) prepareMembersTx(
	ctx context.Context,
	tx pgx.Tx,
	attempt ID,
	envelope Envelope,
	data []byte,
	members [2]MemberIdentity,
	retry int,
) (claim, *Receipt, error) {
	p := memberPreparation{
		authority: a,
		tx:        tx,
		claim:     claim{Attempt: attempt, Organization: envelope.Organization, Envelope: envelope},
		members:   members,
	}
	defer rollback(ctx, tx)
	if err := p.loadAttempt(ctx, data); err != nil {
		return p.claim, nil, err
	}
	if p.row.state == "accepted" {
		result, err := loadReceipt(ctx, tx, envelope.Organization, attempt, true)
		return p.claim, &result, err
	}
	if p.row.state == "aborted" {
		return p.claim, nil, ErrAborted
	}
	if err := p.checkBinding(ctx); err != nil {
		return p.claim, nil, err
	}
	if err := p.renewAttempt(ctx); err != nil {
		return p.claim, nil, err
	}
	if err := p.authorizeMembers(ctx); err != nil {
		return p.claim, nil, err
	}
	if err := p.registerTargets(ctx); err != nil {
		return p.claim, nil, err
	}
	if err := p.reserveCapacity(ctx, retry); err != nil {
		return p.claim, nil, err
	}
	return p.claim, nil, commit(ctx, tx)
}

type memberPreparation struct {
	authority *authority
	tx        pgx.Tx
	claim     claim
	row       attemptRow
	profile   int64
	fresh     bool
	members   [2]MemberIdentity
	capacity  [2]memberCapacityProfile
}

func (p *memberPreparation) loadAttempt(ctx context.Context, data []byte) error {
	var err error
	p.profile, err = profileLock(ctx, p.tx)
	if err != nil {
		return err
	}
	receipt, err := hotstore.NewID()
	if err != nil {
		return err
	}
	if len(p.authority.shards) == 0 {
		return ErrConflict
	}
	c := &p.claim
	selected := p.authority.shards[placementIndex(c.Attempt, len(p.authority.shards))]
	tag, err := dbgen.New(p.tx).
		AuthorityPrepareMembersTxInsertAttempt(ctx, dbgen.AuthorityPrepareMembersTxInsertAttemptParams{
			ID:                  c.Attempt.String(),
			ReceiptID:           receipt.String(),
			Organization:        c.Envelope.Organization.String(),
			Envelope:            data,
			ProfileEpoch:        p.profile,
			Column6:             p.authority.opts.Lease.Microseconds(),
			RecoverySeconds:     int64(p.authority.opts.RecoveryRetention / time.Second),
			CleanupMicroseconds: p.authority.opts.CleanupDeadline.Microseconds(),
			IngestShard:         selected.ID,
		})
	if err != nil {
		return err
	}
	p.fresh = tag.RowsAffected() == 1
	p.row, err = attemptLock(ctx, p.tx, c.Attempt)
	if err != nil {
		return err
	}
	var stored Envelope
	if err = json.Unmarshal(p.row.envelope, &stored); err != nil {
		return err
	}
	if p.row.organization != c.Envelope.Organization.String() || !reflect.DeepEqual(stored, c.Envelope) {
		return ErrConflict
	}
	c.Receipt, err = idFrom(p.row.receipt)
	if err != nil {
		return err
	}
	c.Epoch, c.Profile = p.row.epoch, p.row.profile
	return nil
}

func (p *memberPreparation) checkBinding(ctx context.Context) error {
	var binding *shardBinding
	for i := range p.authority.shards {
		if p.authority.shards[i].ID == p.row.shard {
			binding = &p.authority.shards[i]
			break
		}
	}
	if binding == nil || p.row.placementVersion != 1 {
		return ErrConflict
	}
	supplied := [2]string{p.members[0].Member.String(), p.members[1].Member.String()}
	sort.Strings(supplied[:])
	if supplied != binding.Members {
		return ErrConflict
	}
	bindingData, err := json.Marshal(p.authority.shards)
	if err != nil {
		return err
	}
	matches, err := dbgen.New(p.tx).
		AuthorityPrepareMembersTxCheckIngestCatalog(ctx, dbgen.AuthorityPrepareMembersTxCheckIngestCatalogParams{
			ID:      binding.ID,
			MemberA: binding.Members[0],
			MemberB: binding.Members[1],
			Column4: bindingData,
		})
	if err != nil {
		return err
	}
	if matches == nil {
		return fmt.Errorf("receiptstore: unexpected NULL in AuthorityPrepareMembersTxCheckIngestCatalog.column_1")
	}
	if !*matches {
		return ErrConflict
	}
	return nil
}

func (p *memberPreparation) renewAttempt(ctx context.Context) error {
	if p.fresh {
		return nil
	}
	if p.row.profile != p.profile {
		return ErrStale
	}
	now, err := dbgen.New(p.tx).AuthorityPrepareMembersTxReadClock(ctx)
	if err != nil {
		return err
	}
	if p.row.deadline.After(now) {
		return ErrBusy
	}
	p.claim.Epoch++
	_, err = dbgen.New(p.tx).
		AuthorityPrepareMembersTxUpdateAttempt(ctx, dbgen.AuthorityPrepareMembersTxUpdateAttemptParams{
			ID:      p.claim.Attempt.String(),
			Epoch:   p.claim.Epoch,
			Column3: p.authority.opts.Lease.Microseconds(),
		})
	return err
}

func guardEnvelopeAuthority(ctx context.Context, tx pgx.Tx, c claim) error {
	if err := guardSubmissionAuthorization(ctx, tx, c.Envelope); err != nil {
		return err
	}
	if err := guardSenderAuthorization(ctx, tx, c.Envelope); err != nil {
		return err
	}
	return guardMigrationImport(ctx, tx, c.Attempt, c.Envelope)
}

func (p *memberPreparation) authorizeMembers(ctx context.Context) error {
	if err := guardEnvelopeAuthority(ctx, p.tx, p.claim); err != nil {
		return err
	}
	if p.members[0].Member.String() > p.members[1].Member.String() {
		p.members[0], p.members[1] = p.members[1], p.members[0]
	}
	for i, m := range p.members {
		var err error
		p.capacity[i], err = guardPlacement(ctx, p.tx, m)
		if err != nil {
			return err
		}
	}
	return nil
}

func (p *memberPreparation) registerTargets(ctx context.Context) error {
	for _, m := range p.members {
		if p.fresh {
			if _, err := dbgen.New(p.tx).
				AuthorityPrepareMembersTxInsertTarget(ctx, dbgen.AuthorityPrepareMembersTxInsertTargetParams{
					Attempt:       p.claim.Attempt.String(),
					Member:        m.Member.String(),
					Incarnation:   m.Incarnation.String(),
					FailureDomain: m.FailureDomain,
					OwnerEpoch:    m.OwnerEpoch,
				}); err != nil {
				return err
			}
		} else {
			matches, err := dbgen.New(p.tx).
				AuthorityPrepareMembersTxCheckTarget(ctx, dbgen.AuthorityPrepareMembersTxCheckTargetParams{
					Attempt:       p.claim.Attempt.String(),
					Member:        m.Member.String(),
					Incarnation:   m.Incarnation.String(),
					FailureDomain: m.FailureDomain,
					OwnerEpoch:    m.OwnerEpoch,
				})
			if err != nil {
				return err
			}
			if !matches {
				return ErrConflict
			}
		}
	}
	if p.fresh {
		return registerPreparationSource(ctx, p.tx, p.claim)
	}
	return nil
}

func registerPreparationSource(ctx context.Context, tx pgx.Tx, c claim) error {
	source := c.Envelope.Source
	if source == nil {
		return nil
	}
	tag, err := dbgen.New(tx).
		AuthorityPrepareMembersTxInsertSource(ctx, dbgen.AuthorityPrepareMembersTxInsertSourceParams{
			Connection:  source.Connection.String(),
			Folder:      source.Folder,
			Uidvalidity: int64(source.UIDValidity),
			Uid:         int64(source.UID),
			Attempt:     c.Attempt.String(),
		})
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrSourceConflict
	}
	return nil
}

func (p *memberPreparation) reserveCapacity(ctx context.Context, retry int) error {
	bounded := false
	for i, m := range p.members {
		if err := reserveTargetCapacity(ctx, p.tx, p.claim.Attempt, m, p.fresh, p.capacity[i], retry); err != nil {
			if errors.Is(err, errCapacityRace) {
				return rollbackCapacityRace(ctx, p.tx)
			}
			return err
		}
		bounded = bounded || p.capacity[i].enabled
	}
	if !bounded {
		return nil
	}
	valid, err := dbgen.New(p.tx).AuthorityPrepareMembersTxReadAttempt(ctx, p.claim.Attempt.String())
	if err != nil {
		return err
	}
	if !valid {
		return ErrStale
	}
	return checkMigrationPublication(ctx, p.tx, p.claim.Envelope)
}

// validateTarget runs inside a member's local operation lock. No SQL lock is
// held during disk/stream I/O; abort and cancellation close the resulting gap.
func (a *authority) validateTarget(
	ctx context.Context,
	c claim,
	m *LocalMember,
	owners ...*memberOwner,
) (time.Duration, error) {
	epochOwner := ownerEpoch(owners)
	stored, err := dbgen.New(a.pool).
		AuthorityValidateTarget(ctx, dbgen.AuthorityValidateTargetParams{
			ID:           c.Attempt.String(),
			Member:       m.id.String(),
			Incarnation:  m.bytes.FileID().String(),
			ReceiptID:    c.Receipt.String(),
			Organization: c.Organization.String(),
			OwnerEpoch:   epochOwner,
		})
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	return validatedTargetLease(stored, c)
}

func validatedTargetLease(stored dbgen.AuthorityValidateTargetRow, c claim) (time.Duration, error) {
	number, err := stored.Extract.Float64Value()
	if err != nil {
		return 0, err
	}
	remaining := number.Float64
	if stored.State == "aborted" {
		return 0, ErrAborted
	}
	if stored.State != "preparing" || stored.Epoch != c.Epoch || stored.ProfileEpoch != c.Profile ||
		stored.Epoch_2 != stored.ProfileEpoch ||
		remaining <= 0 {
		return 0, ErrStale
	}
	return time.Duration(remaining * float64(time.Second)), nil
}

func (a *authority) recordCopy(
	ctx context.Context,
	c claim,
	m *LocalMember,
	loc hotstore.Locator,
	owners ...*memberOwner,
) error {
	if loc.File != m.bytes.FileID() || loc.Key != (hotstore.Key{Receipt: c.Receipt, Attempt: c.Attempt}) {
		return ErrConflict
	}
	tx, err := a.begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(ctx, tx)
	r, profile, err := lockLiveClaim(ctx, tx, c)
	if err != nil {
		return err
	}
	if err = guardMemberOwner(ctx, tx, m, owners); err != nil {
		return err
	}
	stored, err := recordCopyLocator(ctx, tx, c, m, loc, owners)
	if err != nil {
		return err
	}
	if err = settleRecordedCopy(ctx, tx, r, c, profile, m, loc, stored); err != nil {
		return err
	}
	return commit(ctx, tx)
}

func (a *authority) renew(ctx context.Context, c claim) error {
	tx, err := a.begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(ctx, tx)
	profile, err := profileLock(ctx, tx)
	if err != nil {
		return err
	}
	r, err := attemptLock(ctx, tx, c.Attempt)
	if err != nil {
		return err
	}
	if err = live(ctx, tx, r, c, profile); err != nil {
		return err
	}
	_, err = dbgen.New(tx).
		AuthorityRenew(ctx, dbgen.AuthorityRenewParams{ID: c.Attempt.String(), Column2: a.opts.Lease.Microseconds()})
	if err != nil {
		return err
	}
	return commit(ctx, tx)
}

type queryer interface {
	dbgen.DBTX
}

func loadReceipt(ctx context.Context, q queryer, org, id ID, byAttempt bool) (Receipt, error) {
	var r Receipt
	var row dbgen.ReceiptByIDRow
	var err error
	if byAttempt {
		byAttemptRow, queryErr := dbgen.New(q).
			ReceiptByAttempt(ctx, dbgen.ReceiptByAttemptParams{Organization: org.String(), Attempt: id.String()})
		row, err = dbgen.ReceiptByIDRow(byAttemptRow), queryErr
	} else {
		row, err = dbgen.New(q).ReceiptByID(ctx, dbgen.ReceiptByIDParams{Organization: org.String(), ID: id.String()})
	}
	receipt, attempt, organization, digest := row.ID, row.Attempt, row.Organization, row.Sha256
	r.ReceivedAt, r.Size, r.ProfileEpoch = row.ReceivedAt, row.RawSize, row.ProfileEpoch
	if errors.Is(err, pgx.ErrNoRows) {
		return r, ErrNotFound
	}
	if err != nil {
		return r, err
	}
	r.ID, err = idFrom(receipt)
	if err != nil {
		return r, err
	}
	r.Attempt, err = idFrom(attempt)
	if err != nil {
		return r, err
	}
	r.Organization, err = idFrom(organization)
	if err != nil {
		return r, err
	}
	if len(digest) != 32 {
		return r, ErrConflict
	}
	copy(r.SHA256[:], digest)
	return r, nil
}

func (a *authority) status(ctx context.Context, org, attempt ID) (Status, error) {
	// Accepted state is terminal in this stage; accepted receipts are immutable.
	var state string
	var epoch int64
	err := func() error {
		sqlcResult, sqlcErr := dbgen.New(a.pool).
			AuthorityStatus(ctx, dbgen.AuthorityStatusParams{ID: attempt.String(), Organization: org.String()})
		if sqlcErr != nil {
			return sqlcErr
		}
		state = sqlcResult.State
		epoch = sqlcResult.Epoch
		return nil
	}()
	if errors.Is(err, pgx.ErrNoRows) {
		return Status{}, ErrNotFound
	}
	if err != nil {
		return Status{}, err
	}
	status := Status{State: state, Epoch: epoch}
	if state == "accepted" {
		r, e := loadReceipt(ctx, a.pool, org, attempt, true)
		if e != nil {
			return Status{}, e
		}
		status.Receipt = &r
	}
	return status, nil
}

func (a *authority) finalize(ctx context.Context, c claim) (Receipt, error) {
	return a.finalizeWithHeader(ctx, c, nil)
}

func (a *authority) finalizeWithHeader(ctx context.Context, c claim, header *capturedHeader) (Receipt, error) {
	tx, err := a.begin(ctx)
	if err != nil {
		return Receipt{}, err
	}
	defer rollback(ctx, tx)
	r, profile, err := lockLiveClaim(ctx, tx, c)
	if err != nil {
		return Receipt{}, err
	}
	if err = guardEnvelopeAuthority(ctx, tx, c); err != nil {
		return Receipt{}, err
	}
	locs, err := finalizationCopies(ctx, tx, c, header)
	if err != nil {
		return Receipt{}, err
	}
	received, err := insertFinalizedReceipt(ctx, tx, c, profile, locs[0], header)
	if err != nil {
		return Receipt{}, err
	}
	if err = publishReceiptDestinations(ctx, tx, c, received); err != nil {
		return Receipt{}, err
	}
	if err = a.finishReceiptAuthority(ctx, tx, c, received, r.retention); err != nil {
		return Receipt{}, err
	}
	if err = a.commitReceipt(ctx, tx, r, c, profile); err != nil {
		return Receipt{}, err
	}
	return Receipt{
		ID:           c.Receipt,
		Attempt:      c.Attempt,
		Organization: c.Organization,
		ReceivedAt:   received,
		Size:         locs[0].Size,
		SHA256:       locs[0].SHA256,
		ProfileEpoch: profile,
	}, nil
}

// Keep the shared profile/attempt locks and initial lease check in this order.
func lockLiveClaim(ctx context.Context, tx pgx.Tx, c claim) (attemptRow, int64, error) {
	profile, err := profileLock(ctx, tx)
	if err != nil {
		return attemptRow{}, profile, err
	}
	r, err := attemptLock(ctx, tx, c.Attempt)
	if err != nil {
		return r, profile, err
	}
	return r, profile, live(ctx, tx, r, c, profile)
}

type finalizationCensus struct {
	domains map[string]bool
	files   map[ID]bool
}

func finalizationCopies(ctx context.Context, tx pgx.Tx, c claim, header *capturedHeader) ([]hotstore.Locator, error) {
	rows, err := tx.Query(ctx, dbgen.AuthorityFinalizeWithHeaderReadTarget, c.Attempt.String())
	if err != nil {
		return nil, err
	}
	var locs []hotstore.Locator
	census := finalizationCensus{domains: map[string]bool{}, files: map[ID]bool{}}
	for rows.Next() {
		var loc hotstore.Locator
		loc, err = census.scanTarget(rows, c)
		if err != nil {
			break
		}
		locs = append(locs, loc)
	}
	rowErr := rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if rowErr != nil {
		return nil, rowErr
	}
	if err = validateFinalizedCopies(locs, header); err != nil {
		return nil, err
	}
	return locs, nil
}

func (census *finalizationCensus) scanTarget(rows pgx.Rows, c claim) (hotstore.Locator, error) {
	var member, inc, domain string
	var raw []byte
	var cancelled bool
	var loc hotstore.Locator
	if err := rows.Scan(&member, &inc, &domain, &raw, &cancelled); err != nil {
		return loc, err
	}
	if len(raw) == 0 {
		return loc, ErrStale
	}
	if err := json.Unmarshal(raw, &loc); err != nil {
		return loc, err
	}
	if cancelled || loc.File.String() != inc || loc.Key != (hotstore.Key{Receipt: c.Receipt, Attempt: c.Attempt}) ||
		census.domains[domain] ||
		census.files[loc.File] {
		return loc, ErrConflict
	}
	census.domains[domain] = true
	census.files[loc.File] = true
	return loc, nil
}

func validateFinalizedCopies(locs []hotstore.Locator, header *capturedHeader) error {
	if len(locs) != 2 {
		return ErrStale
	}
	if locs[0].Size != locs[1].Size || locs[0].SHA256 != locs[1].SHA256 {
		return ErrConflict
	}
	if header != nil {
		for _, loc := range locs {
			if header.size != loc.Size || header.sha256 != loc.SHA256 {
				return ErrConflict
			}
		}
	}
	return nil
}

func insertFinalizedReceipt(
	ctx context.Context,
	tx pgx.Tx,
	c claim,
	profile int64,
	loc hotstore.Locator,
	header *capturedHeader,
) (time.Time, error) {
	received, err := dbgen.New(tx).
		AuthorityFinalizeWithHeaderInsertReceipt(ctx, dbgen.AuthorityFinalizeWithHeaderInsertReceiptParams{
			ID:           c.Receipt.String(),
			Attempt:      c.Attempt.String(),
			Organization: c.Organization.String(),
			RawSize:      loc.Size,
			Sha256:       loc.SHA256[:],
			ProfileEpoch: profile,
		})
	if err != nil {
		return received, err
	}
	if header != nil {
		if _, err = dbgen.New(tx).
			AuthorityFinalizeWithHeaderInsertHeaderMetadata(
				ctx,
				dbgen.AuthorityFinalizeWithHeaderInsertHeaderMetadataParams{
					Receipt:   c.Receipt.String(),
					RawHeader: header.raw,
				},
			); err != nil {
			return received, err
		}
	}
	return received, nil
}

func publishReceiptDestinations(ctx context.Context, tx pgx.Tx, c claim, received time.Time) error {
	var err error
	if err = publishOutbound(ctx, tx, c.Receipt, c.Envelope, received); err != nil {
		return err
	}
	for _, d := range c.Envelope.Destinations {
		if err = publishReceiptDestination(ctx, tx, c.Receipt, d); err != nil {
			return err
		}
	}
	if c.Envelope.MailboxAppend {
		d := c.Envelope.Destinations[0]
		if _, err = publishMailboxInTransaction(
			ctx,
			tx,
			c.Organization,
			c.Receipt,
			d.Inbox,
			c.Envelope.Migration != nil,
		); err != nil {
			return err
		}
		if _, err = dbgen.New(tx).
			AuthorityFinalizeWithHeaderInsertAppendResult(
				ctx,
				dbgen.AuthorityFinalizeWithHeaderInsertAppendResultParams{
					Receipt: c.Receipt.String(),
					Folder:  d.Folder.String(),
				},
			); err != nil {
			return err
		}
	}

	return nil
}

func publishReceiptDestination(ctx context.Context, tx pgx.Tx, receipt ID, d Destination) error {
	addresses, e := json.Marshal(d.Recipients)
	if e != nil {
		return e
	}
	if _, err := dbgen.New(tx).
		AuthorityFinalizeWithHeaderInsertDestination(ctx, dbgen.AuthorityFinalizeWithHeaderInsertDestinationParams{
			Receipt:    receipt.String(),
			Inbox:      d.Inbox.String(),
			Recipients: addresses,
		}); err != nil {
		return err
	}
	if _, err := dbgen.New(tx).
		AuthorityFinalizeWithHeaderInsertReference(ctx, dbgen.AuthorityFinalizeWithHeaderInsertReferenceParams{
			Receipt:      receipt.String(),
			ReferenceKey: d.Inbox.String(),
		}); err != nil {
		return err
	}
	var folder *string
	var date *time.Time
	if d.Folder != (ID{}) {
		folder = new(d.Folder.String())
	}
	if !d.InternalDate.IsZero() {
		date = new(d.InternalDate)
	}
	if _, err := dbgen.New(tx).
		AuthorityFinalizeWithHeaderInsertWork(ctx, dbgen.AuthorityFinalizeWithHeaderInsertWorkParams{
			ID:           receipt.String(),
			Inbox:        d.Inbox.String(),
			Folder:       folder,
			Flags:        d.Flags,
			InternalDate: date,
		}); err != nil {
		return err
	}
	return nil
}

func (a *authority) finishReceiptAuthority(
	ctx context.Context,
	tx pgx.Tx,
	c claim,
	received time.Time,
	retention int64,
) error {
	var err error
	if err = finishMigrationImport(ctx, tx, c); err != nil {
		return err
	}
	if _, err = dbgen.New(tx).
		AuthorityFinalizeWithHeaderInsertReferenceRecovery(
			ctx,
			dbgen.AuthorityFinalizeWithHeaderInsertReferenceRecoveryParams{
				Receipt: c.Receipt.String(),
				Column2: received,
				Column3: retention,
			},
		); err != nil {
		return err
	}
	if _, err = dbgen.New(tx).AuthorityFinalizeWithHeaderUpdateSource(ctx, c.Attempt.String()); err != nil {
		return err
	}
	if _, err = dbgen.New(tx).AuthorityFinalizeWithHeaderUpdateAttempt(ctx, c.Attempt.String()); err != nil {
		return err
	}
	if a.beforeCommit != nil {
		if err = a.beforeCommit(tx); err != nil {
			return err
		}
	}
	return nil
}

func (a *authority) commitReceipt(ctx context.Context, tx pgx.Tx, r attemptRow, c claim, profile int64) error {
	var err error
	// All receipt/folder/profile/migration authority is already locked. The
	// one striped accounting write is last; its wait must not extend a lease or
	// final migration deadline. Recheck publication deadlines after this wait;
	// the migration guard revisits the job row locked by guardMigrationImport.
	if err = addRetainedStorage(ctx, tx, c.Receipt); err != nil {
		return err
	}
	if err = live(ctx, tx, r, c, profile); err != nil {
		return err
	}
	if err = checkMigrationPublication(ctx, tx, c.Envelope); err != nil {
		return err
	}
	if err = commit(ctx, tx); err != nil {
		return err
	}
	if a.afterCommit != nil {
		if err = a.afterCommit(); err != nil {
			return fmt.Errorf("%w: %w", ErrUncertain, err)
		}
	}
	return nil
}

func (a *authority) abort(ctx context.Context, org, attempt ID) error {
	tx, err := a.begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(ctx, tx)
	r, err := attemptLock(ctx, tx, attempt)
	if err != nil {
		return err
	}
	if r.organization != org.String() {
		return ErrNotFound
	}
	if r.state == "accepted" {
		return ErrAccepted
	}
	if _, err = dbgen.New(tx).AuthorityAbortUpdateAttempt(ctx, attempt.String()); err != nil {
		return err
	}
	if _, err = dbgen.New(tx).AuthorityAbortDeleteSource(ctx, attempt.String()); err != nil {
		return err
	}
	if _, err = dbgen.New(tx).AuthorityAbortInsertCleanup(ctx, attempt.String()); err != nil {
		return err
	}
	return commit(ctx, tx)
}

func (a *authority) confirmCancellation(ctx context.Context, attempt ID, m *LocalMember, owners ...*memberOwner) error {
	tx, err := a.begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(ctx, tx)
	r, err := attemptLock(ctx, tx, attempt)
	if err != nil {
		return err
	}
	if r.state != "aborted" {
		return ErrStale
	}
	if err = guardMemberOwner(ctx, tx, m, owners); err != nil {
		return err
	}
	tag, err := dbgen.New(tx).
		AuthorityConfirmCancellationUpdateTarget(ctx, dbgen.AuthorityConfirmCancellationUpdateTargetParams{
			Attempt:     attempt.String(),
			Member:      m.id.String(),
			Incarnation: m.bytes.FileID().String(),
			OwnerEpoch:  ownerEpoch(owners),
		})
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrConflict
	}
	if _, err = dbgen.New(tx).
		AuthorityConfirmCancellationUpdateCleanup(ctx, dbgen.AuthorityConfirmCancellationUpdateCleanupParams{
			Attempt: attempt.String(),
			Member:  m.id.String(),
		}); err != nil {
		return err
	}
	return commit(ctx, tx)
}

func ownerEpoch(owners []*memberOwner) int64 {
	if len(owners) > 0 && owners[0] != nil {
		return owners[0].epoch
	}
	return 0
}

func guardMemberOwner(ctx context.Context, tx pgx.Tx, m *LocalMember, owners []*memberOwner) error {
	if len(owners) > 0 && owners[0] != nil {
		if owners[0].member != m {
			return ErrConflict
		}
		return owners[0].guard(ctx, tx)
	}
	var member string
	if err := func() error {
		sqlcResult, sqlcErr := dbgen.New(tx).GuardMemberOwnerReadMember(ctx, m.id.String())
		if sqlcErr != nil {
			return sqlcErr
		}
		member = sqlcResult
		return nil
	}(); err != nil {
		return err
	}
	var active bool
	if err := func() error {
		sqlcResult, sqlcErr := dbgen.New(tx).GuardMemberOwnerCheckMemberOwner(ctx, member)
		if sqlcErr != nil {
			return sqlcErr
		}
		active = sqlcResult
		return nil
	}(); err != nil {
		return err
	}
	if active {
		return ErrStale
	}
	return nil
}

func recordCopyLocator(
	ctx context.Context,
	tx pgx.Tx,
	c claim,
	m *LocalMember,
	loc hotstore.Locator,
	owners []*memberOwner,
) (dbgen.AuthorityRecordCopyRow, error) {
	raw, err := json.Marshal(loc)
	if err != nil {
		return dbgen.AuthorityRecordCopyRow{}, err
	}
	stored, err := dbgen.New(tx).
		AuthorityRecordCopy(ctx, dbgen.AuthorityRecordCopyParams{
			Attempt:     c.Attempt.String(),
			Member:      m.id.String(),
			Incarnation: loc.File.String(),
			Locator:     raw,
			OwnerEpoch:  ownerEpoch(owners),
		})
	if errors.Is(err, pgx.ErrNoRows) {
		return stored, ErrStale
	}
	return stored, err
}

func settleRecordedCopy(
	ctx context.Context,
	tx pgx.Tx,
	r attemptRow,
	c claim,
	profile int64,
	m *LocalMember,
	loc hotstore.Locator,
	stored dbgen.AuthorityRecordCopyRow,
) error {
	if m.opts.MaxRecords <= 0 {
		return nil
	}
	var lane *int64
	if stored.CapacityLane != nil {
		lane = new(int64(*stored.CapacityLane))
	}
	if err := settleTargetCapacity(
		ctx,
		tx,
		c.Attempt,
		m.id,
		loc,
		stored.CapacityReservedBytes,
		stored.CapacitySettled,
		lane,
	); err != nil {
		return err
	}
	return live(ctx, tx, r, c, profile)
}
