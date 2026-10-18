//go:build linux

package receiptstore

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/cotocisternas/fry/internal/hotstore"
	"github.com/cotocisternas/fry/internal/receiptstore/dbgen"

	"github.com/jackc/pgx/v5"
)

// MigrationFolder is a finite inventory captured from a read-only source SELECT.
// UIDNext is an arrival watermark, not evidence that the source is frozen.
type MigrationFolder struct {
	Source, Destination string
	UIDValidity         uint32
	UIDNext             uint64
	Subscribed          bool
}

// MigrationFreeze records an operator's external write/admission barrier. The
// source reader cannot establish this barrier by observing unchanged UIDs.
// Caller authentication/authorization is supplied by the administrative adapter.
type MigrationFreeze struct {
	Actor          string    `json:"actor"`
	Reason         string    `json:"reason"`
	Barrier        string    `json:"barrier"`
	ArrivalHandoff string    `json:"arrival_handoff"`
	StartedAt      time.Time `json:"started_at"`
}

type MigrationPass struct {
	store                    *Store
	Organization, Job, Inbox ID
	Generation               int64
	Final                    bool
	Deadline                 time.Time
}
type MigrationWindow struct {
	Source, Destination string
	UIDValidity         uint32
	From, Through       uint32
}
type MigrationMessage struct {
	UID          uint32
	Flags        []string
	InternalDate time.Time
	Size         int64
}
type MigrationCopy struct {
	Attempt        ID
	Envelope       Envelope
	AlreadyPresent bool
}

// CreateMigration reserves a NEW inbox. It cannot overwrite or merge an active
// mailbox. Job retries require the identical source fingerprint and destination.
func (s *Store) CreateMigration(ctx context.Context, org, job, inbox ID, fingerprint [32]byte) error {
	if !validMigrationIdentity(org, job, inbox, fingerprint) {
		return ErrConflict
	}
	tx, err := s.db.begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(ctx, tx)
	existing, err := dbgen.New(tx).StoreCreateMigrationReadMigration(ctx, job.String())
	if err == nil {
		if existing.Organization != org.String() || existing.Inbox != inbox.String() ||
			string(existing.Fingerprint) != string(fingerprint[:]) {
			return ErrConflict
		}
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if err = reserveMigrationInbox(ctx, tx, org, inbox); err != nil {
		return err
	}
	if err = recordMigrationReservation(ctx, tx, org, job, inbox, fingerprint); err != nil {
		return err
	}
	return commit(ctx, tx)
}

func reserveMigrationInbox(ctx context.Context, tx pgx.Tx, org, inbox ID) error {
	if err := lockInboxAdmission(ctx, tx, inbox); err != nil {
		return err
	}
	used, err := dbgen.New(tx).StoreCreateMigrationCheckDestination(ctx, inbox.String())
	if err != nil {
		return err
	}
	if used == nil {
		return fmt.Errorf("receiptstore: unexpected NULL in StoreCreateMigrationCheckDestination.column_1")
	}
	if *used {
		return ErrConflict
	}
	tag, err := dbgen.New(tx).
		StoreCreateMigrationInsertInbox(ctx, dbgen.StoreCreateMigrationInsertInboxParams{
			ID:           inbox.String(),
			Organization: org.String(),
		})
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrConflict
	}
	return EnsureInboxInTransaction(ctx, tx, org, inbox)
}

func normalizeMigrationInventory(in []MigrationFolder) ([]MigrationFolder, []byte, error) {
	if len(in) < 1 || len(in) > 4096 {
		return nil, nil, ErrConflict
	}
	out := append([]MigrationFolder(nil), in...)
	sources, destinations := map[string]bool{}, map[string]bool{}
	hasInbox := false
	for i := range out {
		f := &out[i]
		if err := normalizeMigrationFolder(f, sources, destinations); err != nil {
			return nil, nil, err
		}
		sources[f.Source], destinations[f.Destination] = true, true
		if f.Destination == "INBOX" {
			hasInbox = true
		}
	}
	if !hasInbox {
		return nil, nil, fmt.Errorf("receiptstore: migration requires an INBOX destination")
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Source < out[j].Source })
	data, err := json.Marshal(out)
	return out, data, err
}

func normalizeMigrationFolder(f *MigrationFolder, sources, destinations map[string]bool) error {
	var err error
	f.Source, err = folderName(f.Source)
	if err != nil {
		return err
	}
	f.Destination, err = folderName(f.Destination)
	if err != nil {
		return err
	}
	if sources[f.Source] || destinations[f.Destination] || f.UIDValidity == 0 || f.UIDNext < 1 ||
		f.UIDNext > 4294967296 {
		return ErrConflict
	}
	return nil
}

// BeginMigrationPass fences every prior pass, retaining accepted copies but
// rescanning metadata. Interrupted passes can ResumeMigration instead. A new final
// pass requires an explicit recent barrier and has a hard 30-minute deadline.
func (s *Store) BeginMigrationPass(
	ctx context.Context,
	org, job ID,
	inventory []MigrationFolder,
	freeze *MigrationFreeze,
) (*MigrationPass, error) {
	inventory, data, err := normalizeMigrationInventory(inventory)
	if err != nil {
		return nil, err
	}
	tx, err := s.db.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer rollback(ctx, tx)
	p, now, err := s.nextMigrationPass(ctx, tx, org, job, freeze != nil)
	if err != nil {
		return nil, err
	}
	phase, attestation, deadline, err := p.freezeMetadata(freeze, now)
	if err != nil {
		return nil, err
	}
	if err = replaceMigrationFolders(ctx, tx, job, p.Inbox.String(), inventory); err != nil {
		return nil, err
	}
	if err = p.recordMigrationPass(ctx, tx, phase, attestation, deadline, data, inventory, freeze); err != nil {
		return nil, err
	}
	if err = p.guard(ctx, tx, false); err != nil {
		return nil, err
	}
	return p, commit(ctx, tx)
}

func (p *MigrationPass) freezeMetadata(freeze *MigrationFreeze, now time.Time) (string, []byte, *time.Time, error) {
	if freeze == nil {
		return "copying", nil, nil, nil
	}
	if !freeze.valid(now) {
		return "", nil, nil, ErrConflict
	}
	p.Deadline = freeze.StartedAt.Add(30 * time.Minute)
	attestation, err := json.Marshal(freeze)
	return "final", attestation, new(p.Deadline), err
}

func (f *MigrationFreeze) valid(now time.Time) bool {
	return validMigrationText(f.Actor, 256) && validMigrationText(f.Reason, 4096) &&
		validMigrationText(f.Barrier, 4096) &&
		validMigrationText(f.ArrivalHandoff, 4096) &&
		!f.StartedAt.IsZero() &&
		!f.StartedAt.After(now) &&
		f.StartedAt.Add(30*time.Minute).After(now)
}
func validMigrationText(value string, limit int) bool { return value != "" && len(value) <= limit }

func replaceMigrationFolders(ctx context.Context, tx pgx.Tx, job ID, inbox string, inventory []MigrationFolder) error {
	var err error
	if _, err = dbgen.New(tx).StoreBeginMigrationPassDeleteMigrationFolder(ctx, job.String()); err != nil {
		return err
	}
	for _, f := range inventory {
		if err = createMigrationFolder(ctx, tx, job, inbox, f); err != nil {
			return err
		}
	}
	return nil
}

func createMigrationFolder(ctx context.Context, tx pgx.Tx, job ID, inbox string, f MigrationFolder) error {
	var err error
	id, e := hotstore.NewID()
	if e != nil {
		return e
	}
	if _, err = dbgen.New(tx).
		StoreBeginMigrationPassInsertFolder(ctx, dbgen.StoreBeginMigrationPassInsertFolderParams{
			ID:    id.String(),
			Inbox: inbox,
			Name:  f.Destination,
		}); err != nil {
		return err
	}
	dest, err := dbgen.New(tx).
		StoreBeginMigrationPassReadFolder(ctx, dbgen.StoreBeginMigrationPassReadFolderParams{
			Inbox: inbox,
			Name:  f.Destination,
		})
	if err != nil {
		return err
	}
	did, e := idFrom(dest)
	if e != nil {
		return e
	}
	if err = recordNewFolderName(ctx, tx, did); err != nil {
		return err
	}
	if f.UIDValidity < 1 || f.UIDNext < 1 || f.UIDNext > 1<<32 {
		return ErrConflict
	}
	if _, err = dbgen.New(tx).
		StoreBeginMigrationPassInsertMigrationFolder(ctx, dbgen.StoreBeginMigrationPassInsertMigrationFolderParams{
			Job:         job.String(),
			SourceName:  f.Source,
			Destination: f.Destination,
			Folder:      dest,
			Uidvalidity: int64(f.UIDValidity),
			UpperUid:    int64(f.UIDNext - 1),
			Subscribed:  f.Subscribed,
		}); err != nil {
		return err
	}
	return nil
}

func (s *Store) ResumeMigration(ctx context.Context, org, job ID) (*MigrationPass, error) {
	p := &MigrationPass{store: s, Organization: org, Job: job}
	var inbox, phase string
	var deadline *time.Time
	err := func() error {
		sqlcResult, sqlcErr := dbgen.New(s.db.pool).
			StoreResumeMigration(ctx, dbgen.StoreResumeMigrationParams{ID: job.String(), Organization: org.String()})
		if sqlcErr != nil {
			return sqlcErr
		}
		inbox = sqlcResult.Inbox
		p.Generation = sqlcResult.Generation
		phase = sqlcResult.Phase
		deadline = sqlcResult.Deadline
		return nil
	}()
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if p.Generation == 0 || phase == "complete" {
		return nil, ErrStale
	}
	p.Inbox, err = idFrom(inbox)
	p.Final = phase == "final" || phase == "ready"
	if deadline != nil {
		p.Deadline = *deadline
	}
	return p, err
}

func (p *MigrationPass) guard(ctx context.Context, tx pgx.Tx, allowReady bool) error {
	row, err := dbgen.New(tx).
		MigrationPassGuard(ctx, dbgen.MigrationPassGuardParams{
			ID:           p.Job.String(),
			Organization: p.Organization.String(),
			Inbox:        p.Inbox.String(),
		})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if row.Column3 == nil {
		return fmt.Errorf("receiptstore: unexpected NULL in MigrationPassGuard.Column3")
	}
	if row.Generation != p.Generation || !*row.Column3 || !validMigrationPhase(row.Phase, allowReady) {
		return ErrStale
	}
	return nil
}

func validMigrationPhase(phase string, allowReady bool) bool {
	return phase == "copying" || phase == "final" || (allowReady && phase == "ready")
}

// NextWindow bounds honest-server UID SEARCH cardinality; it never uses '*'.
//
//nolint:nilnil // A nil window with no error marks the successful end of this migration pass.
func (p *MigrationPass) NextWindow(ctx context.Context, width uint32) (*MigrationWindow, error) {
	if width < 1 || width > 1024 {
		return nil, ErrConflict
	}
	tx, err := p.store.db.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer rollback(ctx, tx)
	if err = p.guard(ctx, tx, false); err != nil {
		return nil, err
	}
	row, err := dbgen.New(tx).MigrationPassNextWindow(ctx, p.Job.String())
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return decodeMigrationWindow(row, width)
}

func decodeMigrationWindow(row dbgen.MigrationPassNextWindowRow, width uint32) (*MigrationWindow, error) {
	w := &MigrationWindow{Source: row.SourceName, Destination: row.Destination}
	var err error
	w.UIDValidity, err = sqlcUint32(row.Uidvalidity)
	if err != nil {
		return nil, err
	}
	next, err := sqlcUint64(row.NextUid)
	if err != nil {
		return nil, err
	}
	upper, err := sqlcUint64(row.UpperUid)
	if err != nil {
		return nil, err
	}
	through := min(upper, next+uint64(width)-1)
	if next > 1<<32-1 || through > 1<<32-1 {
		return nil, ErrConflict
	}
	w.From = uint32(next)
	w.Through = uint32(through)
	return w, nil
}

// AdvanceWindow commits only after all retained observed UIDs have durable mail.
// The source adapter must distinguish verified disappearance from command failure.
func (p *MigrationPass) AdvanceWindow(ctx context.Context, w MigrationWindow, present []uint32) error {
	if !validMigrationPresence(w, present) {
		return ErrConflict
	}
	tx, err := p.store.db.begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(ctx, tx)
	if err = p.guard(ctx, tx, false); err != nil {
		return err
	}
	if err = p.requireMigrationCopies(ctx, tx, w, present); err != nil {
		return err
	}
	if err = p.reconcileMigrationPresence(ctx, tx, w, present); err != nil {
		return err
	}
	tag, err := dbgen.New(tx).
		MigrationPassAdvanceWindowUpdateMigrationFolder(
			ctx,
			dbgen.MigrationPassAdvanceWindowUpdateMigrationFolderParams{
				Job:         p.Job.String(),
				SourceName:  w.Source,
				Uidvalidity: int64(w.UIDValidity),
				NextUid:     int64(w.From),
				NextUid_2:   int64(w.Through) + 1,
				UpperUid:    int64(w.Through),
			},
		)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return ErrStale
	}
	if err = p.guard(ctx, tx, false); err != nil {
		return err
	}
	return commit(ctx, tx)
}

// Observe records exact flags and immutable source metadata. A returned attempt
// is stable across retry; accepting it commits placement and its ledger together.
func (p *MigrationPass) Observe(ctx context.Context, w MigrationWindow, m MigrationMessage) (MigrationCopy, error) {
	var result MigrationCopy
	flags, err := normalizeMigrationMessage(w, &m)
	if err != nil {
		return result, err
	}
	// Preserve receipt-before-job lock ordering across the external lookup.
	receipt, err := dbgen.New(p.store.db.pool).
		MigrationPassObserveReadMigrationEntry(ctx, dbgen.MigrationPassObserveReadMigrationEntryParams{
			Job:         p.Job.String(),
			SourceName:  w.Source,
			Uidvalidity: int64(w.UIDValidity),
			Uid:         int64(m.UID),
		})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return result, err
	}
	tx, err := p.store.db.begin(ctx)
	if err != nil {
		return result, err
	}
	defer rollback(ctx, tx)
	if err = lockMigrationReceipt(ctx, tx, p.Organization, receipt); err != nil {
		return result, err
	}
	if err = p.guard(ctx, tx, false); err != nil {
		return result, err
	}
	result, err = p.observeMigrationEntry(ctx, tx, w, m, flags, receipt)
	if err != nil {
		return result, err
	}
	if err = p.guard(ctx, tx, false); err != nil {
		return result, err
	}
	return result, commit(ctx, tx)
}

func normalizeMigrationMessage(w MigrationWindow, m *MigrationMessage) ([]string, error) {
	if m.UID < w.From || m.UID > w.Through || m.InternalDate.IsZero() || m.Size < 0 || m.Size > 256<<20 {
		return nil, ErrConflict
	}
	flags, err := canonicalFlags(m.Flags)
	if err != nil {
		return nil, err
	}
	m.InternalDate = m.InternalDate.UTC().Truncate(time.Microsecond)
	return flags, nil
}

func lockMigrationReceipt(ctx context.Context, tx pgx.Tx, org ID, receipt *string) error {
	if receipt == nil {
		return nil
	}
	rid, err := idFrom(*receipt)
	if err != nil {
		return err
	}
	_, err = lockReceipt(ctx, tx, org, rid)
	return err
}

func (p *MigrationPass) observeMigrationEntry(
	ctx context.Context,
	tx pgx.Tx,
	w MigrationWindow,
	m MigrationMessage,
	flags []string,
	receipt *string,
) (MigrationCopy, error) {
	var result MigrationCopy
	folder, err := p.observedMigrationFolder(ctx, tx, w)
	if err != nil {
		return result, err
	}
	entry, err := p.upsertMigrationObservation(ctx, tx, w, m, flags)
	if err != nil {
		return result, err
	}
	if entry.Mail != nil {
		if err = updateObservedMigrationMail(ctx, tx, entry.Mail, receipt, folder, flags); err != nil {
			return result, err
		}
		result.AlreadyPresent = true
	} else {
		result, err = p.prepareObservedMigrationCopy(ctx, tx, entry.ID, folder, entry.Attempt, m, flags)
		if err != nil {
			return result, err
		}
	}
	if _, err = dbgen.New(tx).
		MigrationPassObserveUpdateMigrationEntry2(ctx, dbgen.MigrationPassObserveUpdateMigrationEntry2Params{
			ID:             entry.ID,
			SeenGeneration: p.Generation,
			Flags:          flags,
		}); err != nil {
		return result, err
	}
	return result, nil
}

func (p *MigrationPass) observedMigrationFolder(ctx context.Context, tx pgx.Tx, w MigrationWindow) (string, error) {
	var folder string
	var next, upper uint64
	err := func() error {
		sqlcResult, sqlcErr := dbgen.New(tx).
			MigrationPassObserveReadMigrationFolder(ctx, dbgen.MigrationPassObserveReadMigrationFolderParams{
				Job:         p.Job.String(),
				SourceName:  w.Source,
				Uidvalidity: int64(w.UIDValidity),
				Destination: w.Destination,
			})
		if sqlcErr != nil {
			return sqlcErr
		}
		folder = sqlcResult.Folder
		next, sqlcErr = sqlcUint64(sqlcResult.NextUid)
		if sqlcErr != nil {
			return sqlcErr
		}
		upper, sqlcErr = sqlcUint64(sqlcResult.UpperUid)
		if sqlcErr != nil {
			return sqlcErr
		}
		return nil
	}()
	if err != nil {
		return "", err
	}
	if uint64(w.From) != next || uint64(w.Through) > upper || w.Through < w.From {
		return "", ErrStale
	}
	return folder, nil
}

func (p *MigrationPass) upsertMigrationObservation(
	ctx context.Context,
	tx pgx.Tx,
	w MigrationWindow,
	m MigrationMessage,
	flags []string,
) (dbgen.MigrationPassObserveReadMigrationEntryIdMailAttemptInternalDateRawSizeRow, error) {
	freshID, err := hotstore.NewID()
	if err != nil {
		return dbgen.MigrationPassObserveReadMigrationEntryIdMailAttemptInternalDateRawSizeRow{}, err
	}
	if _, err = dbgen.New(tx).
		MigrationPassObserveInsertMigrationEntry(ctx, dbgen.MigrationPassObserveInsertMigrationEntryParams{
			ID:             freshID.String(),
			Job:            p.Job.String(),
			SourceName:     w.Source,
			Uidvalidity:    int64(w.UIDValidity),
			Uid:            int64(m.UID),
			SeenGeneration: p.Generation,
			Flags:          flags,
			InternalDate:   m.InternalDate,
			RawSize:        m.Size,
		}); err != nil {
		return dbgen.MigrationPassObserveReadMigrationEntryIdMailAttemptInternalDateRawSizeRow{}, err
	}
	entry, err := dbgen.New(tx).
		MigrationPassObserveReadMigrationEntryIdMailAttemptInternalDateRawSize(
			ctx,
			dbgen.MigrationPassObserveReadMigrationEntryIdMailAttemptInternalDateRawSizeParams{
				Job:         p.Job.String(),
				SourceName:  w.Source,
				Uidvalidity: int64(w.UIDValidity),
				Uid:         int64(m.UID),
			},
		)
	if err != nil {
		return entry, err
	}
	if !entry.InternalDate.Equal(m.InternalDate) || entry.RawSize != m.Size {
		return entry, ErrConflict
	}
	return entry, nil
}

func updateObservedMigrationMail(
	ctx context.Context,
	tx pgx.Tx,
	mail, receipt *string,
	folder string,
	flags []string,
) error {
	actual, err := dbgen.New(tx).MigrationPassObserveReadMail(ctx, *mail)
	if err != nil {
		return err
	}
	if receipt == nil || actual.Receipt != *receipt {
		return ErrBusy
	}
	if actual.Folder != folder {
		return ErrConflict
	}
	if _, err = dbgen.New(tx).MigrationPassObserveReadFolder(ctx, folder); err != nil {
		return err
	}
	fid, e := idFrom(folder)
	if e != nil {
		return e
	}
	if err = registerKeywords(ctx, tx, fid, flags); err != nil {
		return err
	}
	if _, err = dbgen.New(tx).
		MigrationPassObserveUpdateMail(ctx, dbgen.MigrationPassObserveUpdateMailParams{
			ID:    *mail,
			Flags: flags,
		}); err != nil {
		return err
	}
	return nil
}

func (p *MigrationPass) prepareObservedMigrationCopy(
	ctx context.Context,
	tx pgx.Tx,
	entry, folder string,
	attempt *string,
	m MigrationMessage,
	flags []string,
) (MigrationCopy, error) {
	var result MigrationCopy
	var err error
	var data []byte
	if attempt != nil {
		err = func() error {
			sqlcResult, sqlcErr := dbgen.New(tx).
				MigrationPassObserveReadMigrationCopy(ctx, dbgen.MigrationPassObserveReadMigrationCopyParams{
					Attempt:    *attempt,
					Generation: p.Generation,
				})
			if sqlcErr != nil {
				return sqlcErr
			}
			data = sqlcResult
			return nil
		}()
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return result, err
		}
	}
	if len(data) > 0 {
		result.Attempt, err = idFrom(*attempt)
		if err != nil {
			return result, err
		}
		if err = json.Unmarshal(data, &result.Envelope); err != nil {
			return result, err
		}
	} else {
		return p.newObservedMigrationCopy(ctx, tx, entry, folder, m, flags)
	}
	return result, nil
}

// guardMigrationImport is also the normal-ingress exclusion check. It runs in
// both preparation and acceptance, so delayed uploads cannot cross handoff.
func guardMigrationImport(ctx context.Context, tx pgx.Tx, attempt ID, e Envelope) error {
	if e.Migration == nil {
		return guardOrdinaryMigrationAdmission(ctx, tx, e.Destinations)
	}
	m := e.Migration
	d := e.Destinations[0]
	p := &MigrationPass{Organization: e.Organization, Job: m.Job, Inbox: d.Inbox, Generation: m.Generation}
	if err := p.guard(ctx, tx, false); err != nil {
		return err
	}
	_, data, err := normalize(e)
	if err != nil {
		return err
	}
	var valid bool
	err = func() error {
		sqlcResult, sqlcErr := dbgen.New(tx).
			GuardMigrationImportCheckMigrationCopy(ctx, dbgen.GuardMigrationImportCheckMigrationCopyParams{
				Attempt:    attempt.String(),
				Job:        m.Job.String(),
				Entry:      m.Entry.String(),
				Generation: m.Generation,
				Column5:    data,
				ID:         d.Inbox.String(),
			})
		if sqlcErr != nil {
			return sqlcErr
		}
		valid = sqlcResult
		return nil
	}()
	if err != nil {
		return err
	}
	if !valid {
		return ErrStale
	}
	return nil
}

func finishMigrationImport(ctx context.Context, tx pgx.Tx, c claim) error {
	if c.Envelope.Migration == nil {
		return nil
	}
	m := c.Envelope.Migration
	var expected int64
	var flags []string
	if err := func() error {
		sqlcResult, sqlcErr := dbgen.New(tx).FinishMigrationImportReadMigrationEntry(ctx, m.Entry.String())
		if sqlcErr != nil {
			return sqlcErr
		}
		expected = sqlcResult.RawSize
		flags = sqlcResult.Flags
		return nil
	}(); err != nil {
		return err
	}
	var size int64
	var mail string
	if err := func() error {
		sqlcResult, sqlcErr := dbgen.New(tx).FinishMigrationImportReadReceipt(ctx, c.Receipt.String())
		if sqlcErr != nil {
			return sqlcErr
		}
		size = sqlcResult.RawSize
		mail = sqlcResult.ID
		return nil
	}(); err != nil {
		return err
	}
	if size != expected {
		return ErrConflict
	}
	if err := registerKeywords(ctx, tx, c.Envelope.Destinations[0].Folder, flags); err != nil {
		return err
	}
	if _, err := dbgen.New(tx).
		FinishMigrationImportUpdateMail(ctx, dbgen.FinishMigrationImportUpdateMailParams{
			ID:    mail,
			Flags: flags,
		}); err != nil {
		return err
	}
	if _, err := dbgen.New(tx).
		FinishMigrationImportUpdateMigrationEntry(ctx, dbgen.FinishMigrationImportUpdateMigrationEntryParams{
			ID:   m.Entry.String(),
			Mail: new(mail),
		}); err != nil {
		return err
	}
	_, err := dbgen.New(tx).
		FinishMigrationImportUpdateMigrationCopy(ctx, dbgen.FinishMigrationImportUpdateMigrationCopyParams{
			Attempt: c.Attempt.String(),
			Receipt: new(c.Receipt.String()),
		})
	return err
}

// Inventory returns the persisted source scope for resumptions and final checks.
func (p *MigrationPass) Inventory(ctx context.Context) ([]MigrationFolder, error) {
	var data []byte
	err := func() error {
		sqlcResult, sqlcErr := dbgen.New(p.store.db.pool).
			MigrationPassInventory(ctx, dbgen.MigrationPassInventoryParams{
				ID:           p.Job.String(),
				Organization: p.Organization.String(),
				Generation:   p.Generation,
			})
		if sqlcErr != nil {
			return sqlcErr
		}
		data = sqlcResult
		return nil
	}()
	if err != nil {
		return nil, err
	}
	var result []MigrationFolder
	err = json.Unmarshal(data, &result)
	return result, err
}

// FingerprintMigrationSource binds a job to an account, excluding rotatable secrets.
func FingerprintMigrationSource(address, username string, plaintext bool) [32]byte {
	//nolint:errcheck // This fixed struct contains only JSON strings and a boolean; encoding cannot fail.
	data, _ := json.Marshal(struct {
		Address, Username string
		Plaintext         bool
	}{address, username, plaintext})
	return sha256.Sum256(data)
}

// A missing inbox row cannot be locked. Migration reservation exclusively holds
// this key through its used-destination check and reservation commit.
func lockInboxAdmission(ctx context.Context, tx pgx.Tx, inbox ID) error {
	_, err := dbgen.New(tx).LockInboxAdmission(ctx, new(inbox.String()))
	return err
}

// Ordinary admission shares the reservation key with other normal receipts,
// while still excluding migration reservation even before an inbox row exists.
// Preparation and finalization both recheck migration_job under the row guard;
// normalized destination order remains the acquisition order for multiple keys.
func lockOrdinaryInboxAdmission(ctx context.Context, tx pgx.Tx, inbox ID) error {
	_, err := dbgen.New(tx).LockOrdinaryInboxAdmission(ctx, new(inbox.String()))
	return err
}

func checkMigrationPublication(ctx context.Context, tx pgx.Tx, e Envelope) error {
	if e.Migration == nil {
		return nil
	}
	p := &MigrationPass{
		Organization: e.Organization,
		Job:          e.Migration.Job,
		Inbox:        e.Destinations[0].Inbox,
		Generation:   e.Migration.Generation,
	}
	return p.guard(ctx, tx, false)
}

func validMigrationIdentity(org, job, inbox ID, fingerprint [32]byte) bool {
	return org != (ID{}) && job != (ID{}) && inbox != (ID{}) && fingerprint != ([32]byte{})
}

func recordMigrationReservation(ctx context.Context, tx pgx.Tx, org, job, inbox ID, fingerprint [32]byte) error {
	if _, err := dbgen.New(tx).
		StoreCreateMigrationInsertMigration(ctx, dbgen.StoreCreateMigrationInsertMigrationParams{
			ID:           job.String(),
			Organization: org.String(),
			Inbox:        inbox.String(),
			Fingerprint:  fingerprint[:],
		}); err != nil {
		return err
	}
	if _, err := dbgen.New(tx).
		StoreCreateMigrationUpdateInbox(ctx, dbgen.StoreCreateMigrationUpdateInboxParams{
			ID:           inbox.String(),
			MigrationJob: new(job.String()),
		}); err != nil {
		return err
	}
	if _, err := dbgen.New(tx).
		StoreCreateMigrationInsertMigrationEvent(ctx, dbgen.StoreCreateMigrationInsertMigrationEventParams{
			Job:    job.String(),
			Detail: []byte(fmt.Sprintf(`{"inbox":%q}`, inbox.String())),
		}); err != nil {
		return err
	}
	return nil
}

func (s *Store) nextMigrationPass(
	ctx context.Context,
	tx pgx.Tx,
	org, job ID,
	final bool,
) (*MigrationPass, time.Time, error) {
	current, err := dbgen.New(tx).
		StoreBeginMigrationPassReadMigration(ctx, dbgen.StoreBeginMigrationPassReadMigrationParams{
			ID:           job.String(),
			Organization: org.String(),
		})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, time.Time{}, ErrNotFound
	}
	if err != nil {
		return nil, time.Time{}, err
	}
	if current.Phase == "complete" {
		return nil, time.Time{}, ErrStale
	}
	p := &MigrationPass{store: s, Organization: org, Job: job, Generation: current.Generation + 1, Final: final}
	p.Inbox, err = idFrom(current.Inbox)
	return p, current.Column4, err
}

func (p *MigrationPass) recordMigrationPass(
	ctx context.Context,
	tx pgx.Tx,
	phase string,
	attestation []byte,
	deadline *time.Time,
	data []byte,
	inventory []MigrationFolder,
	freeze *MigrationFreeze,
) error {
	if _, err := dbgen.New(tx).
		StoreBeginMigrationPassUpdateMigration(ctx, dbgen.StoreBeginMigrationPassUpdateMigrationParams{
			ID:                p.Job.String(),
			Generation:        p.Generation,
			Phase:             phase,
			FreezeAttestation: attestation,
			Deadline:          deadline,
			Inventory:         data,
		}); err != nil {
		return err
	}
	detail, err := json.Marshal(struct {
		Inventory []MigrationFolder
		Freeze    *MigrationFreeze
	}{inventory, freeze})
	if err != nil {
		return err
	}
	if _, err := dbgen.New(tx).
		StoreBeginMigrationPassInsertMigrationEvent(ctx, dbgen.StoreBeginMigrationPassInsertMigrationEventParams{
			Job:        p.Job.String(),
			Generation: p.Generation,
			Detail:     detail,
		}); err != nil {
		return err
	}
	return nil
}

func validMigrationPresence(w MigrationWindow, present []uint32) bool {
	return w.From != 0 && w.Through >= w.From && uint64(w.Through)-uint64(w.From) < 1024 && len(present) <= 1024
}

func (p *MigrationPass) requireMigrationCopies(
	ctx context.Context,
	tx pgx.Tx,
	w MigrationWindow,
	present []uint32,
) error {
	seen := map[uint32]bool{}
	for _, uid := range present {
		if uid < w.From || uid > w.Through || seen[uid] {
			return ErrConflict
		}
		seen[uid] = true
		var copied bool
		if err := func() error {
			sqlcResult, sqlcErr := dbgen.New(tx).
				MigrationPassAdvanceWindowCheckMigrationEntry(
					ctx,
					dbgen.MigrationPassAdvanceWindowCheckMigrationEntryParams{
						Job:            p.Job.String(),
						SourceName:     w.Source,
						Uidvalidity:    int64(w.UIDValidity),
						Uid:            int64(uid),
						SeenGeneration: p.Generation,
					},
				)
			if sqlcErr != nil {
				return sqlcErr
			}
			copied = sqlcResult
			return nil
		}(); err != nil {
			return err
		}
		if !copied {
			return ErrBusy
		}
	}
	return nil
}

func (p *MigrationPass) reconcileMigrationPresence(
	ctx context.Context,
	tx pgx.Tx,
	w MigrationWindow,
	present []uint32,
) error {
	var err error
	// A retried window is a fresh complete presence observation. Entries copied
	// before its previous interrupted run must not survive a now-absent UID.
	numbers := make([]int64, len(present))
	for i, uid := range present {
		numbers[i] = int64(uid)
	}
	var disappeared bool
	var phase string
	if err = func() error {
		sqlcResult, sqlcErr := dbgen.New(tx).MigrationPassAdvanceWindowReadMigration(ctx, p.Job.String())
		if sqlcErr != nil {
			return sqlcErr
		}
		phase = sqlcResult
		return nil
	}(); err != nil {
		return err
	}
	if err = func() error {
		sqlcResult, sqlcErr := dbgen.New(tx).
			MigrationPassAdvanceWindowCheckMigrationEntryEXISTSSELECT1(
				ctx,
				dbgen.MigrationPassAdvanceWindowCheckMigrationEntryEXISTSSELECT1Params{
					Job:            p.Job.String(),
					SourceName:     w.Source,
					Uidvalidity:    int64(w.UIDValidity),
					SeenGeneration: p.Generation,
					Uid:            int64(w.From),
					Uid_2:          int64(w.Through),
					Column7:        numbers,
				},
			)
		if sqlcErr != nil {
			return sqlcErr
		}
		disappeared = sqlcResult
		return nil
	}(); err != nil {
		return err
	}
	if disappeared && phase == "final" {
		return ErrConflict
	}
	if _, err = dbgen.New(tx).
		MigrationPassAdvanceWindowUpdateMigrationEntry(ctx, dbgen.MigrationPassAdvanceWindowUpdateMigrationEntryParams{
			Job:            p.Job.String(),
			SourceName:     w.Source,
			Uidvalidity:    int64(w.UIDValidity),
			SeenGeneration: p.Generation,
			Uid:            int64(w.From),
			Uid_2:          int64(w.Through),
			Column7:        numbers,
		}); err != nil {
		return err
	}
	return nil
}

func (p *MigrationPass) newObservedMigrationCopy(
	ctx context.Context,
	tx pgx.Tx,
	entry, folder string,
	m MigrationMessage,
	flags []string,
) (MigrationCopy, error) {
	var result MigrationCopy
	var data []byte
	var err error
	result.Attempt, err = hotstore.NewID()
	if err != nil {
		return result, err
	}
	fid, e := idFrom(folder)
	if e != nil {
		return result, e
	}
	eid, e := idFrom(entry)
	if e != nil {
		return result, e
	}
	env := Envelope{
		MailboxAppend: true,
		Organization:  p.Organization,
		Migration:     &MigrationImport{Job: p.Job, Entry: eid, Generation: p.Generation},
		Destinations:  []Destination{{Inbox: p.Inbox, Folder: fid, Flags: flags, InternalDate: m.InternalDate}},
	}
	result.Envelope, data, err = normalize(env)
	if err != nil {
		return result, err
	}
	if _, err = dbgen.New(tx).
		MigrationPassObserveInsertMigrationCopy(ctx, dbgen.MigrationPassObserveInsertMigrationCopyParams{
			Attempt:    result.Attempt.String(),
			Job:        p.Job.String(),
			Entry:      entry,
			Generation: p.Generation,
			Envelope:   data,
		}); err != nil {
		return result, err
	}
	if _, err = dbgen.New(tx).
		MigrationPassObserveUpdateMigrationEntry(ctx, dbgen.MigrationPassObserveUpdateMigrationEntryParams{
			ID:      entry,
			Attempt: new(result.Attempt.String()),
		}); err != nil {
		return result, err
	}
	return result, nil
}

func guardOrdinaryMigrationAdmission(ctx context.Context, tx pgx.Tx, destinations []Destination) error {
	// Inbox rows may not exist yet for ordinary durable SMTP work.
	for _, d := range destinations {
		if err := lockOrdinaryInboxAdmission(ctx, tx, d.Inbox); err != nil {
			return err
		}
		var job *string
		err := func() error {
			sqlcResult, sqlcErr := dbgen.New(tx).GuardMigrationImportReadInbox(ctx, d.Inbox.String())
			if sqlcErr != nil {
				return sqlcErr
			}
			job = sqlcResult
			return nil
		}()
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		if job != nil {
			return ErrBusy
		}
	}
	return nil
}
