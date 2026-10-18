//go:build linux

package admintransport

// These unversioned adapters preserve the prototype test/automation surface.
// The browser and CLI use the versioned, typed Connect service.
import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/cotocisternas/fry/internal/admin"
	"github.com/cotocisternas/fry/internal/maildirectory"
)

type legacyRoute func(http.ResponseWriter, *http.Request) (any, error)

func (h *HTTP) legacyRoutes() map[string]legacyRoute {
	return map[string]legacyRoute{
		"POST /api/login":               h.legacyPostLogin,
		"GET /api/session":              h.legacyGetSession,
		"POST /api/logout":              h.legacyPostLogout,
		"GET /api/organizations":        h.legacyGetOrganizations,
		"POST /api/organizations":       h.legacyPostOrganizations,
		"GET /api/inboxes":              h.legacyGetInboxes,
		"POST /api/inboxes":             h.legacyPostInboxes,
		"GET /api/sender-grants":        h.legacyGetSenderGrants,
		"POST /api/sender-challenge":    h.legacyPostSenderChallenge,
		"POST /api/sender-verify":       h.legacyPostSenderVerify,
		"POST /api/sender-revoke":       h.legacyPostSenderRevoke,
		"POST /api/sender-override":     h.legacyPostSenderRevoke,
		"GET /api/domains":              h.legacyGetDomains,
		"POST /api/domains":             h.legacyPostDomains,
		"GET /api/aliases":              h.legacyGetAliases,
		"POST /api/aliases":             h.legacyPostAliases,
		"GET /api/mail-accounts":        h.legacyGetMailAccounts,
		"POST /api/mail-accounts":       h.legacyPostMailAccounts,
		"POST /api/mail-access":         h.legacyPostMailAccess,
		"GET /api/member-storage":       h.legacyGetMemberStorage,
		"GET /api/storage-usage":        h.legacyGetStorageUsage,
		"POST /api/storage-quota":       h.legacyPostStorageQuota,
		"GET /api/submission-accounts":  h.legacyGetSubmissionAccounts,
		"POST /api/submission-accounts": h.legacyPostSubmissionAccounts,
		"POST /api/submission-policy":   h.legacyPostSubmissionPolicy,
		"POST /api/submission-password": h.legacyPostSubmissionPassword,
		"POST /api/submission-access":   h.legacyPostSubmissionAccess,
		"GET /api/messages":             h.legacyGetMessages,
		"GET /api/audit":                h.legacyGetAudit,
		"GET /api/users":                h.legacyGetUsers,
		"POST /api/users":               h.legacyPostUsers,
		"POST /api/grants":              h.legacyPostGrants,
		"POST /api/access":              h.legacyPostAccess,
	}
}

func (h *HTTP) serveLegacy(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && r.URL.Path == "/api/content" {
		h.serveContent(w, r)
		return
	}
	route, ok := h.legacyRoutes()[r.Method+" "+r.URL.Path]
	if !ok {
		http.NotFound(w, r)
		return
	}
	value, err := route(w, r)
	if err != nil {
		failure(w, err)
		return
	}
	reply(w, http.StatusOK, value)
}

func (h *HTTP) legacyPostLogin(w http.ResponseWriter, r *http.Request) (value any, err error) {
	ctx := r.Context()
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err = decode(w, r, &input); err == nil {
		var session admin.Session
		session, err = h.service.Login(ctx, input.Username, input.Password)
		if err == nil {
			h.setSession(w, session)
			value = session.Identity
		}
	}
	return value, err
}

func (h *HTTP) legacyGetSession(w http.ResponseWriter, r *http.Request) (value any, err error) {
	ctx := r.Context()
	token := h.token(r)
	value, err = h.service.Identity(ctx, token)
	return value, err
}

func (h *HTTP) legacyPostLogout(w http.ResponseWriter, r *http.Request) (value any, err error) {
	ctx := r.Context()
	token := h.token(r)
	err = h.service.Logout(ctx, token)
	if err == nil {
		// Owner: admin maintainers; review by 2027-01-18.
		// #nosec G124 -- Secure follows NewHTTP's validated origin: HTTP is allowed only on literal loopback IPs.
		http.SetCookie(
			w,
			&http.Cookie{
				Name:     h.cookieName(),
				Path:     "/",
				MaxAge:   -1,
				Secure:   h.origin.Scheme == schemeHTTPS,
				HttpOnly: true,
				SameSite: http.SameSiteLaxMode,
			},
		)
		value = map[string]bool{"ok": true}
	}
	return value, err
}

func (h *HTTP) legacyGetOrganizations(w http.ResponseWriter, r *http.Request) (value any, err error) {
	ctx := r.Context()
	token := h.token(r)
	value, err = h.service.Organizations(ctx, token)
	return value, err
}

func (h *HTTP) legacyPostOrganizations(w http.ResponseWriter, r *http.Request) (value any, err error) {
	ctx := r.Context()
	token := h.token(r)
	var input admin.Organization
	if err = decode(w, r, &input); err == nil {
		if input.ID == "" {
			input.ID, err = identifier()
		}
		if err == nil {
			err = h.service.PutOrganization(ctx, token, input)
			value = input
		}
	}
	return value, err
}

func (h *HTTP) legacyGetInboxes(w http.ResponseWriter, r *http.Request) (value any, err error) {
	ctx := r.Context()
	token := h.token(r)
	value, err = h.service.Inboxes(ctx, token, r.URL.Query().Get("organization"))
	return value, err
}

func (h *HTTP) legacyPostInboxes(w http.ResponseWriter, r *http.Request) (value any, err error) {
	ctx := r.Context()
	token := h.token(r)
	var input admin.Inbox
	if err = decode(w, r, &input); err == nil {
		if input.ID == "" {
			input.ID, err = identifier()
		}
		if err == nil {
			err = h.service.CreateInbox(ctx, token, input)
			value = input
		}
	}

	return value, err
}

func (h *HTTP) legacyGetSenderGrants(w http.ResponseWriter, r *http.Request) (value any, err error) {
	ctx := r.Context()
	token := h.token(r)
	value, err = h.service.SenderGrants(ctx, token, r.URL.Query().Get("organization"))
	return value, err
}

func (h *HTTP) legacyPostSenderChallenge(w http.ResponseWriter, r *http.Request) (value any, err error) {
	ctx := r.Context()
	token := h.token(r)
	var input struct {
		Organization string `json:"organization"`
		Scope        string `json:"scope"`
	}
	if err = decode(w, r, &input); err == nil {
		value, err = h.service.SenderChallenge(ctx, token, input.Organization, input.Scope)
	}
	return value, err
}

func (h *HTTP) legacyPostSenderVerify(w http.ResponseWriter, r *http.Request) (value any, err error) {
	ctx := r.Context()
	token := h.token(r)
	var input struct {
		Organization string `json:"organization"`
		Challenge    string `json:"challenge"`
	}
	if err = decode(w, r, &input); err == nil {
		value, err = h.service.VerifySender(ctx, token, input.Organization, input.Challenge)
	}
	return value, err
}

func (h *HTTP) legacyPostSenderRevoke(w http.ResponseWriter, r *http.Request) (value any, err error) {
	ctx := r.Context()
	token := h.token(r)
	var input struct {
		Organization string `json:"organization"`
		Scope        string `json:"scope"`
		Reason       string `json:"reason"`
	}
	if err = decode(w, r, &input); err == nil {
		if r.URL.Path == "/api/sender-override" {
			value, err = h.service.OverrideSender(ctx, token, input.Organization, input.Scope, input.Reason)
		} else {
			err = h.service.RevokeSender(ctx, token, input.Organization, input.Scope, input.Reason)
			value = map[string]bool{"ok": err == nil}
		}
	}
	return value, err
}

func (h *HTTP) legacyGetDomains(w http.ResponseWriter, r *http.Request) (value any, err error) {
	ctx := r.Context()
	token := h.token(r)
	value, err = h.service.Domains(ctx, token, r.URL.Query().Get("organization"))
	return value, err
}

func (h *HTTP) legacyPostDomains(w http.ResponseWriter, r *http.Request) (value any, err error) {
	ctx := r.Context()
	token := h.token(r)
	var input maildirectory.Domain
	if err = decode(w, r, &input); err == nil {
		err = h.service.PutDomain(ctx, token, input)
		value = input
	}
	return value, err
}

func (h *HTTP) legacyGetAliases(w http.ResponseWriter, r *http.Request) (value any, err error) {
	ctx := r.Context()
	token := h.token(r)
	value, err = h.service.Aliases(ctx, token, r.URL.Query().Get("organization"))
	return value, err
}

func (h *HTTP) legacyPostAliases(w http.ResponseWriter, r *http.Request) (value any, err error) {
	ctx := r.Context()
	token := h.token(r)
	var input maildirectory.Alias
	if err = decode(w, r, &input); err == nil {
		err = h.service.PutAlias(ctx, token, input)
		value = input
	}
	return value, err
}

func (h *HTTP) legacyGetMailAccounts(w http.ResponseWriter, r *http.Request) (value any, err error) {
	ctx := r.Context()
	token := h.token(r)
	value, err = h.service.MailAccounts(ctx, token, r.URL.Query().Get("organization"))
	return value, err
}

func (h *HTTP) legacyPostMailAccounts(w http.ResponseWriter, r *http.Request) (value any, err error) {
	ctx := r.Context()
	token := h.token(r)
	var input struct {
		maildirectory.Account
		Password string `json:"password"`
	}
	if err = decode(w, r, &input); err == nil {
		err = h.service.PutMailAccount(ctx, token, input.Account, input.Password)
		value = input.Account
	}
	return value, err
}

func (h *HTTP) legacyPostMailAccess(w http.ResponseWriter, r *http.Request) (value any, err error) {
	ctx := r.Context()
	token := h.token(r)
	var input struct {
		Organization string `json:"organization"`
		Username     string `json:"username"`
		Enabled      bool   `json:"enabled"`
	}
	if err = decode(w, r, &input); err == nil {
		err = h.service.SetMailAccess(ctx, token, input.Organization, input.Username, input.Enabled)
		value = map[string]bool{"ok": true}
	}

	return value, err
}

func (h *HTTP) legacyGetMemberStorage(w http.ResponseWriter, r *http.Request) (value any, err error) {
	ctx := r.Context()
	token := h.token(r)
	var limit int
	if limit, err = memberStorageLimit(r.URL.Query().Get("limit")); err == nil {
		value, err = h.service.MemberStorage(ctx, token, r.URL.Query().Get("member"), r.URL.Query().Get("after"), limit)
	}
	return value, err
}

func (h *HTTP) legacyGetStorageUsage(w http.ResponseWriter, r *http.Request) (value any, err error) {
	ctx := r.Context()
	token := h.token(r)
	value, err = h.service.StorageUsage(ctx, token, r.URL.Query().Get("organization"))
	return value, err
}

func (h *HTTP) legacyPostStorageQuota(w http.ResponseWriter, r *http.Request) (value any, err error) {
	ctx := r.Context()
	token := h.token(r)
	var input struct {
		Organization string          `json:"organization"`
		LimitBytes   json.RawMessage `json:"limit_bytes"`
	}
	if err = decode(w, r, &input); err == nil {
		var limit *string
		if len(input.LimitBytes) == 0 {
			err = admin.ErrInvalid
		} else if err = json.Unmarshal(input.LimitBytes, &limit); err == nil {
			err = h.service.SetStorageQuota(ctx, token, input.Organization, limit)
		}
		if err != nil {
			if _, ok := errors.AsType[*json.UnmarshalTypeError](err); ok {
				err = admin.ErrInvalid
			}
		}
		value = map[string]bool{"ok": err == nil}
	}
	return value, err
}

func (h *HTTP) legacyGetSubmissionAccounts(w http.ResponseWriter, r *http.Request) (value any, err error) {
	ctx := r.Context()
	token := h.token(r)
	value, err = h.service.SubmissionAccounts(ctx, token, r.URL.Query().Get("organization"))
	return value, err
}

func (h *HTTP) legacyPostSubmissionAccounts(w http.ResponseWriter, r *http.Request) (value any, err error) {
	ctx := r.Context()
	token := h.token(r)
	var input struct {
		Organization      string   `json:"organization"`
		Username          string   `json:"username"`
		Password          string   `json:"password"`
		AuthorizedSenders []string `json:"authorized_senders"`
		AuthorizedDomains []string `json:"authorized_domains"`
		Enabled           bool     `json:"enabled"`
	}
	if err = decode(w, r, &input); err == nil {
		err = h.service.CreateSubmissionAccount(
			ctx,
			token,
			maildirectory.SubmissionAccount{
				Organization:      input.Organization,
				Username:          input.Username,
				Enabled:           input.Enabled,
				AuthorizedSenders: input.AuthorizedSenders,
				AuthorizedDomains: input.AuthorizedDomains,
			},
			input.Password,
		)
		value = map[string]bool{"ok": err == nil}
	}
	return value, err
}

func (h *HTTP) legacyPostSubmissionPolicy(w http.ResponseWriter, r *http.Request) (value any, err error) {
	ctx := r.Context()
	token := h.token(r)
	var input struct {
		Organization      string   `json:"organization"`
		Username          string   `json:"username"`
		AuthorizedSenders []string `json:"authorized_senders"`
		AuthorizedDomains []string `json:"authorized_domains"`
	}
	if err = decode(w, r, &input); err == nil {
		err = h.service.SetSubmissionPolicy(
			ctx,
			token,
			input.Organization,
			input.Username,
			input.AuthorizedSenders,
			input.AuthorizedDomains,
		)
		value = map[string]bool{"ok": err == nil}
	}
	return value, err
}

// Password rotation accepts a secret and never enables an account. Its JSON
// schema rejects enabled; sharing a decoder would weaken unknown-field checks.
//
//nolint:dupl // Keep this decoder paired with its typed credential operation.
func (h *HTTP) legacyPostSubmissionPassword(w http.ResponseWriter, r *http.Request) (value any, err error) {
	ctx := r.Context()
	token := h.token(r)
	var input struct {
		Organization string `json:"organization"`
		Username     string `json:"username"`
		Password     string `json:"password"`
	}
	if err = decode(w, r, &input); err == nil {
		err = h.service.RotateSubmissionPassword(ctx, token, input.Organization, input.Username, input.Password)
		value = map[string]bool{"ok": err == nil}
	}
	return value, err
}

// Access changes accept enabled and reject password fields. This operation has
// its own audit action and must not implicitly rotate credentials.
//
//nolint:dupl // Keep the boolean access adapter distinct from secret-bearing rotation.
func (h *HTTP) legacyPostSubmissionAccess(w http.ResponseWriter, r *http.Request) (value any, err error) {
	ctx := r.Context()
	token := h.token(r)
	var input struct {
		Organization string `json:"organization"`
		Username     string `json:"username"`
		Enabled      bool   `json:"enabled"`
	}
	if err = decode(w, r, &input); err == nil {
		err = h.service.SetSubmissionAccess(ctx, token, input.Organization, input.Username, input.Enabled)
		value = map[string]bool{"ok": err == nil}
	}
	return value, err
}

func (h *HTTP) legacyGetMessages(w http.ResponseWriter, r *http.Request) (value any, err error) {
	ctx := r.Context()
	token := h.token(r)
	value, err = h.service.Messages(ctx, token, r.URL.Query().Get("organization"), r.URL.Query().Get("after"))
	return value, err
}

func (h *HTTP) legacyGetAudit(w http.ResponseWriter, r *http.Request) (value any, err error) {
	ctx := r.Context()
	token := h.token(r)
	before := int64(0)
	if raw := r.URL.Query().Get("before"); raw != "" {
		before, err = strconv.ParseInt(raw, 10, 64)
		if err != nil {
			err = admin.ErrInvalid
		}
	}
	if err == nil {
		value, err = h.service.Audits(ctx, token, before)
	}
	return value, err
}

func (h *HTTP) legacyGetUsers(w http.ResponseWriter, r *http.Request) (value any, err error) {
	ctx := r.Context()
	token := h.token(r)
	value, err = h.service.Users(ctx, token)
	return value, err
}

func (h *HTTP) legacyPostUsers(w http.ResponseWriter, r *http.Request) (value any, err error) {
	ctx := r.Context()
	token := h.token(r)
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Enabled  bool   `json:"enabled"`
	}
	if err = decode(w, r, &input); err == nil {
		var id string
		id, err = h.service.PutUser(ctx, token, input.Username, input.Password, input.Enabled)
		value = map[string]string{"id": id}
	}
	return value, err
}

func (h *HTTP) legacyPostGrants(w http.ResponseWriter, r *http.Request) (value any, err error) {
	ctx := r.Context()
	token := h.token(r)
	var input struct {
		User   string        `json:"user"`
		Grants []admin.Grant `json:"grants"`
	}
	if err = decode(w, r, &input); err == nil {
		err = h.service.ReplaceGrants(ctx, token, input.User, input.Grants)
		value = map[string]bool{"ok": true}
	}
	return value, err
}

func (h *HTTP) legacyPostAccess(w http.ResponseWriter, r *http.Request) (value any, err error) {
	ctx := r.Context()
	token := h.token(r)
	var input struct {
		User    string `json:"user"`
		Enabled bool   `json:"enabled"`
	}
	if err = decode(w, r, &input); err == nil {
		err = h.service.SetEnabled(ctx, token, input.User, input.Enabled)
		value = map[string]bool{"ok": true}
	}
	return value, err
}
