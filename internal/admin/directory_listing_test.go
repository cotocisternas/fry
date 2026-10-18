//go:build linux

package admin

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/cotocisternas/fry/internal/maildirectory"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestDirectoryListingsAuthorizationAndEmptyShape(t *testing.T) {
	f := setup(t)
	manager := f.manager(t, false)
	listings := []struct {
		name string
		read func(context.Context, string, string) (any, error)
	}{
		{"domains", func(ctx context.Context, token, org string) (any, error) {
			return f.service.Domains(ctx, token, org)
		}},
		{"aliases", func(ctx context.Context, token, org string) (any, error) {
			return f.service.Aliases(ctx, token, org)
		}},
		{"mail accounts", func(ctx context.Context, token, org string) (any, error) {
			return f.service.MailAccounts(ctx, token, org)
		}},
	}
	requests := []struct {
		name, token, org, wantJSON string
		want                       error
	}{
		{"invalid scope before authentication", "", "invalid", "null", ErrInvalid},
		{"unauthenticated", "", f.org, "null", ErrUnauthorized},
		{"other organization", manager.Token, f.other, "null", ErrForbidden},
		{"configuration permission suffices", manager.Token, f.org, "[]", nil},
		{"operator wildcard", f.root.Token, f.other, "[]", nil},
	}
	for _, listing := range listings {
		for _, request := range requests {
			t.Run(listing.name+"/"+request.name, func(t *testing.T) {
				got, err := listing.read(t.Context(), request.token, request.org)
				if !errors.Is(err, request.want) {
					t.Fatalf("listing error = %v, want %v", err, request.want)
				}
				raw, encodeErr := json.Marshal(got)
				check(t, encodeErr)
				if string(raw) != request.wantJSON {
					t.Fatalf("listing JSON = %s, want %s", raw, request.wantJSON)
				}
			})
		}
	}
}

func TestDirectoryListingsUseCurrentGrants(t *testing.T) {
	f := setup(t)
	manager := f.manager(t, false)
	check(t, f.service.ReplaceGrants(t.Context(), f.root.Token, manager.Identity.ID, []Grant{{Scope: f.other}}))
	domains, err := f.service.Domains(t.Context(), manager.Token, f.org)
	requireErrorIs(t, err, ErrForbidden)
	aliases, err := f.service.Aliases(t.Context(), manager.Token, f.org)
	requireErrorIs(t, err, ErrForbidden)
	accounts, err := f.service.MailAccounts(t.Context(), manager.Token, f.org)
	requireErrorIs(t, err, ErrForbidden)
	if domains != nil || aliases != nil || accounts != nil {
		t.Fatal("revoked directory access returned rows")
	}
}

func TestDirectoryListingsPreserveScopedRows(t *testing.T) {
	f := setup(t)
	manager := f.manager(t, false)
	seedDirectoryListingRows(t, f)
	domains, err := f.service.Domains(t.Context(), manager.Token, f.org)
	check(t, err)
	wantDomains := []maildirectory.Domain{
		{Name: "listing.test", Organization: f.org, CatchAll: f.inbox, Enabled: false},
	}
	if !reflect.DeepEqual(domains, wantDomains) {
		t.Fatalf("domains = %+v, want %+v", domains, wantDomains)
	}
	aliases, err := f.service.Aliases(t.Context(), manager.Token, f.org)
	check(t, err)
	wantAliases := []maildirectory.Alias{
		{Address: "in@listing.test", Organization: f.org, Inboxes: []string{f.inbox}, Enabled: true},
	}
	if !reflect.DeepEqual(aliases, wantAliases) {
		t.Fatalf("aliases = %+v, want %+v", aliases, wantAliases)
	}
	accounts, err := f.service.MailAccounts(t.Context(), manager.Token, f.org)
	check(t, err)
	wantAccounts := []maildirectory.Account{
		{Username: "listing-reader", Organization: f.org, Inbox: f.inbox, Enabled: false},
	}
	if !reflect.DeepEqual(accounts, wantAccounts) {
		t.Fatalf("accounts = %+v, want %+v", accounts, wantAccounts)
	}
}

func seedDirectoryListingRows(t *testing.T, f *fixture) {
	t.Helper()
	otherInbox := newID(t)
	check(t, f.service.CreateInbox(t.Context(), f.root.Token, Inbox{ID: otherInbox, Organization: f.other}))
	for _, row := range []struct{ org, inbox, domain, username string }{
		{f.org, f.inbox, "listing.test", "listing-reader"},
		{f.other, otherInbox, "other.test", "other-reader"},
	} {
		check(t, f.service.PutDomain(t.Context(), f.root.Token, maildirectory.Domain{
			Name: row.domain, Organization: row.org, CatchAll: row.inbox, Enabled: false,
		}))
		check(t, f.service.PutAlias(t.Context(), f.root.Token, maildirectory.Alias{
			Address: "in@" + row.domain, Organization: row.org, Inboxes: []string{row.inbox}, Enabled: true,
		}))
		check(t, f.service.PutMailAccount(t.Context(), f.root.Token, maildirectory.Account{
			Username: row.username, Organization: row.org, Inbox: row.inbox, Enabled: false,
		}, "listing-mail-password"))
	}
}

func TestDirectoryListingQueryFailureReleasesPolicyLock(t *testing.T) {
	f := setup(t)
	manager := f.manager(t, false)
	// Rename only this fixture's table so authorization succeeds but the real
	// listing query fails. Schema cleanup removes the fault with the fixture.
	_, err := f.db.Exec(t.Context(), "ALTER TABLE fry_domain RENAME TO listing_unavailable")
	check(t, err)
	rows, err := f.service.Domains(t.Context(), manager.Token, f.org)
	var failure *pgconn.PgError
	if !errors.As(err, &failure) || failure.Code != "42P01" || rows != nil {
		t.Fatalf("query failure = %v, rows = %+v; want missing-table error and no rows", err, rows)
	}
	if acquired := f.db.Stat().AcquiredConns(); acquired != 0 {
		t.Fatalf("failed listing retained %d database connections", acquired)
	}
	// A leaked shared lock would prevent this exclusive policy transaction.
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	check(t, f.service.ReplaceGrants(ctx, f.root.Token, manager.Identity.ID, []Grant{{Scope: f.other}}))
}

func TestDirectoryListingCanceledPolicyWaitReleasesTransaction(t *testing.T) {
	f := setup(t)
	tx, err := f.service.begin(t.Context(), true)
	check(t, err)
	defer rollback(t.Context(), tx)
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	rows, err := f.service.Domains(ctx, f.root.Token, f.org)
	if !errors.Is(err, context.DeadlineExceeded) || rows != nil {
		t.Fatalf("canceled listing = %v, rows = %+v", err, rows)
	}
	check(t, tx.Commit(t.Context()))
	ctx, stop := context.WithTimeout(t.Context(), 2*time.Second)
	defer stop()
	// pgx releases canceled connections by asynchronously destroying them. They
	// remain acquired in pool statistics until network cleanup has completed.
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for acquired := f.db.Stat().AcquiredConns(); acquired != 0; acquired = f.db.Stat().AcquiredConns() {
		select {
		case <-ctx.Done():
			t.Fatalf("canceled listing retained %d database connections", acquired)
		case <-tick.C:
		}
	}
	// A subsequent request can use the pool and policy lock after cancellation.
	check(t, f.service.PutOrganization(ctx, f.root.Token, Organization{ID: f.org, Name: "After canceled listing"}))
}
