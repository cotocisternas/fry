//go:build linux

package hotstore

import "testing"

func requireHotCondition(t testing.TB, ok bool, args ...any) {
	t.Helper()
	if !ok {
		t.Fatal(args...)
	}
}

func requireHotConditionf(t testing.TB, ok bool, format string, args ...any) {
	t.Helper()
	if !ok {
		t.Fatalf(format, args...)
	}
}
