package main

import "testing"

func TestStorageQuotaExplicitCLIChoice(t *testing.T) {
	for _, c := range []struct {
		raw     string
		disable bool
	}{
		{"", false},
		{"1", true},
		{"0", false},
		{"-1", false},
		{"9223372036854775808", false},
	} {
		if _, err := storageQuotaPayload("org", c.raw, c.disable); err == nil {
			t.Fatal("unsafe choice accepted", c)
		}
	}
	p, err := storageQuotaPayload("org", "9007199254740993", false)
	if err != nil || p["limit_bytes"] != "9007199254740993" {
		t.Fatal("decimal lost", err)
	}
	p, err = storageQuotaPayload("org", "", true)
	if err != nil || p["limit_bytes"] != nil {
		t.Fatal("disable not explicitnull", err)
	}
}
