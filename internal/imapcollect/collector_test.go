//go:build linux

package imapcollect

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"
)

func TestLiteralRequiresCompleteLengthAndVerifiedTail(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		size int64
		tail error
		want error
	}{
		{"short", "abc", 4, nil, io.ErrUnexpectedEOF},
		{"tail failed", "abc", 3, io.ErrClosedPipe, io.ErrClosedPipe},
		{"complete", "abc", 3, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			r := &literalSource{
				reader:    strings.NewReader(tc.body),
				remaining: tc.size,
				finalize:  func() error { calls++; return tc.tail },
			}
			_, err := io.Copy(io.Discard, r)
			requireTestCondition(t, errors.Is(err, tc.want), err)
			requireTestCondition(t, r.finished.Load() == (tc.want == nil), "premature EOF")
			requireTestCondition(t, (tc.size <= int64(len(tc.body)) || calls == 0), "finalized truncated literal")
		})
	}
}

func TestSearchExpansionBound(t *testing.T) {
	_, err := boundedUIDs(&imap.SearchData{All: imap.UIDSet{{Start: 1, Stop: 4294967295}}})
	requireTestCondition(t, err != nil, "unbounded UID expansion")
	_, err = boundedUIDs(&imap.SearchData{All: imap.UIDSet{{Start: 0, Stop: 1}}})
	requireTestCondition(t, err != nil, "dynamic search result accepted")
	uids, err := boundedUIDs(&imap.SearchData{All: imap.UIDSetNum(1, 3, 9)})
	requireTestCondition(t, (err == nil && len(uids) == 3), uids, err)
}
