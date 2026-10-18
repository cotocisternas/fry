package s3fixture_test

import "testing"

func requireFixtureNoError(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
