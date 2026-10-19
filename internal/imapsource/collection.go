package imapsource

import (
	"context"
	"fmt"

	"github.com/emersion/go-imap/v2"
)

// CollectionReader preserves collection's broad search and permissive metadata policy.
// Operations are serialized; Close may overlap a body Read.
type (
	CollectionReader struct{ *session }
	// Cleanup is granted only by DialCollection when explicitly enabled.
	Cleanup       struct{ source *session }
	SourceMessage struct {
		Folder           string
		UIDValidity, UID uint32
	}
)

func DialCollection(ctx context.Context, cfg Config, deleteSource bool) (*CollectionReader, *Cleanup, error) {
	s, err := dial(ctx, cfg, true, deleteSource)
	if err != nil {
		return nil, nil, err
	}
	if deleteSource && !s.client.Caps().Has(imap.CapUIDPlus) {
		closeAfterFailure(s)
		return nil, nil, fmt.Errorf("imapcollect: safe source cleanup requires UIDPLUS")
	}
	var cleanup *Cleanup
	if deleteSource {
		cleanup = &Cleanup{s}
	}
	return &CollectionReader{s}, cleanup, nil
}

func (r *CollectionReader) Select(name string) (Epoch, error) {
	if err := r.ready(); err != nil {
		return Epoch{}, err
	}
	r.selected = ""
	r.epoch = 0
	data, err := r.client.Select(name, &imap.SelectOptions{ReadOnly: !r.cleanupEnabled}).Wait()
	if err != nil {
		return Epoch{}, fmt.Errorf("imapcollect: cannot select source folder")
	}
	if data.UIDValidity == 0 {
		return Epoch{}, fmt.Errorf("imapcollect: source returned zero UIDVALIDITY")
	}
	r.selected = folderKey(name)
	r.epoch = data.UIDValidity
	return Epoch{data.UIDValidity, uint32(data.UIDNext)}, nil
}

func (r *CollectionReader) Search() ([]imap.UID, error) {
	if err := r.ready(); err != nil {
		return nil, err
	}
	data, err := r.client.UIDSearch(&imap.SearchCriteria{}, nil).Wait()
	if err != nil {
		return nil, fmt.Errorf("imapcollect: source search failed")
	}
	return boundedUIDs(data)
}

func (r *CollectionReader) Metadata(uid uint32) (Metadata, bool, error) {
	if err := r.ready(); err != nil {
		return Metadata{}, false, err
	}
	messages, err := r.client.Fetch(
		imap.UIDSetNum(imap.UID(uid)),
		&imap.FetchOptions{UID: true, Flags: true, InternalDate: true, RFC822Size: true},
	).
		Collect()
	if err != nil {
		return Metadata{}, false, fmt.Errorf("imapcollect: metadata fetch failed")
	}
	if len(messages) == 0 {
		return Metadata{}, false, nil
	}
	if len(messages) != 1 || uint32(messages[0].UID) != uid || messages[0].RFC822Size < 0 ||
		messages[0].RFC822Size > r.maxBytes {
		return Metadata{}, false, fmt.Errorf("imapcollect: invalid or oversized source message")
	}
	m := messages[0]
	flags := make([]string, len(m.Flags))
	for i, f := range m.Flags {
		flags[i] = string(f)
	}
	return Metadata{UID: uid, Size: m.RFC822Size, InternalDate: m.InternalDate, Flags: flags}, true, nil
}

func (c *Cleanup) Delete(message SourceMessage) error {
	s := c.source
	if err := s.ready(); err != nil {
		return err
	}
	if !s.cleanupEnabled || message.UID == 0 || message.UIDValidity == 0 || folderKey(message.Folder) != s.selected ||
		message.UIDValidity != s.epoch {
		return fmt.Errorf("imapcollect: cleanup source identity mismatch")
	}
	set := imap.UIDSetNum(imap.UID(message.UID))
	if _, err := s.client.Store(
		set, &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagDeleted}}, nil,
	).
		Collect(); err != nil {
		return fmt.Errorf("imapcollect: source cleanup flag failed")
	}
	if _, err := s.client.UIDExpunge(set).Collect(); err != nil {
		return fmt.Errorf("imapcollect: source cleanup expunge failed")
	}
	return nil
}

func boundedUIDs(data *imap.SearchData) ([]imap.UID, error) {
	set, ok := data.All.(imap.UIDSet)
	if !ok {
		return nil, fmt.Errorf("imapcollect: source did not return UID search results")
	}
	var total uint64
	for _, r := range set {
		a, b := uint64(r.Start), uint64(r.Stop)
		if a == 0 || b == 0 {
			return nil, fmt.Errorf("imapcollect: dynamic source search result")
		}
		if a > b {
			a, b = b, a
		}
		total += b - a + 1
		if total > 1000000 {
			return nil, fmt.Errorf("imapcollect: source exceeds prototype search limit")
		}
	}
	return data.AllUIDs(), nil
}
