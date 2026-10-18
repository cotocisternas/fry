package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
)

type commandOptions struct {
	origin            string
	username          string
	organization      string
	id                string
	name              string
	grantsPath        string
	limitBytes        string
	disableLimit      bool
	enabled           bool
	after             string
	member            string
	limit             int
	before            int64
	domain            string
	address           string
	inbox             string
	inboxes           string
	authorizedSenders string
	authorizedDomains string
	senderScope       string
	challenge         string
	reason            string
	output            string
	command           string
}

func (o *commandOptions) registerIdentityFlags(flags *flag.FlagSet) {
	flags.StringVar(&o.origin, "origin", "http://127.0.0.1:8080", "configured administration origin")
	flags.StringVar(
		&o.username,
		"username",
		"operator",
		"local administrator username; password comes from FRY_ADMIN_PASSWORD",
	)
	flags.StringVar(&o.organization, "organization", "", "organization ID")
	flags.StringVar(&o.id, "id", "", "record ID (optional for creation)")
	flags.StringVar(&o.name, "name", "", "organization or local account name")
	flags.StringVar(&o.grantsPath, "grants-file", "", "JSON array of the user's complete local grants")
	flags.BoolVar(&o.enabled, "enabled", true, "account enabled status")
}

func (o *commandOptions) registerDirectoryFlags(flags *flag.FlagSet) {
	flags.StringVar(&o.domain, "domain", "", "incoming domain")
	flags.StringVar(&o.address, "address", "", "incoming address")
	flags.StringVar(&o.inbox, "inbox", "", "inbox ID (catch-all for domain-save; empty rejects unknown recipients)")
	flags.StringVar(&o.inboxes, "inboxes", "", "comma-separated destination inbox IDs for alias-save")
}

func (o *commandOptions) registerSubmissionFlags(flags *flag.FlagSet) {
	flags.StringVar(
		&o.authorizedSenders,
		"authorized-senders",
		"",
		"comma-separated permitted outgoing email addresses",
	)
	flags.StringVar(&o.authorizedDomains, "authorized-domains", "", "comma-separated permitted outgoing domains")
	flags.StringVar(&o.senderScope, "scope", "", "outgoing domain or exact email address")
	flags.StringVar(&o.challenge, "challenge", "", "outgoing sender DNS challenge ID")
	flags.StringVar(&o.reason, "reason", "", "reason for sender revocation or global override")
}

func (o *commandOptions) registerStorageFlags(flags *flag.FlagSet) {
	flags.StringVar(
		&o.limitBytes,
		"limit-bytes",
		"",
		"positive decimal advisory storage threshold in retained original bytes",
	)
	flags.BoolVar(&o.disableLimit, "disable-limit", false, "explicitly disable the advisory storage threshold")
	flags.StringVar(&o.after, "after", "", "receipt or member-file ID pagination cursor")
	flags.StringVar(&o.member, "member", "", "member ID for member-storage diagnostics")
	flags.IntVar(&o.limit, "limit", 64, "member-storage file page size (1-64)")
	flags.Int64Var(&o.before, "before", 0, "audit ID pagination cursor")
	flags.StringVar(&o.output, "out", "", "new file for a complete original message; never overwritten")
}

func commandUsage(flags *flag.FlagSet) {
	if _, err := fmt.Fprintln(
		flags.Output(),
		"Usage: fry-admin [flags] COMMAND\nCommands: organizations, organization-save, "+
			"inboxes, inbox-create, domains, domain-save, aliases, alias-save, mail-accounts, "+
			"mail-account-save, mail-access-set, member-storage, storage-usage, "+
			"storage-quota-set, submission-accounts, submission-account-create, "+
			"submission-policy-set, submission-password-set, submission-access-set, "+
			"sender-grants, sender-challenge, sender-verify, sender-revoke, sender-override, "+
			"messages, download, audit, users, user-save, access-set, grants-set\n"+
			"FRY_ADMIN_PASSWORD authenticates. user-save reads FRY_NEW_ADMIN_PASSWORD; "+
			"mail-account-save reads FRY_NEW_MAIL_PASSWORD; submission-account-create and "+
			"submission-password-set read FRY_NEW_SUBMISSION_PASSWORD.",
	); err != nil {
		log.Print("could not write command usage")
	}
	flags.PrintDefaults()
}

// commandUsageError distinguishes invalid command input from failures after a
// request has started. It is safe to correct the input and submit again.
type commandUsageError struct{ cause error }

func (e *commandUsageError) Error() string { return e.cause.Error() }
func (e *commandUsageError) Unwrap() error { return e.cause }

func parseCommand(args []string, output io.Writer) (commandOptions, error) {
	var o commandOptions
	flags := flag.NewFlagSet("fry-admin", flag.ContinueOnError)
	flags.SetOutput(output)
	o.registerIdentityFlags(flags)
	o.registerDirectoryFlags(flags)
	o.registerSubmissionFlags(flags)
	o.registerStorageFlags(flags)
	flags.Usage = func() { commandUsage(flags) }
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return o, err
		}
		return o, &commandUsageError{cause: err}
	}
	if flags.NArg() != 1 {
		flags.Usage()
		return o, &commandUsageError{cause: fmt.Errorf("one command required")}
	}
	o.command = flags.Arg(0)
	return o, nil
}
