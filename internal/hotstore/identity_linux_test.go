//go:build linux

package hotstore

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestInspectFileIdentityNeverRecoversOrMutates(t *testing.T) {
	for _, damage := range []string{"header-only", "incomplete-body", "corrupt-header", "short-header"} {
		t.Run(damage, func(t *testing.T) {
			root := t.TempDir()
			store, err := Open(root, options())
			requireHotNoError(t, err)
			id := store.FileID()
			_, err = store.Append(context.Background(), key(t), bytes.NewBufferString("complete original"))
			requireHotNoError(t, err)
			err = store.Close()
			requireHotNoError(t, err)
			path := filepath.Join(root, "data.fry")
			// Fixed data.fry under t.TempDir; prepares identity fixture; owner: FRY maintainers; expires:
			// 2027-01-18.
			// #nosec G304 -- See the path-safety rationale above.
			raw, err := os.ReadFile(path)
			requireHotNoError(t, err)
			raw = damageIdentity(raw, damage)
			// Fixed data.fry under t.TempDir; intentional corruption fixture; owner: FRY maintainers; expires:
			// 2027-01-18.
			// #nosec G703 -- See the path-safety rationale above.
			err = os.WriteFile(path, raw, 0o600)
			requireHotNoError(t, err)
			sentinel := filepath.Join(root, "stage-identity-sentinel")
			err = os.WriteFile(sentinel, []byte("must remain"), 0o600)
			requireHotNoError(t, err)
			got, err := InspectFileIdentity(root)
			good := damage == "header-only" || damage == "incomplete-body"
			requireHotCondition(
				t,
				(!good || (err == nil && got == id)),
				"valid immutable header not recognized",
				damage,
				err,
			)
			requireHotCondition(t, (good || err != nil), "corrupt/truncated header accepted", damage)
			// Fixed data.fry under t.TempDir; verifies identity inspection preserved bytes; owner: FRY maintainers;
			// expires: 2027-01-18.
			// #nosec G304 -- See the path-safety rationale above.
			after, err := os.ReadFile(path)
			requireHotCondition(t, (err == nil && bytes.Equal(after, raw)), "identity inspection mutated data", err)
			// Fixed sentinel under t.TempDir; verifies identity inspection preserved staging; owner: FRY
			// maintainers; expires: 2027-01-18.
			// #nosec G304 -- See the path-safety rationale above.
			stage, err := os.ReadFile(sentinel)
			requireHotCondition(
				t,
				(err == nil && string(stage) == "must remain"),
				"identity inspection cleaned staging",
				err,
			)
		})
	}
}

func TestInspectFileIdentityDoesNotCreateOrFollowDirectorySymlink(t *testing.T) {
	empty := t.TempDir()
	{
		_, err := InspectFileIdentity(empty)
		requireHotCondition(t, err != nil, "missing image accepted")
	}
	entries, err := os.ReadDir(empty)
	requireHotCondition(t, (err == nil && len(entries) == 0), "identity inspection created files", err)
	root := t.TempDir()
	store, err := Open(root, options())
	requireHotNoError(t, err)
	err = store.Close()
	requireHotNoError(t, err)
	parent := t.TempDir()
	link := filepath.Join(parent, "linked-member")
	err = os.Symlink(root, link)
	requireHotNoError(t, err)
	{
		_, err = InspectFileIdentity(link)
		requireHotCondition(t, err != nil, "symlink directory accepted")
	}
}

func damageIdentity(raw []byte, damage string) []byte {
	switch damage {
	case "header-only":
		raw = raw[:fileHeaderSize]
	case "incomplete-body":
		raw = append(raw, []byte("unfinished-record")...)
	case "corrupt-header":
		raw[0] ^= 1
	case "short-header":
		raw = raw[:fileHeaderSize-1]
	}
	return raw
}
