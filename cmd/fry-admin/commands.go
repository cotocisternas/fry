package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// commandPlan holds CLI intent. Method and path are local selectors for the
// typed RPC client, not requests to the legacy HTTP administration endpoints.
type commandPlan struct {
	method, path string
	data         any
}

func (o commandOptions) plan() (commandPlan, error) {
	actions := map[string]func() (commandPlan, error){
		"organizations":             o.planOrganizations,
		"organization-save":         o.planOrganizationSave,
		"inboxes":                   o.planInboxes,
		"inbox-create":              o.planInboxCreate,
		"domains":                   o.planDomains,
		"domain-save":               o.planDomainSave,
		"aliases":                   o.planAliases,
		"alias-save":                o.planAliasSave,
		"mail-accounts":             o.planMailAccounts,
		"mail-account-save":         o.planMailAccountSave,
		"mail-access-set":           o.planMailAccessSet,
		"sender-grants":             o.planSenderGrants,
		"member-storage":            o.planMemberStorage,
		"storage-usage":             o.planStorageUsage,
		"storage-quota-set":         o.planStorageQuotaSet,
		"submission-accounts":       o.planSubmissionAccounts,
		"submission-account-create": o.planSubmissionAccountCreate,
		"submission-policy-set":     o.planSubmissionAccountCreate,
		"submission-password-set":   o.planSubmissionPasswordSet,
		"submission-access-set":     o.planSubmissionAccessSet,
		"sender-challenge":          o.planSenderChallenge,
		"sender-verify":             o.planSenderVerify,
		"sender-revoke":             o.planSenderRevoke,
		"sender-override":           o.planSenderRevoke,
		"messages":                  o.planMessages,
		"audit":                     o.planAudit,
		"users":                     o.planUsers,
		"user-save":                 o.planUserSave,
		"access-set":                o.planAccessSet,
		"grants-set":                o.planGrantsSet,
	}
	action, ok := actions[o.command]
	if !ok {
		return commandPlan{}, fmt.Errorf("unknown command %q", o.command)
	}
	return action()
}

func (o commandOptions) planOrganizations() (commandPlan, error) {
	return commandPlan{method: http.MethodGet, path: "organizations"}, nil
}

func (o commandOptions) planOrganizationSave() (commandPlan, error) {
	return commandPlan{
		method: http.MethodPost,
		path:   "organizations",
		data:   map[string]string{"id": o.id, "name": o.name},
	}, nil
}

func (o commandOptions) planInboxes() (commandPlan, error) {
	return commandPlan{method: http.MethodGet, path: "inboxes?organization=" + url.QueryEscape(o.organization)}, nil
}

func (o commandOptions) planInboxCreate() (commandPlan, error) {
	return commandPlan{
		method: http.MethodPost,
		path:   "inboxes",
		data:   map[string]string{"id": o.id, "organization": o.organization},
	}, nil
}

func (o commandOptions) planDomains() (commandPlan, error) {
	return commandPlan{method: http.MethodGet, path: "domains?organization=" + url.QueryEscape(o.organization)}, nil
}

func (o commandOptions) planDomainSave() (commandPlan, error) {
	return commandPlan{
		method: http.MethodPost,
		path:   "domains",
		data: map[string]any{
			"name":         o.domain,
			"organization": o.organization,
			"catch_all":    o.inbox,
			"enabled":      o.enabled,
		},
	}, nil
}

func (o commandOptions) planAliases() (commandPlan, error) {
	return commandPlan{method: http.MethodGet, path: "aliases?organization=" + url.QueryEscape(o.organization)}, nil
}

func (o commandOptions) planAliasSave() (commandPlan, error) {
	return commandPlan{
		method: http.MethodPost,
		path:   "aliases",
		data: map[string]any{
			"address":      o.address,
			"organization": o.organization,
			"inboxes":      strings.Split(o.inboxes, ","),
			"enabled":      o.enabled,
		},
	}, nil
}

func (o commandOptions) planMailAccounts() (commandPlan, error) {
	return commandPlan{
		method: http.MethodGet,
		path:   "mail-accounts?organization=" + url.QueryEscape(o.organization),
	}, nil
}

func (o commandOptions) planMailAccountSave() (commandPlan, error) {
	password := os.Getenv("FRY_NEW_MAIL_PASSWORD")
	if password == "" {
		return commandPlan{}, fmt.Errorf("FRY_NEW_MAIL_PASSWORD is required")
	}
	return commandPlan{
		method: http.MethodPost,
		path:   "mail-accounts",
		data: map[string]any{
			"username":     o.name,
			"organization": o.organization,
			"inbox":        o.inbox,
			"enabled":      o.enabled,
			"password":     password,
		},
	}, nil
}

func (o commandOptions) planMailAccessSet() (commandPlan, error) {
	return commandPlan{
		method: http.MethodPost,
		path:   "mail-access",
		data: map[string]any{
			"username":     o.name,
			"organization": o.organization,
			"enabled":      o.enabled,
		},
	}, nil
}

func (o commandOptions) planSenderGrants() (commandPlan, error) {
	return commandPlan{
		method: http.MethodGet,
		path:   "sender-grants?organization=" + url.QueryEscape(o.organization),
	}, nil
}

func (o commandOptions) planMemberStorage() (commandPlan, error) {
	path, err := memberStoragePath(o.member, o.after, o.limit)
	if err != nil {
		return commandPlan{}, err
	}
	return commandPlan{method: http.MethodGet, path: path}, nil
}

func (o commandOptions) planStorageUsage() (commandPlan, error) {
	return commandPlan{
		method: http.MethodGet,
		path:   "storage-usage?organization=" + url.QueryEscape(o.organization),
	}, nil
}

func (o commandOptions) planStorageQuotaSet() (commandPlan, error) {
	payload, e := storageQuotaPayload(o.organization, o.limitBytes, o.disableLimit)
	if e != nil {
		return commandPlan{}, e
	}
	return commandPlan{method: http.MethodPost, path: "storage-quota", data: payload}, nil
}

func (o commandOptions) planSubmissionAccounts() (commandPlan, error) {
	return commandPlan{
		method: http.MethodGet,
		path:   "submission-accounts?organization=" + url.QueryEscape(o.organization),
	}, nil
}

func (o commandOptions) planSubmissionAccountCreate() (commandPlan, error) {
	path, payload, err := submissionProvisioning(
		o.command,
		o.organization,
		o.name,
		o.authorizedSenders,
		o.authorizedDomains,
		o.enabled,
	)
	if err != nil {
		return commandPlan{}, err
	}
	return commandPlan{method: http.MethodPost, path: path, data: payload}, nil
}

func (o commandOptions) planSubmissionPasswordSet() (commandPlan, error) {
	password := os.Getenv("FRY_NEW_SUBMISSION_PASSWORD")
	if password == "" {
		return commandPlan{}, fmt.Errorf("FRY_NEW_SUBMISSION_PASSWORD is required")
	}
	return commandPlan{
		method: http.MethodPost,
		path:   "submission-password",
		data: map[string]string{
			"username":     o.name,
			"organization": o.organization,
			"password":     password,
		},
	}, nil
}

func (o commandOptions) planSubmissionAccessSet() (commandPlan, error) {
	return commandPlan{
		method: http.MethodPost,
		path:   "submission-access",
		data: map[string]any{
			"username":     o.name,
			"organization": o.organization,
			"enabled":      o.enabled,
		},
	}, nil
}

func (o commandOptions) planSenderChallenge() (commandPlan, error) {
	return commandPlan{
		method: http.MethodPost,
		path:   "sender-challenge",
		data: map[string]string{
			"organization": o.organization,
			"scope":        o.senderScope,
		},
	}, nil
}

func (o commandOptions) planSenderVerify() (commandPlan, error) {
	return commandPlan{
		method: http.MethodPost,
		path:   "sender-verify",
		data: map[string]string{
			"organization": o.organization,
			"challenge":    o.challenge,
		},
	}, nil
}

func (o commandOptions) planSenderRevoke() (commandPlan, error) {
	return commandPlan{
		method: http.MethodPost,
		path:   o.command,
		data: map[string]string{
			"organization": o.organization,
			"scope":        o.senderScope,
			"reason":       o.reason,
		},
	}, nil
}

func (o commandOptions) planMessages() (commandPlan, error) {
	return commandPlan{
		method: http.MethodGet,
		path:   "messages?organization=" + url.QueryEscape(o.organization) + "&after=" + url.QueryEscape(o.after),
	}, nil
}

func (o commandOptions) planAudit() (commandPlan, error) {
	return commandPlan{method: http.MethodGet, path: fmt.Sprintf("audit?before=%d", o.before)}, nil
}

func (o commandOptions) planUsers() (commandPlan, error) {
	return commandPlan{method: http.MethodGet, path: "users"}, nil
}

func (o commandOptions) planUserSave() (commandPlan, error) {
	password := os.Getenv("FRY_NEW_ADMIN_PASSWORD")
	if password == "" {
		return commandPlan{}, fmt.Errorf("FRY_NEW_ADMIN_PASSWORD is required")
	}
	return commandPlan{
		method: http.MethodPost,
		path:   "users",
		data: map[string]any{
			"username": o.name,
			"password": password,
			"enabled":  o.enabled,
		},
	}, nil
}

func (o commandOptions) planAccessSet() (commandPlan, error) {
	return commandPlan{
		method: http.MethodPost,
		path:   "access",
		data:   map[string]any{"user": o.id, "enabled": o.enabled},
	}, nil
}

func (o commandOptions) planGrantsSet() (commandPlan, error) {
	file, e := os.Open(o.grantsPath)
	if e != nil {
		return commandPlan{}, e
	}
	defer func() {
		if file != nil {
			warnClose(file, "grants input")
		}
	}()
	var grants []struct {
		Scope   string `json:"scope"`
		Content bool   `json:"content"`
	}
	decoder := json.NewDecoder(io.LimitReader(file, 65537))
	decoder.DisallowUnknownFields()
	if e = decoder.Decode(&grants); e != nil {
		return commandPlan{}, e
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return commandPlan{}, fmt.Errorf("grants file must contain one JSON array")
	}
	if e = file.Close(); e != nil {
		return commandPlan{}, fmt.Errorf("could not close grants input")
	}
	file = nil
	return commandPlan{
		method: http.MethodPost,
		path:   "grants",
		data:   map[string]any{"user": o.id, "grants": grants},
	}, nil
}

func (o commandOptions) download(ctx context.Context, c *client) error {
	if o.output == "" {
		return fmt.Errorf("-out is required for a message download")
	}
	file, e := os.OpenFile(o.output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if e != nil {
		return e
	}
	complete := false
	defer func() {
		if file != nil {
			warnClose(file, "message output")
		}
		if !complete {
			// Remove only the output this invocation created with O_EXCL, chosen by the CLI operator.
			// #nosec G703 -- No email-derived paths; owner: admin maintainers; review by 2027-01-18.
			if err := os.Remove(o.output); err != nil {
				log.Print("could not remove incomplete message output")
			}
		}
	}()
	response, e := c.request(
		ctx,
		http.MethodGet,
		"content?organization="+url.QueryEscape(o.organization)+"&receipt="+url.QueryEscape(o.id),
		nil,
	)
	if e != nil {
		return e
	}
	defer warnClose(response.Body, "administration response")
	if _, e = io.Copy(file, response.Body); e != nil {
		return e
	}
	if e = file.Sync(); e != nil {
		return e
	}
	if e = file.Close(); e != nil {
		return e
	}
	file = nil
	complete = true
	return nil
}
