//go:build linux

package imapcollect

import "testing"

func requireTestCondition(t testing.TB, ok bool, args ...any) {
	t.Helper()
	if !ok {
		t.Fatal(args...)
	}
}
