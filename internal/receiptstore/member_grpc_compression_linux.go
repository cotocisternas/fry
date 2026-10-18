//go:build linux

package receiptstore

import (
	"bytes"
	"compress/flate"
	"errors"
	"io"

	memberv1 "github.com/cotocisternas/fry/gen/member/go/fry/member/v1"
)

// Compression is independent per bounded chunk. Original-byte limits, digest,
// and durable completion remain authoritative. Incompressible chunks stay raw.
type memberChunkEncoder struct {
	storage [memberChunkSize + 1024]byte
	output  *bytes.Buffer
	writer  *flate.Writer
}

func newMemberChunkEncoder() (*memberChunkEncoder, error) {
	encoder := &memberChunkEncoder{}
	encoder.output = bytes.NewBuffer(encoder.storage[:0])
	var err error
	encoder.writer, err = flate.NewWriter(encoder.output, flate.BestSpeed)
	return encoder, err
}

type encodedMemberChunk struct {
	size uint32
	data []byte
}

func (e *memberChunkEncoder) encode(data []byte) (encodedMemberChunk, error) {
	size := len(data)
	if size < 1 || size > memberChunkSize {
		return encodedMemberChunk{}, ErrConflict
	}
	e.output.Reset()
	e.writer.Reset(e.output)
	if _, err := e.writer.Write(data); err != nil {
		return encodedMemberChunk{}, err
	}
	if err := e.writer.Close(); err != nil {
		return encodedMemberChunk{}, err
	}
	if e.output.Len() >= len(data) {
		return encodedMemberChunk{}, nil
	}
	return encodedMemberChunk{size: uint32(size), data: e.output.Bytes()}, nil
}

type memberInflater interface {
	io.ReadCloser
	flate.Resetter
}
type memberChunkDecoder struct {
	storage [2][memberChunkSize]byte
	input   bytes.Reader
	reader  memberInflater
	next    int
}

func (d *memberChunkDecoder) decode(chunk *memberv1.CompressedChunk) ([]byte, error) {
	if chunk.Size == 0 || chunk.Size > memberChunkSize || len(chunk.Data) == 0 || len(chunk.Data) > memberChunkSize {
		return nil, ErrConflict
	}
	d.input.Reset(chunk.Data)
	if err := d.reset(); err != nil {
		return nil, err
	}
	data := d.storage[d.next][:chunk.Size]
	d.next = 1 - d.next
	if _, err := io.ReadFull(d.reader, data); err != nil {
		return nil, errors.Join(ErrConflict, err)
	}
	var extra [1]byte
	n, err := d.reader.Read(extra[:])
	// The private bytes.Reader inflater must terminate with an unadorned EOF.
	//nolint:errorlint // Preserve exact EOF identity.
	if n != 0 || err != io.EOF ||
		d.input.Len() != 0 {
		return nil, errors.Join(ErrConflict, err)
	}
	return data, nil
}

func (d *memberChunkDecoder) reset() error {
	if d.reader != nil {
		return d.reader.Reset(&d.input, nil)
	}
	reader := flate.NewReader(&d.input)
	inflater, ok := reader.(memberInflater)
	if !ok {
		return errors.Join(ErrConflict, reader.Close())
	}
	d.reader = inflater
	return nil
}

func (d *memberChunkDecoder) close() error {
	if d == nil || d.reader == nil {
		return nil
	}
	return d.reader.Close()
}

func memberWriteFrame(
	data []byte,
	compressed *memberv1.CompressedChunk,
	end *memberv1.Completion,
) *memberv1.WriteRequest {
	if end != nil {
		return &memberv1.WriteRequest{Frame: &memberv1.WriteRequest_Completion{Completion: end}}
	}
	if compressed != nil {
		return &memberv1.WriteRequest{Frame: &memberv1.WriteRequest_CompressedData{CompressedData: compressed}}
	}
	return &memberv1.WriteRequest{Frame: &memberv1.WriteRequest_Data{Data: data}}
}

func memberReadFrame(
	data []byte,
	compressed *memberv1.CompressedChunk,
	end *memberv1.Completion,
) *memberv1.ReadResponse {
	if end != nil {
		return &memberv1.ReadResponse{Frame: &memberv1.ReadResponse_Completion{Completion: end}}
	}
	if compressed != nil {
		return &memberv1.ReadResponse{Frame: &memberv1.ReadResponse_CompressedData{CompressedData: compressed}}
	}
	return &memberv1.ReadResponse{Frame: &memberv1.ReadResponse_Data{Data: data}}
}
