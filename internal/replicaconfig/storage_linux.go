//go:build linux

package replicaconfig

import (
	"context"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/cotocisternas/fry/internal/hotstore"
	"github.com/cotocisternas/fry/internal/receiptstore"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Storage is the shared local or private-replica receipt-store configuration.
// Embedding it preserves the executable configuration's top-level JSON fields.
type Storage struct {
	ReplicaTLS             *TLS                `json:"replica_tls"`
	DataDirectory          string              `json:"data_directory"`
	MaxMessageBytes        int64               `json:"max_message_bytes"`
	MemberCapacityBytes    int64               `json:"member_capacity_bytes"`
	SegmentMaxRecords      int64               `json:"segment_max_records,omitempty"`
	SegmentMaxBytes        int64               `json:"segment_max_bytes,omitempty"`
	RecoveryRetentionHours int                 `json:"recovery_retention_hours"`
	CleanupDeadlineHours   int                 `json:"cleanup_deadline_hours"`
	MaxInFlight            int                 `json:"max_in_flight"`
	Members                []MemberConfig      `json:"members,omitempty"`
	IngestShards           []IngestShardConfig `json:"ingest_shards,omitempty"`
}

// IngestShardConfig binds one stable shard ID to an independent replica pair.
type IngestShardConfig struct {
	ID      string         `json:"id"`
	Members []MemberConfig `json:"members"`
}

type MemberConfig struct {
	URL       string `json:"url"`
	ServerKey string `json:"server_key"`
	ID        string `json:"id"`
	Domain    string `json:"domain"`
}

func ParseID(raw string) (receiptstore.ID, error) {
	var result receiptstore.ID
	b, err := hex.DecodeString(raw)
	if err != nil || len(b) != len(result) {
		return result, fmt.Errorf("IDs must be 32 hexadecimal characters")
	}
	copy(result[:], b)
	if result == (receiptstore.ID{}) {
		return result, fmt.Errorf("zero ID is invalid")
	}
	return result, nil
}

// AllMembers enumerates the configured catalog for maintenance. Validate must
// succeed before callers act on the returned members.
func (c Storage) AllMembers() []MemberConfig {
	if len(c.IngestShards) == 0 {
		return append([]MemberConfig(nil), c.Members...)
	}
	var members []MemberConfig
	for _, shard := range c.IngestShards {
		members = append(members, shard.Members...)
	}
	return members
}

func (c Storage) Validate() error {
	if err := c.validateLimits(); err != nil {
		return err
	}
	if len(c.IngestShards) > 0 && len(c.Members) > 0 {
		return fmt.Errorf("members and ingest_shards are mutually exclusive")
	}
	pairs := c.IngestShards
	if len(pairs) == 0 {
		pairs = []IngestShardConfig{{Members: c.Members}}
	} else if len(pairs) > 16 {
		return fmt.Errorf("at most 16 ingestion shards are supported")
	}
	shards := make(map[receiptstore.ID]bool)
	members := make(map[receiptstore.ID]bool)
	for _, pair := range pairs {
		if len(c.IngestShards) > 0 {
			if err := validateShardID(pair.ID, shards); err != nil {
				return err
			}
		}
		if err := c.validatePair(pair, members); err != nil {
			return err
		}
	}
	return nil
}

func (c Storage) validateLimits() error {
	if c.SegmentMaxRecords != 0 || c.SegmentMaxBytes != 0 {
		if c.ReplicaTLS != nil {
			return fmt.Errorf("configure segment limits on each fry-member for remote replicas")
		}
		if err := receiptstore.ValidateMemberOptions(c.memberOptions()); err != nil {
			return fmt.Errorf("storage segment limits: %w", err)
		}
	}
	if !c.validRetention() || (c.ReplicaTLS == nil && c.DataDirectory == "") {
		return fmt.Errorf("a data directory and bounded retention are required")
	}
	return nil
}

func validateShardID(raw string, seen map[receiptstore.ID]bool) error {
	id, err := ParseID(raw)
	if err != nil {
		return fmt.Errorf("shard ID: %w", err)
	}
	if seen[id] {
		return fmt.Errorf("duplicate shard ID")
	}
	seen[id] = true
	return nil
}

func (c Storage) validatePair(pair IngestShardConfig, members map[receiptstore.ID]bool) error {
	if len(pair.Members) != 2 {
		return fmt.Errorf("each ingestion shard requires exactly two members")
	}
	domains := make(map[string]bool)
	for _, member := range pair.Members {
		id, err := ParseID(member.ID)
		if err != nil {
			return fmt.Errorf("member ID: %w", err)
		}
		if members[id] {
			return fmt.Errorf("member may belong to only one ingestion shard")
		}
		members[id] = true
		if member.Domain == "" || strings.TrimSpace(member.Domain) != member.Domain || domains[member.Domain] {
			return fmt.Errorf("each replica pair requires two distinct nonempty failure domains")
		}
		domains[member.Domain] = true
		if err := member.validateTransport(c.ReplicaTLS != nil); err != nil {
			return err
		}
	}
	return nil
}

func (m MemberConfig) validateTransport(remote bool) error {
	if !remote {
		if m.URL != "" || m.ServerKey != "" {
			return fmt.Errorf("remote members require replica_tls")
		}
		return nil
	}
	if m.URL == "" {
		return fmt.Errorf("remote member URL is required")
	}
	_, err := Key(m.ServerKey)
	return err
}

func (c Storage) memberOptions() hotstore.Options {
	return hotstore.Options{
		MaxMessageBytes:      c.MaxMessageBytes,
		MaxBytes:             c.MemberCapacityBytes,
		MaxConcurrentAppends: c.MaxInFlight,
		MaxRecords:           c.SegmentMaxRecords,
		MaxFileBytes:         c.SegmentMaxBytes,
	}
}

// Open closes previously opened members on failure. The returned closer releases
// all members in reverse order; the caller retains ownership of the pool.
func (c Storage) Open(ctx context.Context, pool *pgxpool.Pool) (_ *receiptstore.Store, _ func() error, err error) {
	if err = c.Validate(); err != nil {
		return nil, nil, err
	}
	group := new(memberClosers)
	defer func() {
		if err != nil {
			err = errors.Join(err, group.close())
		}
	}()
	members, err := c.openMembers(ctx, pool, group)
	if err != nil {
		return nil, nil, err
	}
	store, err := c.composeStore(ctx, pool, members)
	if err != nil {
		return nil, nil, err
	}
	return store, group.close, nil
}

type memberClosers struct {
	once    sync.Once
	closers []func() error
	err     error
}

func (g *memberClosers) close() error {
	g.once.Do(func() {
		for _, closeMember := range slices.Backward(g.closers) {
			g.err = errors.Join(g.err, closeMember())
		}
	})
	return g.err
}

func (c Storage) openMembers(
	ctx context.Context,
	pool *pgxpool.Pool,
	group *memberClosers,
) ([]receiptstore.Member, error) {
	configured := c.AllMembers()
	if c.ReplicaTLS != nil {
		tc, err := c.ReplicaTLS.Load()
		if err != nil {
			return nil, err
		}
		return c.openRemoteMembers(ctx, configured, tc, group)
	}
	absolute, err := Directory(c.DataDirectory)
	if err != nil {
		return nil, err
	}
	members := make([]receiptstore.Member, len(configured))
	for i, m := range configured {
		local, err := c.openLocalMember(ctx, pool, absolute, m)
		if err != nil {
			return nil, err
		}
		group.closers = append(group.closers, local.Close)
		members[i] = local
	}
	return members, nil
}

func (c Storage) openLocalMember(
	ctx context.Context,
	pool *pgxpool.Pool,
	absolute string,
	m MemberConfig,
) (*receiptstore.LocalMember, error) {
	if m.URL != "" || m.ServerKey != "" {
		return nil, fmt.Errorf("remote members require replica_tls")
	}
	id, err := ParseID(m.ID)
	if err != nil {
		return nil, err
	}
	directory, err := Directory(filepath.Join(absolute, id.String()))
	if err != nil {
		return nil, err
	}
	return receiptstore.OpenLocalMember(ctx, pool, id, m.Domain, directory, c.memberOptions())
}

func (c Storage) openRemoteMembers(
	ctx context.Context,
	configured []MemberConfig,
	tc *tls.Config,
	group *memberClosers,
) ([]receiptstore.Member, error) {
	members := make([]receiptstore.Member, len(configured))
	for i, m := range configured {
		id, err := ParseID(m.ID)
		if err != nil {
			return nil, err
		}
		key, err := Key(m.ServerKey)
		if err != nil {
			return nil, err
		}
		remote, err := receiptstore.DialRemoteMember(
			ctx,
			receiptstore.RemoteMemberOptions{
				URL:             m.URL,
				TLS:             tc,
				Member:          id,
				FailureDomain:   m.Domain,
				ServerKey:       key,
				MaxMessageBytes: c.MaxMessageBytes,
				MaxConnections:  c.MaxInFlight + 4,
				Timeout:         5 * time.Minute,
			},
		)
		if err != nil {
			return nil, err
		}
		group.closers = append(group.closers, func() error { remote.Close(); return nil })
		members[i] = remote
	}
	return members, nil
}

func (c Storage) composeStore(
	ctx context.Context,
	pool *pgxpool.Pool,
	members []receiptstore.Member,
) (*receiptstore.Store, error) {
	opts := receiptstore.Options{
		Lease:             30 * time.Second,
		RecoveryRetention: time.Duration(c.RecoveryRetentionHours) * time.Hour,
		CleanupDeadline:   time.Duration(c.CleanupDeadlineHours) * time.Hour,
	}
	if len(c.IngestShards) == 0 {
		return receiptstore.NewWithMembers(ctx, pool, [2]receiptstore.Member{members[0], members[1]}, opts)
	}
	shards := make([]receiptstore.IngestShard, len(c.IngestShards))
	for i, cfg := range c.IngestShards {
		id, err := ParseID(cfg.ID)
		if err != nil {
			return nil, err
		}
		shards[i] = receiptstore.IngestShard{ID: id, Members: [2]receiptstore.Member{members[2*i], members[2*i+1]}}
	}
	return receiptstore.NewWithShards(ctx, pool, shards, opts)
}

func (c Storage) validRetention() bool {
	return c.CleanupDeadlineHours >= 0 && c.CleanupDeadlineHours <= 24*365 && c.RecoveryRetentionHours >= 1 &&
		c.RecoveryRetentionHours <= 24*365
}
