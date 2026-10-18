//go:build linux

package main

import "testing"

func checkCommand(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
