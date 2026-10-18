//go:build linux

package hotstore

import "testing"

func requireHotNoError(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
