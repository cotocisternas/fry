//go:build linux

package receiptstore

import (
	"context"

	"github.com/cotocisternas/fry/internal/receiptstore/dbgen"

	"github.com/jackc/pgx/v5"
)

// MailboxSelection binds a protocol session to immutable folder identity. Rename
// does not redirect it, and deletion/recreation of its former name cannot make it
// access the replacement folder. Caller authentication is still checked for each
// operation by the protocol adapter. This handle does not pin message contents.
type MailboxSelection struct {
	store                       *Store
	organization, inbox, folder ID
	validity                    uint32
}
type folderRef struct {
	name     string
	id       ID
	validity uint32
}

func getFolderRef(ctx context.Context, tx pgx.Tx, org, inbox ID, ref folderRef, lock bool) (Folder, error) {
	if ref.id == (ID{}) {
		return getFolder(ctx, tx, org, inbox, ref.name, lock)
	}
	var row dbgen.FolderByIDRow
	var err error
	if lock {
		locked, queryErr := dbgen.New(tx).
			FolderByIDLocked(ctx, dbgen.FolderByIDLockedParams{
				ID:           inbox.String(),
				Organization: org.String(),
				ID_2:         ref.id.String(),
			})
		row, err = dbgen.FolderByIDRow(locked), queryErr
	} else {
		row, err = dbgen.New(tx).
			FolderByID(ctx, dbgen.FolderByIDParams{
				ID:           inbox.String(),
				Organization: org.String(),
				ID_2:         ref.id.String(),
			})
	}
	f, err := folderFromSQL(dbgen.FolderByNameRow(row), err)
	if err == nil && ref.validity != 0 && f.UIDValidity != ref.validity {
		return Folder{}, ErrStale
	}
	return f, err
}

func (s *Store) SelectMailbox(
	ctx context.Context,
	org, inbox ID,
	name string,
) (*MailboxSelection, Folder, []Mail, error) {
	folder, messages, err := s.Mailbox(ctx, org, inbox, name)
	if err != nil {
		return nil, folder, nil, err
	}
	return &MailboxSelection{
		store:        s,
		organization: org,
		inbox:        inbox,
		folder:       folder.ID,
		validity:     folder.UIDValidity,
	}, folder, messages, nil
}

func (m *MailboxSelection) View(ctx context.Context) (Folder, []Mail, error) {
	return m.store.mailbox(ctx, m.organization, m.inbox, folderRef{id: m.folder, validity: m.validity})
}

func (m *MailboxSelection) ChangeFlags(ctx context.Context, uids []uint32, op string, flags []string) error {
	return m.store.changeFlags(
		ctx,
		m.organization,
		m.inbox,
		folderRef{id: m.folder, validity: m.validity},
		uids,
		op,
		flags,
	)
}

func (m *MailboxSelection) Expunge(ctx context.Context, uids []uint32) ([]uint32, error) {
	return m.store.expunge(ctx, m.organization, m.inbox, folderRef{id: m.folder, validity: m.validity}, uids)
}

func (m *MailboxSelection) CopyTo(
	ctx context.Context,
	destination string,
	uids []uint32,
	move bool,
) (CopyResult, error) {
	return m.store.copyMail(
		ctx,
		m.organization,
		m.inbox,
		folderRef{id: m.folder, validity: m.validity},
		destination,
		uids,
		move,
	)
}
