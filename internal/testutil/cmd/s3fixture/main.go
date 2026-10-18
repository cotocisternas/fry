// Command s3fixture is a disposable process-test helper, not an S3 service.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/cotocisternas/fry/internal/testutil/s3fixture"
)

func run(ctx context.Context, args []string, output io.Writer) error {
	flags := flag.NewFlagSet("s3fixture", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	unavailable := flags.String("unavailable-file", "", "presence makes fixture requests return 503")
	listen := flags.String(
		"listen",
		"127.0.0.1:0",
		"numeric loopback listener; restore requires exact retained endpoint",
	)
	stateIn := flags.String("state-in", "", "validated fixture state to restore before listening")
	stateOut := flags.String("state-out", "", "new fixture state file to publish after HTTP closes")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("s3fixture: unexpected arguments")
	}
	var fixture *s3fixture.Fixture
	var err error
	if *stateIn != "" {
		fixture, err = s3fixture.RestoreAtContext(ctx, *stateIn, *listen)
	} else {
		fixture, err = s3fixture.NewAtContext(ctx, s3fixture.Config{}, *listen)
	}
	if err != nil {
		return err
	}
	defer fixture.Close()
	if _, err = fmt.Fprintf(output, "FRY S3 fixture listening on %s\n", fixture.Server.URL); err != nil {
		return err
	}
	return serveFixture(ctx, fixture, *unavailable, *stateOut, output)
}

func serveFixture(
	ctx context.Context,
	fixture *s3fixture.Fixture,
	unavailable, stateOut string,
	output io.Writer,
) error {
	var err error
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			fixture.Close()
			if stateOut != "" {
				if err = fixture.SaveState(stateOut); err != nil {
					return err
				}
				_, err = fmt.Fprintln(output, "FRY S3 fixture state saved")
				return err
			}
			return nil
		case <-ticker.C:
			if unavailable != "" {
				_, err := os.Stat(unavailable)
				fixture.SetUnavailable(err == nil)
			}
		}
	}
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.Stdout)
	stop()
	if err != nil {
		if _, writeErr := fmt.Fprintln(os.Stderr, "S3 fixture failed"); writeErr != nil {
			os.Exit(1)
		}
		os.Exit(1)
	}
}
