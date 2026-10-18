//go:build linux

package hotstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
)

func options() Options {
	return Options{MaxMessageBytes: 4 << 20, MaxBytes: 128 << 20, MaxConcurrentAppends: 8}
}

func key(t *testing.T) Key {
	t.Helper()
	a, e := NewID()
	requireHotNoError(t, e)
	b, e := NewID()
	requireHotNoError(t, e)
	return Key{a, b}
}

func openStore(t *testing.T, dir string, opts Options) *Store {
	t.Helper()
	s, e := Open(dir, opts)
	requireHotNoError(t, e)
	t.Cleanup(func() {
		if e := s.Close(); e != nil {
			t.Error(e)
		}
	})
	return s
}

func appendBytes(t *testing.T, s *Store, k Key, b []byte) Locator {
	t.Helper()
	loc, e := s.Append(context.Background(), k, bytes.NewReader(b))
	requireHotNoError(t, e)
	return loc
}

func readBytes(t *testing.T, s *Store, loc Locator, off, n int64) []byte {
	t.Helper()
	var out bytes.Buffer
	requireHotNoError(t, s.Read(context.Background(), loc, off, n, &out))
	return out.Bytes()
}

func TestRoundTripRangesAndRestart(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir, options())
	sizes := []int{0, 1, frameSize - 1, frameSize, frameSize + 1, 3*frameSize + 13}
	type saved struct {
		loc  Locator
		body []byte
	}
	var records []saved
	for _, n := range sizes {
		body := make([]byte, n)
		for i := range body {
			body[i] = byte((i*19 + i/127) % 251)
		}
		loc := appendBytes(t, s, key(t), body)
		records = append(records, saved{loc, body})
		requireHotCondition(t, (loc.Size == int64(n) && loc.SHA256 == sha256.Sum256(body)), "invalid locator")
		{
			got := readBytes(t, s, loc, 0, loc.Size)
			requireHotCondition(t, bytes.Equal(got, body), "full read changed bytes")
		}
		for _, off := range []int64{0, int64(n / 2), int64(n)} {
			length := min(int64(79), int64(n)-off)
			{
				got := readBytes(t, s, loc, off, length)
				requireHotCondition(t, bytes.Equal(got, body[off:off+length]), "range mismatch")
			}
		}
		if n > frameSize {
			off := int64(frameSize - 7)
			length := min(int64(30), int64(n)-off)
			{
				got := readBytes(t, s, loc, off, length)
				requireHotCondition(t, bytes.Equal(got, body[off:off+length]), "cross-frame range mismatch")
			}
		}
	}
	requireHotNoError(t, s.Close())
	s = openStore(t, dir, options())
	requireHotCondition(t, len(s.Inventory()) == len(records), "lost record after restart")
	for _, r := range records {
		requireHotCondition(t, bytes.Equal(readBytes(t, s, r.loc, 0, r.loc.Size), r.body), "restart changed bytes")
	}
}

func TestRetryAndLocatorIdentity(t *testing.T) {
	s := openStore(t, t.TempDir(), options())
	k := key(t)
	body := []byte("Message-ID: <not-a-storage-key>\r\n\r\nraw\x00bytes\r\n")
	first := appendBytes(t, s, k, body)
	second := appendBytes(t, s, k, body)
	requireHotCondition(t, (first == second && len(s.Inventory()) == 1), "retry duplicated record")
	{
		_, e := s.Append(context.Background(), k, strings.NewReader("different"))
		requireHotConditionf(t, errors.Is(e, ErrConflict), "want conflict, got %v", e)
	}
	separate := appendBytes(t, s, key(t), body)
	requireHotCondition(t, (first != separate && len(s.Inventory()) == 2), "separate attempts incorrectly deduplicated")
	for _, modify := range []func(*Locator){
		func(l *Locator) { l.File[0] ^= 1 },
		func(l *Locator) { l.Offset++ },
		func(l *Locator) { l.Size++ },
		func(l *Locator) { l.SHA256[0] ^= 1 },
		func(l *Locator) { l.Key.Attempt[0] ^= 1 },
	} {
		wrong := first
		modify(&wrong)
		{
			e := s.Read(context.Background(), wrong, 0, 0, io.Discard)
			requireHotConditionf(t, errors.Is(e, ErrInvalidLocator), "want locator rejection, got %v", e)
		}
	}
	for _, r := range [][2]int64{{-1, 1}, {0, -1}, {first.Size + 1, 0}, {first.Size, 1}, {0, 1 << 62}} {
		{
			e := s.Read(context.Background(), first, r[0], r[1], io.Discard)
			requireHotCondition(t, e != nil, "accepted invalid range")
		}
	}
}

type patternReader struct{ remaining, offset, maxRequest int64 }

func (r *patternReader) Read(b []byte) (int, error) {
	if r.offset < 0 || r.remaining < 0 {
		return 0, ErrCorrupt
	}
	r.maxRequest = max(r.maxRequest, int64(len(b)))
	if r.remaining == 0 {
		return 0, io.EOF
	}
	n := int(min(int64(len(b)), r.remaining))
	for i := range n {
		b[i] = byte((uint64(r.offset) + uint64(i)) % 251)
	}
	r.offset += int64(n)
	r.remaining -= int64(n)
	return n, nil
}

func TestLargeStreamingRecord(t *testing.T) {
	if testing.Short() {
		t.Skip("200 MiB synthetic stream")
	}
	opts := Options{MaxMessageBytes: 200 << 20, MaxBytes: 1 << 30, MaxConcurrentAppends: 1}
	dir := t.TempDir()
	s := openStore(t, dir, opts)
	source := &patternReader{remaining: 200 << 20}
	loc, e := s.Append(context.Background(), key(t), source)
	requireHotNoError(t, e)
	requireHotCondition(t, (source.maxRequest <= frameSize && loc.Size == 200<<20), "streaming bounds violated")
	h := sha256.New()
	e = s.Read(context.Background(), loc, 0, loc.Size, h)
	requireHotNoError(t, e)
	requireHotCondition(t, bytes.Equal(h.Sum(nil), loc.SHA256[:]), "large digest mismatch")
	off := int64(117<<20) + frameSize - 3
	got := readBytes(t, s, loc, off, 29)
	for i, b := range got {
		requireHotCondition(t, b == byte((off+int64(i))%251), "large range mismatch")
	}
	e = s.Close()
	requireHotNoError(t, e)
	s = openStore(t, dir, opts)
	requireHotCondition(t, bytes.Equal(readBytes(t, s, loc, off, 29), got), "large restart mismatch")
	t.Logf("200 MiB streamed, verified and reopened; maximum source read request %d bytes", source.maxRequest)
}

func TestAdmissionAndBadStreams(t *testing.T) {
	opts := options()
	opts.MaxMessageBytes = frameSize + 17
	s := openStore(t, t.TempDir(), opts)
	source := &patternReader{remaining: 1 << 30}
	{
		_, e := s.Append(context.Background(), key(t), source)
		requireHotConditionf(t, errors.Is(e, ErrTooLarge), "want size limit, got %v", e)
	}
	if source.offset != opts.MaxMessageBytes+1 {
		t.Fatalf("over-read %d", source.offset)
	}
	requireHotCondition(t, len(s.Inventory()) == 0, "oversized stream published")
	{
		entries, e := filepath.Glob(filepath.Join(s.dir, "stage-*"))
		requireHotCondition(t, (e == nil && len(entries) <= 0), "staging leak", entries, e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	{
		_, e := s.Append(ctx, key(t), strings.NewReader("x"))
		requireHotCondition(t, errors.Is(e, context.Canceled), e)
	}
	{
		_, e := s.Append(context.Background(), key(t), brokenReader{})
		requireHotCondition(t, errors.Is(e, syscall.EIO), e)
	}
	{
		_, e := s.Append(context.Background(), key(t), emptyReader{})
		requireHotCondition(t, errors.Is(e, io.ErrNoProgress), e)
	}
	// Bounds apply even when maximum size is not a whole frame.
	body := bytes.Repeat([]byte{5}, int(opts.MaxMessageBytes))
	loc := appendBytes(t, s, key(t), body)
	requireHotCondition(t, bytes.Equal(readBytes(t, s, loc, 0, loc.Size), body), "unaligned maximum corrupt")
	// Conservative capacity reservation occurs before reading source bytes.
	tiny := options()
	tiny.MaxBytes = fileHeaderSize + 2*encodedSize(tiny.MaxMessageBytes) - 1
	full := openStore(t, t.TempDir(), tiny)
	r := &patternReader{remaining: 1}
	{
		_, e := full.Append(context.Background(), key(t), r)
		requireHotCondition(t, (errors.Is(e, ErrCapacity) && r.offset == 0), "unsafe admission", e)
	}
}

type brokenReader struct{}

func (brokenReader) Read([]byte) (int, error) { return 0, syscall.EIO }

type emptyReader struct{}

func (emptyReader) Read([]byte) (int, error) { return 0, nil }

type blockedReader struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (r *blockedReader) Read([]byte) (int, error) {
	r.once.Do(func() { close(r.entered) })
	<-r.release
	return 0, io.EOF
}

func TestConcurrentStagingAndAppend(t *testing.T) {
	opts := options()
	opts.MaxConcurrentAppends = 2
	s := openStore(t, t.TempDir(), opts)
	slow := &blockedReader{entered: make(chan struct{}), release: make(chan struct{})}
	defer func() {
		select {
		case <-slow.release:
		default:
			close(slow.release)
		}
	}()
	done := make(chan error, 1)
	slowKey := key(t)
	go func() { _, e := s.Append(context.Background(), slowKey, slow); done <- e }()
	<-slow.entered
	// A blocked source must not monopolize the append writer.
	loc := appendBytes(t, s, key(t), []byte("fast"))
	requireHotCondition(t, string(readBytes(t, s, loc, 0, loc.Size)) == "fast", "fast writer blocked")
	other := &blockedReader{entered: make(chan struct{}), release: make(chan struct{})}
	defer func() {
		select {
		case <-other.release:
		default:
			close(other.release)
		}
	}()
	done2 := make(chan error, 1)
	otherKey := key(t)
	go func() { _, e := s.Append(context.Background(), otherKey, other); done2 <- e }()
	<-other.entered
	{
		_, e := s.Append(context.Background(), key(t), strings.NewReader("too many"))
		requireHotCondition(t, errors.Is(e, ErrCapacity), e)
	}
	close(slow.release)
	close(other.release)
	requireHotNoError(t, <-done)
	requireHotNoError(t, <-done2)
	checkConcurrentRetry(t, s)
}

func checkConcurrentRetry(t *testing.T, s *Store) {
	t.Helper()
	// Multiple callers retry one attempt concurrently: one record is published.
	retryKey := key(t)
	var wg sync.WaitGroup
	results := make(chan Locator, 2)
	errs := make(chan error, 2)
	for range 2 {
		wg.Go(func() {
			l, e := s.Append(context.Background(), retryKey, strings.NewReader("retry"))
			results <- l
			errs <- e
		})
	}
	wg.Wait()
	close(results)
	close(errs)
	var previous Locator
	for l := range results {
		requireHotCondition(t, (previous == (Locator{}) || previous == l), "duplicate attempt")
		previous = l
	}
	for e := range errs {
		requireHotNoError(t, e)
	}
}

func TestExclusiveOwnershipAndClose(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir, options())
	loc := appendBytes(t, s, key(t), []byte("body"))
	if second, e := Open(dir, options()); !errors.Is(e, ErrBusy) {
		if second != nil {
			closeForTest(t, second)
		}
		t.Fatalf("second writer allowed: %v", e)
	}
	requireHotNoError(t, s.Close())
	{
		_, e := s.Append(context.Background(), key(t), strings.NewReader("x"))
		requireHotCondition(t, errors.Is(e, ErrClosed), e)
	}
	{
		e := s.Read(context.Background(), loc, 0, loc.Size, io.Discard)
		requireHotCondition(t, errors.Is(e, ErrClosed), e)
	}
	second := openStore(t, dir, options())
	requireHotCondition(t, string(readBytes(t, second, loc, 0, loc.Size)) == "body", "ownership transfer lost bytes")
}

func TestSyncFailurePoisonsWriter(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir, options())
	old := appendBytes(t, s, key(t), []byte("old"))
	attempt := key(t)
	s.syncData = func() error { return syscall.EIO }
	loc, e := s.Append(context.Background(), attempt, strings.NewReader("uncertain"))
	requireHotCondition(
		t,
		((errors.Is(e, ErrUncertain) && errors.Is(e, syscall.EIO)) && loc == (Locator{})),
		"sync error acknowledged",
		loc,
		e,
	)
	{
		_, e = s.Append(context.Background(), key(t), strings.NewReader("later"))
		requireHotCondition(t, errors.Is(e, ErrUncertain), "continued after sync error", e)
	}
	requireHotCondition(t, string(readBytes(t, s, old, 0, old.Size)) == "old", "old data unavailable")
	e = s.Close()
	requireHotNoError(t, e)
	s = openStore(t, dir, options())
	// This failure injection leaves the bytes readable; restart syncs before retry.
	recovered := appendBytes(t, s, attempt, []byte("uncertain"))
	requireHotCondition(
		t,
		(len(s.Inventory()) == 2 && string(readBytes(t, s, recovered, 0, recovered.Size)) == "uncertain"),
		"uncertain retry duplicated/lost bytes",
	)
}

func flipByte(t *testing.T, path string, off int64) {
	t.Helper()
	// Callers pass their t.TempDir fixture path; opens only to inject a single corrupt byte; owner: FRY
	// maintainers; expires: 2027-01-18.
	// #nosec G304 -- See the path-safety rationale above.
	f, e := os.OpenFile(path, os.O_RDWR, 0o600)
	requireHotNoError(t, e)
	defer closeForTest(t, f)
	b := []byte{0}
	_, e = f.ReadAt(b, off)
	requireHotNoError(t, e)
	b[0] ^= 1
	_, e = f.WriteAt(b, off)
	requireHotNoError(t, e)
	e = f.Sync()
	requireHotNoError(t, e)
}

func TestCorruptionDoesNotTruncateLaterRecords(t *testing.T) {
	for _, part := range []string{"file-header", "record-header", "frame", "footer"} {
		t.Run(part, func(t *testing.T) {
			dir := t.TempDir()
			s := openStore(t, dir, options())
			a := appendBytes(t, s, key(t), bytes.Repeat([]byte{1}, frameSize+2))
			b := appendBytes(t, s, key(t), []byte("later accepted record"))
			closeForTest(t, s)
			off := int64(0)
			switch part {
			case "record-header":
				off = a.Offset + 8
			case "frame":
				off = a.Offset + recordHeaderSize + 8
			case "footer":
				off = a.Offset + encodedSize(a.Size) - footerSize
			}
			path := filepath.Join(dir, "data.fry")
			flipByte(t, path, off)
			// Fixed data.fry under t.TempDir; captures bytes before rejected recovery; owner: FRY maintainers;
			// expires: 2027-01-18.
			// #nosec G304 -- See the path-safety rationale above.
			before, e := os.ReadFile(path)
			requireHotNoError(t, e)
			if got, e := Open(dir, options()); !errors.Is(e, ErrCorrupt) {
				if got != nil {
					closeForTest(t, got)
				}
				t.Fatalf("want corruption, got %v", e)
			}
			{
				e := ReconcileTail(
					dir,
					IncompleteTailError{File: a.File, ValidEnd: a.Offset, ObservedSize: int64(len(before))},
					[]Locator{a, b},
				)
				requireHotConditionf(t, errors.Is(e, ErrCorrupt), "corruption repair not refused: %v", e)
			}
			// Fixed data.fry under t.TempDir; verifies rejected recovery preserved bytes; owner: FRY maintainers;
			// expires: 2027-01-18.
			// #nosec G304 -- See the path-safety rationale above.
			after, e := os.ReadFile(path)
			requireHotNoError(t, e)
			requireHotCondition(t, bytes.Equal(before, after), "corruption handling modified file")
		})
	}
}

func TestReadValidatesTouchedFrames(t *testing.T) {
	s := openStore(t, t.TempDir(), options())
	body := bytes.Repeat([]byte{7}, frameSize*2+10)
	loc := appendBytes(t, s, key(t), body)
	flipByte(t, filepath.Join(s.dir, "data.fry"), frameOffset(loc, 1)+8)
	// A partial range does not claim to validate unread frames.
	{
		got := readBytes(t, s, loc, 0, 10)
		requireHotCondition(t, bytes.Equal(got, body[:10]), "unaffected range failed")
	}
	var out bytes.Buffer
	if e := s.Read(context.Background(), loc, frameSize, 10, &out); !errors.Is(e, ErrCorrupt) || out.Len() != 0 {
		t.Fatal("corrupt frame exposed", e, out.Len())
	}
	out.Reset()
	if e := s.Read(context.Background(), loc, 0, loc.Size, &out); !errors.Is(e, ErrCorrupt) || out.Len() != frameSize {
		t.Fatal("full-read integrity contract", e, out.Len())
	}
}

func TestTailRepairRequiresCompleteProtectedLocators(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir, options())
	a := appendBytes(t, s, key(t), []byte("first"))
	b := appendBytes(t, s, key(t), []byte("second"))
	closeForTest(t, s)
	path := filepath.Join(dir, "data.fry")
	info, e := os.Stat(path)
	requireHotNoError(t, e)
	e = os.Truncate(path, info.Size()-1)
	requireHotNoError(t, e)
	_, e = Open(dir, options())
	var tail *IncompleteTailError
	requireHotConditionf(t, errors.As(e, &tail), "want incomplete tail, got %v", e)
	{
		e = ReconcileTail(dir, *tail, []Locator{a, b})
		requireHotCondition(t, errors.Is(e, ErrInvalidLocator), "truncated protected record", e)
	}
	now, e := os.Stat(path)
	requireHotCondition(t, (e == nil && now.Size() == info.Size()-1), "refused repair mutated bytes")
	stale := *tail
	stale.ObservedSize++
	{
		e = ReconcileTail(dir, stale, []Locator{a})
		requireHotCondition(t, e != nil, "stale tail report accepted")
	}
	// Simulate authority establishing the second record was never accepted.
	e = ReconcileTail(dir, *tail, []Locator{a})
	requireHotNoError(t, e)
	s = openStore(t, dir, options())
	requireHotCondition(
		t,
		(len(s.Inventory()) == 1 && string(readBytes(t, s, a, 0, a.Size)) == "first"),
		"repair lost valid prefix",
	)
	c := appendBytes(t, s, key(t), []byte("after repair"))
	requireHotCondition(t, c.Offset == b.Offset, "did not reclaim tail")
}

// A fresh process writes one attempt and exits at a real I/O transition without
// running deferred cleanup. Parent-owned locators survive outside the process.
func TestCrashHelper(t *testing.T) {
	if os.Getenv("FRY_HOTSTORE_CRASH_HELPER") != "1" {
		return
	}
	dir := os.Getenv("FRY_HOTSTORE_CRASH_DIR")
	point := os.Getenv("FRY_HOTSTORE_CRASH_POINT")
	s, e := Open(dir, options())
	if e != nil {
		if _, writeErr := fmt.Fprintln(os.Stderr, e); writeErr != nil {
			os.Exit(83)
		}
		os.Exit(80)
	}
	s.hook = func(p string) error {
		if p == point {
			os.Exit(77)
		}
		return nil
	}
	var k Key
	if e = json.Unmarshal([]byte(os.Getenv("FRY_HOTSTORE_CRASH_KEY")), &k); e != nil {
		os.Exit(81)
	}
	_, e = s.Append(context.Background(), k, bytes.NewReader(bytes.Repeat([]byte{9}, 2*frameSize+31)))
	if _, writeErr := fmt.Fprintln(os.Stderr, "crash point not reached", e); writeErr != nil {
		os.Exit(83)
	}
	os.Exit(82)
}

func TestProcessCrashRecovery(t *testing.T) {
	for _, point := range []string{"after_stage", "append_chunk", "before_sync", "after_sync"} {
		t.Run(point, func(t *testing.T) {
			dir := t.TempDir()
			s := openStore(t, dir, options())
			old := appendBytes(t, s, key(t), []byte("externally recorded durable bytes"))
			closeForTest(t, s)
			k := key(t)
			encoded, e := json.Marshal(k)
			requireHotNoError(t, e)
			// os.Executable selects the current test binary; fixed test selector; no shell; owner: FRY maintainers;
			// expires: 2027-01-18.
			// #nosec G204 -- See the path-safety rationale above.
			cmd := exec.CommandContext(t.Context(), testExecutable(t), "-test.run=^TestCrashHelper$")
			cmd.Env = append(
				os.Environ(),
				"FRY_HOTSTORE_CRASH_HELPER=1",
				"FRY_HOTSTORE_CRASH_DIR="+dir,
				"FRY_HOTSTORE_CRASH_POINT="+point,
				"FRY_HOTSTORE_CRASH_KEY="+string(encoded),
			)
			output, e := cmd.CombinedOutput()
			var exit *exec.ExitError
			requireHotConditionf(t, (errors.As(e, &exit) && exit.ExitCode() == 77), "child failed: %v %s", e, output)
			recovered, e := Open(dir, options())
			if point == "append_chunk" {
				var tail *IncompleteTailError
				requireHotConditionf(t, errors.As(e, &tail), "want tail, got %v", e)
				e = ReconcileTail(dir, *tail, []Locator{old})
				requireHotNoError(t, e)
				recovered, e = Open(dir, options())
			}
			requireHotNoError(t, e)
			defer closeForTest(t, recovered)
			requireHotCondition(
				t,
				string(readBytes(t, recovered, old, 0, old.Size)) == "externally recorded durable bytes",
				"lost old accepted bytes",
			)
			before := len(recovered.Inventory())
			want := 1
			if point == "before_sync" || point == "after_sync" {
				want = 2
			}
			requireHotConditionf(t, before == want, "recovered %d, want %d", before, want)
			loc := appendBytes(t, recovered, k, bytes.Repeat([]byte{9}, 2*frameSize+31))
			requireHotCondition(
				t,
				(len(recovered.Inventory()) == 2 && loc.Size == 2*frameSize+31),
				"crash retry lost or duplicated bytes",
			)
			stages, e := filepath.Glob(filepath.Join(dir, "stage-*"))
			requireHotCondition(t, (e == nil && len(stages) == 0), "orphan stage not reclaimed", e, stages)
		})
	}
}

func TestInventoryMatchesConcurrentWrites(t *testing.T) {
	s := openStore(t, t.TempDir(), options())
	var wg sync.WaitGroup
	results := make(chan Locator, 8)
	errs := make(chan error, 8)
	for i := range uint8(8) {
		k := key(t)
		wg.Add(1)
		go func(i uint8) {
			defer wg.Done()
			loc, e := s.Append(context.Background(), k, bytes.NewReader(bytes.Repeat([]byte{i}, frameSize+int(i))))
			results <- loc
			errs <- e
		}(i)
	}
	wg.Wait()
	close(results)
	close(errs)
	for e := range errs {
		requireHotNoError(t, e)
	}
	var expected []Locator
	for l := range results {
		expected = append(expected, l)
	}
	actual := s.Inventory()
	sort.Slice(expected, func(i, j int) bool { return expected[i].Offset < expected[j].Offset })
	sort.Slice(actual, func(i, j int) bool { return actual[i].Offset < actual[j].Offset })
	requireHotCondition(t, reflect.DeepEqual(expected, actual), "inventory lost concurrent write")
	for _, loc := range actual {
		requireHotNoError(t, s.Read(context.Background(), loc, 0, loc.Size, io.Discard))
	}
}

func TestPartialAppendErrorStopsWriter(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir, options())
	old := appendBytes(t, s, key(t), []byte("protected"))
	s.hook = func(point string) error {
		if point == "append_chunk" {
			return syscall.ENOSPC
		}
		return nil
	}
	loc, err := s.Append(context.Background(), key(t), bytes.NewReader(bytes.Repeat([]byte{3}, 2*frameSize)))
	requireHotConditionf(
		t,
		((loc == (Locator{}) && errors.Is(err, ErrUncertain)) && errors.Is(err, syscall.ENOSPC)),
		"partial append acknowledged: %v %v",
		loc,
		err,
	)
	{
		_, err = s.Append(context.Background(), key(t), strings.NewReader("later"))
		requireHotCondition(t, errors.Is(err, ErrUncertain), "writer continued", err)
	}
	closeForTest(t, s)
	_, err = Open(dir, options())
	var tail *IncompleteTailError
	requireHotCondition(t, errors.As(err, &tail), "missing partial tail", err)
	err = ReconcileTail(dir, *tail, []Locator{old})
	requireHotNoError(t, err)
	reopened := openStore(t, dir, options())
	requireHotCondition(t, string(readBytes(t, reopened, old, 0, old.Size)) == "protected", "lost protected record")
}

func TestIncompleteRecordBoundaries(t *testing.T) {
	for _, cut := range []int64{
		1,
		recordHeaderSize - 1,
		recordHeaderSize,
		recordHeaderSize + 8,
		recordHeaderSize + frameSize + 11,
		encodedSize(frameSize+19) - 1,
	} {
		t.Run(fmt.Sprint(cut), func(t *testing.T) {
			dir := t.TempDir()
			s := openStore(t, dir, options())
			a := appendBytes(t, s, key(t), []byte("protected"))
			b := appendBytes(t, s, key(t), bytes.Repeat([]byte{6}, frameSize+19))
			closeForTest(t, s)
			requireHotNoError(t, os.Truncate(filepath.Join(dir, "data.fry"), b.Offset+cut))
			_, err := Open(dir, options())
			var tail *IncompleteTailError
			requireHotCondition(t, (errors.As(err, &tail) && tail.ValidEnd == b.Offset), "wrong recovery boundary", err)
			{
				err = ReconcileTail(dir, *tail, []Locator{a, b})
				requireHotCondition(t, errors.Is(err, ErrInvalidLocator), "protected tail discarded", err)
			}
			err = ReconcileTail(dir, *tail, []Locator{a})
			requireHotNoError(t, err)
			s = openStore(t, dir, options())
			requireHotCondition(t, string(readBytes(t, s, a, 0, a.Size)) == "protected", "changed protected prefix")
		})
	}
}

func TestChecksumAndFrameSequenceValidation(t *testing.T) {
	for _, mutation := range []string{"rechecksummed-payload", "frame-index", "oversized-header"} {
		t.Run(mutation, func(t *testing.T) {
			dir := t.TempDir()
			s := openStore(t, dir, options())
			loc := appendBytes(t, s, key(t), []byte("original bytes"))
			// Fixed data.fry under t.TempDir; injects checksummed corruption; owner: FRY maintainers; expires:
			// 2027-01-18.
			// #nosec G304 -- See the path-safety rationale above.
			f, err := os.OpenFile(filepath.Join(dir, "data.fry"), os.O_RDWR, 0o600)
			requireHotNoError(t, err)
			damageRecord(t, f, loc, mutation)
			err = f.Sync()
			requireHotNoError(t, err)
			closeForTest(t, f)
			{
				err = s.Read(context.Background(), loc, 0, loc.Size, io.Discard)
				requireHotCondition(t, errors.Is(err, ErrCorrupt), "mutation not detected during read", err)
			}
			closeForTest(t, s)
			if reopened, err := Open(dir, options()); !errors.Is(err, ErrCorrupt) {
				if reopened != nil {
					closeForTest(t, reopened)
				}
				t.Fatal("mutation not detected on restart", err)
			}
		})
	}
}

type cancelReader struct {
	cancel context.CancelFunc
	read   bool
}

func (r *cancelReader) Read(b []byte) (int, error) {
	if r.read {
		return 0, io.EOF
	}
	r.read = true
	r.cancel()
	b[0] = 'x'
	return 1, nil
}

func TestAppendCancellationAfterStagingReleasesAdmission(t *testing.T) {
	dir := t.TempDir()
	opts := boundedHotOptions(1)
	opts.MaxConcurrentAppends = 1
	emptySize, err := ImageSize()
	requireHotNoError(t, err)
	maxRecordBytes, err := EncodedRecordBytes(opts.MaxMessageBytes)
	requireHotNoError(t, err)
	// One append consumes all available staging admission and the only record slot.
	opts.MaxBytes = emptySize + 2*maxRecordBytes
	s := openStore(t, dir, opts)
	k := key(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	staged := false
	s.hook = func(point string) error {
		if point == "after_stage" {
			staged = true
			cancel()
		}
		return nil
	}
	loc, err := s.Append(ctx, k, strings.NewReader("cancel this fully staged original"))
	requireHotCondition(t, staged, "append did not complete staging before cancellation")
	requireHotConditionf(
		t,
		((loc == (Locator{}) && errors.Is(err, context.Canceled)) && !errors.Is(err, ErrUncertain)),
		"post-staging cancellation must be a clean non-commit: %v %v",
		loc,
		err,
	)
	{
		got := s.Inventory()
		requireHotConditionf(t, len(got) == 0, "canceled append published records: %v", got)
	}
	requireNoStagingFiles(t, dir)
	// A different body for the same key proves cancellation published no identity;
	// successful admission also proves its slot and byte reservations were released.
	s.hook = nil
	body := bytes.Repeat([]byte("r"), 128)
	loc = appendBytes(t, s, k, body)
	{
		got := s.Inventory()
		requireHotConditionf(
			t,
			(len(got) == 1 && got[0] == loc),
			"replacement append did not publish exactly one record: %v",
			got,
		)
	}
	{
		got := readBytes(t, s, loc, 0, loc.Size)
		requireHotCondition(t, bytes.Equal(got, body), "replacement append changed original bytes")
	}
	requireHotNoError(t, s.Close())
	reopened := openStore(t, dir, opts)
	{
		got := readBytes(t, reopened, loc, 0, loc.Size)
		requireHotCondition(t, bytes.Equal(got, body), "replacement original did not survive close and reopen")
	}
}

func requireNoStagingFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	requireHotNoError(t, err)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "stage-") {
			t.Fatalf("canceled append leaked staging: %s", entry.Name())
		}
	}
}

func TestCancellationAndDestinationErrors(t *testing.T) {
	s := openStore(t, t.TempDir(), options())
	ctx, cancel := context.WithCancel(context.Background())
	{
		_, err := s.Append(ctx, key(t), &cancelReader{cancel: cancel})
		requireHotCondition(t, errors.Is(err, context.Canceled), "staging cancellation ignored", err)
	}
	requireHotCondition(t, len(s.Inventory()) == 0, "cancelled staging published")
	loc := appendBytes(t, s, key(t), []byte("valid"))
	{
		err := s.Read(ctx, loc, 0, loc.Size, io.Discard)
		requireHotCondition(t, errors.Is(err, context.Canceled), err)
	}
	{
		err := s.Read(context.Background(), loc, 0, loc.Size, shortWriter{})
		requireHotCondition(t, errors.Is(err, io.ErrShortWrite), "short write ignored", err)
	}
	// A consumer failure does not poison the stored bytes or other readers.
	requireHotCondition(t, string(readBytes(t, s, loc, 0, loc.Size)) == "valid", "destination failure changed record")
}

type shortWriter struct{}

func (shortWriter) Write(b []byte) (int, error) { return len(b) - 1, nil }

func closeForTest(t *testing.T, closer io.Closer) {
	t.Helper()
	if err := closer.Close(); err != nil {
		t.Error("close", err)
	}
}

func testExecutable(t *testing.T) string {
	t.Helper()
	path, err := os.Executable()
	requireHotNoError(t, err)
	return path
}

func TestStagingCleanupPreservesDurableAppendAndRetry(t *testing.T) {
	for _, retry := range []bool{false, true} {
		t.Run(fmt.Sprint(retry), func(t *testing.T) {
			dir := t.TempDir()
			s := openStore(t, dir, options())
			k, body := key(t), []byte("durable original")
			var previous Locator
			if retry {
				previous = appendBytes(t, s, k, body)
			}
			s.hook = removeStagingHook(dir)
			loc, err := s.Append(t.Context(), k, bytes.NewReader(body))
			requireHotConditionf(
				t,
				((err == nil && loc != (Locator{})) && (!retry || loc == previous)),
				"durable result lost: %v %v",
				loc,
				err,
			)
			{
				got := readBytes(t, s, loc, 0, loc.Size)
				requireHotCondition(t, bytes.Equal(got, body), "durable bytes changed")
			}
			{
				_, err = s.Append(t.Context(), key(t), strings.NewReader("later"))
				requireHotCondition(t, errors.Is(err, ErrUncertain), "cleanup failure did not fence later writes", err)
			}
			closeForTest(t, s)
			reopened := openStore(t, dir, options())
			{
				got := appendBytes(t, reopened, k, body)
				requireHotCondition(
					t,
					(got == loc && len(reopened.Inventory()) == 1),
					"durable result did not survive reopen",
				)
			}
		})
	}
}

type terminalErrorReader struct{ err error }

func (r terminalErrorReader) Read([]byte) (int, error) { return 0, r.err }

func TestWrappedEOFDoesNotPublishStaging(t *testing.T) {
	s := openStore(t, t.TempDir(), options())
	for _, terminal := range []error{io.EOF, io.ErrUnexpectedEOF} {
		err := fmt.Errorf("source failed: %w", terminal)
		loc, got := s.Append(
			t.Context(),
			key(t),
			io.MultiReader(strings.NewReader("partial"), terminalErrorReader{err}),
		)
		requireHotConditionf(
			t,
			((loc == (Locator{}) && errors.Is(got, err)) && len(s.Inventory()) == 0),
			"wrapped EOF was accepted: %v %v",
			loc,
			got,
		)
	}
}

func damageRecord(t *testing.T, f *os.File, loc Locator, mutation string) {
	t.Helper()
	var err error
	if mutation == "oversized-header" {
		header, err := recordHeader(loc.Key, maxMessageSize+1, loc.SHA256)
		requireHotNoError(t, err)
		_, err = f.WriteAt(header, loc.Offset)
		requireHotNoError(t, err)
	} else {
		frame := make([]byte, 12+loc.Size)
		_, err = f.ReadAt(frame, frameOffset(loc, 0))
		requireHotNoError(t, err)
		if mutation == "frame-index" {
			frame[4] = 1
		} else {
			frame[8] ^= 1
		}
		putCRC(frame)
		_, err = f.WriteAt(frame, frameOffset(loc, 0))
		requireHotNoError(t, err)
	}
}

func removeStagingHook(dir string) func(string) error {
	return func(point string) error {
		if point != "after_stage" {
			return nil
		}
		stages, err := filepath.Glob(filepath.Join(dir, "stage-*"))
		if err != nil {
			return err
		}
		if len(stages) != 1 {
			return fmt.Errorf("expected one staging file, got %d", len(stages))
		}
		// The open descriptor remains readable; the later unlink will fail.
		return os.Remove(stages[0])
	}
}
