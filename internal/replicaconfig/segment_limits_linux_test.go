//go:build linux

package replicaconfig

import (
	"encoding/json"
	"testing"
)

func TestStorageSegmentConfiguration(t *testing.T) {
	c := testStorage()
	requireReplicaNoError(
		t,
		json.Unmarshal(
			[]byte(
				"{\"max_message_bytes\":1024,\"member_capacity_bytes\":1048576,\"max_in"+
					"_flight\":2,\"segment_max_records\":8,\"segment_max_bytes\":16384}",
			),
			&c,
		),
	)
	if c.SegmentMaxRecords != 8 || c.SegmentMaxBytes != 16384 {
		t.Fatal("segment limits were not decoded")
	}
	requireReplicaNoError(t, c.Validate())
	for _, tc := range []struct {
		name   string
		change func(*Storage)
	}{
		{"missing byte limit", func(c *Storage) { c.SegmentMaxBytes = 0 }},
		{"missing record limit", func(c *Storage) { c.SegmentMaxRecords = 0 }},
		{"negative record limit", func(c *Storage) { c.SegmentMaxRecords = -1 }},
		{"message cannot fit", func(c *Storage) { c.SegmentMaxBytes = c.MaxMessageBytes }},
		{"no rewrite headroom", func(c *Storage) { c.MemberCapacityBytes = c.SegmentMaxBytes }},
		{"remote limits belong to member", func(c *Storage) { c.ReplicaTLS = &TLS{} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := c
			tc.change(&changed)
			if err := changed.Validate(); err == nil {
				t.Fatal("invalid or inapplicable segment limits accepted")
			}
		})
	}
}
