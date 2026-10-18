//go:build linux

package receiptstore

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"hash"
	"io"
	"sync"

	memberv1 "github.com/cotocisternas/fry/gen/member/go/fry/member/v1"
)

// One pending chunk prevents exact-length consumers from observing the final
// payload before digest, EOF and (for reads) the member's closure proof.
type memberChunkReader struct {
	receive             func() ([]byte, *memberv1.CompressedChunk, *memberv1.Completion, error)
	closer              io.Closer
	digest              hash.Hash
	limit, size, frames int64
	pending, out        []byte
	started, done       bool
	err                 error
	once                sync.Once
	closeErr            error
	exact               bool
	decoder             *memberChunkDecoder
}

func newMemberChunkReader(
	receive func() ([]byte, *memberv1.CompressedChunk, *memberv1.Completion, error),
	limit int64,
	closer io.Closer,
) *memberChunkReader {
	return &memberChunkReader{receive: receive, limit: limit, closer: closer, digest: sha256.New()}
}

func (r *memberChunkReader) frame() ([]byte, bool, error) {
	data, compressed, end, err := r.receive()
	if err != nil {
		return nil, false, errors.Join(ErrConflict, remoteRPCError(err))
	}
	r.frames++
	if r.frames > r.limit/1024+1024 {
		return nil, false, ErrConflict
	}
	if end != nil {
		return nil, true, r.complete(end)
	}
	data, err = r.decodePayload(data, compressed)
	if err != nil {
		return nil, false, err
	}

	if len(data) == 0 || len(data) > memberChunkSize || int64(len(data)) > r.limit-r.size {
		return nil, false, ErrConflict
	}
	r.size += int64(len(data))
	if _, err = r.digest.Write(data); err != nil {
		return nil, false, err
	}
	return data, false, nil
}

func (r *memberChunkReader) complete(end *memberv1.Completion) error {
	if (r.exact && r.size != r.limit) || end.Size != r.size || !bytes.Equal(end.Sha256, r.digest.Sum(nil)) {
		return ErrConflict
	}
	_, _, _, err := r.receive()
	if err != io.EOF { //nolint:errorlint // Integrity requires an unadorned EOF sentinel.
		return errors.Join(ErrConflict, remoteRPCError(err))
	}
	return r.decoder.close()
}

func (r *memberChunkReader) advance() error {
	if !r.started {
		r.started = true
		first, end, err := r.frame()
		if err != nil {
			return err
		}
		r.pending, r.done = first, end
		if end {
			return io.EOF
		}
	}
	next, end, err := r.frame()
	if err != nil {
		return err
	}
	r.out, r.pending, r.done = r.pending, next, end
	return nil
}

func (r *memberChunkReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.err != nil {
		return 0, r.err
	}
	if len(r.out) == 0 {
		if r.done {
			return 0, io.EOF
		}
		if err := r.advance(); err != nil {
			r.err = err
			return 0, err
		}
	}
	n := copy(p, r.out)
	r.out = r.out[n:]
	return n, nil
}

func (r *memberChunkReader) Close() error {
	r.once.Do(func() {
		if r.closer != nil {
			r.closeErr = r.closer.Close()
		}
	})
	return r.closeErr
}

type memberChunkWriter struct {
	send        func([]byte, *memberv1.CompressedChunk, *memberv1.Completion) error
	limit, size int64
	digest      hash.Hash
	err         error
	encoder     *memberChunkEncoder
}

func newMemberChunkWriter(
	send func([]byte, *memberv1.CompressedChunk, *memberv1.Completion) error,
	limit int64,
) *memberChunkWriter {
	return &memberChunkWriter{send: send, limit: limit, digest: sha256.New()}
}

func (w *memberChunkWriter) Write(p []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	if int64(len(p)) > w.limit-w.size {
		w.err = ErrConflict
		return 0, w.err
	}
	total := 0
	for len(p) > 0 {
		n := min(len(p), memberChunkSize)
		if w.err = w.sendPayload(p[:n]); w.err != nil {
			return total, w.err
		}
		if _, w.err = w.digest.Write(p[:n]); w.err != nil {
			return total, w.err
		}
		w.size += int64(n)
		total += n
		p = p[n:]
	}
	return total, nil
}

func (w *memberChunkWriter) finish() error {
	if w.err != nil {
		return w.err
	}
	w.err = w.send(nil, nil, &memberv1.Completion{Size: w.size, Sha256: w.digest.Sum(nil)})
	return w.err
}

func (w *memberChunkWriter) sendPayload(data []byte) error {
	if len(data) < 1024 {
		return w.send(data, nil, nil)
	}
	if w.encoder == nil {
		var err error
		w.encoder, err = newMemberChunkEncoder()
		if err != nil {
			return err
		}
	}
	compressed, err := w.encoder.encode(data)
	if err != nil {
		return err
	}
	if compressed.data == nil {
		return w.send(data, nil, nil)
	}
	return w.send(nil, &memberv1.CompressedChunk{Size: compressed.size, Data: compressed.data}, nil)
}

func (r *memberChunkReader) decodePayload(data []byte, compressed *memberv1.CompressedChunk) ([]byte, error) {
	if compressed == nil {
		return data, nil
	}
	if r.decoder == nil {
		r.decoder = &memberChunkDecoder{}
	}
	return r.decoder.decode(compressed)
}
