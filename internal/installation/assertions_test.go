//go:build linux

package installation_test

import "testing"

func checkCommand(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
