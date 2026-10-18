//go:build linux

package imapserver

import (
	"reflect"
	"slices"
	"time"

	"github.com/cotocisternas/fry/internal/receiptstore"

	"github.com/emersion/go-imap/v2"
	wire "github.com/emersion/go-imap/v2/imapserver"
)

func flags(m receiptstore.Mail) []imap.Flag {
	result := make([]imap.Flag, len(m.Flags))
	for i, f := range m.Flags {
		result[i] = imap.Flag(f)
	}
	return result
}

func hasFlag(m receiptstore.Mail, flag string) bool {
	return slices.Contains(m.Flags, flag)
}

// current never replaces the session's sequence-number view. Until EXPUNGE is
// permitted, vanished UIDs keep their old slots; no other message takes a slot.
func (s *session) current() (map[uint32]receiptstore.Mail, error) {
	ctx, c := s.operation()
	defer c()
	f, m, err := s.selection.View(ctx)
	if err != nil {
		return nil, safe(err)
	}
	if f.UIDValidity != s.validity {
		closeAfterFailure(s.conn.NetConn())
		return nil, no("Mailbox identity changed")
	}
	result := make(map[uint32]receiptstore.Mail, len(m))
	for _, msg := range m {
		result[msg.UID] = msg
	}
	return result, nil
}

func (s *session) Poll(w *wire.UpdateWriter, allowExpunge bool) error {
	if s.selection == nil {
		return nil
	}
	ctx, c := s.operation()
	defer c()
	f, now, err := s.selection.View(ctx)
	if err != nil {
		return safe(err)
	}
	if f.UIDValidity != s.validity {
		closeAfterFailure(s.conn.NetConn())
		return no("Mailbox identity changed")
	}
	if !reflect.DeepEqual(s.keywords, f.Keywords) {
		if err = w.WriteMailboxFlags(mailboxFlags(f.Keywords)); err != nil {
			return err
		}
		s.keywords = f.Keywords
	}
	current := map[uint32]receiptstore.Mail{}
	for _, m := range now {
		current[m.UID] = m
	}
	s.view, err = compactView(s.view, func(seq uint32, old receiptstore.Mail) (receiptstore.Mail, bool, error) {
		return pollMessage(w, current, allowExpunge, seq, old)
	})
	if err != nil {
		return err
	}
	added := s.appendVisible(now)

	count, err := wireCount(len(s.view))
	if err != nil {
		return err
	}
	if added {
		return w.WriteNumMessages(count)
	}
	return nil
}

func (s *session) Idle(w *wire.UpdateWriter, stop <-chan struct{}) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return nil
		case <-s.server.ctx.Done():
			return s.server.ctx.Err()
		case <-ticker.C:
			if err := s.Poll(w, true); err != nil {
				return err
			}
		}
	}
}

func rangeContains(start, stop, n, maximum uint32) bool {
	if start == 0 {
		start = maximum
	}
	if stop == 0 {
		stop = maximum
	}
	if start > stop {
		start, stop = stop, start
	}
	return n >= start && n <= stop && n != 0
}

func (s *session) matches(set imap.NumSet, seq, uid uint32) bool {
	count, err := wireCount(len(s.view))
	if err != nil {
		return false
	}
	var maxUID uint32
	if len(s.view) > 0 {
		maxUID = s.view[len(s.view)-1].UID
	}
	switch set := set.(type) {
	case imap.SeqSet:
		for _, r := range set {
			if rangeContains(r.Start, r.Stop, seq, count) {
				return true
			}
		}
	case imap.UIDSet:
		for _, r := range set {
			if rangeContains(uint32(r.Start), uint32(r.Stop), uid, maxUID) {
				return true
			}
		}
	}
	return false
}

func (s *session) selectedUIDs(set imap.NumSet) []uint32 {
	var result []uint32
	for i, m := range s.view {
		if s.matches(set, uint32(i+1), m.UID) {
			result = append(result, m.UID)
		}
	}
	return result
}

func (s *session) Store(w *wire.FetchWriter, set imap.NumSet, f *imap.StoreFlags, _ *imap.StoreOptions) error {
	if s.readOnly {
		return no("Mailbox is read-only")
	}
	op := storeOperation(f.Op)
	values := make([]string, len(f.Flags))
	for i, v := range f.Flags {
		values[i] = string(v)
	}
	ctx, c := s.operation()
	defer c()
	err := s.selection.ChangeFlags(ctx, s.selectedUIDs(set), op, values)
	if err != nil {
		return safe(err)
	}
	current, err := s.current()
	if err != nil {
		return err
	}
	return s.writeStoredFlags(w, set, f.Silent, current)
}

func (s *session) Expunge(w *wire.ExpungeWriter, set *imap.UIDSet) error {
	// CLOSE on an EXAMINE mailbox succeeds without deleting anything.
	if s.readOnly {
		return nil
	}
	var selected []uint32
	for i, m := range s.view {
		if set == nil || s.matches(*set, uint32(i+1), m.UID) {
			selected = append(selected, m.UID)
		}
	}
	ctx, c := s.operation()
	defer c()
	_, err := s.selection.Expunge(ctx, selected)
	if err != nil {
		return safe(err)
	}
	// Include removals by other sessions so emitted sequence numbers remain valid.
	current, err := s.current()
	if err != nil {
		return err
	}
	s.view, err = compactView(s.view, func(seq uint32, m receiptstore.Mail) (receiptstore.Mail, bool, error) {
		if _, exists := current[m.UID]; exists {
			return m, false, nil
		}
		return m, true, w.WriteExpunge(seq)
	})
	return err
}

// compactView visits each original slot once. The current sequence number counts
// only retained slots, so consecutive removals emit the same sequence number.
// A failed callback preserves its original slot and the untouched suffix while
// retaining all earlier successful updates/removals. Unused slots are cleared.
func compactView(
	view []receiptstore.Mail,
	visit func(uint32, receiptstore.Mail) (receiptstore.Mail, bool, error),
) ([]receiptstore.Mail, error) {
	kept := 0
	for i, old := range view {
		next, remove, err := visit(uint32(kept+1), old)
		if err != nil {
			kept += copy(view[kept:], view[i:])
			clear(view[kept:])
			return view[:kept], err
		}
		if !remove {
			view[kept] = next
			kept++
		}
	}
	clear(view[kept:])
	return view[:kept], nil
}

func pollMessage(
	w *wire.UpdateWriter,
	current map[uint32]receiptstore.Mail,
	allowExpunge bool,
	seq uint32,
	old receiptstore.Mail,
) (receiptstore.Mail, bool, error) {
	m, exists := current[old.UID]
	if !exists && allowExpunge {
		return old, true, w.WriteExpunge(seq)
	}
	if exists && !reflect.DeepEqual(old.Flags, m.Flags) {
		return m, false, w.WriteMessageFlags(seq, imap.UID(m.UID), flags(m))
	}
	return old, false, nil
}

func (s *session) appendVisible(now []receiptstore.Mail) bool {
	var highest uint32
	for _, m := range s.view {
		if m.UID > highest {
			highest = m.UID
		}
	}
	added := false
	for _, m := range now {
		if m.UID > highest {
			s.view = append(s.view, m)
			added = true
		}
	}
	return added
}

func storeOperation(op imap.StoreFlagsOp) string {
	switch op {
	case imap.StoreFlagsSet:
		return "set"
	case imap.StoreFlagsAdd:
		return "add"
	case imap.StoreFlagsDel:
		return "remove"
	}
	return ""
}

func (s *session) writeStoredFlags(
	w *wire.FetchWriter,
	set imap.NumSet,
	silent bool,
	current map[uint32]receiptstore.Mail,
) error {
	for i, old := range s.view {
		if !s.matches(set, uint32(i+1), old.UID) {
			continue
		}
		m, ok := current[old.UID]
		if !ok {
			continue
		}
		s.view[i] = m
		if !silent {
			response := w.CreateMessage(uint32(i + 1))
			response.WriteUID(imap.UID(m.UID))
			response.WriteFlags(flags(m))
			if err := response.Close(); err != nil {
				return err
			}
		}
	}
	return nil
}
