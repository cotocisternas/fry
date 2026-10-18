//go:build linux

package receiptstore

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/cotocisternas/fry/internal/receiptstore/dbgen"

	"github.com/jackc/pgx/v5"
)

var ErrRelayBinding = errors.New(
	"receiptstore: relay destination mismatch, unbound history, or missing pending destination",
)

// RelayDestination contains routing/account identity only. Passwords and TLS
// trust roots may rotate independently and must never be stored in this catalog.
type RelayDestination struct {
	ID                           ID
	Address, Transport, Username string
}

func (r RelayDestination) Canonical() (RelayDestination, error) {
	host, port, err := net.SplitHostPort(r.Address)
	number, portErr := strconv.Atoi(port)
	if err != nil || portErr != nil || !r.validAccount(host, number) {
		return r, fmt.Errorf("%w: invalid relay address or account", ErrRelayBinding)
	}
	host, err = canonicalRelayHost(host)
	if err != nil {
		return r, err
	}
	if err = r.validateTransport(host); err != nil {
		return r, err
	}
	r.Address = net.JoinHostPort(host, strconv.Itoa(number))
	return r, nil
}

func (r RelayDestination) validAccount(host string, port int) bool {
	return port >= 1 && port <= 65535 && host != "" && r.ID != (ID{}) && len(r.Username) <= 1024 &&
		!strings.ContainsAny(r.Username, "\x00\r\n")
}

func canonicalRelayHost(host string) (string, error) {
	if ip := net.ParseIP(host); ip != nil {
		return ip.String(), nil
	}
	host = strings.ToLower(host)
	if len(host) > 253 {
		return "", ErrRelayBinding
	}
	for _, character := range host {
		if !relayHostCharacter(character) {
			return "", ErrRelayBinding
		}
	}
	return host, nil
}

func relayHostCharacter(c rune) bool {
	return c == '.' || c == '-' || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
}

func (r RelayDestination) validateTransport(host string) error {
	switch r.Transport {
	case "tls", "starttls":
		if r.Username == "" {
			return ErrRelayBinding
		}
	case "loopback":
		if !net.ParseIP(host).IsLoopback() {
			return ErrRelayBinding
		}
	default:
		return ErrRelayBinding
	}
	return nil
}

// BindRelayDestinations permanently binds IDs before a worker can resolve
// accepted mail. All configured bindings and pending-destination checks commit
// together. Old history is not evidence of which previously configured endpoint
// actually held that ID, so a missing binding cannot adopt existing jobs.
func (s *Store) BindRelayDestinations(ctx context.Context, destinations []RelayDestination) error {
	canonical, ids, err := canonicalRelayDestinations(destinations)
	if err != nil {
		return err
	}
	tx, err := s.db.begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(ctx, tx)
	// Serializes first bindings and the whole catalog consistency check, including
	// concurrent startup with a different destination for the same ID.
	if _, err = tx.Exec(ctx, `LOCK TABLE fry_relay_binding IN EXCLUSIVE MODE`); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `LOCK TABLE fry_outbound,fry_failure_notice IN SHARE MODE`); err != nil {
		return err
	}
	for _, r := range canonical {
		if err = bindRelayDestination(ctx, tx, r); err != nil {
			return err
		}
	}
	var missing bool
	if err = func() error {
		sqlcResult, sqlcErr := dbgen.New(tx).StoreBindRelayDestinationsCheckOutboundQueued(ctx, ids)
		if sqlcErr != nil {
			return sqlcErr
		}
		missing = sqlcResult
		return nil
	}(); err != nil {
		return err
	}
	if missing {
		return ErrRelayBinding
	}
	return commit(ctx, tx)
}

// HasPendingOutbound reports work that cannot safely lose its outbound worker:
// queued recipients, pending failure notices, or a currently leased submission.
// Terminal history alone does not prevent disabling outbound. Future work from
// other coordinators still requires deployment-wide configuration coordination.
func (s *Store) HasPendingOutbound(ctx context.Context) (bool, error) {
	var pending bool
	err := func() error {
		sqlcResult, sqlcErr := dbgen.New(s.db.pool).StoreHasPendingOutbound(ctx)
		if sqlcErr != nil {
			return sqlcErr
		}
		if sqlcResult == nil {
			return fmt.Errorf("receiptstore: unexpected NULL in StoreHasPendingOutbound.column_1")
		}
		pending = *sqlcResult
		return nil
	}()
	return pending, err
}

func canonicalRelayDestinations(destinations []RelayDestination) ([]RelayDestination, []string, error) {
	if len(destinations) < 1 || len(destinations) > 1024 {
		return nil, nil, fmt.Errorf("%w: invalid catalog size", ErrRelayBinding)
	}
	canonical := make([]RelayDestination, 0, len(destinations))
	ids := make([]string, 0, len(destinations))
	seen := map[ID]bool{}
	for _, destination := range destinations {
		r, err := destination.Canonical()
		if err != nil {
			return nil, nil, err
		}
		if seen[r.ID] {
			return nil, nil, fmt.Errorf("%w: duplicate relay", ErrRelayBinding)
		}
		seen[r.ID] = true
		canonical = append(canonical, r)
		ids = append(ids, r.ID.String())
	}
	return canonical, ids, nil
}

func bindRelayDestination(ctx context.Context, tx pgx.Tx, r RelayDestination) error {
	queries := dbgen.New(tx)
	stored, err := queries.StoreBindRelayDestinationsReadRelayBinding(ctx, r.ID.String())
	if err == nil {
		if stored.Address != r.Address || stored.Transport != r.Transport || stored.Username != r.Username {
			return ErrRelayBinding
		}
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	history, err := queries.StoreBindRelayDestinationsCheckOutbound(ctx, r.ID.String())
	if err != nil {
		return err
	}
	if history {
		return ErrRelayBinding
	}
	_, err = queries.StoreBindRelayDestinationsInsertRelayBinding(
		ctx,
		dbgen.StoreBindRelayDestinationsInsertRelayBindingParams{
			ID:        r.ID.String(),
			Address:   r.Address,
			Transport: r.Transport,
			Username:  r.Username,
		},
	)
	return err
}
