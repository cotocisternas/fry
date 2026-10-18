//go:build linux

package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/cotocisternas/fry/internal/admin"
	"github.com/cotocisternas/fry/internal/receiptstore"
)

func TestSubmissionOnlyOrganizationVisibleToAdministration(t *testing.T) {
	ctx := context.Background()
	var err error
	db := commandDatabase(t, "fry_submission_runtime_")
	checkCommand(t, receiptstore.Migrate(ctx, db))
	checkCommand(t, admin.Migrate(ctx, db))
	var cfg config
	checkCommand(
		t,
		json.Unmarshal(
			[]byte(
				"{\"outbound\":{\"submission\":{\"accounts\":[{\"username\":\"sender\",\"organization"+
					"\":\"01010101010101010101010101010101\"}]}}}",
			),
			&cfg,
		),
	)
	// No routes or IMAP account: registration cannot depend on a mailbox.
	{
		_, err := prepareDirectory(ctx, db, nil, cfg)
		checkCommand(t, err)
	}
	org := cfg.Outbound.Submission.Accounts[0].Organization
	var name string
	if err = db.QueryRow(ctx, `SELECT name FROM fry_organization WHERE id=$1`, org).
		Scan(&name); err != nil ||
		name != org {
		t.Fatalf("outbound organization absent: %q %v", name, err)
	}
	{
		_, err := db.Exec(ctx, `UPDATE fry_organization SET name='Send-only customer' WHERE id=$1`, org)
		checkCommand(t, err)
	}
	{
		_, err := prepareDirectory(ctx, db, nil, cfg)
		checkCommand(t, err)
	}
	if err = db.QueryRow(ctx, `SELECT name FROM fry_organization WHERE id=$1`, org).
		Scan(&name); err != nil ||
		name != "Send-only customer" {
		t.Fatalf("bootstrap replaced name: %q %v", name, err)
	}
}
