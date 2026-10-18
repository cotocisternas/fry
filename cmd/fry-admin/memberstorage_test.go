package main

import (
	"net/url"
	"strings"
	"testing"
)

func TestMemberStorageCLIQueryAndBounds(t *testing.T) {
	member, after := strings.Repeat("a", 32), strings.Repeat("b", 32)
	path, err := memberStoragePath(member, after, 1)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(path)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Path != "member-storage" || parsed.Query().Get("member") != member ||
		parsed.Query().Get("after") != after ||
		parsed.Query().Get("limit") != "1" {
		t.Fatal("wrong diagnostic request", path)
	}
	path, err = memberStoragePath(member, "", 64)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err = url.Parse(path)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Query().Has("after") {
		t.Fatal("empty cursor was transmitted")
	}
}

func TestMemberStorageCLIRejectsInvalidBounds(t *testing.T) {
	member := strings.Repeat("a", 32)
	for _, c := range []struct {
		member, after string
		limit         int
	}{
		{"", "", 64},
		{strings.Repeat("0", 32), "", 64},
		{strings.Repeat("A", 32), "", 64},
		{member, "bad", 64},
		{member, strings.Repeat("0", 32), 64},
		{member, "", 0},
		{member, "", 65},
	} {
		if _, err := memberStoragePath(c.member, c.after, c.limit); err == nil {
			t.Fatal("invalid CLI diagnostic request accepted", c)
		}
	}
}
