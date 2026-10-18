// Package replicastream frames bounded replica streams. HTTP status, declared
// length, and connection closure are not storage-completion evidence. A sender
// calls Finish only after its source operation has completed successfully.
package replicastream

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"hash"
	"hash/crc32"
	"io"
)

const (
	ChunkSize           = 64 << 10
	dataFrame, endFrame = byte(1), byte(2)
)

var (
	ErrProtocol  = errors.New("replicastream: malformed or incomplete stream")
	ErrIntegrity = errors.New("replicastream: integrity mismatch")
	ErrLimit     = errors.New("replicastream: byte limit exceeded")
	ErrFinished  = errors.New("replicastream: already finished")
	crcTable     = crc32.MakeTable(crc32.Castagnoli)
)

// Writer does not own dst. A failed write permanently prevents Finish; callers
// close/abort their transport on source errors and must not emit success then.
type Writer struct {
	dst         io.Writer
	limit, size int64
	digest      hash.Hash
	err         error
	finished    bool
}

func NewWriter(dst io.Writer, limit int64) *Writer {
	w := &Writer{dst: dst, limit: limit, digest: sha256.New()}
	if dst == nil || limit < 0 {
		w.err = ErrLimit
	}
	return w
}

func (w *Writer) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	if w.finished {
		return 0, ErrFinished
	}
	if int64(len(p)) > w.limit-w.size {
		w.err = ErrLimit
		return 0, w.err
	}
	total := 0
	for len(p) > 0 {
		n := min(len(p), ChunkSize)
		if err := w.frame(dataFrame, p[:n]); err != nil {
			w.err = err
			return total, err
		}
		if _, err := w.digest.Write(p[:n]); err != nil {
			w.err = err
			return total, err
		}
		w.size += int64(n)
		total += n
		p = p[n:]
	}
	return total, nil
}

func (w *Writer) Finish() error {
	if w.err != nil {
		return w.err
	}
	if w.finished {
		return ErrFinished
	}
	w.finished = true
	if w.size < 0 {
		w.err = ErrLimit
		return w.err
	}
	var end [40]byte
	binary.BigEndian.PutUint64(end[:8], uint64(w.size))
	copy(end[8:], w.digest.Sum(nil))
	w.err = w.frame(endFrame, end[:])
	return w.err
}

func (w *Writer) frame(kind byte, p []byte) error {
	length := len(p)
	if length < 0 || length > ChunkSize {
		return ErrLimit
	}
	var header [8]byte
	header[0], header[1], header[2], header[3] = 'F', 'R', 1, kind
	binary.BigEndian.PutUint32(header[4:], uint32(length))
	checksum := crc32.Update(crc32.Checksum(header[:], crcTable), crcTable, p)
	var tail [4]byte
	binary.BigEndian.PutUint32(tail[:], checksum)
	for _, part := range [][]byte{header[:], p, tail[:]} {
		n, err := w.dst.Write(part)
		if err != nil {
			return err
		}
		if n != len(part) {
			return io.ErrShortWrite
		}
	}
	return nil
}

// Reader verifies frames, total size, and digest, then requires exact underlying
// EOF after success. It keeps one frame ahead: the last payload chunk is never
// returned before completion is verified, including to io.CopyN callers. Earlier
// chunks may have been staged and never independently authorize acceptance.
// The caller owns src and must make its reads interruptible by closing it.
type Reader struct {
	src           io.Reader
	limit, size   int64
	digest        hash.Hash
	buffers       [2][ChunkSize]byte
	pending, out  []byte
	next          int
	started, done bool
	err           error
}

func NewReader(src io.Reader, limit int64) *Reader {
	r := &Reader{src: src, limit: limit, digest: sha256.New()}
	if src == nil || limit < 0 {
		r.err = ErrLimit
	}
	return r
}

func (r *Reader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.err != nil {
		return 0, r.err
	}
	for len(r.out) == 0 {
		if r.done {
			return 0, io.EOF
		}
		if !r.started {
			r.started = true
			first, end, err := r.frame()
			if err != nil {
				r.err = err
				return 0, err
			}
			if end {
				r.done = true
				return 0, io.EOF
			}
			r.pending = first
		}
		next, end, err := r.frame()
		if err != nil {
			r.err = err
			return 0, err
		}
		r.out = r.pending
		r.pending = next
		r.done = end
	}
	n := copy(p, r.out)
	r.out = r.out[n:]
	return n, nil
}

func (r *Reader) frame() ([]byte, bool, error) {
	kind, buffer, err := r.readFrame()
	if err != nil {
		return nil, false, err
	}
	if kind == dataFrame {
		r.size += int64(len(buffer))
		if _, err := r.digest.Write(buffer); err != nil {
			return nil, false, err
		}
		return buffer, false, nil
	}
	if err := r.verifyCompletion(buffer); err != nil {
		return nil, false, err
	}
	return nil, true, nil
}

func validFrameSize(kind byte, length uint32) bool {
	switch kind {
	case dataFrame:
		return length > 0 && length <= ChunkSize
	case endFrame:
		return length == 40
	default:
		return false
	}
}

func (r *Reader) readFrame() (byte, []byte, error) {
	var header [8]byte
	if _, err := io.ReadFull(r.src, header[:]); err != nil {
		return 0, nil, errors.Join(ErrProtocol, err)
	}
	if [3]byte(header[:3]) != [3]byte{'F', 'R', 1} {
		return 0, nil, ErrProtocol
	}
	kind, length := header[3], binary.BigEndian.Uint32(header[4:])
	if !validFrameSize(kind, length) {
		return 0, nil, ErrProtocol
	}
	if kind == dataFrame && int64(length) > r.limit-r.size {
		return 0, nil, ErrLimit
	}
	buffer := r.buffers[r.next][:length]
	r.next = 1 - r.next
	if _, err := io.ReadFull(r.src, buffer); err != nil {
		return 0, nil, errors.Join(ErrProtocol, err)
	}
	var tail [4]byte
	if _, err := io.ReadFull(r.src, tail[:]); err != nil {
		return 0, nil, errors.Join(ErrProtocol, err)
	}
	if crc32.Update(crc32.Checksum(header[:], crcTable), crcTable, buffer) != binary.BigEndian.Uint32(tail[:]) {
		return 0, nil, ErrIntegrity
	}
	return kind, buffer, nil
}

func (r *Reader) verifyCompletion(buffer []byte) error {
	if r.size < 0 || binary.BigEndian.Uint64(buffer[:8]) != uint64(r.size) ||
		!equalDigest(buffer[8:], r.digest.Sum(nil)) {
		return ErrIntegrity
	}
	var extra [1]byte
	if n, err := io.ReadFull(r.src, extra[:]); n != 0 || err != io.EOF {
		return ErrProtocol
	}
	return nil
}

func equalDigest(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
