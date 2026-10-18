package hotstore

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
)

// ID is an opaque identity, independent of an email's Message-ID header.
type ID [16]byte

func NewID() (ID, error)     { var id ID; _, err := rand.Read(id[:]); return id, err }
func (id ID) String() string { return hex.EncodeToString(id[:]) }

// Key identifies one immutable byte-write attempt. Retrying it with different
// content is an error. It is not an authorization or a business deduplication key.
type Key struct{ Receipt, Attempt ID }

// Locator identifies a verified record in one file incarnation. Persist it
// outside this store before using it as authoritative recovery evidence.
type Locator struct {
	File   ID
	Key    Key
	Offset int64
	Size   int64
	SHA256 [32]byte
}

const (
	frameSize        = 64 * 1024
	fileHeaderSize   = 64
	recordHeaderSize = 96
	footerSize       = 64
	maxMessageSize   = 256 * 1024 * 1024
)

var (
	castagnoli        = crc32.MakeTable(crc32.Castagnoli)
	ErrCorrupt        = errors.New("hotstore: corrupt record or file")
	ErrConflict       = errors.New("hotstore: attempt already contains different bytes")
	ErrInvalidLocator = errors.New("hotstore: unknown or mismatched locator")
)

func encodedSize(n int64) int64 {
	return recordHeaderSize + n + ((n+frameSize-1)/frameSize)*12 + footerSize
}

func putCRC(b []byte) {
	binary.LittleEndian.PutUint32(b[len(b)-4:], crc32.Checksum(b[:len(b)-4], castagnoli))
}

func validCRC(b []byte) bool {
	return binary.LittleEndian.Uint32(b[len(b)-4:]) == crc32.Checksum(b[:len(b)-4], castagnoli)
}

func zeros(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}

func fileHeader(id ID) []byte {
	b := make([]byte, fileHeaderSize)
	copy(b, "FRYHOT01")
	copy(b[8:24], id[:])
	binary.LittleEndian.PutUint32(b[24:28], frameSize)
	putCRC(b)
	return b
}

func parseFileHeader(b []byte) (ID, error) {
	var id ID
	if string(b[:8]) != "FRYHOT01" || !validCRC(b) || binary.LittleEndian.Uint32(b[24:28]) != frameSize ||
		!zeros(b[28:60]) {
		return id, ErrCorrupt
	}
	copy(id[:], b[8:24])
	if id == (ID{}) {
		return id, ErrCorrupt
	}
	return id, nil
}

func recordHeader(key Key, size int64, digest [32]byte) ([]byte, error) {
	if size < 0 {
		return nil, ErrTooLarge
	}
	b := make([]byte, recordHeaderSize)
	copy(b, "FRYREC01")
	copy(b[8:24], key.Receipt[:])
	copy(b[24:40], key.Attempt[:])
	binary.LittleEndian.PutUint64(b[40:48], uint64(size))
	copy(b[48:80], digest[:])
	putCRC(b)
	return b, nil
}

func parseRecordHeader(b []byte, file ID, off int64) (Locator, error) {
	var loc Locator
	if string(b[:8]) != "FRYREC01" || !validCRC(b) || !zeros(b[80:92]) {
		return loc, ErrCorrupt
	}
	n := binary.LittleEndian.Uint64(b[40:48])
	if n > maxMessageSize {
		return loc, ErrCorrupt
	}
	loc.File, loc.Offset, loc.Size = file, off, int64(n)
	copy(loc.Key.Receipt[:], b[8:24])
	copy(loc.Key.Attempt[:], b[24:40])
	copy(loc.SHA256[:], b[48:80])
	if loc.Key.Receipt == (ID{}) || loc.Key.Attempt == (ID{}) {
		return loc, ErrCorrupt
	}
	return loc, nil
}

func footer(header []byte, size int64) ([]byte, error) {
	if size < 0 || size > maxMessageSize {
		return nil, ErrTooLarge
	}
	total := encodedSize(size)
	if total < 0 {
		return nil, ErrTooLarge
	}
	b := make([]byte, footerSize)
	copy(b, "FRYEND01")
	binary.LittleEndian.PutUint64(b[8:16], uint64(total))
	h := sha256.Sum256(header)
	copy(b[16:48], h[:])
	putCRC(b)
	return b, nil
}

func checkFooter(b, header []byte, size int64) error {
	if size < 0 || size > maxMessageSize {
		return ErrCorrupt
	}
	total := encodedSize(size)
	if total < 0 {
		return ErrCorrupt
	}
	h := sha256.Sum256(header)
	if string(b[:8]) != "FRYEND01" || !validCRC(b) || binary.LittleEndian.Uint64(b[8:16]) != uint64(total) ||
		string(b[16:48]) != string(h[:]) ||
		!zeros(b[48:60]) {
		return ErrCorrupt
	}
	return nil
}

func writeAll(w io.Writer, b []byte) error {
	n, err := w.Write(b)
	if err == nil && n != len(b) {
		err = io.ErrShortWrite
	}
	return err
}

func frameOffset(loc Locator, index int64) int64 {
	return loc.Offset + recordHeaderSize + index*(frameSize+12)
}

func readFrame(r io.ReaderAt, loc Locator, index int64, buf []byte) ([]byte, error) {
	if index < 0 || index >= maxMessageSize/frameSize || loc.Size < 0 || loc.Size > maxMessageSize {
		return nil, ErrCorrupt
	}
	n := min(int64(frameSize), loc.Size-index*frameSize)
	if n <= 0 {
		return nil, ErrCorrupt
	}
	b := buf[:8+n+4]
	if _, err := r.ReadAt(b, frameOffset(loc, index)); err != nil {
		return nil, fmt.Errorf("%w: frame: %w", ErrCorrupt, err)
	}
	if binary.LittleEndian.Uint32(b[:4]) != uint32(n) || binary.LittleEndian.Uint32(b[4:8]) != uint32(index) ||
		!validCRC(b) {
		return nil, ErrCorrupt
	}
	return b[8 : 8+n], nil
}
