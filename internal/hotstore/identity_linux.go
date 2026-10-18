//go:build linux

package hotstore

import (
	"errors"
	"io"
	"os"
	"path/filepath"
)

// InspectFileIdentity reads only the immutable header. It neither opens the
// store for writing nor scans records, creates files, syncs, or removes staging.
func InspectFileIdentity(dir string) (id ID, err error) {
	var zero ID
	directory, err := os.Lstat(dir)
	if err != nil {
		return zero, err
	}
	if !directory.IsDir() {
		return zero, ErrCorrupt
	}
	path := filepath.Join(dir, "data.fry")
	info, err := os.Lstat(path)
	if err != nil {
		return zero, err
	}
	if !info.Mode().IsRegular() {
		return zero, ErrCorrupt
	}
	// Fixed data.fry in caller-owned directory; preceding Lstat requires regular file; callers fence mutation;
	// owner: FRY maintainers; expires: 2027-01-18.
	// #nosec G304 -- See the path-safety rationale above.
	f, err := os.Open(path)
	if err != nil {
		return zero, err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	b := make([]byte, fileHeaderSize)
	if _, err = io.ReadFull(f, b); err != nil {
		return zero, err
	}
	return parseFileHeader(b)
}
