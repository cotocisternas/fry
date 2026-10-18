//go:build linux

package receiptstore

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/cotocisternas/fry/internal/hotstore"
	"github.com/cotocisternas/fry/internal/receiptstore/dbgen"

	"github.com/jackc/pgx/v5"
)

func lockInboxNamespace(ctx context.Context, tx pgx.Tx, org, inbox ID) error {
	err := func() error {
		sqlcResult, sqlcErr := dbgen.New(tx).
			LockInboxNamespace(ctx, dbgen.LockInboxNamespaceParams{ID: inbox.String(), Organization: org.String()})
		if sqlcErr != nil {
			return sqlcErr
		}
		_ = sqlcResult
		return nil
	}()
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// New folders allocate from a global monotonic sequence, so their namespace is
// newer than any historical use of the name. Call under the inbox namespace lock.
func recordNewFolderName(ctx context.Context, tx pgx.Tx, id ID) error {
	_, err := dbgen.New(tx).RecordNewFolderName(ctx, id.String())
	return err
}

// RenameFolder atomically moves ordinary folders and their inferiors, preserving
// identity/UIDs. Virtual parents are recognized consistently with ListFolders.
// INBOX uses a separate atomic message-transfer transaction.
func (s *Store) RenameFolder(ctx context.Context, org, inbox ID, name, newName string) error {
	name, newName, err := normalizedFolderRename(name, newName)
	if err != nil {
		return err
	}
	if name == "INBOX" {
		return s.renameInbox(ctx, org, inbox, newName)
	}
	tx, err := s.db.begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(ctx, tx)
	all, names, err := lockedRenameEntries(ctx, tx, org, inbox)
	if err != nil {
		return err
	}
	affected, err := planFolderRename(all, names, name, newName)
	if err != nil {
		return err
	}
	if err = applyFolderRename(ctx, tx, inbox, newName, names, affected); err != nil {
		return err
	}
	return commit(ctx, tx)
}

// ListedFolder includes virtual hierarchy names and, for subscriptions, retained
// names without a live folder. No virtual entry is a selectable byte container.
type ListedFolder struct {
	Name                   string
	Selectable, Subscribed bool
}

func (s *Store) ListFolders(ctx context.Context, org, inbox ID, subscribedOnly bool) ([]ListedFolder, error) {
	tx, err := s.db.begin(ctx)
	if err != nil {
		return nil, err
	}
	defer rollback(ctx, tx)
	// Serialize this namespace snapshot against create/rename/delete/subscribe.
	if err = lockInboxNamespace(ctx, tx, org, inbox); err != nil {
		return nil, err
	}
	listing := folderListing{entries: map[string]ListedFolder{}, subscriptions: map[string]bool{}}
	if err = listing.readFolders(ctx, tx, inbox); err != nil {
		return nil, err
	}
	if err = listing.readSubscriptions(ctx, tx, inbox, subscribedOnly); err != nil {
		return nil, err
	}
	if err = commit(ctx, tx); err != nil {
		return nil, err
	}
	return listing.result(subscribedOnly), nil
}

func createFolderAncestors(ctx context.Context, tx pgx.Tx, inbox ID, name string, existing map[string]bool) error {
	var ancestors []string
	for prefix := name; strings.Contains(prefix, "/"); {
		prefix = prefix[:strings.LastIndexByte(prefix, '/')]
		if prefix != "" && !existing[prefix] {
			ancestors = append(ancestors, prefix)
			existing[prefix] = true
		}
	}
	for i := len(ancestors) - 1; i >= 0; i-- {
		id, err := hotstore.NewID()
		if err != nil {
			return err
		}
		if _, err = dbgen.New(tx).
			CreateFolderAncestors(ctx, dbgen.CreateFolderAncestorsParams{
				ID:    id.String(),
				Inbox: inbox.String(),
				Name:  ancestors[i],
			}); err != nil {
			return err
		}
		if err = recordNewFolderName(ctx, tx, id); err != nil {
			return err
		}
	}
	return nil
}

// INBOX keeps its identity, next UID and descendants. Its current memberships
// transfer atomically to a newly created destination, with independent UIDs.
func (s *Store) renameInbox(ctx context.Context, org, inbox ID, destination string) error {
	source, snapshot, err := s.Mailbox(ctx, org, inbox, "INBOX")
	if err != nil {
		return err
	}
	if len(snapshot) > 1000000 {
		return ErrConflict
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
	names, current, err := lockInboxTransfer(ctx, tx, org, inbox, source, destination, guards)
	if err != nil {
		return err
	}
	id, err := createInboxTransferTarget(ctx, tx, inbox, destination, names)
	if err != nil {
		return err
	}
	target, err := getFolderRef(ctx, tx, org, inbox, folderRef{id: id}, false)
	if err != nil {
		return err
	}
	if _, err = copyMemberships(ctx, tx, target, current, true); err != nil {
		return err
	}
	return commit(ctx, tx)
}

func normalizedFolderRename(name, newName string) (string, string, error) {
	name, err := folderName(name)
	if err != nil {
		return "", "", err
	}
	newName, err = folderName(newName)
	if err != nil {
		return "", "", err
	}
	if newName == "INBOX" || name == newName || (name != "INBOX" && strings.HasPrefix(newName, name+"/")) {
		return "", "", ErrConflict
	}
	return name, newName, nil
}

type folderRenameEntry struct {
	id         ID
	name, next string
	validity   uint32
}

func lockedRenameEntries(ctx context.Context, tx pgx.Tx, org, inbox ID) ([]folderRenameEntry, map[string]bool, error) {
	var err error
	if err = lockInboxNamespace(ctx, tx, org, inbox); err != nil {
		return nil, nil, err
	}
	// Read and lock immutable IDs in the same order used by COPY/MOVE. No
	// source-string LIKE matching: percent/underscore are ordinary name bytes.
	rows, err := tx.Query(ctx, dbgen.StoreRenameFolderReadFolder, inbox.String())
	if err != nil {
		return nil, nil, err
	}
	var all []folderRenameEntry
	names := map[string]bool{}
	for rows.Next() {
		var e folderRenameEntry
		var id string
		if err = rows.Scan(&id, &e.name, &e.validity); err != nil {
			break
		}
		e.id, err = idFrom(id)
		if err != nil {
			break
		}
		all = append(all, e)
		names[e.name] = true
	}
	rowErr := rows.Err()
	rows.Close()
	if err != nil {
		return nil, nil, err
	}
	if rowErr != nil {
		return nil, nil, rowErr
	}
	return all, names, nil
}

func planFolderRename(
	all []folderRenameEntry,
	names map[string]bool,
	name, newName string,
) ([]folderRenameEntry, error) {
	var affected []folderRenameEntry
	// Existing virtual destinations count as existing mailbox hierarchy names.
	if err := availableFolderHierarchy(names, newName); err != nil {
		return nil, err
	}
	moving := map[string]bool{}
	for _, e := range all {
		if e.name == name || strings.HasPrefix(e.name, name+"/") {
			e.next = newName + strings.TrimPrefix(e.name, name)
			if _, err := folderName(e.next); err != nil {
				return nil, err
			}
			affected = append(affected, e)
			moving[e.name] = true
		}
	}
	if len(affected) == 0 {
		return nil, ErrNotFound
	}
	for _, e := range affected {
		if names[e.next] && !moving[e.next] {
			return nil, ErrConflict
		}
	}
	return affected, nil
}

func availableFolderHierarchy(names map[string]bool, destination string) error {
	for name := range names {
		if name == destination || strings.HasPrefix(name, destination+"/") {
			return ErrConflict
		}
	}
	return nil
}

func applyFolderRename(
	ctx context.Context,
	tx pgx.Tx,
	inbox ID,
	newName string,
	names map[string]bool,
	affected []folderRenameEntry,
) error {
	// New destination ancestors are selectable, unsubscribed folders. Leave
	// source ancestors intact, and update each immutable folder in scan order.
	if err := createFolderAncestors(ctx, tx, inbox, newName, names); err != nil {
		return err
	}
	for _, entry := range affected {
		if err := renameFolderEntry(ctx, tx, inbox, entry); err != nil {
			return err
		}
	}
	return nil
}

func renameFolderEntry(ctx context.Context, tx pgx.Tx, inbox ID, entry folderRenameEntry) error {
	prior, err := dbgen.New(tx).
		StoreRenameFolderReadFolderName(ctx, dbgen.StoreRenameFolderReadFolderNameParams{
			Inbox: inbox.String(),
			Name:  entry.next,
		})
	var validity uint32
	if err == nil {
		validity, err = sqlcUint32(prior.Uidvalidity)
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if err == nil && needsNewFolderValidity(validity, entry.validity, prior.Folder, entry.id) {
		next, err := dbgen.New(tx).StoreRenameFolderReadSequence(ctx)
		if err != nil {
			return err
		}
		entry.validity, err = sqlcUint32(next)
		if err != nil {
			return err
		}
	}
	if _, err = dbgen.New(tx).
		StoreRenameFolderUpdateFolder(ctx, dbgen.StoreRenameFolderUpdateFolderParams{
			Name:        entry.next,
			Uidvalidity: int64(entry.validity),
			ID:          entry.id.String(),
		}); err != nil {
		return err
	}
	return recordNewFolderName(ctx, tx, entry.id)
}

func needsNewFolderValidity(prior, current uint32, priorFolder string, id ID) bool {
	return prior > current || prior == current && priorFolder != id.String()
}

type folderListing struct {
	entries       map[string]ListedFolder
	subscriptions map[string]bool
}

func (listing folderListing) readFolders(ctx context.Context, tx pgx.Tx, inbox ID) error {
	rows, err := tx.Query(ctx, dbgen.StoreListFoldersReadFolder, inbox.String())
	if err != nil {
		return err
	}
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			break
		}
		listing.entries[name] = ListedFolder{Name: name, Selectable: true}
	}
	rowErr := rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if rowErr != nil {
		return rowErr
	}
	return nil
}

func (listing folderListing) readSubscriptions(ctx context.Context, tx pgx.Tx, inbox ID, subscribedOnly bool) error {
	rows, err := tx.Query(ctx, dbgen.StoreListFoldersReadSubscription, inbox.String())
	if err != nil {
		return err
	}
	for rows.Next() {
		var name string
		if err = rows.Scan(&name); err != nil {
			break
		}
		listing.subscriptions[name] = true
		e := listing.entries[name]
		e.Name = name
		e.Subscribed = true
		if e.Selectable || subscribedOnly {
			listing.entries[name] = e
		}
	}
	rowErr := rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if rowErr != nil {
		return rowErr
	}
	return nil
}

func (listing folderListing) result(subscribedOnly bool) []ListedFolder {
	// Expose virtual parents for ordinary LIST. Subscribed-only listing retains
	// explicit subscribed names; protocol wildcard parent synthesis is separate.
	if !subscribedOnly {
		listing.addVirtualParents()
	}
	var result []ListedFolder
	for _, entry := range listing.entries {
		entry.Subscribed = listing.subscriptions[entry.Name]
		if !subscribedOnly || entry.Subscribed {
			result = append(result, entry)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

func (listing folderListing) addVirtualParents() {
	var parents []string
	for name := range listing.entries {
		for strings.Contains(name, "/") {
			name = name[:strings.LastIndexByte(name, '/')]
			if name != "" {
				parents = append(parents, name)
			}
		}
	}
	for _, name := range parents {
		if _, exists := listing.entries[name]; !exists {
			listing.entries[name] = ListedFolder{Name: name}
		}
	}
}

func lockInboxTransfer(
	ctx context.Context,
	tx pgx.Tx,
	org, inbox ID,
	source Folder,
	destination string,
	guards mailboxReceiptGuards,
) (map[string]bool, []Mail, error) {
	var err error
	if err = lockInboxNamespace(ctx, tx, org, inbox); err != nil {
		return nil, nil, err
	}
	names, err := lockedInboxFolderNames(ctx, tx, inbox)
	if err != nil {
		return nil, nil, err
	}
	if err = availableFolderHierarchy(names, destination); err != nil {
		return nil, nil, err
	}
	if _, err = getFolderRef(
		ctx,
		tx,
		org,
		inbox,
		folderRef{id: source.ID, validity: source.UIDValidity},
		false,
	); err != nil {
		return nil, nil, err
	}
	current, err := mailboxRows(ctx, tx, source.ID)
	if err != nil {
		return nil, nil, err
	}
	for _, m := range current {
		if !guards.contains(m.Receipt) {
			return nil, nil, ErrBusy
		}
	}
	return names, current, nil
}

func lockedInboxFolderNames(ctx context.Context, tx pgx.Tx, inbox ID) (map[string]bool, error) {
	// Lock all folder IDs in order, matching ordinary rename and COPY/MOVE.
	rows, err := tx.Query(ctx, dbgen.StoreRenameInboxReadFolder, inbox.String())
	if err != nil {
		return nil, err
	}
	names := map[string]bool{}
	for rows.Next() {
		var id, name string
		if err = rows.Scan(&id, &name); err != nil {
			break
		}
		names[name] = true
	}
	rowErr := rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if rowErr != nil {
		return nil, rowErr
	}
	return names, nil
}

func createInboxTransferTarget(
	ctx context.Context,
	tx pgx.Tx,
	inbox ID,
	destination string,
	names map[string]bool,
) (ID, error) {
	var err error
	if err = createFolderAncestors(ctx, tx, inbox, destination, names); err != nil {
		return ID{}, err
	}
	id, err := hotstore.NewID()
	if err != nil {
		return ID{}, err
	}
	if _, err = dbgen.New(tx).
		StoreRenameInboxInsertFolder(ctx, dbgen.StoreRenameInboxInsertFolderParams{
			ID:    id.String(),
			Inbox: inbox.String(),
			Name:  destination,
		}); err != nil {
		return ID{}, err
	}
	if err = recordNewFolderName(ctx, tx, id); err != nil {
		return ID{}, err
	}
	return id, nil
}
