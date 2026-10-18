//go:build linux

package replicaconfig

import "testing"

func requireReplicaNoError(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
