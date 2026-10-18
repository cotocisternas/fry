// Command member-maintenance exercises the real private maintenance transport
// for disposable process rehearsals, without adding a production API.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/cotocisternas/fry/internal/hotstore"

	memberv1 "github.com/cotocisternas/fry/gen/member/go/fry/member/v1"
	memberconnect "github.com/cotocisternas/fry/gen/member/go/fry/member/v1/memberv1connect"
)

type report struct {
	Before identity `json:"before"`
	After  identity `json:"after"`
	States []string `json:"states"`
}

func run(ctx context.Context, args []string, output io.Writer) error {
	cfg, action, err := configuration(args)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	rpc, closeClient, err := client(cfg)
	if err != nil {
		return err
	}
	defer closeClient()
	before, err := readIdentity(ctx, rpc, cfg)
	if err != nil {
		return err
	}
	states, err := maintain(ctx, rpc, before, action)
	if err != nil {
		return err
	}
	after, err := readIdentity(ctx, rpc, cfg)
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(report{Before: before, After: after, States: states})
}

func maintain(
	ctx context.Context,
	rpc memberconnect.MemberServiceClient,
	before identity,
	action string,
) ([]string, error) {
	operation, err := hotstore.NewID()
	if err != nil {
		return nil, err
	}
	kind := memberv1.MaintenanceKind_MAINTENANCE_KIND_ROTATE
	if action == "reclaim-sealed" {
		kind = memberv1.MaintenanceKind_MAINTENANCE_KIND_SEALED_RECLAIM
	}
	request := &memberv1.MaintenanceRequest{
		Target:    &memberv1.Target{Incarnation: before.Incarnation, OwnerEpoch: before.OwnerEpoch},
		Operation: operation.String(),
		Kind:      kind,
	}
	var states []string
	for _, statusOnly := range []bool{false, true, false} {
		request.StatusOnly = statusOnly
		response, err := rpc.Maintenance(ctx, request)
		if err != nil {
			return nil, err
		}
		if response.State != "complete" || response.Identity != nil {
			return nil, fmt.Errorf("maintenance/status/replay not complete")
		}
		states = append(states, response.State)
	}
	return states, nil
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.Stdout)
	stop()
	if err != nil {
		if _, writeErr := fmt.Fprintln(os.Stderr, "Member maintenance failed:", err); writeErr != nil {
			os.Exit(1)
		}
		os.Exit(1)
	}
}
