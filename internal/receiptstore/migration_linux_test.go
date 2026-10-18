//go:build linux

package receiptstore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func migrationJob(t *testing.T, f *fixture) (ID, ID) {
	t.Helper()
	job, inbox := newID(t), newID(t)
	requireReceiptNoError(
		t,
		f.s.CreateMigration(
			context.Background(),
			f.org,
			job,
			inbox,
			FingerprintMigrationSource("source:993", "user", false),
		),
	)
	return job, inbox
}

func migrationFreeze() *MigrationFreeze {
	return &MigrationFreeze{
		Actor:          "operator",
		Reason:         "planned cutover",
		Barrier:        "source writes and admission disabled",
		ArrivalHandoff: "incoming mail held in queue",
		StartedAt:      time.Now().Add(-time.Second),
	}
}

func migrationBegin(t *testing.T, f *fixture, job ID, inventory []MigrationFolder, final bool) *MigrationPass {
	t.Helper()
	var freeze *MigrationFreeze
	if final {
		freeze = migrationFreeze()
	}
	p, err := f.s.BeginMigrationPass(context.Background(), f.org, job, inventory, freeze)
	requireReceiptNoError(t, err)
	return p
}

func migrationScan(
	t *testing.T,
	f *fixture,
	p *MigrationPass,
	messages map[string]map[uint32]MigrationMessage,
	bodies map[string]string,
) map[string]Receipt {
	t.Helper()
	ctx := context.Background()
	receipts := map[string]Receipt{}
	for {
		w, err := p.NextWindow(ctx, 2)
		requireReceiptNoError(t, err)
		if w == nil {
			break
		}
		var present []uint32
		for uid := w.From; uid <= w.Through; uid++ {
			m, ok := messages[w.Source][uid]
			if !ok {
				continue
			}
			c, err := p.Observe(ctx, *w, m)
			requireReceiptNoError(t, err)
			if !c.AlreadyPresent {
				r, acceptErr := f.s.Accept(
					ctx,
					c.Attempt,
					c.Envelope,
					io.NopCloser(strings.NewReader(bodies[w.Source])),
				)
				requireReceiptConditionf(
					t,
					acceptErr == nil,
					"accept %s UID %d generation %d: %v",
					w.Source,
					uid,
					p.Generation,
					acceptErr,
				)
				receipts[w.Source] = r
			}
			present = append(present, uid)
		}
		requireReceiptNoError(t, p.AdvanceWindow(ctx, *w, present))
	}
	return receipts
}

func migrationSeal(t *testing.T, p *MigrationPass, inventory []MigrationFolder) {
	t.Helper()
	{
		_, err := p.Reconcile(context.Background(), 1024)
		requireReceiptCondition(t, err == nil, err)
	}
	{
		digest, err := p.Seal(context.Background(), inventory)
		requireReceiptCondition(t, err == nil, digest, err)
		requireReceiptCondition(t, digest != ([32]byte{}), digest, err)
	}
}

func TestMigrationStagingAndIdentity(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	job, inbox := migrationJob(t, f)
	fingerprint := FingerprintMigrationSource("source:993", "user", false)
	{
		err := f.s.CreateMigration(ctx, f.org, job, inbox, fingerprint)
		requireReceiptCondition(t, err == nil, "idempotent create", err)
	}
	for _, args := range []struct {
		org, job, inbox ID
		fp              [32]byte
	}{
		{newID(t), job, inbox, fingerprint},
		{f.org, job, newID(t), fingerprint},
		{f.org, job, inbox, FingerprintMigrationSource("other", "user", false)},
		{f.org, newID(t), inbox, fingerprint},
	} {
		{
			err := f.s.CreateMigration(ctx, args.org, args.job, args.inbox, args.fp)
			requireReceiptCondition(t, errors.Is(err, ErrConflict), "identity conflict", err)
		}
	}
	active := newID(t)
	requireReceiptNoError(t, f.s.EnsureInbox(ctx, f.org, active))
	{
		err := f.s.CreateMigration(ctx, f.org, newID(t), active, fingerprint)
		requireReceiptCondition(t, errors.Is(err, ErrConflict), "overwrote active inbox", err)
	}
	{
		_, _, err := f.s.Mailbox(ctx, f.org, inbox, "INBOX")
		requireReceiptCondition(t, err != nil, "staging mailbox visible")
	}
	{
		err := f.s.CreateFolder(ctx, f.org, inbox, "ordinary")
		requireReceiptCondition(t, err != nil, "staging folder mutation permitted")
	}
	env := f.envelope(t)
	env.Destinations[0].Inbox = inbox
	{
		_, err := f.s.Accept(ctx, newID(t), env, io.NopCloser(strings.NewReader("ordinary")))
		requireReceiptCondition(t, errors.Is(err, ErrBusy), "ordinary ingress", err)
	}
	{
		_, err := f.s.BeginMigrationPass(
			ctx,
			newID(t),
			job,
			[]MigrationFolder{{Source: "INBOX", Destination: "INBOX", UIDValidity: 1, UIDNext: 1}},
			nil,
		)
		requireReceiptCondition(t, errors.Is(err, ErrNotFound), "organization binding", err)
	}
}

func TestMigrationFinalPassReconcilesSourceChanges(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	job, inbox := migrationJob(t, f)
	date := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	message := MigrationMessage{UID: 1, Flags: []string{"\\Seen"}, InternalDate: date, Size: 4}
	first := []MigrationFolder{
		{Source: "INBOX", Destination: "INBOX", UIDValidity: 1, UIDNext: 3},
		{Source: "Old", Destination: "Old", UIDValidity: 7, UIDNext: 2},
		{Source: "Reset", Destination: "Reset", UIDValidity: 8, UIDNext: 2},
	}
	p := migrationBegin(t, f, job, first, false)
	gone := message
	gone.UID = 2
	original := migrationScan(
		t,
		f,
		p,
		map[string]map[uint32]MigrationMessage{
			"INBOX": {1: message, 2: gone},
			"Old":   {1: message},
			"Reset": {1: message},
		},
		map[string]string{"INBOX": "body", "Old": "body", "Reset": "body"},
	)
	retained := sealBackgroundMigration(t, f, job, p, first)
	final := []MigrationFolder{
		{Source: "INBOX", Destination: "INBOX", UIDValidity: 1, UIDNext: 4, Subscribed: true},
		{Source: "Renamed", Destination: "Renamed", UIDValidity: 7, UIDNext: 2},
		{Source: "Reset", Destination: "Reset", UIDValidity: 9, UIDNext: 2},
	}
	q := migrationBegin(t, f, job, final, true)
	n, err := q.Reconcile(ctx, 100)
	requireReceiptCondition(t, n == 0, "partial scan removed mail", n, err)
	requireReceiptCondition(t, errors.Is(err, ErrBusy), "partial scan removed mail", n, err)
	{
		_, err := p.NextWindow(ctx, 1)
		requireReceiptCondition(t, errors.Is(err, ErrStale), "old pass not fenced", err)
	}
	message.Flags = []string{"\\Flagged", "custom"}
	added := message
	added.UID = 3
	imported := migrationScan(
		t,
		f,
		q,
		map[string]map[uint32]MigrationMessage{
			"INBOX":   {1: message, 3: added},
			"Renamed": {1: message},
			"Reset":   {1: message},
		},
		map[string]string{"INBOX": "body", "Renamed": "body", "Reset": "body"},
	)
	migrationSeal(t, q, final)
	requireReceiptCondition(t, imported["Reset"].ID != original["Reset"].ID, "UIDVALIDITY reset reused receipt")
	requireReceiptNoError(t, q.Activate(ctx))
	{
		err := q.Activate(ctx)
		requireReceiptCondition(t, err == nil, "activation retry", err)
	}
	requireActivatedMigrationMailboxes(t, ctx, f, inbox, retained, date, q)
}

func TestMigrationLostAcceptanceReplyRecoversLedger(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	job, _ := migrationJob(t, f)
	inventory := []MigrationFolder{{Source: "INBOX", Destination: "INBOX", UIDValidity: 1, UIDNext: 2}}
	p := migrationBegin(t, f, job, inventory, false)
	w, err := p.NextWindow(ctx, 1)
	requireReceiptNoError(t, err)
	m := MigrationMessage{UID: 1, InternalDate: time.Now().UTC().Truncate(time.Microsecond), Size: 4}
	c, err := p.Observe(ctx, *w, m)
	requireReceiptNoError(t, err)
	f.s.db.afterCommit = func() error { return io.ErrUnexpectedEOF }
	{
		_, err = f.s.Accept(ctx, c.Attempt, c.Envelope, io.NopCloser(strings.NewReader("body")))
		requireReceiptCondition(t, errors.Is(err, ErrUncertain), err)
	}
	f.s.db.afterCommit = nil
	resumed, err := f.s.ResumeMigration(ctx, f.org, job)
	requireReceiptNoError(t, err)
	recovered, err := resumed.Observe(ctx, *w, m)
	requireReceiptCondition(t, err == nil, recovered, err)
	requireReceiptCondition(t, recovered.AlreadyPresent, recovered, err)
	err = resumed.AdvanceWindow(ctx, *w, []uint32{1})
	requireReceiptNoError(t, err)
	migrationSeal(t, resumed, inventory)
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT count(*) FROM fry_receipt`) == 1,
		"lost response duplicated durable mail",
	)
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT count(*) FROM fry_mail`) == 1,
		"lost response duplicated durable mail",
	)
}

func TestMigrationFinalDeadlineAndSizeMismatch(t *testing.T) {
	t.Run("expired deadline", testMigrationExpiredDeadline)
	t.Run("size mismatch", testMigrationSizeMismatch)
}

func TestMigrationSupersededAttemptCannotFinalizeAndIsReaped(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	job, _ := migrationJob(t, f)
	inventory := []MigrationFolder{{Source: "INBOX", Destination: "INBOX", UIDValidity: 1, UIDNext: 2}}
	p := migrationBegin(t, f, job, inventory, false)
	w, err := p.NextWindow(ctx, 1)
	requireReceiptNoError(t, err)
	m := MigrationMessage{UID: 1, InternalDate: time.Now().UTC().Truncate(time.Microsecond), Size: 4}
	c, err := p.Observe(ctx, *w, m)
	requireReceiptNoError(t, err)
	retry, err := p.Observe(ctx, *w, m)
	requireReceiptCondition(t, err == nil, "attempt retry unstable", retry, err)
	requireReceiptCondition(t, retry.Attempt == c.Attempt, "attempt retry unstable", retry, err)
	pending := prepare(t, f, c.Attempt, c.Envelope)
	copies(t, f, pending, "body")
	q := migrationBegin(t, f, job, inventory, true)
	{
		_, err = f.s.db.finalize(ctx, pending)
		requireReceiptCondition(t, errors.Is(err, ErrStale), "superseded generation finalized", err)
	}
	{
		n, err := f.s.ReapMigrationAttempts(ctx, f.org, job, 100)
		requireReceiptCondition(t, err == nil, "obsolete attempt reaping", n, err)
		requireReceiptCondition(t, n == 1, "obsolete attempt reaping", n, err)
	}
	{
		st := status(t, f, c.Attempt)
		requireReceiptCondition(t, st.State == "aborted", st)
	}
	migrationScan(t, f, q, map[string]map[uint32]MigrationMessage{"INBOX": {1: m}}, map[string]string{"INBOX": "body"})
	migrationSeal(t, q, inventory)
	err = q.Activate(ctx)
	requireReceiptNoError(t, err)
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT count(*) FROM fry_receipt`) == 1,
		"superseded attempt published receipt",
	)
}

func TestMigrationResumedWindowDisappearance(t *testing.T) {
	for _, accepted := range []bool{false, true} {
		name := "pending"
		if accepted {
			name = "accepted"
		}
		t.Run(name, func(t *testing.T) {
			testResumedBackgroundMigrationDisappearance(t, accepted)
		})
	}
	for _, accepted := range []bool{false, true} {
		name := "final pending"
		if accepted {
			name = "final accepted"
		}
		t.Run(name, func(t *testing.T) {
			testResumedFinalMigrationDisappearance(t, accepted)
		})
	}
}

func TestMigrationRejectsPreviouslyAcceptedDestination(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	env := f.envelope(t)
	inbox := env.Destinations[0].Inbox
	accept(t, f.s, newID(t), env, "ordinary accepted delivery")
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT count(*) FROM fry_inbox WHERE id=$1`, inbox.String()) == 0,
		"test requires destination not yet published",
	)
	{
		err := f.s.CreateMigration(ctx, f.org, newID(t), inbox, FingerprintMigrationSource("source:993", "user", false))
		requireReceiptCondition(t, errors.Is(err, ErrConflict), "reserved previously accepted destination", err)
	}
	requireReceiptNoError(t, f.s.EnsureInbox(ctx, f.org, inbox))
	{
		_, err := f.s.PublishMailboxes(ctx, 10)
		requireReceiptCondition(t, err == nil, err)
	}
	_, mail, err := f.s.Mailbox(ctx, f.org, inbox, "INBOX")
	requireReceiptCondition(t, err == nil, "reservation disrupted ordinary work", mail, err)
	requireReceiptCondition(t, len(mail) == 1, "reservation disrupted ordinary work", mail, err)
}

func TestMigrationReservationSerializesWithOrdinaryAdmission(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	for range 4 {
		env := f.envelope(t)
		inbox := env.Destinations[0].Inbox
		job := newID(t)
		attempt := newID(t)
		// Hold the admission lock while both contenders start, so they must pass
		// through the same serialization point even though the inbox does not exist.
		blocker, err := f.pool.Begin(ctx)
		requireReceiptNoError(t, err)
		err = lockInboxAdmission(ctx, blocker, inbox)
		requireReceiptNoError(t, err)
		start := make(chan struct{})
		ordinary := make(chan error, 1)
		migration := make(chan error, 1)
		go func() {
			<-start
			_, err := f.s.Accept(ctx, attempt, env, io.NopCloser(strings.NewReader("ordinary")))
			ordinary <- err
		}()
		go func() {
			<-start
			migration <- f.s.CreateMigration(
				ctx,
				f.org,
				job,
				inbox,
				FingerprintMigrationSource("source:993", "user", false),
			)
		}()
		close(start)
		err = blocker.Commit(ctx)
		requireReceiptNoError(t, err)
		ordinaryErr, migrationErr := <-ordinary, <-migration
		requireMigrationAdmissionWinner(t, f, job, inbox, ordinaryErr, migrationErr)
	}
}

func TestMigrationPublicationDeadlineRecheckedAfterWork(t *testing.T) {
	t.Run("finalize", testMigrationFinalizeDeadlineAfterWork)
	t.Run("activate", testMigrationActivateDeadlineAfterWork)
}

func requireActivatedMigrationMailboxes(
	t *testing.T,
	ctx context.Context,
	f *fixture,
	inbox ID,
	retained string,
	date time.Time,
	q *MigrationPass,
) {
	t.Helper()
	_, mail, err := f.s.Mailbox(ctx, f.org, inbox, "INBOX")
	requireReceiptCondition(t, err == nil, mail, err)
	requireReceiptCondition(t, len(mail) == 2, mail, err)
	requireReceiptCondition(t, mail[0].Receipt.String() == retained, "retained identity or metadata changed", mail[0])
	requireReceiptCondition(t, mail[0].Date.Equal(date), "retained identity or metadata changed", mail[0])
	requireReceiptCondition(
		t,
		reflect.DeepEqual(mail[0].Flags, []string{"\\Flagged", "custom"}),
		"retained identity or metadata changed",
		mail[0],
	)
	var raw bytes.Buffer
	{
		err := f.s.Read(ctx, f.org, mail[0].Receipt, 0, mail[0].Size, &raw)
		requireReceiptCondition(t, err == nil, "retained raw bytes", raw.String(), err)
		requireReceiptCondition(t, raw.String() == "body", "retained raw bytes", raw.String(), err)
	}
	{
		_, _, err := f.s.Mailbox(ctx, f.org, inbox, "Old")
		requireReceiptCondition(t, err != nil, "obsolete source folder survived")
	}
	{
		_, _, err := f.s.Mailbox(ctx, f.org, inbox, "Renamed")
		requireReceiptCondition(t, err == nil, "renamed source missing", err)
	}
	{
		_, err := q.NextWindow(ctx, 1)
		requireReceiptCondition(t, errors.Is(err, ErrStale), "activated pass mutable", err)
	}
	requireReceiptCondition(
		t,
		scalarContext(ctx, t, f.pool, `SELECT count(*) FROM fry_mail`) == 4,
		"wrong reconciled membership count",
	)
}

func testResumedBackgroundMigrationDisappearance(t *testing.T, accepted bool) {
	t.Helper()
	f := setup(t)
	ctx := context.Background()
	job, _ := migrationJob(t, f)
	inventory := []MigrationFolder{{Source: "INBOX", Destination: "INBOX", UIDValidity: 1, UIDNext: 2}}
	p := migrationBegin(t, f, job, inventory, false)
	w, err := p.NextWindow(ctx, 1)
	requireReceiptNoError(t, err)
	c, err := p.Observe(ctx, *w, MigrationMessage{UID: 1, InternalDate: time.Now(), Size: 4})
	requireReceiptNoError(t, err)
	if accepted {
		accept(t, f.s, c.Attempt, c.Envelope, "body")
	} else {
		prepare(t, f, c.Attempt, c.Envelope)
	}
	// The source expunged this UID after the process died before checkpointing.
	resumed, err := f.s.ResumeMigration(ctx, f.org, job)
	requireReceiptNoError(t, err)
	{
		err = resumed.AdvanceWindow(ctx, *w, nil)
		requireReceiptCondition(t, err == nil, "retry of successful empty SEARCH", err)
	}
	migrationSeal(t, resumed, inventory)
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT count(*) FROM fry_mail`) == 0,
		"disappeared UID left ghost membership",
	)
	requireReceiptCondition(
		t,
		scalar(
			t,
			f.pool,
			`SELECT count(*) FROM fry_migration_entry WHERE job=$1 AND (seen_generation=$2 OR `+
				`attempt IS NOT NULL)`,
			job.String(),
			resumed.Generation,
		) == 0,
		"disappeared UID still blocks scan completion",
	)
}

func testResumedFinalMigrationDisappearance(t *testing.T, accepted bool) {
	t.Helper()
	f := setup(t)
	ctx := context.Background()
	job, _ := migrationJob(t, f)
	inventory := []MigrationFolder{{Source: "INBOX", Destination: "INBOX", UIDValidity: 1, UIDNext: 2}}
	p := migrationBegin(t, f, job, inventory, true)
	w, err := p.NextWindow(ctx, 1)
	requireReceiptNoError(t, err)
	c, err := p.Observe(ctx, *w, MigrationMessage{UID: 1, InternalDate: time.Now(), Size: 4})
	requireReceiptNoError(t, err)
	if accepted {
		accept(t, f.s, c.Attempt, c.Envelope, "body")
	}
	resumed, err := f.s.ResumeMigration(ctx, f.org, job)
	requireReceiptNoError(t, err)
	{
		err = resumed.AdvanceWindow(ctx, *w, nil)
		requireReceiptCondition(t, errors.Is(err, ErrConflict), "freeze violation did not fail closed", err)
	}
	requireReceiptCondition(
		t,
		scalar(
			t,
			f.pool,
			`SELECT count(*) FROM fry_migration_entry WHERE job=$1 AND seen_generation=$2 AND `+
				`attempt=$3`,
			job.String(),
			resumed.Generation,
			c.Attempt.String(),
		) == 1,
		"failed final window modified ledger",
	)
	expected := int64(0)
	if accepted {
		expected = 1
	}
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT count(*) FROM fry_mail`) == expected,
		"failed final window removed accepted mail",
	)
	{
		_, err = resumed.Reconcile(ctx, 100)
		requireReceiptCondition(t, errors.Is(err, ErrBusy), "incomplete final scan reconciled", err)
	}
	{
		_, err = resumed.Seal(ctx, inventory)
		requireReceiptCondition(t, errors.Is(err, ErrBusy), "incomplete final scan sealed", err)
	}
}

func testMigrationFinalizeDeadlineAfterWork(t *testing.T) {
	t.Helper()
	f := setup(t)
	ctx := context.Background()
	job, _ := migrationJob(t, f)
	p := migrationBegin(
		t,
		f,
		job,
		[]MigrationFolder{{Source: "INBOX", Destination: "INBOX", UIDValidity: 1, UIDNext: 2}},
		true,
	)
	w, err := p.NextWindow(ctx, 1)
	requireReceiptNoError(t, err)
	c, err := p.Observe(ctx, *w, MigrationMessage{UID: 1, InternalDate: time.Now(), Size: 4})
	requireReceiptNoError(t, err)
	pending := prepare(t, f, c.Attempt, c.Envelope)
	copies(t, f, pending, "body")
	f.s.db.beforeCommit = func(tx pgx.Tx) error {
		if _, err := tx.Exec(
			ctx,
			`UPDATE fry_migration SET deadline=clock_timestamp()+interval '20 milliseconds' WHERE `+
				`id=$1`,
			job.String(),
		); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `SELECT pg_sleep(0.05)`)
		return err
	}
	{
		_, err = f.s.db.finalize(ctx, pending)
		requireReceiptCondition(t, errors.Is(err, ErrStale), "finalized after final deadline", err)
	}
	f.s.db.beforeCommit = nil
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT count(*) FROM fry_receipt`) == 0,
		"expired finalize published receipt or mail",
	)
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT count(*) FROM fry_mail`) == 0,
		"expired finalize published receipt or mail",
	)
	{
		st := status(t, f, c.Attempt)
		requireReceiptCondition(t, st.State != "accepted", "expired finalize accepted attempt")
	}
}

func testMigrationActivateDeadlineAfterWork(t *testing.T) {
	t.Helper()
	f := setup(t)
	ctx := context.Background()
	job, inbox := migrationJob(t, f)
	inventory := []MigrationFolder{{Source: "INBOX", Destination: "INBOX", UIDValidity: 1, UIDNext: 1}}
	p := migrationBegin(t, f, job, inventory, true)
	migrationSeal(t, p, inventory)
	// This models the inbox update taking longer than the remaining freeze.
	execSQL(
		t,
		f.pool,
		`CREATE FUNCTION delay_migration_activation() RETURNS trigger LANGUAGE plpgsql AS $$ `+
			`BEGIN IF OLD.migration_job IS NOT NULL AND NEW.migration_job IS NULL THEN PERFORM `+
			`pg_sleep(0.3); END IF; RETURN NEW; END $$`,
	)
	execSQL(
		t,
		f.pool,
		`CREATE TRIGGER delay_activation BEFORE UPDATE ON fry_inbox FOR EACH ROW EXECUTE `+
			`FUNCTION delay_migration_activation()`,
	)
	execSQL(
		t,
		f.pool,
		`UPDATE fry_migration SET deadline=clock_timestamp()+interval '150 milliseconds' WHERE `+
			`id=$1`,
		job.String(),
	)
	{
		err := p.Activate(ctx)
		requireReceiptCondition(t, errors.Is(err, ErrStale), "activated after deadline during inbox update", err)
	}
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT count(*) FROM fry_migration WHERE id=$1 AND phase='ready'`, job.String()) == 1,
		"expired activation changed migration phase",
	)
	requireReceiptCondition(
		t,
		scalar(
			t,
			f.pool,
			`SELECT count(*) FROM fry_inbox WHERE id=$1 AND migration_job=$2`,
			inbox.String(),
			job.String(),
		) == 1,
		"expired activation exposed staging",
	)
	{
		_, _, err := f.s.Mailbox(ctx, f.org, inbox, "INBOX")
		requireReceiptCondition(t, err != nil, "expired activation enabled mailbox")
	}
}

func sealBackgroundMigration(
	t *testing.T,
	f *fixture,
	job ID,
	p *MigrationPass,
	first []MigrationFolder,
) string {
	t.Helper()
	ctx := context.Background()
	migrationSeal(t, p, first)
	{
		err := p.Activate(ctx)
		requireReceiptCondition(t, errors.Is(err, ErrConflict), "background activated", err)
	}
	var retained string
	requireReceiptNoError(
		t,
		f.pool.QueryRow(ctx, `SELECT m.receipt FROM fry_migration_entry e JOIN fry_mail m ON m.id=e.mail WHERE `+
			`e.job=$1 AND e.source_name='INBOX' AND e.uid=1`, job.String()).
			Scan(&retained),
	)
	return retained
}

func testMigrationExpiredDeadline(t *testing.T) {
	t.Helper()
	f := setup(t)
	ctx := context.Background()
	job, inbox := migrationJob(t, f)
	inventory := []MigrationFolder{{Source: "INBOX", Destination: "INBOX", UIDValidity: 1, UIDNext: 1}}
	for _, freeze := range []*MigrationFreeze{
		{},
		{
			Actor:          "operator",
			Reason:         "reason",
			Barrier:        "barrier",
			ArrivalHandoff: "queue",
			StartedAt:      time.Now().Add(-31 * time.Minute),
		},
	} {
		{
			_, err := f.s.BeginMigrationPass(ctx, f.org, job, inventory, freeze)
			requireReceiptCondition(t, errors.Is(err, ErrConflict), "invalid freeze accepted", err)
		}
	}
	p := migrationBegin(t, f, job, inventory, true)
	migrationSeal(t, p, inventory)
	execSQL(
		t,
		f.pool,
		`UPDATE fry_migration SET deadline=clock_timestamp()-interval '1 second' WHERE id=$1`,
		job.String(),
	)
	{
		err := p.Activate(ctx)
		requireReceiptCondition(t, errors.Is(err, ErrStale), "expired activation", err)
	}
	{
		_, _, err := f.s.Mailbox(ctx, f.org, inbox, "INBOX")
		requireReceiptCondition(t, err != nil, "expired migration exposed inbox")
	}
}

func testMigrationSizeMismatch(t *testing.T) {
	t.Helper()
	f := setup(t)
	ctx := context.Background()
	job, _ := migrationJob(t, f)
	p := migrationBegin(
		t,
		f,
		job,
		[]MigrationFolder{{Source: "INBOX", Destination: "INBOX", UIDValidity: 1, UIDNext: 2}},
		false,
	)
	w, err := p.NextWindow(ctx, 1)
	requireReceiptNoError(t, err)
	c, err := p.Observe(ctx, *w, MigrationMessage{UID: 1, InternalDate: time.Now(), Size: 5})
	requireReceiptNoError(t, err)
	{
		_, err = f.s.Accept(ctx, c.Attempt, c.Envelope, io.NopCloser(strings.NewReader("body")))
		requireReceiptCondition(t, errors.Is(err, ErrConflict), "wrong size accepted", err)
	}
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT count(*) FROM fry_receipt`) == 0,
		"size mismatch published mail",
	)
	requireReceiptCondition(
		t,
		scalar(t, f.pool, `SELECT count(*) FROM fry_mail`) == 0,
		"size mismatch published mail",
	)
}

func requireMigrationAdmissionWinner(t *testing.T, f *fixture, job, inbox ID, ordinaryErr, migrationErr error) {
	t.Helper()
	switch {
	case ordinaryErr == nil:
		requireReceiptCondition(
			t,
			errors.Is(migrationErr, ErrConflict),
			"accepted work and migration both admitted",
			migrationErr,
		)
		requireReceiptCondition(
			t,
			scalar(t, f.pool, `SELECT count(*) FROM fry_migration WHERE id=$1`, job.String()) == 0,
			"conflicting migration persisted",
		)
	case migrationErr == nil:
		requireReceiptCondition(
			t,
			errors.Is(ordinaryErr, ErrBusy),
			"migration did not exclude ordinary ingress",
			ordinaryErr,
		)
		requireReceiptCondition(
			t,
			scalar(t, f.pool, `SELECT count(*) FROM fry_destination WHERE inbox=$1`, inbox.String()) == 0,
			"ordinary destination escaped staging exclusion",
		)
	default:
		t.Fatal("neither contender admitted", ordinaryErr, migrationErr)
	}
}
