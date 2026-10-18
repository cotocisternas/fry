//go:build linux

package receiptstore

import (
	"context"
	"errors"
	"testing"

	"github.com/cotocisternas/fry/internal/hotstore"
)

func TestExplicitMaintenanceRejectsUnknownMember(t *testing.T) {
	for name, run := range map[string]func(*Store, context.Context, ID) error{
		"rewrite":        (*Store).ReclaimHot,
		"rotate":         (*Store).RotateHot,
		"sealed reclaim": (*Store).ReclaimSealed,
	} {
		t.Run(name, func(t *testing.T) {
			id := newID(t)
			if err := run(&Store{}, t.Context(), id); !errors.Is(err, ErrNotFound) {
				t.Fatalf("unknown member: %v", err)
			}
		})
	}
}

func TestExplicitMaintenanceRejectsUnavailableMemberAndReleasesGate(t *testing.T) {
	for _, state := range []struct {
		name                           string
		closed, frozen, transportOwned bool
		want                           error
	}{
		{"transport-owned", true, true, true, ErrStale},
		{"closed", true, true, false, hotstore.ErrClosed},
		{"frozen", false, true, false, ErrUncertain},
	} {
		for name, run := range map[string]func(*Store, context.Context, ID) error{
			"rewrite":        (*Store).ReclaimHot,
			"rotate":         (*Store).RotateHot,
			"sealed reclaim": (*Store).ReclaimSealed,
		} {
			t.Run(name+"/"+state.name, func(t *testing.T) {
				id := newID(t)
				m := &LocalMember{
					bytes:  copyTestStore(t, 1024),
					closed: state.closed, frozen: state.frozen, transportOwned: state.transportOwned,
				}
				store := &Store{memberByID: map[ID]Member{id: m}}
				if err := run(store, t.Context(), id); !errors.Is(err, state.want) {
					t.Fatalf("unavailable member: got %v, want %v", err, state.want)
				}
				if !m.gate.TryLock() {
					t.Fatal("failed dispatch retained the exclusive member gate")
				}
				m.gate.Unlock()
			})
		}
	}
}
