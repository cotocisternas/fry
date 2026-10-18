//go:build linux

package admin

import (
	"context"
	"sync"
	"testing"

	"github.com/cotocisternas/fry/internal/credentials"
)

// Share only immutable encodings made by the real password hasher. Each fixture
// persists its own users and grants, then obtains fresh sessions through Login.
var fixtureOperatorHash = sync.OnceValues(func() (string, error) {
	return credentials.HashPassword("test-operator-secret")
})

var fixtureManagerHash = sync.OnceValues(func() (string, error) {
	return credentials.HashPassword("test-manager-secret")
})

func seedFixtureOperator(t *testing.T, service *Service) {
	t.Helper()
	ctx := context.Background()
	encoded, err := fixtureOperatorHash()
	check(t, err)
	tx, err := service.begin(ctx, true)
	check(t, err)
	defer rollback(ctx, tx)
	check(t, bootstrapIdentity(ctx, tx, "operator", encoded, true))
	check(t, tx.Commit(ctx))
}

func seedFixtureManager(t *testing.T, f *fixture) string {
	t.Helper()
	ctx := context.Background()
	encoded, err := fixtureManagerHash()
	check(t, err)
	tx, err := f.service.begin(ctx, true)
	check(t, err)
	defer rollback(ctx, tx)
	id, err := putLocalUser(ctx, tx, f.root.Identity.ID, "manager", encoded, true)
	check(t, err)
	check(t, tx.Commit(ctx))
	return id
}

// Account security tests exercise public password issuance, including its
// creation audits, instead of using the prehashed general-purpose fixtures.
func setupLocalAccounts(t *testing.T) (*fixture, Session) {
	t.Helper()
	f := setupUninitialized(t)
	ctx := context.Background()
	created, err := f.service.Bootstrap(ctx, "operator", "test-operator-secret", true)
	check(t, err)
	if !created {
		t.Fatal("bootstrap did not create operator")
	}
	f.initialize(t)
	id, err := f.service.PutUser(ctx, f.root.Token, "manager", "test-manager-secret", true)
	check(t, err)
	manager := f.managerSession(t, id, false)
	requireAccountCreationAudits(t, f, manager.Identity.ID)
	return f, manager
}

func requireAccountCreationAudits(t *testing.T, f *fixture, manager string) {
	t.Helper()
	events, err := f.service.Audits(context.Background(), f.root.Token, 0)
	check(t, err)
	for _, want := range []struct{ action, target string }{
		{"bootstrap", f.root.Identity.ID},
		{"account-update", manager},
	} {
		count := 0
		for _, event := range events {
			if event.Action == want.action {
				if event.Actor != f.root.Identity.ID || event.Target != want.target || event.Outcome != "committed" {
					t.Fatalf("wrong account creation audit: %+v", event)
				}
				count++
			}
		}
		if count != 1 {
			t.Fatalf("account creation audit %q count=%d, want 1", want.action, count)
		}
	}
}
