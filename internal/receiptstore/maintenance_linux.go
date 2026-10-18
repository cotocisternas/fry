//go:build linux

package receiptstore

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/cotocisternas/fry/internal/hotstore"
	"github.com/cotocisternas/fry/internal/receiptstore/dbgen"

	"github.com/jackc/pgx/v5"
)

type MaintenanceOptions struct {
	// MaxActions bounds scheduling attempts, including empty scans. MaxReceipts
	// bounds each new pack batch, expiry sweep, or cleanup scan. Compaction
	// retains the existing Packer bound of 1,024 members per source pack.
	MaxActions, MaxReceipts int
	MaxDuration             time.Duration
}

type MaintenanceResult struct {
	Actions, Expired, Finished, Packed, Compacted, Retired int
	// Oversized is a bounded sample (at most MaxReceipts) of otherwise eligible
	// receipts whose single-record image exceeds the pack size limit.
	Oversized int
}

// Maintenance schedules existing storage lifecycle operations. It does not
// acquire cleanup authority or reclaim hot files. Cold-operation organization
// cursors provide round-robin opportunities within this worker only: they are
// not durable and do not coordinate fairness or ownership among distributed
// workers. SweepExpiry uses global ordering; FinishCleanup advances a separate
// process-local deadline scan so blocked debt does not retain every turn.
type Maintenance struct {
	store  *Store
	packer *Packer
	opts   MaintenanceOptions
	slot   chan struct{}
	next   int
	cursor [3]string
	maxRaw int64
}

func NewMaintenance(store *Store, packer *Packer, opts MaintenanceOptions) (*Maintenance, error) {
	if store == nil || (packer != nil && packer.receipts != store) || !validMaintenanceOptions(opts) {
		return nil, fmt.Errorf("receiptstore: invalid maintenance options")
	}
	m := &Maintenance{store: store, packer: packer, opts: opts, slot: make(chan struct{}, 1)}
	if packer != nil {
		m.maxRaw = maximumPackRecord(packer.opts.MaxPackBytes)
	}
	return m, nil
}

// RunOnce has no internal polling loop. All operations share the caller's
// deadline capped by MaxDuration. Selection is advisory; Packer and Store retain
// their own receipt locks, leases, staging ownership and actual-read checks.
// Each cold category is attempted at most once per pass, so a failed candidate
// is deferred until the caller schedules another pass. Errors accompany partial
// results; normal conflicts with another worker are skipped.
func (m *Maintenance) RunOnce(ctx context.Context) (MaintenanceResult, error) {
	var result MaintenanceResult
	select {
	case m.slot <- struct{}{}:
		defer func() { <-m.slot }()
	default:
		return result, ErrBusy
	}
	ctx, cancel := context.WithTimeout(ctx, m.opts.MaxDuration)
	defer cancel()
	categories := 2
	if m.packer != nil {
		categories = 5
	}
	var resultErr error
	// One visit per category bounds retries even with an oversized action budget.
	for range min(m.opts.MaxActions, categories) {
		if err := ctx.Err(); err != nil {
			return result, errors.Join(resultErr, err)
		}
		action := m.next
		m.next = (m.next + 1) % categories
		result.Actions++
		err := m.runAction(ctx, action, &result)
		if maintenanceActionFailed(err) {
			resultErr = errors.Join(resultErr, fmt.Errorf("maintenance action %d: %w", action, err))
		}
	}
	return result, resultErr
}

func (m *Maintenance) oversized(ctx context.Context) (int, error) {
	limit := m.opts.MaxReceipts
	if limit < 1 || limit > 1024 {
		return 0, ErrConflict
	}
	queryLimit := int32(limit)
	var count int
	err := func() error {
		sqlcResult, sqlcErr := dbgen.New(m.store.db.pool).
			MaintenanceOversized(ctx, dbgen.MaintenanceOversizedParams{RawSize: m.maxRaw, Limit: queryLimit})
		if sqlcErr != nil {
			return sqlcErr
		}
		count = int(sqlcResult)
		return nil
	}()
	return count, err
}

// nextOrganization issues at most two one-row queries, wrapping the cursor.
// An org with many old messages cannot consume the entire candidate window.
func (m *Maintenance) nextOrganization(ctx context.Context, kind int) (ID, error) {
	cursor := m.cursor[kind]
	for range 2 {
		var raw string
		var err error
		queries := dbgen.New(m.store.db.pool)
		switch kind {
		case 0:
			raw, err = queries.MaintenanceNextPackOrganization(
				ctx,
				dbgen.MaintenanceNextPackOrganizationParams{RawSize: m.maxRaw, Organization: cursor},
			)
		case 1:
			raw, err = queries.MaintenanceNextCompactOrganization(ctx, cursor)
		case 2:
			raw, err = queries.MaintenanceNextRetireOrganization(ctx, cursor)
		default:
			return ID{}, ErrConflict
		}
		if errors.Is(err, pgx.ErrNoRows) {
			if cursor == "" {
				return ID{}, nil
			}
			cursor = ""
			continue
		}
		if err != nil {
			return ID{}, err
		}
		id, err := idFrom(raw)
		if err == nil {
			m.cursor[kind] = raw
		}
		return id, err
	}
	return ID{}, nil
}

func (m *Maintenance) pack(ctx context.Context) (int, error) {
	limit := m.opts.MaxReceipts
	if limit < 1 || limit > 1024 {
		return 0, ErrConflict
	}
	org, err := m.nextOrganization(ctx, 0)
	if err != nil || org == (ID{}) {
		return 0, err
	}
	rows, err := dbgen.New(m.store.db.pool).
		MaintenancePack(ctx, dbgen.MaintenancePackParams{
			RawSize:      m.maxRaw,
			Organization: org.String(),
			Limit:        int32(limit),
		})
	if err != nil {
		return 0, err
	}
	ids, err := m.packBatch(rows)
	if err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		return 0, nil
	}
	_, err = m.packer.Pack(ctx, org, ids)
	if err != nil {
		return 0, err
	}
	return len(ids), nil
}

func (m *Maintenance) compact(ctx context.Context) (int, error) {
	org, err := m.nextOrganization(ctx, 1)
	if err != nil || org == (ID{}) {
		return 0, err
	}
	var raw string
	err = func() error {
		sqlcResult, sqlcErr := dbgen.New(m.store.db.pool).MaintenanceCompact(ctx, org.String())
		if sqlcErr != nil {
			return sqlcErr
		}
		raw = sqlcResult
		return nil
	}()
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	id, err := idFrom(raw)
	if err != nil {
		return 0, err
	}
	_, err = m.packer.Compact(ctx, org, id)
	if err != nil {
		return 0, err
	}
	return 1, nil
}

func (m *Maintenance) retire(ctx context.Context) (int, error) {
	org, err := m.nextOrganization(ctx, 2)
	if err != nil || org == (ID{}) {
		return 0, err
	}
	var raw string
	err = func() error {
		sqlcResult, sqlcErr := dbgen.New(m.store.db.pool).MaintenanceRetire(ctx, org.String())
		if sqlcErr != nil {
			return sqlcErr
		}
		raw = sqlcResult
		return nil
	}()
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	id, err := idFrom(raw)
	if err != nil {
		return 0, err
	}
	if err = m.packer.Retire(ctx, org, id); err != nil {
		return 0, err
	}
	return 1, nil
}

// Use the framing implementation rather than approximating raw byte totals.
func maximumPackRecord(maxBytes int64) int64 {
	low, high := int64(0), maxBytes
	for low < high {
		middle := low + (high-low+1)/2
		size, err := hotstore.ImageSize(middle)
		if err == nil && size <= maxBytes {
			low = middle
		} else {
			high = middle - 1
		}
	}
	return low
}

func (m *Maintenance) runAction(ctx context.Context, action int, result *MaintenanceResult) (err error) {
	switch action {
	case 0:
		result.Expired, err = m.store.SweepExpiry(ctx, m.opts.MaxReceipts)
	case 1:
		result.Finished, err = m.store.FinishCleanup(ctx, m.opts.MaxReceipts)
	case 2:
		result.Retired, err = m.retire(ctx)
	case 3:
		result.Compacted, err = m.compact(ctx)
	case 4:
		result.Oversized, err = m.oversized(ctx)
		if err == nil {
			result.Packed, err = m.pack(ctx)
		}
	}
	return err
}

func maintenanceActionFailed(err error) bool {
	return err != nil && !errors.Is(err, ErrBusy) && !errors.Is(err, ErrConflict) && !errors.Is(err, ErrStale) &&
		!errors.Is(err, ErrPackInUse) &&
		!errors.Is(err, ErrNotFound)
}

func (m *Maintenance) packBatch(rows []dbgen.MaintenancePackRow) ([]ID, error) {
	var ids []ID
	var err error
	var sizes []int64
	var policy int64
	for _, sqlcRow := range rows {
		var raw string
		var size, retention int64
		raw = sqlcRow.ID
		size = sqlcRow.RawSize
		retention = sqlcRow.RecoverySeconds
		if len(ids) > 0 && retention != policy {
			continue
		}
		candidate, e := hotstore.ImageSize(append(sizes, size)...)
		if e != nil {
			err = e
			break
		}
		if candidate > m.packer.opts.MaxPackBytes {
			continue
		}
		id, e := idFrom(raw)
		if e != nil {
			err = e
			break
		}
		policy = retention
		ids = append(ids, id)
		sizes = append(sizes, size)
	}
	if err != nil {
		return nil, err
	}
	return ids, nil
}

func validMaintenanceOptions(opts MaintenanceOptions) bool {
	return opts.MaxActions >= 1 && opts.MaxActions <= 1024 && opts.MaxReceipts >= 1 && opts.MaxReceipts <= 1024 &&
		opts.MaxDuration > 0 &&
		opts.MaxDuration <= 5*time.Minute
}
