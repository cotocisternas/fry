//go:build linux

package imapserver

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/cotocisternas/fry/internal/receiptstore"

	"github.com/emersion/go-imap/v2"
)

func TestCompactViewFailureRetainsUnemittedSuffix(t *testing.T) {
	failure := errors.New("writer failed")
	for _, failUID := range []uint32{0, 1, 2, 4, 6} {
		t.Run(fmt.Sprint(failUID), func(t *testing.T) {
			backing := make([]receiptstore.Mail, 6)
			for i := range backing {
				backing[i] = receiptstore.Mail{UID: uint32(i + 1), Flags: []string{"old"}}
			}
			emission := compactionEmission{failUID: failUID, failure: failure}
			view, err := compactView(backing, emission.visit)
			visited, seqs := emission.visited, emission.seqs

			requireTestCondition(t, (err != nil) == (failUID != 0), err)
			assertCompactedSuffix(t, backing, view, failUID)

			for i, uid := range visited {
				removedBefore := (uid - 1 + 1) / 2
				expected := uid - removedBefore
				requireTestConditionf(t, seqs[i] == expected, "sequence %v for visited %v", seqs, visited)
			}
			requireTestCondition(t, (failUID == 0 || len(visited) == int(failUID)), "visited suffix after failure")
		})
	}
}

type compactionWire struct {
	conn   net.Conn
	reader *bufio.Reader
	tag    int
}

func newCompactionWire(t *testing.T, f *fixture) *compactionWire {
	t.Helper()
	conn, err := (&net.Dialer{Timeout: time.Second}).DialContext(t.Context(), "tcp", f.address)
	requireTestCondition(t, err == nil, err)
	t.Cleanup(func() { testCleanup(t)(conn.Close()) })
	testCheck(t)(conn.SetDeadline(time.Now().Add(10 * time.Second)))
	w := &compactionWire{conn: conn, reader: bufio.NewReader(conn)}
	{
		_, err = w.reader.ReadString('\n')
		requireTestCondition(t, err == nil, err)
	}
	w.command(t, `LOGIN test synthetic-only-password`)
	w.command(t, `SELECT INBOX`)
	return w
}

func (w *compactionWire) command(t *testing.T, command string) []string {
	t.Helper()
	w.tag++
	tag := fmt.Sprintf("c%d", w.tag)
	{
		_, err := fmt.Fprintf(w.conn, "%s %s\r\n", tag, command)
		requireTestCondition(t, err == nil, err)
	}
	var lines []string
	for {
		line, err := w.reader.ReadString('\n')
		requireTestCondition(t, err == nil, err)
		line = strings.TrimSpace(line)
		lines = append(lines, line)
		if strings.HasPrefix(line, tag+" ") {
			requireTestConditionf(t, strings.HasPrefix(line, tag+" OK"), "command %s failed: %v", command, lines)
			return lines
		}
	}
}

func compactionExpunges(lines []string) []uint32 {
	var result []uint32
	for _, line := range lines {
		var seq uint32
		var name string
		if _, err := fmt.Sscanf(line, "* %d %s", &seq, &name); err == nil && name == "EXPUNGE" {
			result = append(result, seq)
		}
	}
	return result
}

func compactionMail(t *testing.T, f *fixture) {
	t.Helper()
	for i := range 6 {
		f.add(t, fmt.Sprintf("Subject: compaction %d\r\n\r\nbody %d\r\n", i, i))
	}
}

func TestWireConsecutiveAndInterleavedExpungeMove(t *testing.T) {
	for _, operation := range []string{"expunge", "move"} {
		t.Run(operation, func(t *testing.T) {
			f := setup(t)
			compactionMail(t, f)
			w := newCompactionWire(t, f)
			var lines []string
			if operation == "expunge" {
				w.command(t, `UID STORE 1,2,4,6 +FLAGS.SILENT (\Deleted)`)
				lines = w.command(t, `UID EXPUNGE 1,2,4,6`)
			} else {
				w.command(t, `CREATE Archive`)
				lines = w.command(t, `UID MOVE 1,2,4,6 Archive`)
				assertMoveCopyOrder(t, lines)
			}
			{
				got := compactionExpunges(lines)
				requireTestConditionf(
					t,
					reflect.DeepEqual(got, []uint32{1, 1, 2, 3}),
					"EXPUNGE numbers %v: %v",
					got,
					lines,
				)
			}
			fetched := strings.Join(w.command(t, `FETCH 1:* (UID)`), "\n")
			requireTestConditionf(
				t,
				(strings.Contains(fetched, "* 1 FETCH (UID 3)") && strings.Contains(fetched, "* 2 FETCH (UID 5)")),
				"survivors shifted incorrectly %s",
				fetched,
			)
			if operation == "move" {
				selected := strings.Join(w.command(t, `SELECT Archive`), "\n")
				requireTestCondition(t, strings.Contains(selected, "* 4 EXISTS"), selected)
			}
		})
	}
}

func TestWirePollRetainsTombstonesAndUpdatesFlags(t *testing.T) {
	f := setup(t)
	compactionMail(t, f)
	waiting := newCompactionWire(t, f)
	actor := f.client(t)
	{
		_, err := actor.Select("INBOX", nil).Wait()
		requireTestCondition(t, err == nil, err)
	}
	{
		_, err := actor.Store(
			imap.UIDSetNum(1, 2, 4, 6),
			&imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagDeleted}},
			nil,
		).
			Collect()
		requireTestCondition(t, err == nil, err)
	}
	{
		_, err := actor.UIDExpunge(imap.UIDSetNum(1, 2, 4, 6)).Collect()
		requireTestCondition(t, err == nil, err)
	}
	{
		_, err := actor.Store(
			imap.UIDSetNum(5),
			&imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: []imap.Flag{imap.FlagSeen}},
			nil,
		).
			Collect()
		requireTestCondition(t, err == nil, err)
	}
	before := waiting.command(t, `FETCH 3,5 (UID FLAGS)`)
	requireTestConditionf(t, len(compactionExpunges(before)) == 0, "FETCH removed tombstones: %v", before)
	joined := strings.Join(before, "\n")
	requireTestConditionf(
		t,
		((strings.Contains(joined, "* 3 FETCH (UID 3") &&
			strings.Contains(joined, "* 5 FETCH (UID 5")) &&
			strings.Contains(joined, `\Seen`)),
		"tombstone slots/flags lost: %v",
		before,
	)
	after := waiting.command(t, `NOOP`)
	{
		got := compactionExpunges(after)
		requireTestConditionf(
			t,
			reflect.DeepEqual(got, []uint32{1, 1, 2, 3}),
			"poll EXPUNGE numbers %v: %v",
			got,
			after,
		)
	}
	final := strings.Join(waiting.command(t, `FETCH 2 (UID FLAGS)`), "\n")
	requireTestConditionf(
		t,
		(strings.Contains(final, "* 2 FETCH (UID 5") && strings.Contains(final, `\Seen`)),
		"surviving flags lost: %s",
		final,
	)
}

var compactionBenchmarkSink uint64

// BenchmarkCompactView includes the linear copy that restores its reusable
// input. It isolates slice work from SMTP, SQL, mailbox loading and TLS; results
// are algorithm evidence only, with no timing threshold or capacity claim.
func BenchmarkCompactView(b *testing.B) {
	for _, size := range []int{1000, 10000, 100000} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			seed := make([]receiptstore.Mail, size)
			for i := range seed {
				seed[i].UID = uint32(i + 1)
			}
			work := make([]receiptstore.Mail, size)
			var emitted uint64
			visit := func(seq uint32, m receiptstore.Mail) (receiptstore.Mail, bool, error) {
				remove := m.UID%3 != 0
				if remove {
					emitted += uint64(seq)
				}
				return m, remove, nil
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				copy(work, seed)
				if _, err := compactView(work, visit); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/(float64(b.N)*float64(size)), "ns/slot")
			compactionBenchmarkSink = emitted
		})
	}
}

func assertCompactedSuffix(t *testing.T, backing, view []receiptstore.Mail, failUID uint32) {
	t.Helper()
	var want []uint32
	for uid := uint32(1); uid <= 6; uid++ {
		if (failUID != 0 && uid >= failUID) || uid%2 == 0 {
			want = append(want, uid)
		}
	}
	got := make([]uint32, len(view))
	for i, m := range view {
		got[i] = m.UID
		flags := "updated"
		if failUID != 0 && m.UID >= failUID {
			flags = "old"
		}
		requireTestConditionf(t, (len(m.Flags) == 1 && m.Flags[0] == flags), "lost update/suffix %+v", view)
	}
	requireTestConditionf(t, reflect.DeepEqual(got, want), "got %v want %v", got, want)
	for _, m := range backing[len(view):] {
		requireTestConditionf(t, reflect.DeepEqual(m, receiptstore.Mail{}), "unused reference retained %+v", m)
	}
}

func assertMoveCopyOrder(t *testing.T, lines []string) {
	t.Helper()
	copyIndex, expungeIndex := -1, -1
	for i, line := range lines {
		if strings.Contains(line, "COPYUID") {
			copyIndex = i
		}
		if strings.Contains(line, "EXPUNGE") && expungeIndex < 0 {
			expungeIndex = i
		}
	}
	requireTestConditionf(t, (copyIndex >= 0 && copyIndex < expungeIndex), "MOVE copy data order %v", lines)
}

type compactionEmission struct {
	failUID       uint32
	failure       error
	visited, seqs []uint32
}

func (e *compactionEmission) visit(seq uint32, m receiptstore.Mail) (receiptstore.Mail, bool, error) {
	e.visited = append(e.visited, m.UID)
	e.seqs = append(e.seqs, seq)
	if m.UID == e.failUID {
		return receiptstore.Mail{}, true, e.failure
	}
	if m.UID%2 == 1 {
		return m, true, nil
	}
	m.Flags = []string{"updated"}
	return m, false, nil
}
