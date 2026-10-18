//go:build linux

package receiptstore

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/cotocisternas/fry/internal/hotstore"
	"github.com/cotocisternas/fry/internal/receiptstore/dbgen"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const memberFileHeaderBytes int64 = 64

const capacityPreparationRetries = 8

var errCapacityRace = errors.New("receiptstore: capacity candidate changed")

// ValidateMemberOptions validates byte-store options and durable rewrite headroom.
func ValidateMemberOptions(opts hotstore.Options) error {
	_, err := memberStagingBytes(opts)
	return err
}

func memberStagingBytes(opts hotstore.Options) (int64, error) {
	if err := hotstore.ValidateOptions(opts); err != nil {
		return 0, err
	}
	if opts.MaxRecords == 0 {
		return 0, nil
	}
	encoded, err := hotstore.EncodedRecordBytes(opts.MaxMessageBytes)
	if err != nil {
		return 0, err
	}
	n := int64(opts.MaxConcurrentAppends)
	if n > math.MaxInt64/2/encoded {
		return 0, fmt.Errorf("receiptstore: staging budget overflow")
	}
	h := 2 * n * encoded
	if opts.MaxFileBytes > (math.MaxInt64-h)/2 || 2*opts.MaxFileBytes+h > opts.MaxBytes {
		return 0, fmt.Errorf("receiptstore: member capacity requires two capped files plus staging")
	}
	return h, nil
}

// Version 1 limits worst-case byte reservation tails rather than promising
// utilization under arbitrary mixtures of record and byte pressure.
func memberCapacityLaneCount(opts hotstore.Options) int64 {
	if opts.MaxRecords == 0 {
		return 0
	}
	e, err := hotstore.EncodedRecordBytes(opts.MaxMessageBytes)
	if err != nil || e <= 0 {
		return 0
	}
	n := (opts.MaxFileBytes - memberFileHeaderBytes) / e / 8
	if n < 1 {
		n = 1
	}
	if n > 16 {
		n = 16
	}
	if n > int64(opts.MaxConcurrentAppends) {
		n = int64(opts.MaxConcurrentAppends)
	}
	if n > opts.MaxRecords {
		n = opts.MaxRecords
	}
	return n
}

func capacityLaneCeilings(opts hotstore.Options, lane int64) (int64, int64) {
	n := memberCapacityLaneCount(opts)
	records := opts.MaxRecords / n
	if lane < opts.MaxRecords%n {
		records++
	}
	bytes := (opts.MaxFileBytes - memberFileHeaderBytes) / n
	if lane < (opts.MaxFileBytes-memberFileHeaderBytes)%n {
		bytes++
	}
	return records, bytes
}

func capacityLaneStart(attempt, member, file ID, retry int, count int64) int64 {
	// Callers use the validated 1..16 lane catalog and a bounded retry index.
	if count < 1 || count > 16 || retry < 0 || retry >= capacityPreparationRetries {
		panic("receiptstore: invalid capacity lane selection")
	}
	data := append([]byte("fry/capacity-lane/v1/"), attempt[:]...)
	data = append(data, member[:]...)
	data = append(data, file[:]...)
	h := sha256.Sum256(data)
	lane := binary.BigEndian.Uint64(h[:8]) % uint64(count)
	return (int64(lane&15) + int64(retry)) % count
}

func preflightMemberProfile(ctx context.Context, pool *pgxpool.Pool, id ID, opts hotstore.Options) error {
	if _, err := memberStagingBytes(opts); err != nil {
		return err
	}
	exists, err := dbgen.New(pool).PreflightMemberProfileCheckMember(ctx, id.String())
	if err != nil {
		return err
	}
	profile, err := dbgen.New(pool).PreflightMemberProfileReadMemberProfile(ctx, id.String())
	if !exists && errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if err = validateFrozenMemberProfile(exists, profile, opts); err != nil {
		return err
	}
	if !profile.Enabled {
		return nil
	}
	if err = preflightActiveCapacity(ctx, pool, id, profile); err != nil {
		return err
	}
	return preflightCapacityLanes(ctx, pool, id, opts, int64(profile.LaneCount))
}

func validateFrozenMemberProfile(
	exists bool,
	profile dbgen.PreflightMemberProfileReadMemberProfileRow,
	opts hotstore.Options,
) error {
	if !exists || profile.Enabled != (opts.MaxRecords > 0) || profile.LaneVersion != 1 {
		return ErrConflict
	}
	if !profile.Enabled {
		if profile.LaneCount != 0 {
			return ErrConflict
		}
		return nil
	}
	expected, err := memberStagingBytes(opts)
	if err != nil {
		return err
	}
	if !sameFrozenCapacityLimits(profile, opts, expected) {
		return ErrConflict
	}
	return nil
}

func sameFrozenCapacityLimits(
	profile dbgen.PreflightMemberProfileReadMemberProfileRow,
	opts hotstore.Options,
	staging int64,
) bool {
	return profile.MaxMessageBytes == opts.MaxMessageBytes &&
		profile.MaxConcurrentAppends == int64(opts.MaxConcurrentAppends) &&
		profile.MaxBytes == opts.MaxBytes &&
		profile.MaxRecords == opts.MaxRecords &&
		profile.MaxFileBytes == opts.MaxFileBytes &&
		profile.StagingBytes == staging &&
		int64(profile.LaneCount) == memberCapacityLaneCount(opts)
}

func preflightActiveCapacity(
	ctx context.Context,
	pool *pgxpool.Pool,
	id ID,
	profile dbgen.PreflightMemberProfileReadMemberProfileRow,
) error {
	invalid, err := dbgen.New(pool).
		PreflightMemberProfileReadMemberFile(ctx, dbgen.PreflightMemberProfileReadMemberFileParams{
			Member:       id.String(),
			ChargedBytes: profile.MaxFileBytes,
			Column3:      profile.StagingBytes,
			Column4:      profile.MaxBytes,
		})
	if err != nil {
		return err
	}
	if invalid == nil {
		return fmt.Errorf("receiptstore: unexpected NULL in PreflightMemberProfileReadMemberFile.column_1")
	}
	if *invalid {
		return ErrConflict
	}
	return nil
}

type capacityLaneUsage struct {
	lane, recordCeiling, byteCeiling, records, bytes *int64
}

func (usage capacityLaneUsage) complete() bool {
	return usage.lane != nil && usage.recordCeiling != nil && usage.byteCeiling != nil && usage.records != nil &&
		usage.bytes != nil
}

func (usage capacityLaneUsage) validate(opts hotstore.Options, seen, lanes int64) error {
	if !usage.complete() {
		return ErrConflict
	}
	if *usage.lane != seen || seen >= lanes {
		return ErrConflict
	}
	expectedR, expectedB := capacityLaneCeilings(opts, *usage.lane)
	if *usage.recordCeiling != expectedR || *usage.byteCeiling != expectedB || *usage.records < 0 ||
		*usage.records > *usage.recordCeiling ||
		*usage.bytes < 0 ||
		*usage.bytes > *usage.byteCeiling {
		return ErrConflict
	}
	return nil
}

type capacityLaneCensus struct {
	previous    string
	seen, lanes int64
	opts        hotstore.Options
}

func (census *capacityLaneCensus) scan(rows pgx.Rows) error {
	var file string
	var usage capacityLaneUsage
	if err := rows.Scan(
		&file,
		&usage.lane,
		&usage.recordCeiling,
		&usage.byteCeiling,
		&usage.records,
		&usage.bytes,
	); err != nil {
		return err
	}
	if file != census.previous {
		if census.previous != "" && census.seen != census.lanes {
			return ErrConflict
		}
		census.previous, census.seen = file, 0
	}
	if err := usage.validate(census.opts, census.seen, census.lanes); err != nil {
		return err
	}
	census.seen++
	return nil
}

func preflightCapacityLanes(ctx context.Context, pool *pgxpool.Pool, id ID, opts hotstore.Options, lanes int64) error {
	// One bounded lane set per active file; removed history is not scanned.
	rows, err := pool.Query(ctx, dbgen.PreflightMemberProfileReadMemberFileRemoved, id.String())
	if err != nil {
		return err
	}
	defer rows.Close()
	census := capacityLaneCensus{opts: opts, lanes: lanes}
	for rows.Next() {
		if err = census.scan(rows); err != nil {
			return err
		}
	}
	if err = rows.Err(); err != nil {
		return err
	}
	if census.previous != "" && census.seen != lanes {
		return ErrConflict
	}
	return nil
}

func insertMemberProfile(ctx context.Context, tx pgx.Tx, id ID, opts hotstore.Options) error {
	h, err := memberStagingBytes(opts)
	if err != nil {
		return err
	}
	lanes := memberCapacityLaneCount(opts)
	if lanes < 0 || lanes > 16 {
		return ErrConflict
	}
	_, err = dbgen.New(tx).
		InsertMemberProfile(ctx, dbgen.InsertMemberProfileParams{
			Member:               id.String(),
			Enabled:              opts.MaxRecords > 0,
			MaxMessageBytes:      opts.MaxMessageBytes,
			MaxConcurrentAppends: int64(opts.MaxConcurrentAppends),
			MaxBytes:             opts.MaxBytes,
			MaxRecords:           opts.MaxRecords,
			MaxFileBytes:         opts.MaxFileBytes,
			StagingBytes:         h,
			LaneCount:            int16(lanes),
		})
	return err
}

func insertCapacityLanes(ctx context.Context, tx pgx.Tx, member, file ID, opts hotstore.Options) error {
	for lane := range memberCapacityLaneCount(opts) {
		r, b := capacityLaneCeilings(opts, lane)
		if _, err := dbgen.New(tx).
			InsertCapacityLanes(ctx, dbgen.InsertCapacityLanesParams{
				Member:        member.String(),
				FileID:        file.String(),
				Lane:          int16(lane),
				RecordCeiling: r,
				ByteCeiling:   b,
			}); err != nil {
			return err
		}
	}
	return nil
}

type memberCapacityProfile struct {
	enabled                            bool
	maxMsg, maxRecords, maxFile, lanes int64
}

func reserveTargetCapacity(
	ctx context.Context,
	tx pgx.Tx,
	attempt ID,
	target MemberIdentity,
	fresh bool,
	p memberCapacityProfile,
	retry int,
) error {
	if !p.enabled {
		return nil
	}
	if !fresh {
		return validateExistingReservation(ctx, tx, attempt, target, p.lanes)
	}
	encoded, err := hotstore.EncodedRecordBytes(p.maxMsg)
	if err != nil {
		return err
	}
	if p.lanes <= 0 || p.lanes > 16 {
		return ErrConflict
	}
	start := capacityLaneStart(attempt, target.Member, target.Incarnation, retry, p.lanes)
	result, err := dbgen.New(tx).
		ReserveTargetCapacityCandidate(ctx, dbgen.ReserveTargetCapacityCandidateParams{
			Member:  target.Member.String(),
			FileID:  target.Incarnation.String(),
			Column3: encoded,
			Column4: start,
			Column5: p.lanes,
			Attempt: attempt.String(),
		})
	if err != nil {
		return err
	}
	return capacityReservationOutcome(result)
}

func validateExistingReservation(ctx context.Context, tx pgx.Tx, attempt ID, target MemberIdentity, lanes int64) error {
	stored, err := dbgen.New(tx).
		ReserveTargetCapacityReadTarget(ctx, dbgen.ReserveTargetCapacityReadTargetParams{
			Attempt:     attempt.String(),
			Member:      target.Member.String(),
			Incarnation: target.Incarnation.String(),
		})
	if err != nil {
		return err
	}
	if stored.CapacityReservedBytes == nil || stored.CapacityLane == nil || *stored.CapacityLane < 0 ||
		int64(*stored.CapacityLane) >= lanes {
		return ErrConflict
	}
	return nil
}

func capacityReservationOutcome(result dbgen.ReserveTargetCapacityCandidateRow) error {
	if !result.Exists && result.Count == 0 && result.Count_2 == 0 {
		return hotstore.ErrCapacity
	}
	if result.Exists && result.Count == 0 && result.Count_2 == 0 {
		return errCapacityRace
	}
	if result.Count != 1 || result.Count_2 != 1 {
		return ErrConflict
	}
	return nil
}

// A retry is private and safe only after a confirmed whole-transaction rollback.
func rollbackCapacityRace(ctx context.Context, tx pgx.Tx) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := tx.Rollback(ctx); err != nil {
		return fmt.Errorf("%w: capacity rollback: %w", ErrUncertain, err)
	}
	return errCapacityRace
}

func settleTargetCapacity(
	ctx context.Context,
	tx pgx.Tx,
	attempt, member ID,
	loc hotstore.Locator,
	reservation *int64,
	settled bool,
	lane *int64,
) error {
	actual, err := hotstore.EncodedRecordBytes(loc.Size)
	if err != nil {
		return err
	}
	if err = validateCapacitySettlement(actual, reservation, lane); err != nil {
		return err
	}
	if settled {
		if actual != *reservation {
			return ErrConflict
		}
		return nil
	}
	laneNumber := *lane
	if laneNumber < 0 || laneNumber >= 16 {
		return ErrConflict
	}
	if actual != *reservation {
		return refundTargetCapacity(ctx, tx, attempt, member, loc, *reservation, actual, laneNumber)
	}
	tag, err := dbgen.New(tx).
		SettleTargetCapacityUpdateTarget(ctx, dbgen.SettleTargetCapacityUpdateTargetParams{
			Attempt:                 attempt.String(),
			Member:                  member.String(),
			CapacityReservedBytes:   sqlcPointer(actual),
			Incarnation:             loc.File.String(),
			CapacityReservedBytes_2: sqlcPointer(*reservation),
			CapacityLane:            sqlcPointer(int16(laneNumber)),
		})
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrConflict
	}
	return nil
}

func reserveRewriteCapacity(ctx context.Context, tx pgx.Tx, member ID) (int64, error) {
	var enabled bool
	var c, h, budget int64
	if err := func() error {
		sqlcResult, sqlcErr := dbgen.New(tx).ReserveRewriteCapacityReadMemberProfile(ctx, member.String())
		if sqlcErr != nil {
			return sqlcErr
		}
		enabled = sqlcResult.Enabled
		c = sqlcResult.MaxFileBytes
		h = sqlcResult.StagingBytes
		budget = sqlcResult.MaxBytes
		return nil
	}(); err != nil {
		return 0, err
	}
	if !enabled {
		return 0, nil
	}
	var charged int64
	if err := func() error {
		sqlcResult, sqlcErr := dbgen.New(tx).ReserveRewriteCapacityCheckMemberFile(ctx, member.String())
		if sqlcErr != nil {
			return sqlcErr
		}
		charged = sqlcResult
		return nil
	}(); err != nil {
		return 0, err
	}
	if charged > budget-h || c > budget-h-charged {
		return 0, hotstore.ErrCapacity
	}
	return c, nil
}

// A survivor remains in its original lane. Each new census is a subset of the
// old lane, so neither repacking nor cross-lane borrowing is needed.
func setSuccessorCapacity(
	ctx context.Context,
	tx pgx.Tx,
	member, file, job ID,
	opts hotstore.Options,
	records []hotRecord,
	locators map[ID]hotstore.Locator,
) error {
	if opts.MaxRecords == 0 {
		return nil
	}
	counts, bytes, err := successorLaneUsage(opts, records, locators)
	if err != nil {
		return err
	}
	for lane := range memberCapacityLaneCount(opts) {
		tag, err := dbgen.New(tx).
			SetSuccessorCapacity(ctx, dbgen.SetSuccessorCapacityParams{
				Member:          member.String(),
				FileID:          file.String(),
				Lane:            int16(lane),
				ReservedRecords: counts[lane],
				ReservedBytes:   bytes[lane],
				RewriteJob:      sqlcPointer(job.String()),
			})
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrConflict
		}
	}
	return nil
}

func validateCapacitySettlement(actual int64, reservation, lane *int64) error {
	if reservation == nil || lane == nil || *lane < 0 || *lane >= 16 || actual > *reservation {
		return ErrConflict
	}
	return nil
}

func refundTargetCapacity(
	ctx context.Context,
	tx pgx.Tx,
	attempt, member ID,
	loc hotstore.Locator,
	reservation, actual, lane int64,
) error {
	if lane < 0 || lane >= 16 {
		return ErrConflict
	}
	result, err := dbgen.New(tx).
		SettleTargetCapacityRefunded(ctx, dbgen.SettleTargetCapacityRefundedParams{
			Member:          member.String(),
			FileID:          loc.File.String(),
			ReservedBytes:   reservation,
			ReservedBytes_2: actual,
			Attempt:         attempt.String(),
			Lane:            int16(lane),
		})
	if err != nil {
		return err
	}
	if result.Count != 1 || result.Count_2 != 1 {
		return ErrConflict
	}
	return nil
}

func successorLaneUsage(
	opts hotstore.Options,
	records []hotRecord,
	locators map[ID]hotstore.Locator,
) ([16]int64, [16]int64, error) {
	var counts, bytes [16]int64
	for _, r := range records {
		if !r.keep {
			continue
		}
		if r.lane == nil || *r.lane < 0 || *r.lane >= memberCapacityLaneCount(opts) {
			return counts, bytes, ErrConflict
		}
		encoded, err := hotstore.EncodedRecordBytes(locators[r.receipt].Size)
		if err != nil {
			return counts, bytes, err
		}
		lane := *r.lane
		ceilingR, ceilingB := capacityLaneCeilings(opts, lane)
		if counts[lane] >= ceilingR || encoded > ceilingB-bytes[lane] {
			return counts, bytes, ErrConflict
		}
		counts[lane]++
		bytes[lane] += encoded
	}
	return counts, bytes, nil
}
