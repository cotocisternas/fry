// Package receiptstore is the second, experimental ingestion stage. It combines
// PostgreSQL receipt authority and two in-process local byte members. It is not
// yet an SMTP server, a remote replication transport, or an AZ durability proof.
package receiptstore

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/cotocisternas/fry/internal/hotstore"
)

type ID = hotstore.ID

type Destination struct {
	Inbox      ID
	Recipients []string
	// Zero Folder selects INBOX; an explicit identity survives a folder rename.
	Folder       ID
	Flags        []string
	InternalDate time.Time
}
type Source struct {
	Connection       ID
	Folder           string
	UIDValidity, UID uint32
}

// Envelope is an already-authorized, already-resolved destination snapshot.
// Address resolution and organization/inbox ownership validation belong to the
// future authenticated ingress layer, not this internal storage adapter.
type Envelope struct {
	// MailboxAppend publishes its single explicit folder membership atomically
	// with acceptance. It has no SMTP envelope recipients or background delivery.
	MailboxAppend bool
	Organization  ID
	MailFrom      string
	Destinations  []Destination
	Source        *Source
	Outbound      *Outbound
	Migration     *MigrationImport
}

type SenderAuthorization struct {
	Grant   ID
	Version int64
}

// SubmissionAuthorization captures one authentication epoch. Receipt preparation
// and finalization fence it against rotation/disablement; accepted replay does not.
type SubmissionAuthorization struct {
	Username string
	Version  int64
}

// Outbound captures immutable submission policy and authorization proofs.
// RetryWindow must be explicitly bounded.
type Outbound struct {
	SubmissionAuthorization *SubmissionAuthorization `json:",omitempty"`
	SenderAuthorization     *SenderAuthorization     `json:",omitempty"`
	Relay                   ID
	Recipients              []string
	RetryWindow             time.Duration
}
type Options struct {
	Lease             time.Duration
	RecoveryRetention time.Duration
	// Zero selects five minutes for reads and 24 hours for active-byte cleanup.
	MaxReadLifetime time.Duration
	CleanupDeadline time.Duration
}
type Receipt struct {
	ID, Attempt, Organization ID
	ReceivedAt                time.Time
	Size                      int64
	SHA256                    [32]byte
	ProfileEpoch              int64
}
type Status struct {
	State   string
	Epoch   int64
	Receipt *Receipt
}

var (
	ErrBusy           = errors.New("receiptstore: attempt has an active lease or member operation")
	ErrNotFound       = errors.New("receiptstore: attempt or receipt not found")
	ErrConflict       = errors.New("receiptstore: attempt identity conflicts with its original request")
	ErrAborted        = errors.New("receiptstore: attempt aborted")
	ErrAccepted       = errors.New("receiptstore: accepted receipt cannot be aborted")
	ErrStale          = errors.New("receiptstore: stale lease, generation, or protection profile")
	ErrUncertain      = errors.New("receiptstore: transaction outcome uncertain; query status by attempt")
	ErrSourceConflict = errors.New("receiptstore: source message already reserved; resolve its original attempt")
	ErrExpired        = errors.New("receiptstore: receipt has no live references")
)

// MigrationImport binds an import to its durable job generation and ledger entry.
// Only the migration ledger constructs this field; acceptance verifies its envelope.
type MigrationImport struct {
	Job, Entry ID
	Generation int64
}

func normalize(e Envelope) (Envelope, []byte, error) {
	if err := validateEnvelopeMode(e); err != nil {
		return e, nil, err
	}
	if e.Outbound != nil {
		if err := copyOutboundAuthorization(&e); err != nil {
			return e, nil, err
		}
		return normalizeOutbound(e)
	}
	if !validIngestEnvelope(e) {
		return e, nil, fmt.Errorf("receiptstore: invalid envelope")
	}
	byInbox, err := normalizeDestinations(e.Destinations, e.MailboxAppend)
	if err != nil {
		return e, nil, err
	}
	e.Destinations = sortedDestinations(byInbox)
	if e.Source != nil {
		source := *e.Source
		e.Source = &source
		if err := validateSource(source); err != nil {
			return e, nil, err
		}
	}
	b, err := json.Marshal(e)
	if err == nil && len(b) > 64*1024 {
		err = fmt.Errorf("receiptstore: envelope exceeds 64 KiB")
	}
	return e, b, err
}

func validateEnvelopeMode(e Envelope) error {
	if e.Migration != nil &&
		(!e.MailboxAppend || e.Migration.Job == (ID{}) || e.Migration.Entry == (ID{}) || e.Migration.Generation < 1) {
		return ErrConflict
	}
	if e.MailboxAppend {
		return validateMailboxAppend(e)
	}
	return nil
}

func validateMailboxAppend(e Envelope) error {
	if e.Outbound != nil || e.Source != nil || e.MailFrom != "" || len(e.Destinations) != 1 {
		return ErrConflict
	}
	if e.Destinations[0].Folder == (ID{}) || len(e.Destinations[0].Recipients) != 0 {
		return ErrConflict
	}
	return nil
}

func copyOutboundAuthorization(e *Envelope) error {
	outboundCopy := *e.Outbound
	e.Outbound = &outboundCopy
	if original := e.Outbound.SubmissionAuthorization; original != nil {
		if original.Username == "" || len(original.Username) > 128 || original.Version < 1 {
			return ErrConflict
		}
		proof := *original
		e.Outbound.SubmissionAuthorization = &proof
	}
	if original := e.Outbound.SenderAuthorization; original != nil {
		return copySenderAuthorization(e, original)
	}
	return nil
}

func copySenderAuthorization(e *Envelope, original *SenderAuthorization) error {
	if original.Grant == (ID{}) || original.Version < 1 || e.MailFrom == "" {
		return ErrConflict
	}
	proof := *original
	outbound := *e.Outbound
	outbound.SenderAuthorization = &proof
	e.Outbound = &outbound
	return nil
}

func normalizeDestinations(destinations []Destination, mailboxAppend bool) (map[ID]Destination, error) {
	byInbox := map[ID]Destination{}
	for _, d := range destinations {
		if d.Inbox == (ID{}) || (len(d.Recipients) == 0 && !mailboxAppend) {
			return nil, fmt.Errorf("receiptstore: invalid destination")
		}
		var err error
		d.Flags, err = canonicalFlags(d.Flags)
		if err != nil {
			return nil, err
		}
		if !d.InternalDate.IsZero() {
			d.InternalDate = d.InternalDate.UTC().Truncate(time.Microsecond)
		}
		prior, exists := byInbox[d.Inbox]
		if exists && !sameDestinationMetadata(prior, d) {
			return nil, ErrConflict
		}
		d.Recipients, err = mergeDestinationRecipients(prior.Recipients, d.Recipients)
		if err != nil {
			return nil, err
		}
		byInbox[d.Inbox] = d
	}
	return byInbox, nil
}

func sameDestinationMetadata(a, b Destination) bool {
	return a.Folder == b.Folder && a.InternalDate.Equal(b.InternalDate) && slices.Equal(a.Flags, b.Flags)
}

func mergeDestinationRecipients(prior, next []string) ([]string, error) {
	addresses := append([]string(nil), prior...)
	for _, address := range next {
		if address == "" {
			return nil, fmt.Errorf("receiptstore: empty recipient")
		}
		addresses = append(addresses, address)
	}
	return addresses, nil
}

func sortedDestinations(byInbox map[ID]Destination) []Destination {
	destinations := make([]Destination, 0, len(byInbox))
	for _, destination := range byInbox {
		addresses := destination.Recipients
		sort.Strings(addresses)
		unique := addresses[:0]
		for _, a := range addresses {
			if len(unique) == 0 || unique[len(unique)-1] != a {
				unique = append(unique, a)
			}
		}
		destination.Recipients = unique
		destinations = append(destinations, destination)
	}
	sort.Slice(
		destinations,
		func(i, j int) bool { return bytes.Compare(destinations[i].Inbox[:], destinations[j].Inbox[:]) < 0 },
	)
	return destinations
}

func validateSource(source Source) error {
	if source.Connection == (ID{}) || source.Folder == "" || source.UID == 0 || source.UIDValidity == 0 {
		return fmt.Errorf("receiptstore: invalid source identity")
	}
	return nil
}

func validIngestEnvelope(e Envelope) bool {
	return e.Organization != (ID{}) && len(e.Destinations) > 0 && len(e.Destinations) <= 1024
}
