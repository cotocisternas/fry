//go:build linux

package receiptstore

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"io"
	"testing"
)

// Each block is a fresh cryptographic digest. This generated source exercises
// raw fallback without allocating or retaining an entire original in the test.
type memberEntropySource struct {
	remaining int64
	counter   uint64
}

func (s *memberEntropySource) Read(p []byte) (int, error) {
	if s.remaining == 0 {
		return 0, io.EOF
	}
	n := int(min(int64(len(p)), s.remaining))
	for offset := 0; offset < n; {
		var seed [8]byte
		binary.LittleEndian.PutUint64(seed[:], s.counter)
		s.counter++
		block := sha256.Sum256(seed[:])
		offset += copy(p[offset:n], block[:])
	}
	s.remaining -= int64(n)
	return n, nil
}

func TestMemberGRPCCompressionUsesRawFallbackForIncompressibleData(t *testing.T) {
	encoder, err := newMemberChunkEncoder()
	requireReceiptNoError(t, err)
	data := make([]byte, memberChunkSize)
	_, err = io.ReadFull(&memberEntropySource{remaining: memberChunkSize}, data)
	requireReceiptNoError(t, err)
	encoded, err := encoder.encode(data)
	requireReceiptNoError(t, err)
	requireReceiptCondition(t, encoded.data == nil, "incompressible chunk was expanded")
}

func TestMemberGRPCIncompressibleOriginalRoundTrip(t *testing.T) {
	f := setup(t)
	members, _ := remoteFixture(t, f)
	store, err := NewWithMembers(t.Context(), f.pool, [2]Member{members[0], members[1]}, f.s.db.opts)
	requireReceiptNoError(t, err)
	const size = 2 << 20
	expected := sha256.New()
	original := io.TeeReader(&memberEntropySource{remaining: size}, expected)
	receipt, err := store.Accept(t.Context(), newID(t), f.envelope(t), io.NopCloser(original))
	requireReceiptNoError(t, err)
	actual := sha256.New()
	requireReceiptNoError(t, store.Read(context.WithoutCancel(t.Context()), f.org, receipt.ID, 0, size, actual))
	requireReceiptCondition(t, string(actual.Sum(nil)) == string(expected.Sum(nil)), "raw fallback original differs")
}
