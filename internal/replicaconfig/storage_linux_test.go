//go:build linux

package replicaconfig

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"
)

func testStorage() Storage {
	c := Storage{DataDirectory: "unused", RecoveryRetentionHours: 24}
	for i := range 2 {
		c.IngestShards = append(
			c.IngestShards,
			IngestShardConfig{ID: fmt.Sprintf("%032x", i+101), Members: []MemberConfig{
				{ID: fmt.Sprintf("%032x", i*2+1), Domain: "zone-a"},
				{ID: fmt.Sprintf("%032x", i*2+2), Domain: "zone-b"},
			}},
		)
	}
	return c
}

func TestStorageCatalogValidation(t *testing.T) {
	for _, tt := range []struct {
		name   string
		change func(*Storage)
	}{
		{"ambiguous", func(c *Storage) { c.Members = c.IngestShards[0].Members }},
		{"empty", func(c *Storage) { c.IngestShards = nil }},
		{"single replica", func(c *Storage) { c.IngestShards[0].Members = c.IngestShards[0].Members[:1] }},
		{"shared member", func(c *Storage) { c.IngestShards[1].Members[0].ID = c.IngestShards[0].Members[0].ID }},
		{"same shard", func(c *Storage) { c.IngestShards[1].ID = c.IngestShards[0].ID }},
		{"same failure domain", func(c *Storage) { c.IngestShards[1].Members[0].Domain = "zone-b" }},
		{"empty failure domain", func(c *Storage) { c.IngestShards[0].Members[0].Domain = "" }},
		{"zero shard", func(c *Storage) { c.IngestShards[0].ID = fmt.Sprintf("%032x", 0) }},
		{"zero member", func(c *Storage) { c.IngestShards[0].Members[0].ID = fmt.Sprintf("%032x", 0) }},
		{"invalid remote", func(c *Storage) { c.ReplicaTLS = &TLS{} }},
		{"remote without trust", func(c *Storage) { c.IngestShards[0].Members[0].URL = "https://127.0.0.1:9999" }},
		{"too many shards", func(c *Storage) {
			for len(c.IngestShards) < 17 {
				i := len(c.IngestShards)
				c.IngestShards = append(c.IngestShards, IngestShardConfig{
					ID: fmt.Sprintf("%032x", i+101),
					Members: []MemberConfig{
						{
							ID:     fmt.Sprintf("%032x", i*2+1),
							Domain: "zone-a",
						},
						{
							ID:     fmt.Sprintf("%032x", i*2+2),
							Domain: "zone-b",
						},
					},
				})
			}
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := testStorage()
			tt.change(&c)
			if c.Validate() == nil {
				t.Fatal("invalid catalog accepted")
			}
		})
	}
}

func TestStorageCatalogMemberEnumeration(t *testing.T) {
	c := testStorage()
	requireReplicaNoError(t, c.Validate())
	members := c.AllMembers()
	if len(members) != 4 || members[3].ID != c.IngestShards[1].Members[1].ID {
		t.Fatal("maintenance omitted catalog members")
	}
	members[0].ID = "mutated"
	if c.IngestShards[0].Members[0].ID == "mutated" {
		t.Fatal("enumeration aliases configuration")
	}
	c.Members = c.IngestShards[0].Members
	c.IngestShards = nil
	if err := c.Validate(); err != nil {
		t.Fatal("legacy configuration:", err)
	}
	members = c.AllMembers()
	members[0].ID = "mutated"
	if c.Members[0].ID == "mutated" {
		t.Fatal("legacy enumeration aliases configuration")
	}
}

func TestStorageExamplesValidate(t *testing.T) {
	for _, name := range []string{
		"local-smtp",
		"local-mail",
		"local-admin",
		"local-collector",
		"local-migration",
		"public-mail",
		"local-shards",
	} {
		t.Run(name, func(t *testing.T) {
			// G304: name comes only from the literal fixture allowlist above; owner FRY maintainers,
			// review by 2027-01-18.
			//nolint:gosec // Validated configuration/fixture path.
			b, err := os.ReadFile("../../examples/" + name + ".json")
			requireReplicaNoError(t, err)
			var c Storage
			err = json.Unmarshal(b, &c)
			requireReplicaNoError(t, err)
			err = c.Validate()
			requireReplicaNoError(t, err)
		})
	}
}
