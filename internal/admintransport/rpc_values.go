//go:build linux

package admintransport

import (
	"time"

	adminv1 "github.com/cotocisternas/fry/gen/admin/go/fry/admin/v1"
	"github.com/cotocisternas/fry/internal/admin"
	"github.com/cotocisternas/fry/internal/maildirectory"
	"github.com/cotocisternas/fry/internal/receiptstore"
)

func mapValues[A, B any](values []A, convert func(A) B) []B {
	result := make([]B, 0, len(values))
	for _, value := range values {
		result = append(result, convert(value))
	}
	return result
}

func timestamp(value time.Time) string { return value.UTC().Format(time.RFC3339Nano) }
func optionalTimestamp(value *time.Time) *string {
	if value == nil {
		return nil
	}
	result := timestamp(*value)
	return &result
}

func toGrant(value admin.Grant) *adminv1.Grant {
	return &adminv1.Grant{
		Scope:   value.Scope,
		Content: value.Content,
		Source:  value.Source,
	}
}

func toIdentity(value admin.Identity) *adminv1.Identity {
	return &adminv1.Identity{
		Id:      value.ID,
		Name:    value.Name,
		Grants:  mapValues(value.Grants, toGrant),
		Csrf:    value.CSRF,
		Expires: timestamp(value.Expires),
	}
}

func toOrganization(value admin.Organization) *adminv1.Organization {
	return &adminv1.Organization{
		Id:   value.ID,
		Name: value.Name,
	}
}

func toUser(value admin.User) *adminv1.User {
	return &adminv1.User{
		Id:      value.ID,
		Name:    value.Name,
		Method:  value.Method,
		Enabled: value.Enabled,
		Grants:  mapValues(value.Grants, toGrant),
	}
}

func toInbox(value admin.Inbox) *adminv1.Inbox {
	return &adminv1.Inbox{
		Id:           value.ID,
		Organization: value.Organization,
		Messages:     value.Messages,
	}
}

func toMessage(value admin.Message) *adminv1.Message {
	return &adminv1.Message{
		Id:       value.ID,
		Size:     value.Size,
		Received: timestamp(value.Received),
		State:    value.State,
	}
}

func toAudit(value admin.Audit) *adminv1.Audit {
	return &adminv1.Audit{
		Id:           value.ID,
		At:           timestamp(value.At),
		Actor:        value.Actor,
		Action:       value.Action,
		Organization: value.Organization,
		Target:       value.Target,
		Outcome:      value.Outcome,
	}
}

func toSenderChallenge(value admin.SenderChallenge) *adminv1.SenderChallenge {
	return &adminv1.SenderChallenge{
		Id:           value.ID,
		Organization: value.Organization,
		Scope:        value.Scope,
		DnsName:      value.DNSName,
		TxtValue:     value.TXTValue,
		ExpiresAt:    timestamp(value.ExpiresAt),
	}
}

func toSenderGrant(value admin.SenderGrant) *adminv1.SenderGrant {
	return &adminv1.SenderGrant{
		Id:           value.ID,
		Organization: value.Organization,
		Scope:        value.Scope,
		Version:      value.Version,
		Active:       value.Active,
		Evidence:     value.Evidence,
		Actor:        value.Actor,
		Reason:       value.Reason,
		VerifiedAt:   optionalTimestamp(value.VerifiedAt),
	}
}

func toStorageUsage(value admin.StorageUsage) *adminv1.StorageUsage {
	return &adminv1.StorageUsage{
		Organization:         value.Organization,
		RetainedOriginals:    value.RetainedOriginals,
		RetainedBytes:        value.RetainedBytes,
		LimitBytes:           value.LimitBytes,
		PolicyVersion:        value.PolicyVersion,
		MeasuredAt:           timestamp(value.MeasuredAt),
		SampledAt:            optionalTimestamp(value.SampledAt),
		SampledBytes:         value.SampledBytes,
		SampledOriginals:     value.SampledOriginals,
		SampledPolicyVersion: value.SampledPolicyVersion,
		AlertActive:          value.AlertActive,
		SampleStale:          value.SampleStale,
		LastTransitionAt:     optionalTimestamp(value.LastTransitionAt),
		LastTransitionState:  value.LastTransitionState,
	}
}

func toDomain(value maildirectory.Domain) *adminv1.Domain {
	return &adminv1.Domain{
		Name:         value.Name,
		Organization: value.Organization,
		CatchAll:     value.CatchAll,
		Enabled:      value.Enabled,
	}
}

func toAlias(value maildirectory.Alias) *adminv1.Alias {
	return &adminv1.Alias{
		Address:      value.Address,
		Organization: value.Organization,
		Inboxes:      value.Inboxes,
		Enabled:      value.Enabled,
	}
}

func toAccount(value maildirectory.Account) *adminv1.Account {
	return &adminv1.Account{
		Username:     value.Username,
		Organization: value.Organization,
		Inbox:        value.Inbox,
		Enabled:      value.Enabled,
	}
}

func toSubmissionAccount(value maildirectory.SubmissionAccount) *adminv1.SubmissionAccount {
	return &adminv1.SubmissionAccount{
		Username:          value.Username,
		Organization:      value.Organization,
		Enabled:           value.Enabled,
		Version:           value.Version,
		AuthorizedSenders: value.AuthorizedSenders,
		AuthorizedDomains: value.AuthorizedDomains,
	}
}

func toMemberStorageSnapshot(value receiptstore.MemberStorageSnapshot) *adminv1.MemberStorageSnapshot {
	return &adminv1.MemberStorageSnapshot{
		Member:        value.Member,
		SqlSnapshotAt: timestamp(value.SQLSnapshotAt),
		Profile:       toMemberStorageProfile(value.Profile),
		Current:       toMemberStorageCurrent(value.Current),
		Owner:         toMemberStorageOwner(value.Owner),
		Files:         mapValues(value.Files, toMemberStorageFile),
		NextFile:      value.NextFile,
		Runtime:       toMemberStorageRuntime(value.Runtime),
	}
}

func toMemberStorageProfile(value receiptstore.MemberStorageProfile) *adminv1.MemberStorageProfile {
	return &adminv1.MemberStorageProfile{
		Bounded:              value.Bounded,
		LimitsFrozen:         value.LimitsFrozen,
		MaxMessageBytes:      value.MaxMessageBytes,
		MaxConcurrentAppends: value.MaxConcurrentAppends,
		MaxBytes:             value.MaxBytes,
		MaxRecords:           value.MaxRecords,
		MaxFileBytes:         value.MaxFileBytes,
		StagingReservedBytes: value.StagingReservedBytes,
		LaneVersion:          value.LaneVersion,
		LaneCount:            value.LaneCount,
	}
}

func toMemberStorageCurrent(value receiptstore.MemberStorageCurrent) *adminv1.MemberStorageCurrent {
	return &adminv1.MemberStorageCurrent{
		FileId:                  value.FileID,
		ChargedBytes:            value.ChargedBytes,
		ReservedRecords:         value.ReservedRecords,
		ReservedRecordBytes:     value.ReservedRecordBytes,
		CanFitMaxRecordSnapshot: value.CanFitMaxRecordSnapshot,
		Lanes:                   mapValues(value.Lanes, toMemberStorageLane),
	}
}

func toMemberStorageLane(value receiptstore.MemberStorageLane) *adminv1.MemberStorageLane {
	return &adminv1.MemberStorageLane{
		Lane:            value.Lane,
		RecordCeiling:   value.RecordCeiling,
		ByteCeiling:     value.ByteCeiling,
		ReservedRecords: value.ReservedRecords,
		ReservedBytes:   value.ReservedBytes,
	}
}

func toMemberStorageOwner(value receiptstore.MemberStorageOwner) *adminv1.MemberStorageOwner {
	return &adminv1.MemberStorageOwner{
		Epoch:  value.Epoch,
		Closed: value.Closed,
	}
}

func toMemberStorageFile(value receiptstore.MemberStorageFile) *adminv1.MemberStorageFile {
	return &adminv1.MemberStorageFile{
		FileId:        value.FileID,
		State:         value.State,
		ChargedBytes:  value.ChargedBytes,
		SealedRecords: value.SealedRecords,
		RewriteJob:    value.RewriteJob,
	}
}

func toMemberStorageRuntime(value receiptstore.MemberStorageRuntime) *adminv1.MemberStorageRuntime {
	return &adminv1.MemberStorageRuntime{
		Status:           value.Status,
		CheckedAt:        timestamp(value.CheckedAt),
		SampledAt:        optionalTimestamp(value.SampledAt),
		Member:           value.Member,
		FailureDomain:    value.FailureDomain,
		BoundOwnerEpoch:  value.BoundOwnerEpoch,
		OwnerEpoch:       value.OwnerEpoch,
		CurrentFile:      value.CurrentFile,
		Closed:           value.Closed,
		Frozen:           value.Frozen,
		ClosureUncertain: value.ClosureUncertain,
	}
}

func fromOrganization(value *adminv1.PutOrganizationRequest) admin.Organization {
	return admin.Organization{
		ID:   value.GetId(),
		Name: value.GetName(),
	}
}

func fromInbox(value *adminv1.CreateInboxRequest) admin.Inbox {
	return admin.Inbox{
		ID:           value.GetId(),
		Organization: value.GetOrganization(),
	}
}

func fromDomain(value *adminv1.PutDomainRequest) maildirectory.Domain {
	return maildirectory.Domain{
		Name:         value.GetName(),
		Organization: value.GetOrganization(),
		CatchAll:     value.GetCatchAll(),
		Enabled:      value.GetEnabled(),
	}
}

func fromAlias(value *adminv1.PutAliasRequest) maildirectory.Alias {
	return maildirectory.Alias{
		Address:      value.GetAddress(),
		Organization: value.GetOrganization(),
		Inboxes:      value.GetInboxes(),
		Enabled:      value.GetEnabled(),
	}
}

func fromAccount(value *adminv1.PutMailAccountRequest) maildirectory.Account {
	return maildirectory.Account{
		Username:     value.GetUsername(),
		Organization: value.GetOrganization(),
		Inbox:        value.GetInbox(),
		Enabled:      value.GetEnabled(),
	}
}

func fromGrant(value *adminv1.Grant) admin.Grant {
	return admin.Grant{
		Scope:   value.GetScope(),
		Content: value.GetContent(),
		Source:  value.GetSource(),
	}
}
