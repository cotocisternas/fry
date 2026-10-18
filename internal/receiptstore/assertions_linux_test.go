//go:build linux

package receiptstore

import "testing"

func requireReceiptNoError(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func requireReceiptCondition(t testing.TB, ok bool, message ...any) {
	t.Helper()
	if !ok {
		t.Fatal(message...)
	}
}

func requireReceiptConditionf(t testing.TB, ok bool, format string, args ...any) {
	t.Helper()
	if !ok {
		t.Fatalf(format, args...)
	}
}
