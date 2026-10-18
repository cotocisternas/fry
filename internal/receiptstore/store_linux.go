//go:build linux

package receiptstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cotocisternas/fry/internal/hotstore"
	"github.com/cotocisternas/fry/internal/receiptstore/dbgen"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Store owns receipt acceptance, but not its supplied pool or members. The
// caller must not close these dependencies while operations are running.
type Store struct {
	headerCaptures   chan struct{}
	headerParses     chan struct{}
	cleanupSelection chan struct{}
	cleanupCycle     *cleanupCycle
	mailboxSelection chan struct{}
	mailboxCursor    string
	cold             atomic.Pointer[Packer]
	db               *authority
	members          []Member
	shards           []IngestShard
	memberByID       map[ID]Member
	afterCopies      func() error
	beforeReadGates  func()
}

func New(ctx context.Context, pool *pgxpool.Pool, members [2]*LocalMember, opts Options) (*Store, error) {
	return NewWithMembers(ctx, pool, [2]Member{members[0], members[1]}, opts)
}

// NewWithMembers accepts local or mutually authenticated remote storage members.
// Its stable legacy shard ID is 00000000000000000000000000000001.
// Existing authority without a bound catalog requires explicit offline conversion.
func NewWithMembers(ctx context.Context, pool *pgxpool.Pool, members [2]Member, opts Options) (*Store, error) {
	return NewWithShards(ctx, pool, []IngestShard{{ID: legacyIngestShardID(), Members: members}}, opts)
}

func NewWithShards(ctx context.Context, pool *pgxpool.Pool, shards []IngestShard, opts Options) (*Store, error) {
	if !validIngestShardCount(shards) {
		return nil, fmt.Errorf("receiptstore: require1..16 ingestion shards")
	}
	shards = append([]IngestShard(nil), shards...)
	sort.Slice(shards, func(i, j int) bool { return shards[i].ID.String() < shards[j].ID.String() })
	members, err := shardMembers(shards)
	if err != nil {
		return nil, err
	}
	opts, err = storeOptions(pool, opts)
	if err != nil {
		return nil, err
	}
	if err = validateStoreMembers(pool, members); err != nil {
		return nil, err
	}
	byID, err := indexStoreMembers(members)
	if err != nil {
		return nil, err
	}
	unlockCatalogMembers := lockMemberSet(members)
	defer unlockCatalogMembers()
	if err = validateLockedShards(shards, members); err != nil {
		return nil, err
	}
	if err = checkStoreDurability(ctx, pool); err != nil {
		return nil, err
	}
	if err := checkStorageUsageSchema(ctx, pool); err != nil {
		return nil, err
	}
	catalog, err := bindShards(ctx, pool, shards)
	if err != nil {
		return nil, err
	}
	return &Store{
		headerCaptures:   make(chan struct{}, 16),
		headerParses:     make(chan struct{}, 1),
		cleanupSelection: make(chan struct{}, 1),
		mailboxSelection: make(chan struct{}, 1),
		db:               &authority{pool: pool, opts: opts, shards: catalog},
		members:          members,
		shards:           shards,
		memberByID:       byID,
	}, nil
}

func (s *Store) Status(ctx context.Context, organization, attempt ID) (Status, error) {
	return s.db.status(ctx, organization, attempt)
}

// Accept takes ownership of source and always closes it. Close must unblock
// Read (as for a network connection or pipe). This contract is required for
// cancellation and lease-loss shutdown; an arbitrary blocking Reader is unsafe.
// A retry of an accepted attempt returns its existing receipt without reading
// the source. Request metadata must match. The caller must reuse an attempt ID
// only for the same operation; use Status to resolve a lost acknowledgment.
// Errors leave durable target obligations, never authorize byte deletion, and
// may require Status/Abort. Unexpired attempts reject overlapping Accept calls.
func (s *Store) Accept(
	ctx context.Context,
	attempt ID,
	envelope Envelope,
	source io.ReadCloser,
) (_ Receipt, resultErr error) {
	if source == nil {
		return Receipt{}, fmt.Errorf("receiptstore: source required")
	}
	ownedSource := &ownedStream{ReadCloser: source}
	source = ownedSource
	defer ownedSource.closeAcceptance(&resultErr)
	if attempt == (ID{}) {
		return Receipt{}, fmt.Errorf("receiptstore: attempt required")
	}
	envelope, data, err := normalize(envelope)
	if err != nil {
		return Receipt{}, err
	}
	c, pair, accepted, err := s.prepareAcceptance(ctx, attempt, envelope, data)
	if err != nil {
		return Receipt{}, err
	}
	if accepted != nil {
		return *accepted, nil
	}
	capture := s.beginHeaderCapture()
	if capture != nil {
		defer func() { <-s.headerCaptures }()
	}
	workCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	streams := newAcceptanceStreams(workCtx, ownedSource)
	defer streams.close()
	if err = s.streamAcceptanceCopies(workCtx, cancel, source, c, pair, streams, capture); err != nil {
		return Receipt{}, err
	}
	if closeErr := ownedSource.Close(); closeErr != nil {
		return Receipt{}, closeErr
	}
	return s.finalizeAcceptance(workCtx, c, capture)
}

func (s *Store) finalizeAcceptance(ctx context.Context, c claim, capture *headerCapture) (Receipt, error) {
	if s.afterCopies != nil {
		if err := s.afterCopies(); err != nil {
			return Receipt{}, err
		}
	}
	return s.db.finalizeWithHeader(ctx, c, s.finishHeaderCapture(capture))
}

func broadcastCaptured(ctx context.Context, source io.Reader, writers [2]*io.PipeWriter, capture *headerCapture) error {
	buf := make([]byte, 64*1024)
	empty := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := source.Read(buf)
		if e := writeAcceptanceRead(writers, capture, buf, n, err, &empty); e != nil {
			return e
		}
		if err == io.EOF {
			for _, w := range writers {
				finishPipe(w, nil)
			}
			return nil
		}
		if err != nil {
			return err
		}
	}
}

type countedWriter struct {
	w   io.Writer
	n   int64
	err error
}

func (w *countedWriter) Write(b []byte) (int, error) {
	n, e := w.w.Write(b)
	w.n += int64(n)
	if e == nil && n != len(b) {
		e = io.ErrShortWrite
	}
	w.err = e
	return n, e
}

// Read reads an accepted receipt scoped to organization. It tries another hot
// member and then an attached published pack only before emitting any bytes;
// otherwise the caller must restart the download after the error.
// Authorization beyond organization scoping and automatic replica repair remain
// the responsibility of later authenticated protocol and repair adapters.
func (s *Store) Read(ctx context.Context, organization, receipt ID, offset, length int64, dst io.Writer) error {
	if dst == nil {
		return fmt.Errorf("receiptstore: destination required")
	}
	r, err := s.Open(ctx, organization, receipt, offset, length)
	if err != nil {
		return err
	}
	_, err = io.Copy(dst, r)
	return errors.Join(err, r.Close())
}

func (s *Store) readPinned(
	ctx context.Context,
	organization, receipt, pin ID,
	offset, length int64,
	dst io.Writer,
	cold *coldLocation,
) error {
	if dst == nil {
		return fmt.Errorf("receiptstore: destination required")
	}
	if _, err := loadReceipt(ctx, s.db.pool, organization, receipt, false); err != nil {
		return err
	}
	targets, err := s.pinnedMembers(ctx, organization, receipt)
	if err != nil {
		return err
	}
	if s.beforeReadGates != nil {
		s.beforeReadGates()
	}
	unlock := lockMemberSet(targets)
	defer unlock()
	copies, err := s.pinnedCopies(ctx, organization, receipt)
	if err != nil {
		return err
	}
	request := pinnedReadRequest{
		organization: organization,
		receipt:      receipt,
		pin:          pin,
		offset:       offset,
		length:       length,
		dst:          dst,
	}
	return s.readPinnedCandidates(ctx, request, copies, cold)
}

func (s *Store) readPinnedCandidates(
	ctx context.Context,
	request pinnedReadRequest,
	copies []pinnedCopy,
	cold *coldLocation,
) error {
	var failures []error
	for _, c := range copies {
		attempted, terminal, err := s.readPinnedCopy(ctx, request, c)
		if terminal {
			return err
		}
		if attempted {
			failures = append(failures, err)
		}
	}
	if adapter := s.cold.Load(); adapter != nil && cold != nil {
		if err := adapter.readLocation(ctx, *cold, request.offset, request.length, request.dst); err == nil {
			return nil
		} else {
			failures = append(failures, err)
		}
	}
	if len(failures) == 0 {
		return ErrNotFound
	}
	return errors.Join(failures...)
}

// Close can be requested concurrently by cancellation and normal return.
type ownedStream struct {
	io.ReadCloser
	once sync.Once
	err  error
}

func (s *ownedStream) Close() error { s.cancelRead(); return s.err }

// The same order is used for every operation spanning both local members.
func lockMemberSet(members []Member) func() {
	members = append([]Member(nil), members...)
	sort.Slice(members, func(i, j int) bool { return memberID(members[i]).String() < memberID(members[j]).String() })
	var locked []*LocalMember
	for _, m := range members {
		if local, ok := m.(*LocalMember); ok {
			local.gate.RLock()
			locked = append(locked, local)
		}
	}
	return func() {
		for _, l := range slices.Backward(locked) {
			l.gate.RUnlock()
		}
	}
}

func memberID(m Member) ID {
	if v, ok := m.(*LocalMember); ok {
		return v.id
	}
	return m.identity().Member
}

func shardMembers(shards []IngestShard) ([]Member, error) {
	var members []Member
	for i, v := range shards {
		if v.ID == (ID{}) || (i > 0 && shards[i-1].ID == v.ID) {
			return nil, ErrConflict
		}
		members = append(members, v.Members[:]...)
	}
	return members, nil
}

func storeOptions(pool *pgxpool.Pool, opts Options) (Options, error) {
	if opts.MaxReadLifetime == 0 {
		opts.MaxReadLifetime = 5 * time.Minute
	}
	if opts.CleanupDeadline == 0 {
		opts.CleanupDeadline = 24 * time.Hour
	}
	if opts.MaxReadLifetime < time.Millisecond || opts.MaxReadLifetime > 5*time.Minute ||
		opts.CleanupDeadline < time.Second {
		return opts, fmt.Errorf("receiptstore: invalid lifecycle options")
	}
	if !validStoreOptions(pool, opts) {
		return opts, fmt.Errorf("receiptstore: invalid options")
	}
	return opts, nil
}

func validateStoreMembers(pool *pgxpool.Pool, members []Member) error {
	for _, m := range members {
		switch v := m.(type) {
		case *LocalMember:
			if v == nil || v.pool != pool {
				return fmt.Errorf("receiptstore: invalid local member")
			}
		case *RemoteMember:
			if v == nil {
				return fmt.Errorf("receiptstore: invalid remote member")
			}
		default:
			return fmt.Errorf("receiptstore: invalid member")
		}
	}
	return nil
}

func indexStoreMembers(members []Member) (map[ID]Member, error) {
	byID := map[ID]Member{}
	for _, m := range members {
		id := memberID(m)
		if byID[id] != nil {
			return nil, ErrConflict
		}
		byID[id] = m
	}
	return byID, nil
}

func validateLockedShards(shards []IngestShard, members []Member) error {
	for _, m := range members {
		if local, ok := m.(*LocalMember); ok && local.bytes == nil {
			return ErrConflict
		}
	}
	for _, v := range shards {
		first, second := v.Members[0].identity(), v.Members[1].identity()
		if first.Member == second.Member || first.Incarnation == second.Incarnation ||
			first.FailureDomain == second.FailureDomain {
			return fmt.Errorf("receiptstore: two distinct members, incarnations and failure domains required")
		}
	}
	return nil
}

func checkStoreDurability(ctx context.Context, pool *pgxpool.Pool) error {
	settings, err := dbgen.New(pool).NewWithShards(ctx)
	if err != nil {
		return err
	}
	if settings.CurrentSetting != "on" || settings.CurrentSetting_2 != "on" {
		return fmt.Errorf("receiptstore: PostgreSQL fsync and full_page_writes must be on")
	}
	return nil
}

func (s *Store) prepareAcceptance(
	ctx context.Context,
	attempt ID,
	envelope Envelope,
	data []byte,
) (claim, [2]Member, *Receipt, error) {
	accepted, found, err := s.acceptedRetry(ctx, attempt, envelope, data)
	if err != nil || found {
		return claim{}, [2]Member{}, accepted, err
	}
	pair, err := s.refreshAttemptPair(ctx, attempt)
	if err != nil {
		return claim{}, pair, nil, err
	}
	c, accepted, err := s.prepareAvailablePair(ctx, attempt, envelope, data, pair)
	return c, pair, accepted, err
}

func (s *Store) acceptedRetry(ctx context.Context, attempt ID, envelope Envelope, data []byte) (*Receipt, bool, error) {
	// Resolve accepted retries through authority before contacting members.
	// prepareMembers verifies the entire immutable envelope on this path.
	state, err := dbgen.New(s.db.pool).StoreAccept(ctx, attempt.String())
	if isNoRows(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if state != "accepted" {
		return nil, false, nil
	}
	_, accepted, err := s.db.prepareMembers(ctx, attempt, envelope, data, [2]MemberIdentity{})
	if err != nil {
		return nil, false, err
	}
	if accepted == nil {
		return nil, false, ErrConflict
	}
	return accepted, true, nil
}

func (s *Store) refreshAttemptPair(ctx context.Context, attempt ID) ([2]Member, error) {
	pair, err := s.pairForAttempt(ctx, attempt)
	if err != nil {
		return [2]Member{}, err
	}
	for _, m := range pair {
		if remote, ok := m.(*RemoteMember); ok {
			if err := remote.refresh(ctx); err != nil {
				return [2]Member{}, err
			}
		}
	}
	return pair, nil
}

func (s *Store) prepareAvailablePair(
	ctx context.Context,
	attempt ID,
	envelope Envelope,
	data []byte,
	pair [2]Member,
) (claim, *Receipt, error) {
	unlock := lockMemberSet(pair[:])
	for _, m := range pair {
		if !m.available() {
			unlock()
			return claim{}, nil, ErrStale
		}
	}
	c, accepted, err := s.db.prepareMembers(
		ctx,
		attempt,
		envelope,
		data,
		[2]MemberIdentity{pair[0].identity(), pair[1].identity()},
	)
	unlock()
	return c, accepted, err
}

func (s *Store) renewAcceptance(
	ctx context.Context,
	c claim,
	cancel context.CancelCauseFunc,
	stopRenewal <-chan struct{},
) {
	ticker := time.NewTicker(s.db.opts.Lease / 3)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-stopRenewal:
			return
		case <-ticker.C:
			renewalCtx, stop := context.WithTimeout(ctx, s.db.opts.Lease/3)
			e := s.db.renew(renewalCtx, c)
			stop()
			if e != nil {
				cancel(e)
				return
			}
		}
	}
}

type acceptanceStreams struct {
	readers [2]*io.PipeReader
	writers [2]*io.PipeWriter
	stop    func() bool
}

func newAcceptanceStreams(ctx context.Context, source *ownedStream) *acceptanceStreams {
	streams := &acceptanceStreams{}
	for i := range streams.readers {
		streams.readers[i], streams.writers[i] = io.Pipe()
	}
	streams.stop = context.AfterFunc(ctx, func() {
		source.cancelRead()
		for i := range streams.readers {
			finishPipe(streams.readers[i], context.Cause(ctx))
			finishPipe(streams.writers[i], context.Cause(ctx))
		}
	})
	return streams
}

func (streams *acceptanceStreams) close() {
	for i := range streams.readers {
		finishPipe(streams.readers[i], nil)
		finishPipe(streams.writers[i], nil)
	}
	streams.stop()
}

func (s *Store) streamAcceptanceCopies(
	workCtx context.Context,
	cancel context.CancelCauseFunc,
	source io.Reader,
	c claim,
	pair [2]Member,
	streams *acceptanceStreams,
	capture *headerCapture,
) error {
	// Lease renewal is coordinator work, and does not hold locks during byte I/O.
	renewalDone := make(chan struct{})
	stopRenewal := make(chan struct{})
	go func() {
		defer close(renewalDone)
		s.renewAcceptance(workCtx, c, cancel, stopRenewal)
	}()
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i, m := range pair {
		wg.Add(1)
		go func(i int, m Member) {
			defer wg.Done()
			e := m.write(workCtx, s.db, c, streams.readers[i])
			errs <- e
			if e != nil {
				cancel(e)
			}
		}(i, m)
	}
	streamErr := broadcastCaptured(workCtx, source, streams.writers, capture)
	if streamErr != nil {
		cancel(streamErr)
	}
	wg.Wait()
	close(errs)
	close(stopRenewal)
	<-renewalDone
	return acceptanceCopyErrors(workCtx, streamErr, errs)
}

func acceptanceCopyErrors(workCtx context.Context, streamErr error, errs <-chan error) error {
	var failures []error
	if streamErr != nil {
		failures = append(failures, streamErr)
	}
	for e := range errs {
		if e != nil {
			failures = append(failures, e)
		}
	}
	if cause := context.Cause(workCtx); cause != nil {
		failures = append(failures, cause)
	}
	if len(failures) > 0 {
		return errors.Join(failures...)
	}
	return nil
}

func writeAcceptanceChunk(writers [2]*io.PipeWriter, capture *headerCapture, chunk []byte) error {
	if capture != nil {
		capture.observe(chunk)
	}
	for _, w := range writers {
		if _, e := w.Write(chunk); e != nil {
			return e
		}
	}
	return nil
}

func validIngestShardCount(shards []IngestShard) bool {
	return len(shards) >= 1 && len(shards) <= 16
}

func validStoreOptions(pool *pgxpool.Pool, opts Options) bool {
	return pool != nil && opts.Lease >= 300*time.Millisecond && opts.Lease <= 24*time.Hour &&
		opts.RecoveryRetention >= time.Second &&
		opts.RecoveryRetention%time.Second == 0
}

func (s *ownedStream) closeAcceptance(resultErr *error) {
	closeErr := s.Close()
	// An accepted retry or completed commit remains authoritative. Source
	// cleanup cannot turn it into a failed, retryable operation.
	if *resultErr != nil {
		*resultErr = errors.Join(*resultErr, closeErr)
	}
}

func writeAcceptanceRead(
	writers [2]*io.PipeWriter,
	capture *headerCapture,
	buf []byte,
	n int,
	readErr error,
	empty *int,
) error {
	if n > 0 {
		if err := writeAcceptanceChunk(writers, capture, buf[:n]); err != nil {
			return err
		}
		*empty = 0
	} else if readErr == nil {
		*empty++
		if *empty >= 100 {
			return io.ErrNoProgress
		}
	}
	return nil
}

func (s *Store) pinnedMembers(ctx context.Context, organization, receipt ID) ([]Member, error) {
	// Resolve immutable target member IDs, then lock only their local gates.
	// Locators are read again below after a rewrite can no longer replace them.
	memberRows, err := s.db.pool.Query(ctx, dbgen.StoreReadPinnedReadTarget, receipt.String(), organization.String())
	if err != nil {
		return nil, err
	}
	var targets []Member
	for memberRows.Next() {
		var raw string
		if err = memberRows.Scan(&raw); err != nil {
			break
		}
		id, e := idFrom(raw)
		if e != nil {
			err = e
			break
		}
		if m := s.memberByID[id]; m != nil {
			targets = append(targets, m)
		}
	}
	memberErr := memberRows.Err()
	memberRows.Close()
	if err != nil {
		return nil, err
	}
	if memberErr != nil {
		return nil, memberErr
	}
	return targets, nil
}

type pinnedCopy struct {
	member, inc string
	loc         hotstore.Locator
}

type pinnedReadRequest struct {
	organization, receipt, pin ID
	offset, length             int64
	dst                        io.Writer
}

func (s *Store) pinnedCopies(ctx context.Context, organization, receipt ID) ([]pinnedCopy, error) {
	rows, err := s.db.pool.Query(
		ctx,
		dbgen.StoreReadPinnedReadTargetTMemberTIncarnationTLocator,
		receipt.String(),
		organization.String(),
	)
	if err != nil {
		return nil, err
	}
	var copies []pinnedCopy
	for rows.Next() {
		var c pinnedCopy
		var b []byte
		if err = rows.Scan(&c.member, &c.inc, &b); err != nil {
			break
		}
		if err = json.Unmarshal(b, &c.loc); err != nil {
			break
		}
		copies = append(copies, c)
	}
	rowErr := rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if rowErr != nil {
		return nil, rowErr
	}
	return copies, nil
}

func (s *Store) readPinnedCopy(
	ctx context.Context,
	request pinnedReadRequest,
	c pinnedCopy,
) (attempted, terminal bool, err error) {
	id, err := idFrom(c.member)
	if err != nil {
		return false, true, err
	}
	m := s.memberByID[id]
	if m == nil {
		return false, false, nil
	}
	if m.identity().Member.String() != c.member {
		return false, false, nil
	}
	writer := &countedWriter{w: request.dst}
	err = m.read(ctx, request.pin, request.organization, request.receipt, c.loc, request.offset, request.length, writer)
	if err == nil {
		return true, true, nil
	}
	if writer.n > 0 || writer.err != nil || ctx.Err() != nil || errors.Is(err, errReadClosureUncertain) {
		return true, true, err
	}
	return true, false, err
}
