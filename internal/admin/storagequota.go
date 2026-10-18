//go:build linux

package admin

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/cotocisternas/fry/internal/admin/dbgen"
	"github.com/cotocisternas/fry/internal/receiptstore"

	"github.com/jackc/pgx/v5"
)

type StorageUsage struct {
	Organization         string     `json:"organization"`
	RetainedOriginals    string     `json:"retained_originals"`
	RetainedBytes        string     `json:"retained_bytes"`
	LimitBytes           *string    `json:"limit_bytes"`
	PolicyVersion        string     `json:"policy_version"`
	MeasuredAt           time.Time  `json:"measured_at"`
	SampledAt            *time.Time `json:"sampled_at"`
	SampledBytes         *string    `json:"sampled_bytes"`
	SampledOriginals     *string    `json:"sampled_originals"`
	SampledPolicyVersion *string    `json:"sampled_policy_version"`
	AlertActive          bool       `json:"alert_active"`
	SampleStale          bool       `json:"sample_stale"`
	LastTransitionAt     *time.Time `json:"last_transition_at"`
	LastTransitionState  *string    `json:"last_transition_state"`
}

func decimalPointer(v *int64) *string {
	if v == nil {
		return nil
	}
	s := strconv.FormatInt(*v, 10)
	return &s
}

func parseStorageLimit(value string) (int64, error) {
	if value == "" {
		return 0, ErrInvalid
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return 0, ErrInvalid
		}
	}
	number, err := strconv.ParseInt(value, 10, 64)
	if err != nil || number <= 0 {
		return 0, ErrInvalid
	}
	return number, nil
}

func storageOrganization(ctx context.Context, tx pgx.Tx, token, org string) error {
	if !validID(org) {
		return ErrInvalid
	}
	who, err := authorize(ctx, tx, token)
	if err != nil {
		return err
	}
	if !allowed(who, org, false) {
		return ErrForbidden
	}
	var exists bool
	if exists, err = dbgen.New(tx).OrganizationExistsForStorage(ctx, org); err != nil {
		return err
	}
	if !exists {
		return ErrInvalid
	}
	return nil
}

func (s *Service) StorageUsage(ctx context.Context, token, org string) (StorageUsage, error) {
	result := StorageUsage{Organization: org, PolicyVersion: "0", SampleStale: true}
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return result, err
	}
	defer rollback(ctx, tx)
	if err = storageOrganization(ctx, tx, token, org); err != nil {
		return result, err
	}
	id, err := mailID(org)
	if err != nil {
		return result, err
	}
	usage, err := receiptstore.ReadOrganizationStorageUsage(ctx, tx, id)
	if err != nil {
		return result, err
	}
	result.RetainedOriginals = strconv.FormatInt(usage.RetainedOriginals, 10)
	result.RetainedBytes = strconv.FormatInt(usage.RetainedBytes, 10)
	policy, err := storagePolicy(ctx, tx, org)
	if err != nil {
		return result, err
	}
	if result.MeasuredAt, err = dbgen.New(tx).GetMeasurementTime(ctx); err != nil {
		return result, err
	}
	applyStoragePolicy(&result, policy)
	err = readStorageTransition(ctx, tx, org, &result)
	return result, err
}

func (s *Service) SetStorageQuota(ctx context.Context, token, org string, raw *string) error {
	var limit *int64
	if raw != nil {
		number, err := parseStorageLimit(*raw)
		if err != nil {
			return err
		}
		limit = &number
	}
	tx, err := s.begin(ctx, false)
	if err != nil {
		return err
	}
	defer rollback(ctx, tx)
	if err = storageOrganization(ctx, tx, token, org); err != nil {
		return err
	}
	if _, err = dbgen.New(tx).
		UpsertStorageQuota(ctx, dbgen.UpsertStorageQuotaParams{Organization: org, LimitBytes: limit}); err != nil {
		return err
	}
	who, err := authorize(ctx, tx, token)
	if err != nil {
		return err
	}
	if err = audit(ctx, tx, who.ID, "storage-quota-update", org, org); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// EvaluateStorageQuotas samples a bounded due set. Quotas never gate admission.
// Each policy row alone is claimed; receipt/usage rows are read by one MVCC SUM.
func (s *Service) EvaluateStorageQuotas(ctx context.Context, limit int) (int, error) {
	if limit < 1 || limit > 128 {
		return 0, ErrInvalid
	}
	processed := 0
	for processed < limit {
		sampled, err := s.evaluateNextStorageQuota(ctx)
		if err != nil {
			return processed, err
		}
		if !sampled {
			break
		}
		processed++
	}
	return processed, nil
}

func (s *Service) evaluateNextStorageQuota(ctx context.Context) (bool, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer rollback(ctx, tx)
	if _, err = dbgen.New(tx).RequireQuotaSynchronousCommit(ctx); err != nil {
		return false, err
	}
	row, err := dbgen.New(tx).ClaimNextStorageQuota(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err = sampleStorageQuota(ctx, tx, row); err != nil {
		return false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return false, err
	}
	return true, nil
}

func sampleStorageQuota(ctx context.Context, tx pgx.Tx, row dbgen.ClaimNextStorageQuotaRow) error {
	id, err := mailID(row.Organization)
	if err != nil {
		return err
	}
	usage, err := receiptstore.ReadOrganizationStorageUsage(ctx, tx, id)
	if err != nil {
		return err
	}
	exceeded := row.LimitBytes != nil && usage.RetainedBytes > *row.LimitBytes
	if exceeded != row.Active {
		state := "resolved"
		if exceeded {
			state = "active"
		}
		_, err = dbgen.New(tx).
			CreateStorageAlert(
				ctx,
				dbgen.CreateStorageAlertParams{
					Organization:  row.Organization,
					PolicyVersion: row.Version,
					State:         state,
					RetainedBytes: usage.RetainedBytes,
					LimitBytes:    row.LimitBytes,
				},
			)
		if err != nil {
			return err
		}
	}
	_, err = dbgen.New(tx).
		RecordStorageQuotaSample(
			ctx,
			dbgen.RecordStorageQuotaSampleParams{
				Organization: row.Organization,
				SampledBytes: &usage.RetainedBytes,
				SampledCount: &usage.RetainedOriginals,
				Active:       exceeded,
			},
		)
	return err
}

// CheckStorageQuotaSchema requires the fresh advisory schema; it never backfills
// policy/accounting or guesses a migration for an older installation.
func (s *Service) CheckStorageQuotaSchema(ctx context.Context) error {
	if _, err := dbgen.New(s.db).CheckStorageQuotaPolicySchema(ctx); err != nil {
		return fmt.Errorf("admin: initialized advisory storage schema required: %w", err)
	}
	if _, err := dbgen.New(s.db).CheckStorageQuotaAlertSchema(ctx); err != nil {
		return fmt.Errorf("admin: initialized advisory storage schema required: %w", err)
	}
	index, err := dbgen.New(s.db).StorageQuotaDueIndexExists(ctx)
	if err != nil {
		return err
	}
	if !index {
		return fmt.Errorf("admin: advisory storage due index required")
	}
	return nil
}

func storagePolicy(ctx context.Context, tx pgx.Tx, org string) (dbgen.GetStorageQuotaRow, error) {
	policy, err := dbgen.New(tx).GetStorageQuota(ctx, org)
	if errors.Is(err, pgx.ErrNoRows) {
		return policy, nil
	}
	return policy, err
}

func applyStoragePolicy(result *StorageUsage, policy dbgen.GetStorageQuotaRow) {
	result.PolicyVersion = strconv.FormatInt(policy.Version, 10)
	result.LimitBytes = decimalPointer(policy.LimitBytes)
	result.SampledBytes = decimalPointer(policy.SampledBytes)
	result.SampledOriginals = decimalPointer(policy.SampledCount)
	result.SampledPolicyVersion = decimalPointer(policy.SampledVersion)
	result.SampledAt = policy.SampledAt
	result.AlertActive = policy.Active
	result.SampleStale = policy.SampledAt == nil || policy.SampledVersion == nil ||
		*policy.SampledVersion != policy.Version ||
		result.MeasuredAt.Sub(*policy.SampledAt) > 60*time.Second
}

func readStorageTransition(ctx context.Context, tx pgx.Tx, org string, result *StorageUsage) error {
	transition, err := dbgen.New(tx).GetLatestStorageAlert(ctx, org)
	if err == nil {
		result.LastTransitionAt = &transition.OccurredAt
		result.LastTransitionState = &transition.State
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	return err
}
