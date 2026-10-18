//go:build linux

// Package hotstore implements the first local, Linux-only byte-storage spike.
// Append success means one local fsync completed, not that FRY accepted email.
// Replication, database authority, authorization and reference lifecycle belong
// to the future receiptstore module. The on-disk format is experimental.
package hotstore

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

var (
	ErrBusy      = errors.New("hotstore: directory already locked")
	ErrClosed    = errors.New("hotstore: closed")
	ErrCapacity  = errors.New("hotstore: local admission budget exhausted")
	ErrTooLarge  = errors.New("hotstore: message exceeds configured limit")
	ErrUncertain = errors.New("hotstore: append outcome uncertain; close and reconcile before writing again")
)

// Options bounds admission, disk use and concurrent staging. MaxBytes includes
// the append file and worst-case temporary copy reservations, not filesystem
// metadata or unrelated files. Actual ENOSPC remains an I/O failure.
type Options struct {
	MaxMessageBytes      int64
	MaxBytes             int64
	MaxConcurrentAppends int
	MaxRecords           int64
	MaxFileBytes         int64
}

// Store owns a private directory on a local filesystem, held under flock.
// The directory must exist, and its parent chain must already be durable.
// Do not replace the lock file or let another process modify store files.
type Store struct {
	life                         sync.RWMutex
	mu                           sync.Mutex
	file, lock                   *os.File
	dir                          string
	id                           ID
	opts                         Options
	size, reserved               int64
	pendingRecords, pendingBytes int64
	active                       int
	records                      map[Key]Locator
	closed                       bool
	poisoned                     bool
	// Fault seams for tests; production always uses the actual file Sync.
	syncData func() error
	hook     func(string) error
}

func lockDirectory(dir string) (*os.File, error) {
	// Caller selects an existing private store directory; Stat verifies directory type; owner: FRY maintainers;
	// expires: 2027-01-18.
	// #nosec G703 -- See the path-safety rationale above.
	info, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("hotstore: not a directory")
	}
	// Fixed writer.lock in verified caller-owned private directory; directory mutation is forbidden by Store
	// contract; owner: FRY maintainers; expires: 2027-01-18.
	// #nosec G304 G703 -- See the path-safety rationale above.
	f, err := os.OpenFile(filepath.Join(dir, "writer.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return nil, errors.Join(fmt.Errorf("%w: %w", ErrBusy, err), f.Close())
	}
	return f, nil
}

func syncDirectory(dir string) error {
	// Only syncs the caller-owned store directory; never interprets record data as a path; owner: FRY maintainers;
	// expires: 2027-01-18.
	// #nosec G304 G703 -- See the path-safety rationale above.
	f, e := os.Open(dir)
	if e != nil {
		return e
	}
	return errors.Join(f.Sync(), f.Close())
}

// Open validates every complete record before allowing reads or appends. It
// never automatically truncates a damaged or incomplete append file. Orphan
// staging files are discarded under the exclusive lock: no locator ever points
// to staging, and a complete append copy is required before Append returns.
func Open(dir string, opts Options) (_ *Store, err error) {
	if err := ValidateOptions(opts); err != nil {
		return nil, err
	}
	lock, err := lockDirectory(dir)
	if err != nil {
		return nil, err
	}
	var f *os.File
	defer func() {
		if err != nil {
			if f != nil {
				err = errors.Join(err, f.Close())
			}
			err = errors.Join(err, lock.Close())
		}
	}()
	f, err = openAppendFile(dir)
	if err != nil {
		return nil, err
	}
	id, records, size, err := scan(f, opts)
	if err != nil {
		return nil, err
	}
	// Complete records may have survived a lost pre-fsync reply in the page cache.
	// Make them durable before retry can return their recovered locator.
	if err = f.Sync(); err != nil {
		return nil, err
	}
	if err = cleanStagingFiles(dir); err != nil {
		return nil, err
	}
	return &Store{
		file:     f,
		lock:     lock,
		dir:      dir,
		id:       id,
		opts:     opts,
		size:     size,
		records:  records,
		syncData: f.Sync,
	}, nil
}

// Caller owns cleanup even when initialization fails after opening the file.
func openAppendFile(dir string) (*os.File, error) {
	path := filepath.Join(dir, "data.fry")
	// Fixed data.fry in private directory under writer lock; exclusive creation prevents replacement; owner: FRY
	// maintainers; expires: 2027-01-18.
	// #nosec G304 G703 -- See the path-safety rationale above.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err == nil {
		if err = initializeAppendFile(f, dir); err != nil {
			return f, err
		}
	} else if errors.Is(err, os.ErrExist) {
		// Fixed existing data.fry in caller-owned private directory under writer lock; full scan follows; owner:
		// FRY maintainers; expires: 2027-01-18.
		// #nosec G304 G703 -- See the path-safety rationale above.
		f, err = os.OpenFile(path, os.O_RDWR, 0o600)
		if err != nil {
			return f, err
		}
	} else {
		return f, err
	}
	return f, nil
}

func initializeAppendFile(f *os.File, dir string) error {
	id, e := NewID()
	if e != nil {
		return e
	}
	if e = writeAll(f, fileHeader(id)); e != nil {
		return e
	}
	if e = f.Sync(); e != nil {
		return e
	}
	if e = syncDirectory(dir); e != nil {
		return e
	}
	return nil
}

func cleanStagingFiles(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if len(entry.Name()) > 6 && entry.Name()[:6] == "stage-" && !entry.IsDir() {
			// ReadDir yields one basename; stage prefix and non-directory checks restrict deletion under writer
			// lock; owner: FRY maintainers; expires: 2027-01-18.
			// #nosec G703 -- See the path-safety rationale above.
			if err = os.Remove(filepath.Join(dir, entry.Name())); err != nil {
				return err
			}
		}
	}
	if err = syncDirectory(dir); err != nil {
		return err
	}
	return nil
}

// Inventory is a snapshot of complete records, including writes whose replies
// may have been lost. It does not establish that any receipt was accepted by FRY.
// The prototype keeps this index in memory; this is not production-scale sizing.
func (s *Store) Inventory() []Locator {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Locator, 0, len(s.records))
	for _, loc := range s.records {
		out = append(out, loc)
	}
	return out
}

func (s *Store) fault(point string) error {
	if s.hook != nil {
		return s.hook(point)
	}
	return nil
}

// Append stages a bounded stream outside the append lock, then appends and
// fsyncs the complete framed record. Concurrent staging is bounded and excess
// work is rejected with ErrCapacity. Retry the same Key after a lost reply;
// identical content returns the same locator, different content ErrConflict.
// Context is checked between I/O calls: callers must cancel/close any source
// Reader whose Read can block. Cancellation after append begins is ambiguous.
func (s *Store) Append(ctx context.Context, key Key, src io.Reader) (result Locator, resultErr error) {
	var none Locator
	if !validAttempt(key) || src == nil {
		return none, fmt.Errorf("hotstore: invalid attempt")
	}
	if err := ctx.Err(); err != nil {
		return none, err
	}
	s.life.RLock()
	defer s.life.RUnlock()
	reservation := 2 * encodedSize(s.opts.MaxMessageBytes)
	growth, err := s.reserveAppend(key, reservation)
	if err != nil {
		return none, err
	}
	defer s.releaseAppend(reservation, growth)
	stage, err := os.CreateTemp(s.dir, "stage-")
	if err != nil {
		return none, err
	}
	defer func() {
		if cleanupErr := s.cleanStage(stage); cleanupErr != nil && resultErr != nil {
			result = Locator{}
			resultErr = errors.Join(resultErr, cleanupErr)
		}
	}()
	loc, err := s.stage(ctx, stage, key, src)
	if err != nil {
		return none, err
	}
	if err = s.fault("after_stage"); err != nil {
		return none, err
	}
	return s.appendStaged(ctx, stage, loc)
}

// reserveAppend and releaseAppend account for staging outside the append lock.
func (s *Store) reserveAppend(key Key, reservation int64) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, ErrClosed
	}
	if s.poisoned {
		return 0, ErrUncertain
	}
	if s.active >= s.opts.MaxConcurrentAppends || reservation > s.opts.MaxBytes-s.size-s.reserved {
		return 0, ErrCapacity
	}
	growth := int64(0)
	if _, exists := s.records[key]; !exists && s.opts.MaxRecords > 0 {
		growth = encodedSize(s.opts.MaxMessageBytes)
		if s.pendingRecords >= s.opts.MaxRecords-int64(len(s.records)) ||
			growth > s.opts.MaxFileBytes-s.size-s.pendingBytes {
			return 0, ErrCapacity
		}
		s.pendingRecords++
		s.pendingBytes += growth
	}
	s.active++
	s.reserved += reservation
	return growth, nil
}

func (s *Store) releaseAppend(reservation, growth int64) {
	s.mu.Lock()
	s.active--
	s.reserved -= reservation
	if growth > 0 {
		s.pendingRecords--
		s.pendingBytes -= growth
	}
	s.mu.Unlock()
}

// Cleanup cannot revoke a durable success. Fence later writes when staging leaks.
func (s *Store) cleanStage(stage *os.File) error {
	cleanupErr := errors.Join(stage.Close(), os.Remove(stage.Name()))
	if cleanupErr == nil {
		return nil
	}
	s.mu.Lock()
	s.poisoned = true
	s.mu.Unlock()
	return fmt.Errorf("%w: staging cleanup: %w", ErrUncertain, cleanupErr)
}

func (s *Store) appendStaged(ctx context.Context, stage *os.File, loc Locator) (Locator, error) {
	var none Locator
	var err error
	key := loc.Key
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.poisoned {
		return none, ErrUncertain
	}
	if err = ctx.Err(); err != nil {
		return none, err
	}
	if old, ok := s.records[key]; ok {
		if old.Size != loc.Size || old.SHA256 != loc.SHA256 {
			return none, ErrConflict
		}
		return old, nil
	}
	if s.atFileCapacity(loc.Size) {
		return none, ErrCapacity
	}
	loc.File, loc.Offset = s.id, s.size
	if _, err = stage.Seek(0, io.SeekStart); err != nil {
		return none, err
	}
	if _, err = s.file.Seek(s.size, io.SeekStart); err != nil {
		return none, err
	}
	// Once any append write starts, errors cannot be interpreted as non-commit.
	fail := func(e error) (Locator, error) { s.poisoned = true; return none, fmt.Errorf("%w: %w", ErrUncertain, e) }
	if err := s.copyAndSyncStage(ctx, stage); err != nil {
		return fail(err)
	}
	s.records[key] = loc
	s.size += encodedSize(loc.Size)
	return loc, nil
}

// Caller holds mu; any failure after this boundary poisons the append authority.
func (s *Store) copyAndSyncStage(ctx context.Context, stage *os.File) error {
	var err error
	buf := make([]byte, frameSize)
	for {
		if err = ctx.Err(); err != nil {
			return err
		}
		n, e := stage.Read(buf)
		if n > 0 {
			if err = writeAll(s.file, buf[:n]); err != nil {
				return err
			}
			if err = s.fault("append_chunk"); err != nil {
				return err
			}
		}
		if e == io.EOF {
			break
		}
		if e != nil {
			return e
		}
	}
	return s.syncAppend()
}

func (s *Store) syncAppend() error {
	var err error
	if err = s.fault("before_sync"); err != nil {
		return err
	}
	if err = s.syncData(); err != nil {
		return err
	}
	if err = s.fault("after_sync"); err != nil {
		return err
	}
	return nil
}

func (s *Store) stage(ctx context.Context, f *os.File, key Key, src io.Reader) (Locator, error) {
	loc := Locator{Key: key}
	if err := writeAll(f, make([]byte, recordHeaderSize)); err != nil {
		return loc, err
	}
	hash := sha256.New()
	buf := make([]byte, frameSize+12)
	for index := uint32(0); ; index++ {
		if err := ctx.Err(); err != nil {
			return loc, err
		}
		n, done, err := readStageFrame(ctx, src, buf, s.opts.MaxMessageBytes-loc.Size)
		if err != nil {
			return loc, err
		}

		if n > 0 {
			loc.Size += int64(n)
			if err := writeStagedFrame(f, hash, buf, n, index); err != nil {
				return loc, err
			}
		}

		if done {
			break
		}
	}
	copy(loc.SHA256[:], hash.Sum(nil))
	return finishStage(f, loc)
}

// Read only one byte beyond the remaining allowance, even for an untrusted stream.
func readStageFrame(ctx context.Context, src io.Reader, buf []byte, remaining int64) (int, bool, error) {
	limit := min(int64(frameSize), remaining+1)
	n, err := io.ReadFull(&contextReader{ctx: ctx, r: src}, buf[8:8+limit])
	if int64(n) > remaining {
		return n, false, ErrTooLarge
	}
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		return n, true, nil
	}
	return n, false, err
}

func writeStagedFrame(f *os.File, hash io.Writer, buf []byte, n int, index uint32) error {
	if n < 0 || n > frameSize {
		return ErrTooLarge
	}
	if _, err := hash.Write(buf[8 : 8+n]); err != nil {
		return err
	}
	binary.LittleEndian.PutUint32(buf[:4], uint32(n))
	binary.LittleEndian.PutUint32(buf[4:8], index)
	b := buf[:8+n+4]
	putCRC(b)
	return writeAll(f, b)
}

func finishStage(f *os.File, loc Locator) (Locator, error) {
	header, err := recordHeader(loc.Key, loc.Size, loc.SHA256)
	if err != nil {
		return loc, err
	}
	end, err := footer(header, loc.Size)
	if err != nil {
		return loc, err
	}
	if err := writeAll(f, end); err != nil {
		return loc, err
	}
	if n, err := f.WriteAt(header, 0); err != nil {
		return loc, err
	} else if n != len(header) {
		return loc, io.ErrShortWrite
	}
	return loc, nil
}

type contextReader struct {
	ctx   context.Context
	r     io.Reader
	empty int
}

func (r *contextReader) Read(b []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.r.Read(b)
	if n == 0 && err == nil {
		r.empty++
		if r.empty >= 100 {
			return 0, io.ErrNoProgress
		}
	} else {
		r.empty = 0
	}
	return n, err
}

// Read writes exactly the requested payload range, validating each complete
// touched frame before exposing its bytes. A full read also verifies SHA-256.
// Earlier bytes can already have reached dst if a later frame/digest fails.
// dst must support cancellation itself if Write may block. No retention or
// authorization semantics are provided by this internal local adapter.
func (s *Store) Read(ctx context.Context, loc Locator, offset, length int64, dst io.Writer) error {
	if offset < 0 || length < 0 || offset > loc.Size || length > loc.Size-offset || dst == nil {
		return fmt.Errorf("hotstore: invalid read range")
	}
	s.life.RLock()
	defer s.life.RUnlock()
	s.mu.Lock()
	closed := s.closed
	expected, ok := s.records[loc.Key]
	s.mu.Unlock()
	if closed {
		return ErrClosed
	}
	if !ok || expected != loc {
		return ErrInvalidLocator
	}
	return readRecord(ctx, s.file, loc, offset, length, dst)
}

// Close waits for active reads/appends. It cannot interrupt a blocked caller's
// Reader/Writer. Closing does not convert an uncertain append into an ACK.
func (s *Store) Close() error {
	s.life.Lock()
	defer s.life.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return errors.Join(s.file.Close(), s.lock.Close())
}

// FileID returns this open file's immutable incarnation for preregistering a
// write target. It does not assert current health or authorize any write.
func (s *Store) FileID() ID { return s.id }

func validAttempt(key Key) bool { return key.Receipt != (ID{}) && key.Attempt != (ID{}) }

// Caller holds mu; pending reservations are handled separately by admission.
func (s *Store) atFileCapacity(size int64) bool {
	return s.opts.MaxRecords > 0 &&
		(int64(len(s.records)) >= s.opts.MaxRecords || encodedSize(size) > s.opts.MaxFileBytes-s.size)
}
