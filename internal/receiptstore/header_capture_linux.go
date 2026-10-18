//go:build linux

package receiptstore

import (
	"bytes"
	"crypto/sha256"
	"hash"
)

// capturedHeader is derived only from a successfully consumed acceptance stream.
// Its full-stream proof must match both durable copies before publication.
type capturedHeader struct {
	raw    []byte
	size   int64
	sha256 [32]byte
}

type headerCapture struct {
	raw       []byte
	digest    hash.Hash
	size      int64
	lineBytes int
	lastCR    bool
	complete  bool
	unusable  bool
}

// Sixteen permits bound retained prefixes to 16 MiB per Store, not total RSS:
// adaptive growth, parser copies, hashing and database encoding add transients.
// The permit remains held through finalize, including database waits.
func (s *Store) beginHeaderCapture() *headerCapture {
	select {
	case s.headerCaptures <- struct{}{}:
		return &headerCapture{digest: sha256.New()}
	default:
		return nil
	}
}

func (c *headerCapture) observe(chunk []byte) {
	//nolint:errcheck // The capture owns a crypto/sha256 hash; its Write always succeeds.
	c.digest.Write(chunk)
	c.size += int64(len(chunk))
	if c.complete || c.unusable {
		return
	}
	end := len(chunk)
	for i, b := range chunk {
		if b == '\n' {
			if c.lineBytes == 0 || c.lineBytes == 1 && c.lastCR {
				c.complete = true
				end = i + 1
				break
			}
			c.lineBytes = 0
		} else {
			c.lineBytes++
		}
		c.lastCR = b == '\r'
	}
	// Decide the boundary before allocating/appending, so body bytes never enter
	// the retained prefix, even when header and body arrive in one large read.
	needed := len(c.raw) + end
	if needed > MaxHeaderBytes {
		c.raw = nil
		c.unusable = true
		return
	}
	if needed > cap(c.raw) {
		capacity := min(MaxHeaderBytes, max(4096, cap(c.raw)*2, needed))
		next := make([]byte, len(c.raw), capacity)
		copy(next, c.raw)
		c.raw = next
	}
	c.raw = append(c.raw, chunk[:end]...)
}

// Parsing is optional and never waits behind another parser. It occurs only
// after both replica writers succeed, before any finalization SQL locks.
func (s *Store) finishHeaderCapture(c *headerCapture) *capturedHeader {
	if c == nil || !c.complete || c.unusable {
		return nil
	}
	defer func() { c.raw = nil }()
	select {
	case s.headerParses <- struct{}{}:
		defer func() { <-s.headerParses }()
	default:
		return nil
	}
	raw, err := readHeaderMetadata(bytes.NewReader(c.raw))
	if err != nil || len(raw) != len(c.raw) {
		return nil
	}
	// Transfer the collector's capacity-clamped allocation. The parser's
	// validation copy is transient under the single parse permit.
	proof := &capturedHeader{raw: c.raw, size: c.size}
	copy(proof.sha256[:], c.digest.Sum(nil))
	return proof
}
