//go:build linux

package imapmigrate

import "testing"

func requireTestCondition(t testing.TB, ok bool, args ...any) {
	t.Helper()
	if !ok {
		t.Fatal(args...)
	}
}

func requireTestConditionf(t testing.TB, ok bool, format string, args ...any) {
	t.Helper()
	if !ok {
		t.Fatalf(format, args...)
	}
}
