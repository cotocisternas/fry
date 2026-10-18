//go:build linux

package receiptstore

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/cotocisternas/fry/internal/hotstore"
	"github.com/cotocisternas/fry/internal/receiptstore/dbgen"

	"github.com/jackc/pgx/v5"
)

// MemberStorageSnapshot separates an MVCC reservation snapshot from a later,
// best-effort owner sample. Charges are not filesystem usage or free disk space.
type MemberStorageSnapshot struct {
	Member        string               `json:"member"`
	SQLSnapshotAt time.Time            `json:"sql_snapshot_at"`
	Profile       MemberStorageProfile `json:"profile"`
	Current       MemberStorageCurrent `json:"current"`
	Owner         MemberStorageOwner   `json:"owner"`
	Files         []MemberStorageFile  `json:"files"`
	NextFile      *string              `json:"next_file"`
	Runtime       MemberStorageRuntime `json:"runtime"`
}

// Legacy profile numbers record initial registration, not frozen runtime limits.
type MemberStorageProfile struct {
	Bounded              bool   `json:"bounded"`
	LimitsFrozen         bool   `json:"limits_frozen"`
	MaxMessageBytes      string `json:"max_message_bytes"`
	MaxConcurrentAppends string `json:"max_concurrent_appends"`
	MaxBytes             string `json:"max_bytes"`
	MaxRecords           string `json:"max_records"`
	MaxFileBytes         string `json:"max_file_bytes"`
	StagingReservedBytes string `json:"staging_reserved_bytes"`
	LaneVersion          string `json:"lane_version"`
	LaneCount            string `json:"lane_count"`
}
type MemberStorageCurrent struct {
	FileID                  string              `json:"file_id"`
	ChargedBytes            string              `json:"charged_bytes"`
	ReservedRecords         *string             `json:"reserved_records"`
	ReservedRecordBytes     *string             `json:"reserved_record_bytes"`
	CanFitMaxRecordSnapshot *bool               `json:"can_fit_max_record_snapshot"`
	Lanes                   []MemberStorageLane `json:"lanes"`
}
type MemberStorageLane struct {
	Lane            string `json:"lane"`
	RecordCeiling   string `json:"record_ceiling"`
	ByteCeiling     string `json:"byte_ceiling"`
	ReservedRecords string `json:"reserved_records"`
	ReservedBytes   string `json:"reserved_bytes"`
}

// An unclosed owner row is conservative proof debt, not a process liveness test.
type MemberStorageOwner struct {
	Epoch  *string `json:"epoch"`
	Closed *bool   `json:"closed"`
}
type MemberStorageFile struct {
	FileID        string  `json:"file_id"`
	State         string  `json:"state"`
	ChargedBytes  string  `json:"charged_bytes"`
	SealedRecords *string `json:"sealed_records"`
	RewriteJob    *string `json:"rewrite_job"`
}

func storageDecimal(v int64) string { return strconv.FormatInt(v, 10) }
func storageDecimalPointer(v *int64) *string {
	if v == nil {
		return nil
	}
	s := storageDecimal(*v)
	return &s
}

// The tuple lower bound implies member >= $1, so member <= $1 restricts the
// seek to exactly one member. Explicit member equality lets PostgreSQL drop the
// leading ORDER BY key and choose the global file_id index, filtering history.
const memberStorageFilesQuery = dbgen.StoreMemberStorageReadMemberFile

// MemberStorage reports one configured member and at most 64 catalog rows. It
// takes no authority row locks, scans no receipts, and does no filesystem I/O.
// Pagination across calls is not a stable inventory during concurrent changes.
func (s *Store) MemberStorage(ctx context.Context, member, afterFile ID, limit int) (MemberStorageSnapshot, error) {
	result := MemberStorageSnapshot{Member: member.String(), Files: []MemberStorageFile{}}
	if member == (ID{}) || limit < 1 || limit > 64 {
		return result, fmt.Errorf("receiptstore: invalid member storage request")
	}
	candidate := s.memberByID[member]
	if candidate == nil {
		return result, ErrNotFound
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, err := s.db.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return result, err
	}
	defer rollback(ctx, tx)
	limits, err := readMemberStorageProfile(ctx, tx, member, &result)
	if err != nil {
		return result, err
	}
	if err = readMemberStorageLanes(ctx, tx, member, limits, &result); err != nil {
		return result, err
	}
	queryLimit := int32(limit + 1)
	if err = readMemberStorageFiles(ctx, tx, member, afterFile, limit, queryLimit, &result); err != nil {
		return result, err
	}
	// No pool connection or database transaction remains held during an owner RPC.
	if err = tx.Commit(ctx); err != nil {
		return result, err
	}
	result.Runtime = sampleMemberStorage(ctx, candidate)
	return result, nil
}

type storageCurrentLimits struct {
	message, records, file, lanes int64
}

func readMemberStorageProfile(
	ctx context.Context,
	tx pgx.Tx,
	member ID,
	result *MemberStorageSnapshot,
) (storageCurrentLimits, error) {
	var message, n, budget, records, file, staging, version, lanes, charge int64
	var current string
	var epoch *int64
	err := tx.QueryRow(ctx, dbgen.StoreMemberStorageReadMember, member.String()).
		Scan(
			&result.SQLSnapshotAt,
			&result.Profile.Bounded,
			&message,
			&n,
			&budget,
			&records,
			&file,
			&staging,
			&version,
			&lanes,
			&current,
			&charge,
			&epoch,
			&result.Owner.Closed,
		)
	if err != nil {
		return storageCurrentLimits{}, err
	}
	if _, err = idFrom(current); err != nil {
		return storageCurrentLimits{}, err
	}
	result.Owner.Epoch = storageDecimalPointer(epoch)
	result.Profile.LimitsFrozen = result.Profile.Bounded
	result.Profile.MaxMessageBytes = storageDecimal(message)
	result.Profile.MaxConcurrentAppends = storageDecimal(n)
	result.Profile.MaxBytes = storageDecimal(budget)
	result.Profile.MaxRecords = storageDecimal(records)
	result.Profile.MaxFileBytes = storageDecimal(file)
	result.Profile.StagingReservedBytes = storageDecimal(staging)
	result.Profile.LaneVersion = storageDecimal(version)
	result.Profile.LaneCount = storageDecimal(lanes)
	result.Current = MemberStorageCurrent{
		FileID:       current,
		ChargedBytes: storageDecimal(charge),
		Lanes:        []MemberStorageLane{},
	}
	if lanes < 0 || lanes > 16 {
		return storageCurrentLimits{}, ErrConflict
	}
	return storageCurrentLimits{message, records, file, lanes}, nil
}

func readMemberStorageLanes(
	ctx context.Context,
	tx pgx.Tx,
	member ID,
	limits storageCurrentLimits,
	result *MemberStorageSnapshot,
) error {
	laneRows, err := dbgen.New(tx).
		StoreMemberStorageReadMemberFileLane(ctx, dbgen.StoreMemberStorageReadMemberFileLaneParams{
			Member: member.String(),
			FileID: result.Current.FileID,
		})
	if err != nil {
		return err
	}
	totals := memberStorageLaneTotals{limits: limits}
	if result.Profile.Bounded {
		totals.encoded, err = hotstore.EncodedRecordBytes(limits.message)
		if err != nil {
			return err
		}
	}
	for _, laneRow := range laneRows {
		if err = totals.add(laneRow, &result.Current); err != nil {
			return err
		}
	}
	if int64(len(result.Current.Lanes)) != limits.lanes {
		return ErrConflict
	}
	if result.Profile.Bounded {
		usedR, usedB := storageDecimal(totals.records), storageDecimal(totals.bytes)
		result.Current.ReservedRecords = &usedR
		result.Current.ReservedRecordBytes = &usedB
		result.Current.CanFitMaxRecordSnapshot = &totals.fit
	}
	return nil
}

type memberStorageLaneTotals struct {
	limits                  storageCurrentLimits
	records, bytes, encoded int64
	fit                     bool
}

func (total *memberStorageLaneTotals) add(
	row dbgen.StoreMemberStorageReadMemberFileLaneRow,
	current *MemberStorageCurrent,
) error {
	lane, ceilingR, ceilingB, usedR, usedB := int64(row.Lane), row.RecordCeiling, row.ByteCeiling,
		row.ReservedRecords, row.ReservedBytes
	if lane != int64(len(current.Lanes)) || usedR < 0 || usedR > ceilingR || usedB < 0 || usedB > ceilingB ||
		usedR > total.limits.records-total.records ||
		usedB > total.limits.file-memberFileHeaderBytes-total.bytes {
		return ErrConflict
	}
	total.fit = total.fit || (usedR < ceilingR && total.encoded <= ceilingB-usedB)
	total.records += usedR
	total.bytes += usedB
	current.Lanes = append(
		current.Lanes,
		MemberStorageLane{
			storageDecimal(lane),
			storageDecimal(ceilingR),
			storageDecimal(ceilingB),
			storageDecimal(usedR),
			storageDecimal(usedB),
		},
	)
	return nil
}

func readMemberStorageFiles(
	ctx context.Context,
	tx pgx.Tx,
	member, afterFile ID,
	limit int,
	queryLimit int32,
	result *MemberStorageSnapshot,
) error {
	after := ""
	if afterFile != (ID{}) {
		after = afterFile.String()
	}
	fileRows, err := dbgen.New(tx).
		StoreMemberStorageReadMemberFile(ctx, dbgen.StoreMemberStorageReadMemberFileParams{
			Member:   member.String(),
			Member_2: after,
			Limit:    queryLimit,
		})
	if err != nil {
		return err
	}
	for _, fileRow := range fileRows {
		var f MemberStorageFile
		var bytes int64
		var count *int64
		f.FileID, f.State, bytes, count, f.RewriteJob = fileRow.FileID, fileRow.State,
			fileRow.ChargedBytes, fileRow.SealedRecords, fileRow.RewriteJob
		if _, err = idFrom(f.FileID); err != nil {
			break
		}
		f.ChargedBytes = storageDecimal(bytes)
		f.SealedRecords = storageDecimalPointer(count)
		if len(result.Files) == limit {
			last := result.Files[limit-1].FileID
			result.NextFile = &last
			break
		}
		result.Files = append(result.Files, f)
	}
	if err != nil {
		return err
	}
	return nil
}
