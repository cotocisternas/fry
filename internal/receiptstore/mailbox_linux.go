//go:build linux

package receiptstore

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/cotocisternas/fry/internal/hotstore"
	"github.com/cotocisternas/fry/internal/receiptstore/dbgen"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Folder and Mail describe mutable mailbox state. Receipt bytes remain immutable.
type Folder struct {
	ID          ID
	Name        string
	UIDValidity uint32
	UIDNext     uint64
	Subscribed  bool
	Keywords    []string
}
type Mail struct {
	ID, Receipt ID
	UID         uint32
	Flags       []string
	Date        time.Time
	Size        int64
}

func folderName(name string) (string, error) {
	if strings.EqualFold(name, "INBOX") {
		name = "INBOX"
	}
	if name == "" || len(name) > 255 || strings.ContainsAny(name, "\x00\r\n") {
		return "", fmt.Errorf("receiptstore: invalid folder name")
	}
	return name, nil
}

// EnsureInbox binds a stable inbox identity to exactly one organization. It also
// creates INBOX without changing its UIDVALIDITY on restart.
func (s *Store) EnsureInbox(ctx context.Context, org, inbox ID) error {
	if org == (ID{}) || inbox == (ID{}) {
		return ErrConflict
	}
	tx, err := s.db.begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(ctx, tx)
	if err = EnsureInboxInTransaction(ctx, tx, org, inbox); err != nil {
		return err
	}
	return commit(ctx, tx)
}

// EnsureInboxInTransaction lets the administrative authority commit mailbox
// creation with its permission check and audit. Callers own commit/rollback.
func EnsureInboxInTransaction(ctx context.Context, tx pgx.Tx, org, inbox ID) error {
	if org == (ID{}) || inbox == (ID{}) {
		return ErrConflict
	}
	if _, err := dbgen.New(tx).
		EnsureInboxInTransactionInsertInbox(ctx, dbgen.EnsureInboxInTransactionInsertInboxParams{
			ID:           inbox.String(),
			Organization: org.String(),
		}); err != nil {
		return err
	}
	actual, err := dbgen.New(tx).EnsureInboxInTransactionReadInbox(ctx, inbox.String())
	if err != nil {
		return err
	}
	if actual != org.String() {
		return ErrConflict
	}
	id, err := hotstore.NewID()
	if err != nil {
		return err
	}
	tag, err := dbgen.New(tx).
		EnsureInboxInTransactionInsertFolder(ctx, dbgen.EnsureInboxInTransactionInsertFolderParams{
			ID:    id.String(),
			Inbox: inbox.String(),
		})
	if err != nil {
		return err
	}
	return recordInboxFolder(ctx, tx, inbox, id, tag.RowsAffected() > 0)
}

func recordInboxFolder(ctx context.Context, tx pgx.Tx, inbox, folder ID, created bool) error {
	if !created {
		return nil
	}
	if _, err := dbgen.New(tx).EnsureInboxInTransactionInsertSubscription(ctx, inbox.String()); err != nil {
		return err
	}
	return recordNewFolderName(ctx, tx, folder)
}

func scanFolder(row pgx.Row) (Folder, error) {
	var f Folder
	var id string
	err := row.Scan(&id, &f.Name, &f.UIDValidity, &f.UIDNext, &f.Subscribed, &f.Keywords)
	if errors.Is(err, pgx.ErrNoRows) {
		return f, ErrNotFound
	}
	if err != nil {
		return f, err
	}
	f.ID, err = idFrom(id)
	return f, err
}

func folderFromSQL(row dbgen.FolderByNameRow, err error) (Folder, error) {
	var f Folder
	if errors.Is(err, pgx.ErrNoRows) {
		return f, ErrNotFound
	}
	if err != nil {
		return f, err
	}
	f.ID, err = idFrom(row.ID)
	if err != nil {
		return f, err
	}
	f.UIDValidity, err = sqlcUint32(row.Uidvalidity)
	if err != nil {
		return f, err
	}
	f.UIDNext, err = sqlcUint64(row.Uidnext)
	f.Name, f.Subscribed, f.Keywords = row.Name, row.Exists, row.Keywords
	return f, err
}

func getFolder(ctx context.Context, tx pgx.Tx, org, inbox ID, name string, lock bool) (Folder, error) {
	name, err := folderName(name)
	if err != nil {
		return Folder{}, err
	}
	var row dbgen.FolderByNameRow
	if lock {
		locked, queryErr := dbgen.New(tx).
			FolderByNameLocked(ctx, dbgen.FolderByNameLockedParams{
				ID:           inbox.String(),
				Organization: org.String(),
				Name:         name,
			})
		row, err = dbgen.FolderByNameRow(locked), queryErr
	} else {
		row, err = dbgen.New(tx).
			FolderByName(ctx, dbgen.FolderByNameParams{ID: inbox.String(), Organization: org.String(), Name: name})
	}
	return folderFromSQL(row, err)
}

func (s *Store) Folders(ctx context.Context, org, inbox ID) ([]Folder, error) {
	rows, err := s.db.pool.Query(ctx, dbgen.StoreFolders, inbox.String(), org.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Folder
	for rows.Next() {
		f, e := scanFolder(rows)
		if e != nil {
			return nil, e
		}
		result = append(result, f)
	}
	return result, rows.Err()
}

func (s *Store) CreateFolder(ctx context.Context, org, inbox ID, name string) error {
	// A trailing hierarchy separator declares intent to create inferiors; it
	// is not part of the mailbox name (RFC 3501 section 6.3.3).
	name = strings.TrimSuffix(name, "/")
	name, err := folderName(name)
	if err != nil {
		return err
	}
	id, err := hotstore.NewID()
	if err != nil {
		return err
	}
	tx, err := s.db.begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(ctx, tx)
	if err = func() error {
		sqlcResult, sqlcErr := dbgen.New(tx).
			StoreCreateFolderReadInbox(ctx, dbgen.StoreCreateFolderReadInboxParams{
				ID:           inbox.String(),
				Organization: org.String(),
			})
		if sqlcErr != nil {
			return sqlcErr
		}
		_ = sqlcResult
		return nil
	}(); err != nil {
		return err
	}
	tag, err := dbgen.New(tx).
		StoreCreateFolderInsertFolder(ctx, dbgen.StoreCreateFolderInsertFolderParams{
			ID:    id.String(),
			Inbox: inbox.String(),
			Name:  name,
		})
	if err == nil && tag.RowsAffected() == 0 {
		return ErrConflict
	}
	if err != nil {
		return err
	}
	if err = recordNewFolderName(ctx, tx, id); err != nil {
		return err
	}
	return commit(ctx, tx)
}

// SubscribeFolder keeps subscription names independent of folder lifetime.
// Shared mailbox credentials currently share one subscription set per inbox.
func (s *Store) SubscribeFolder(ctx context.Context, org, inbox ID, name string, subscribe bool) error {
	name, err := folderName(name)
	if err != nil {
		return err
	}
	tx, err := s.db.begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(ctx, tx)
	if err = lockInboxNamespace(ctx, tx, org, inbox); err != nil {
		return err
	}
	if subscribe {
		_, err = dbgen.New(tx).
			StoreSubscribeFolderInsertSubscription(ctx, dbgen.StoreSubscribeFolderInsertSubscriptionParams{
				Inbox: inbox.String(),
				Name:  name,
			})
	} else {
		_, err = dbgen.New(tx).
			StoreSubscribeFolderDeleteSubscription(ctx, dbgen.StoreSubscribeFolderDeleteSubscriptionParams{
				Inbox: inbox.String(),
				Name:  name,
			})
	}
	if err != nil {
		return err
	}
	return commit(ctx, tx)
}

// PublishMailboxes gives one eligible organization a bounded publication turn.
// Turns rotate lexicographically through a process-local cursor, wrapping once.
// Restart resets the cursor; independent workers can choose the same candidates. Each destination's
// membership, UID allocation and pending->visible ownership transfer commit
// together. Replaying work after publication or expunge cannot create a copy.
func (s *Store) PublishMailboxes(ctx context.Context, limit int) (int, error) {
	if limit < 1 || limit > 1024 {
		return 0, ErrConflict
	}
	batch, err := s.selectMailboxTurn(ctx, limit)
	if err != nil {
		return 0, err
	}
	count := 0
	var firstError error
	for _, w := range batch {
		published, e := s.publishMailbox(ctx, w.o, w.r, w.i)
		if e != nil {
			if firstError == nil {
				firstError = e
			}
			retryErr := s.deferMailboxWork(ctx, w, e)
			if retryErr != nil {
				return count, errors.Join(firstError, retryErr)
			}
			continue
		}
		if published {
			count++
		}
	}
	return count, firstError
}

// mailboxWork is a candidate, not a lease. Receipt publication rechecks all
// authority under its existing transaction locks; other workers may select it.
type mailboxWork struct{ r, i, o ID }

// selectMailboxTurn serializes only selection and its process-local cursor. An
// organization gets one opportunity even if its later publication blocks/fails.
func (s *Store) selectMailboxTurn(ctx context.Context, limit int) ([]mailboxWork, error) {
	if limit < 1 || limit > 1024 {
		return nil, ErrConflict
	}
	select {
	case s.mailboxSelection <- struct{}{}:
		defer func() { <-s.mailboxSelection }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	due, err := dbgen.New(s.db.pool).StoreSelectMailboxTurnReadClock(ctx)
	if err != nil {
		return nil, err
	}
	org, found, err := s.nextMailboxOrganization(ctx, due)
	if err != nil || !found {
		return nil, err
	}
	// Advance before fetching the batch so even a timeout after selection cannot
	// let this organization's debt retain the next process-local opportunity.
	s.mailboxCursor = org
	// Keep the ordered work scan outside the authority semi-join. The measured
	// plan can stop after limit eligible candidates instead of sorting an entire
	// due organization. An ineligible inbox prefix can still require more scans;
	// this is a candidate/result bound, not a hard database CPU/row-read bound.
	rows, err := dbgen.New(s.db.pool).
		StoreSelectMailboxTurnReadWork(ctx, dbgen.StoreSelectMailboxTurnReadWorkParams{
			Organization: org,
			AvailableAt:  due,
			Limit:        int32(limit),
		})
	if err != nil {
		return nil, err
	}
	return decodeMailboxWork(rows)
}

func (s *Store) publishMailbox(ctx context.Context, org, receipt, inbox ID) (bool, error) {
	tx, err := s.db.begin(ctx)
	if err != nil {
		return false, err
	}
	defer rollback(ctx, tx)
	published, err := publishMailboxInTransaction(ctx, tx, org, receipt, inbox, false)
	if err != nil {
		return false, err
	}
	if err = commit(ctx, tx); err != nil {
		return false, err
	}
	return published, nil
}

func publishMailboxInTransaction(ctx context.Context, tx pgx.Tx, org, receipt, inbox ID, migration bool) (bool, error) {
	state, err := lockReceipt(ctx, tx, org, receipt)
	if err != nil {
		return false, err
	}
	ready, err := mailboxPublicationReady(ctx, tx, receipt, inbox, state)
	if err != nil || !ready {
		return false, err
	}
	work, err := dbgen.New(tx).
		PublishMailboxInTransactionReadWork(ctx, dbgen.PublishMailboxInTransactionReadWorkParams{
			Receipt: receipt.String(),
			Inbox:   inbox.String(),
		})
	if err != nil {
		return false, err
	}
	folder, err := mailboxPublicationFolder(ctx, tx, org, inbox, migration, work.Folder)
	if err != nil {
		return false, err
	}
	if err = insertPublishedMailbox(ctx, tx, folder, receipt, inbox, work); err != nil {
		return false, err
	}
	return true, nil
}

func mailboxPublicationReady(ctx context.Context, tx pgx.Tx, receipt, inbox ID, state string) (bool, error) {
	exists, err := dbgen.New(tx).
		PublishMailboxInTransactionCheckWork(ctx, dbgen.PublishMailboxInTransactionCheckWorkParams{
			Receipt: receipt.String(),
			Inbox:   inbox.String(),
		})
	if err != nil {
		return false, err
	}
	if !exists {
		return false, nil
	}
	if state != "live" {
		return false, ErrExpired
	}
	// An earlier explicit release must not be undone by a delayed worker.
	exists, err = dbgen.New(tx).
		PublishMailboxInTransactionCheckReference(ctx, dbgen.PublishMailboxInTransactionCheckReferenceParams{
			Receipt:      receipt.String(),
			ReferenceKey: inbox.String(),
		})
	if err != nil {
		return false, err
	}
	if !exists {
		return false, ErrExpired
	}
	return true, nil
}

func mailboxPublicationFolder(
	ctx context.Context,
	tx pgx.Tx,
	org, inbox ID,
	migration bool,
	folder *string,
) (Folder, error) {
	if folder == nil {
		return getFolder(ctx, tx, org, inbox, "INBOX", true)
	}
	row, err := dbgen.New(tx).
		PublishMailboxInTransactionReadSubscription(ctx, dbgen.PublishMailboxInTransactionReadSubscriptionParams{
			ID:           *folder,
			ID_2:         inbox.String(),
			Organization: org.String(),
			Column4:      migration,
		})
	return folderFromSQL(dbgen.FolderByNameRow(row), err)
}

func insertPublishedMailbox(
	ctx context.Context,
	tx pgx.Tx,
	f Folder,
	receipt, inbox ID,
	work dbgen.PublishMailboxInTransactionReadWorkRow,
) error {
	if f.UIDNext > 4294967295 {
		return fmt.Errorf("receiptstore: UID space exhausted")
	}
	if err := registerKeywords(ctx, tx, f.ID, work.Flags); err != nil {
		return err
	}
	id, err := hotstore.NewID()
	if err != nil {
		return err
	}
	if _, err = dbgen.New(tx).
		PublishMailboxInTransactionInsertMail(ctx, dbgen.PublishMailboxInTransactionInsertMailParams{
			ID:           id.String(),
			Folder:       f.ID.String(),
			Uid:          int64(f.UIDNext),
			Receipt:      receipt.String(),
			InternalDate: work.InternalDate,
			Flags:        work.Flags,
		}); err != nil {
		return err
	}
	if _, err = dbgen.New(tx).PublishMailboxInTransactionUpdateFolder(ctx, f.ID.String()); err != nil {
		return err
	}
	if _, err = dbgen.New(tx).
		PublishMailboxInTransactionInsertReference(ctx, dbgen.PublishMailboxInTransactionInsertReferenceParams{
			Receipt:      receipt.String(),
			ReferenceKey: id.String(),
		}); err != nil {
		return err
	}
	if err = releaseMailboxRef(ctx, tx, receipt, "pending-mailbox", inbox.String()); err != nil {
		return err
	}
	_, err = dbgen.New(tx).
		PublishMailboxInTransactionDeleteWork(ctx, dbgen.PublishMailboxInTransactionDeleteWorkParams{
			Receipt: receipt.String(),
			Inbox:   inbox.String(),
		})
	return err
}

func releaseMailboxRef(ctx context.Context, tx pgx.Tx, receipt ID, kind, key string) error {
	if _, err := dbgen.New(tx).
		ReleaseMailboxRefInsertReferenceRelease(ctx, dbgen.ReleaseMailboxRefInsertReferenceReleaseParams{
			Receipt:      receipt.String(),
			Kind:         kind,
			ReferenceKey: key,
		}); err != nil {
		return err
	}
	_, err := dbgen.New(tx).
		ReleaseMailboxRefUpdateReference(ctx, dbgen.ReleaseMailboxRefUpdateReferenceParams{
			Receipt:      receipt.String(),
			Kind:         kind,
			ReferenceKey: key,
		})
	return err
}

func mailboxRows(ctx context.Context, tx pgx.Tx, folder ID) ([]Mail, error) {
	rows, err := tx.Query(ctx, dbgen.MailboxRows, folder.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Mail
	for rows.Next() {
		var m Mail
		var id, receipt string
		if err = rows.Scan(&id, &receipt, &m.UID, &m.Flags, &m.Date, &m.Size); err != nil {
			return nil, err
		}
		m.ID, err = idFrom(id)
		if err != nil {
			return nil, err
		}
		m.Receipt, err = idFrom(receipt)
		if err != nil {
			return nil, err
		}
		result = append(result, m)
	}
	return result, rows.Err()
}

// Mailbox returns a coherent read-only snapshot without blocking folder writers. The local IMAP
// prototype retains its own sequence snapshot; callers must not treat current
// row positions as the sequence numbers of an older selected session.
func (s *Store) Mailbox(ctx context.Context, org, inbox ID, name string) (Folder, []Mail, error) {
	return s.mailbox(ctx, org, inbox, folderRef{name: name})
}

func (s *Store) mailbox(ctx context.Context, org, inbox ID, ref folderRef) (Folder, []Mail, error) {
	tx, err := s.db.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return Folder{}, nil, err
	}
	defer rollback(ctx, tx)
	f, err := getFolderRef(ctx, tx, org, inbox, ref, false)
	if err != nil {
		return f, nil, err
	}
	messages, err := mailboxRows(ctx, tx, f.ID)
	if err != nil {
		return f, nil, err
	}
	return f, messages, commit(ctx, tx)
}

func canonicalFlags(flags []string) ([]string, error) {
	known := map[string]string{
		"\\seen":     "\\Seen",
		"\\answered": "\\Answered",
		"\\flagged":  "\\Flagged",
		"\\deleted":  "\\Deleted",
		"\\draft":    "\\Draft",
	}
	set := map[string]bool{}
	if len(flags) > 128 {
		return nil, fmt.Errorf("receiptstore: flag count limit")
	}
	for _, f := range flags {
		v, ok := known[strings.ToLower(f)]
		if !ok {
			if err := validateMailboxKeyword(f); err != nil {
				return nil, err
			}
			v = strings.ToLower(f)
		}
		set[v] = true
	}
	result := []string{}
	for f := range set {
		result = append(result, f)
	}
	sort.Strings(result)
	return result, nil
}

// ChangeFlags updates selected stable UIDs, never mutable sequence positions.
func (s *Store) ChangeFlags(
	ctx context.Context,
	org, inbox ID,
	name string,
	uids []uint32,
	op string,
	flags []string,
) error {
	return s.changeFlags(ctx, org, inbox, folderRef{name: name}, uids, op, flags)
}

func (s *Store) changeFlags(
	ctx context.Context,
	org, inbox ID,
	ref folderRef,
	uids []uint32,
	op string,
	flags []string,
) error {
	flags, err := canonicalFlags(flags)
	if err != nil {
		return err
	}
	if len(uids) > 1000000 || !validMailboxFlagOperation(op) {
		return ErrConflict
	}
	tx, err := s.db.begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(ctx, tx)
	f, err := getFolderRef(ctx, tx, org, inbox, ref, true)
	if err != nil {
		return err
	}
	tag, err := mutateMailboxFlags(ctx, tx, f.ID, uids, op, flags)
	if err != nil {
		return err
	}
	if op != "remove" && tag.RowsAffected() > 0 {
		if err = registerKeywords(ctx, tx, f.ID, flags); err != nil {
			return err
		}
	}
	return commit(ctx, tx)
}

// Expunge removes only currently deleted members from a supplied stable UID
// snapshot. Lock receipts before the folder to match publication/cleanup order.
func (s *Store) Expunge(ctx context.Context, org, inbox ID, name string, uids []uint32) ([]uint32, error) {
	return s.expunge(ctx, org, inbox, folderRef{name: name}, uids)
}

func (s *Store) expunge(ctx context.Context, org, inbox ID, ref folderRef, uids []uint32) ([]uint32, error) {
	if len(uids) > 1000000 {
		return nil, ErrConflict
	}
	initial, snapshot, err := s.mailbox(ctx, org, inbox, ref)
	if err != nil {
		return nil, err
	}
	ref = folderRef{id: initial.ID, validity: initial.UIDValidity}
	selected := selectedMailboxUIDs(uids)
	guards := mailboxGuards(snapshot, selected)
	tx, err := s.db.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer rollback(ctx, tx)
	if err = guards.lock(ctx, tx, org); err != nil {
		return nil, err
	}

	f, err := getFolderRef(ctx, tx, org, inbox, ref, true)
	if err != nil {
		return nil, err
	}
	current, err := mailboxRows(ctx, tx, f.ID)
	if err != nil {
		return nil, err
	}
	removed, err := s.expungeMemberships(ctx, tx, current, selected, guards)
	if err != nil {
		return nil, err
	}
	return removed, commit(ctx, tx)
}

// CopyResult maps source UIDs to newly allocated destination UIDs in order.
type CopyResult struct {
	UIDValidity         uint32
	Source, Destination []uint32
}

func (s *Store) CopyMail(
	ctx context.Context,
	org, inbox ID,
	source, destination string,
	uids []uint32,
	move bool,
) (CopyResult, error) {
	return s.copyMail(ctx, org, inbox, folderRef{name: source}, destination, uids, move)
}

func (s *Store) copyMail(
	ctx context.Context,
	org, inbox ID,
	ref folderRef,
	destination string,
	uids []uint32,
	move bool,
) (CopyResult, error) {
	var result CopyResult
	if len(uids) > 1000000 {
		return result, ErrConflict
	}
	src, snapshot, err := s.mailbox(ctx, org, inbox, ref)
	if err != nil {
		return result, err
	}
	ref = folderRef{id: src.ID, validity: src.UIDValidity}
	wanted := selectedMailboxUIDs(uids)
	guards := mailboxGuards(snapshot, wanted)
	tx, err := s.db.begin(ctx)
	if err != nil {
		return result, err
	}
	defer rollback(ctx, tx)
	if err = guards.lock(ctx, tx, org); err != nil {
		return result, err
	}

	dst, err := lockCopyFolders(ctx, tx, org, inbox, src, ref, destination)
	if err != nil {
		return result, err
	}
	messages, err := mailboxRows(ctx, tx, src.ID)
	if err != nil {
		return result, err
	}
	selected, err := guardedCopyMessages(messages, wanted, guards)
	if err != nil {
		return result, err
	}
	result, err = copyMemberships(ctx, tx, dst, selected, move)
	if err != nil {
		return result, err
	}
	return result, commit(ctx, tx)
}

// copyMemberships requires the caller to hold receipt guards and the affected
// folder guards. Publication of new ownership precedes release of old ownership.
func copyMemberships(ctx context.Context, tx pgx.Tx, dst Folder, messages []Mail, move bool) (CopyResult, error) {
	var result CopyResult
	names := copiedMailboxKeywords(messages)
	if err := registerKeywords(ctx, tx, dst.ID, names); err != nil {
		return result, err
	}
	var err error
	result.UIDValidity = dst.UIDValidity
	for _, m := range messages {
		uid, err := copyMailboxMembership(ctx, tx, dst, m, move)
		if err != nil {
			return result, err
		}
		result.Source = append(result.Source, m.UID)
		result.Destination = append(result.Destination, uid)
		dst.UIDNext++
	}
	if dst.UIDNext < 1 || dst.UIDNext > 1<<32 {
		return result, ErrConflict
	}
	if _, err = dbgen.New(tx).
		CopyMembershipsUpdateFolder(ctx, dbgen.CopyMembershipsUpdateFolderParams{
			Uidnext: int64(dst.UIDNext),
			ID:      dst.ID.String(),
		}); err != nil {
		return result, err
	}
	return result, nil
}

func (s *Store) DeleteFolder(ctx context.Context, org, inbox ID, name string) error {
	name, err := folderName(name)
	if err != nil {
		return err
	}
	if name == "INBOX" {
		return ErrConflict
	}
	initial, snapshot, err := s.Mailbox(ctx, org, inbox, name)
	if err != nil {
		return err
	}
	guards := mailboxGuards(snapshot, nil)
	tx, err := s.db.begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(ctx, tx)
	if err = guards.lock(ctx, tx, org); err != nil {
		return err
	}
	f, current, err := lockDeletedFolder(ctx, tx, org, inbox, name, initial, guards)
	if err != nil {
		return err
	}
	if err = s.deleteFolderMemberships(ctx, tx, f, current); err != nil {
		return err
	}
	return commit(ctx, tx)
}

// Folder looks up only folder identity and UID metadata, without listing mail.
func (s *Store) Folder(ctx context.Context, org, inbox ID, name string) (Folder, error) {
	tx, err := s.db.begin(ctx)
	if err != nil {
		return Folder{}, err
	}
	defer rollback(ctx, tx)
	return getFolder(ctx, tx, org, inbox, name, false)
}

type AppendPlacement struct {
	Folder      ID
	UIDValidity uint32
	UID         uint32
}

// Appended returns the immutable acceptance placement, even after later expunge.
func (s *Store) Appended(ctx context.Context, org, receipt ID) (AppendPlacement, error) {
	var result AppendPlacement
	var folder string
	err := func() error {
		sqlcResult, sqlcErr := dbgen.New(s.db.pool).
			StoreAppended(ctx, dbgen.StoreAppendedParams{Organization: org.String(), ID: receipt.String()})
		if sqlcErr != nil {
			return sqlcErr
		}
		folder = sqlcResult.Folder
		result.UIDValidity, sqlcErr = sqlcUint32(sqlcResult.Uidvalidity)
		if sqlcErr != nil {
			return sqlcErr
		}
		result.UID, sqlcErr = sqlcUint32(sqlcResult.Uid)
		if sqlcErr != nil {
			return sqlcErr
		}
		return nil
	}()
	if errors.Is(err, pgx.ErrNoRows) {
		return result, ErrNotFound
	}
	if err != nil {
		return result, err
	}
	result.Folder, err = idFrom(folder)
	return result, err
}

// registerKeywords requires the folder guard. Its bounded permanent catalog
// survives flag removal, so SELECT and updates advertise stable keyword names.
func registerKeywords(ctx context.Context, tx pgx.Tx, folder ID, flags []string) error {
	names := []string{}
	for _, flag := range flags {
		if !strings.HasPrefix(flag, "\\") {
			names = append(names, flag)
		}
	}
	if len(names) == 0 {
		return nil
	}
	_, err := dbgen.New(tx).RegisterKeywords(ctx, dbgen.RegisterKeywordsParams{ID: folder.String(), Column2: names})
	return err
}

func (s *Store) deferMailboxWork(ctx context.Context, work mailboxWork, cause error) error {
	code := "storage_error"
	if errors.Is(cause, ErrNotFound) {
		code = "destination_unavailable"
	}
	if errors.Is(cause, ErrExpired) {
		code = "receipt_expired"
	}
	_, err := dbgen.New(s.db.pool).
		StorePublishMailboxes(ctx, dbgen.StorePublishMailboxesParams{
			Receipt:   work.r.String(),
			Inbox:     work.i.String(),
			LastError: new(code),
		})
	return err
}

func (s *Store) nextMailboxOrganization(ctx context.Context, due time.Time) (string, bool, error) {
	queries := dbgen.New(s.db.pool)
	org, err := queries.NextMailboxOrganization(
		ctx,
		dbgen.NextMailboxOrganizationParams{DueAt: due, AfterOrganization: s.mailboxCursor},
	)
	if errors.Is(err, pgx.ErrNoRows) {
		org, err = queries.WrappedMailboxOrganization(
			ctx,
			dbgen.WrappedMailboxOrganizationParams{DueAt: due, AfterOrganization: s.mailboxCursor},
		)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return org, true, nil
}

func decodeMailboxWork(rows []dbgen.StoreSelectMailboxTurnReadWorkRow) ([]mailboxWork, error) {
	var err error
	var batch []mailboxWork
	for _, sqlcRow := range rows {
		var a, b, c string
		a = sqlcRow.Receipt
		b = sqlcRow.Inbox
		c = sqlcRow.Organization
		r, e := idFrom(a)
		if e != nil {
			err = e
			break
		}
		i, e := idFrom(b)
		if e != nil {
			err = e
			break
		}
		o, e := idFrom(c)
		if e != nil {
			err = e
			break
		}
		batch = append(batch, mailboxWork{r, i, o})
	}
	if err != nil {
		return nil, err
	}
	return batch, nil
}

func validateMailboxKeyword(f string) error {
	if len(f) == 0 || len(f) > 128 {
		return fmt.Errorf("receiptstore: invalid keyword length")
	}
	for _, c := range []byte(f) {
		if c <= 32 || c >= 127 || strings.ContainsRune("(){%*\"\\]", rune(c)) {
			return fmt.Errorf("receiptstore: invalid flag")
		}
	}
	return nil
}

func mutateMailboxFlags(
	ctx context.Context,
	tx pgx.Tx,
	folder ID,
	uids []uint32,
	op string,
	flags []string,
) (pgconn.CommandTag, error) {
	var err error
	numbers := make([]int64, len(uids))
	for i, u := range uids {
		numbers[i] = int64(u)
	}
	queries := dbgen.New(tx)
	var tag pgconn.CommandTag
	switch op {
	case "add":
		tag, err = queries.AddMailboxFlags(
			ctx,
			dbgen.AddMailboxFlagsParams{Folder: folder.String(), Uids: numbers, Flags: flags},
		)
	case "remove":
		tag, err = queries.RemoveMailboxFlags(
			ctx,
			dbgen.RemoveMailboxFlagsParams{Folder: folder.String(), Uids: numbers, Flags: flags},
		)
	default:
		tag, err = queries.SetMailboxFlags(
			ctx,
			dbgen.SetMailboxFlagsParams{Folder: folder.String(), Uids: numbers, Flags: flags},
		)
	}
	return tag, err
}

func selectedMailboxUIDs(uids []uint32) map[uint32]bool {
	selected := map[uint32]bool{}
	for _, uid := range uids {
		selected[uid] = true
	}
	return selected
}

type mailboxReceiptGuards struct {
	receipts map[string]ID
	keys     []string
}

func mailboxGuards(snapshot []Mail, selected map[uint32]bool) mailboxReceiptGuards {
	guards := mailboxReceiptGuards{receipts: map[string]ID{}}
	for _, message := range snapshot {
		if selected == nil || selected[message.UID] {
			guards.receipts[message.Receipt.String()] = message.Receipt
		}
	}
	for key := range guards.receipts {
		guards.keys = append(guards.keys, key)
	}
	sort.Strings(guards.keys)
	return guards
}

func (guards mailboxReceiptGuards) lock(ctx context.Context, tx pgx.Tx, org ID) error {
	for _, key := range guards.keys {
		if _, err := lockReceipt(ctx, tx, org, guards.receipts[key]); err != nil {
			return err
		}
	}
	return nil
}

func (guards mailboxReceiptGuards) contains(receipt ID) bool {
	_, ok := guards.receipts[receipt.String()]
	return ok
}

func (s *Store) expungeMemberships(
	ctx context.Context,
	tx pgx.Tx,
	current []Mail,
	selected map[uint32]bool,
	guards mailboxReceiptGuards,
) ([]uint32, error) {
	var removed []uint32
	for _, message := range current {
		if !selected[message.UID] {
			continue
		}
		if !guards.contains(message.Receipt) {
			continue
		}
		if !slices.Contains(message.Flags, "\\Deleted") {
			continue
		}
		if err := s.expungeMembership(ctx, tx, message); err != nil {
			return nil, err
		}
		removed = append(removed, message.UID)
	}
	return removed, nil
}

func (s *Store) expungeMembership(ctx context.Context, tx pgx.Tx, message Mail) error {
	if _, err := dbgen.New(tx).StoreExpunge(ctx, message.ID.String()); err != nil {
		return err
	}
	if err := releaseMailboxRef(ctx, tx, message.Receipt, "mailbox", message.ID.String()); err != nil {
		return err
	}
	_, err := s.reconcileLifetime(ctx, tx, message.Receipt)
	return err
}

func lockCopyFolders(
	ctx context.Context,
	tx pgx.Tx,
	org, inbox ID,
	src Folder,
	ref folderRef,
	destination string,
) (Folder, error) {
	var err error
	dst, err := getFolder(ctx, tx, org, inbox, destination, false)
	if err != nil {
		return Folder{}, err
	}
	// Every path that holds both folder guards uses immutable ID order.
	ordered := []ID{src.ID}
	if src.ID != dst.ID {
		ordered = append(ordered, dst.ID)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].String() < ordered[j].String() })
	for _, id := range ordered {
		if err = func() error {
			sqlcResult, sqlcErr := dbgen.New(tx).StoreCopyMail(ctx, id.String())
			if sqlcErr != nil {
				return sqlcErr
			}
			_ = sqlcResult
			return nil
		}(); err != nil {
			return Folder{}, err
		}
	}
	check, err := getFolderRef(ctx, tx, org, inbox, ref, false)
	if err != nil {
		return Folder{}, err
	}
	if check.ID != src.ID {
		return Folder{}, ErrConflict
	}
	check, err = getFolder(ctx, tx, org, inbox, destination, false)
	if err != nil {
		return Folder{}, err
	}
	if check.ID != dst.ID {
		return Folder{}, ErrConflict
	}
	return check, nil
}

func guardedCopyMessages(messages []Mail, wanted map[uint32]bool, guards mailboxReceiptGuards) ([]Mail, error) {
	var selected []Mail
	for _, m := range messages {
		if !wanted[m.UID] {
			continue
		}
		if !guards.contains(m.Receipt) {
			return nil, ErrBusy
		}
		selected = append(selected, m)
	}
	return selected, nil
}

func copiedMailboxKeywords(messages []Mail) []string {
	keywords := map[string]bool{}
	for _, m := range messages {
		for _, flag := range m.Flags {
			if !strings.HasPrefix(flag, "\\") {
				keywords[flag] = true
			}
		}
	}
	names := make([]string, 0, len(keywords))
	for name := range keywords {
		names = append(names, name)
	}
	return names
}

func copyMailboxMembership(ctx context.Context, tx pgx.Tx, dst Folder, m Mail, move bool) (uint32, error) {
	var err error
	if dst.UIDNext > 4294967295 {
		return 0, fmt.Errorf("receiptstore: UID space exhausted")
	}
	id, e := hotstore.NewID()
	if e != nil {
		return 0, e
	}
	if _, err = dbgen.New(tx).
		CopyMembershipsInsertMail(ctx, dbgen.CopyMembershipsInsertMailParams{
			ID:           id.String(),
			Folder:       dst.ID.String(),
			Uid:          int64(dst.UIDNext),
			Receipt:      m.Receipt.String(),
			Flags:        m.Flags,
			InternalDate: m.Date,
		}); err != nil {
		return 0, err
	}
	if _, err = dbgen.New(tx).
		CopyMembershipsInsertReference(ctx, dbgen.CopyMembershipsInsertReferenceParams{
			Receipt:      m.Receipt.String(),
			ReferenceKey: id.String(),
		}); err != nil {
		return 0, err
	}
	if move {
		if _, err = dbgen.New(tx).CopyMembershipsDeleteMail(ctx, m.ID.String()); err != nil {
			return 0, err
		}
		if err = releaseMailboxRef(ctx, tx, m.Receipt, "mailbox", m.ID.String()); err != nil {
			return 0, err
		}
	}
	return uint32(dst.UIDNext), nil
}

func lockDeletedFolder(
	ctx context.Context,
	tx pgx.Tx,
	org, inbox ID,
	name string,
	initial Folder,
	guards mailboxReceiptGuards,
) (Folder, []Mail, error) {
	var err error
	if err = lockInboxNamespace(ctx, tx, org, inbox); err != nil {
		return Folder{}, nil, err
	}
	f, err := getFolder(ctx, tx, org, inbox, name, true)
	if err != nil {
		return Folder{}, nil, err
	}
	if f.ID != initial.ID {
		return Folder{}, nil, ErrConflict
	}
	current, err := mailboxRows(ctx, tx, f.ID)
	if err != nil {
		return Folder{}, nil, err
	}
	for _, m := range current {
		if !guards.contains(m.Receipt) {
			return Folder{}, nil, ErrBusy
		}
	}
	return f, current, nil
}

func (s *Store) deleteFolderMemberships(ctx context.Context, tx pgx.Tx, f Folder, current []Mail) error {
	var err error
	if _, err = dbgen.New(tx).StoreDeleteFolderDeleteMail(ctx, f.ID.String()); err != nil {
		return err
	}
	for _, m := range current {
		if err = releaseMailboxRef(ctx, tx, m.Receipt, "mailbox", m.ID.String()); err != nil {
			return err
		}
		if _, err = s.reconcileLifetime(ctx, tx, m.Receipt); err != nil {
			return err
		}
	}
	if _, err = dbgen.New(tx).StoreDeleteFolderDeleteFolder(ctx, f.ID.String()); err != nil {
		return err
	}
	return nil
}

func validMailboxFlagOperation(op string) bool {
	return op == "set" || op == "add" || op == "remove"
}
