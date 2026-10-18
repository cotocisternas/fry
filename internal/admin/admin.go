//go:build linux

// Package admin owns administrator identities, current permissions, and audits.
// Mail protocol accounts occupy a separate authentication namespace.
package admin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/cotocisternas/fry/db"
	"github.com/cotocisternas/fry/internal/admin/dbgen"
	"github.com/cotocisternas/fry/internal/credentials"
	"github.com/cotocisternas/fry/internal/dbschema"
	"github.com/cotocisternas/fry/internal/hotstore"
	"github.com/cotocisternas/fry/internal/maildirectory"
	"github.com/cotocisternas/fry/internal/receiptstore"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SchemaDefinition returns only this module's SQL for atomic installation.
func SchemaDefinition() dbschema.Component {
	return dbschema.Component{Name: "admin", SQL: db.AdminSchema()}
}

var (
	ErrUnauthorized = errors.New("admin: authentication required")
	ErrForbidden    = errors.New("admin: permission denied")
	ErrInvalid      = errors.New("admin: invalid request")
	ErrConflict     = errors.New("admin: conflicting state")
)

type Service struct {
	senderDNS TXTResolver
	db        *pgxpool.Pool
	mail      *receiptstore.Store
	lifetime  time.Duration
	passwords chan struct{}
}
type Grant struct {
	Scope   string `json:"scope"`
	Content bool   `json:"content"`
	Source  string `json:"source,omitempty"`
}
type Identity struct {
	ID      string    `json:"id"`
	Name    string    `json:"name"`
	Grants  []Grant   `json:"grants"`
	CSRF    string    `json:"csrf"`
	Expires time.Time `json:"expires"`
}
type Session struct {
	Token    string
	Identity Identity
}

func Migrate(ctx context.Context, db *pgxpool.Pool) error {
	tx, err := db.Begin(ctx)
	if err != nil {
		return err
	}
	defer rollback(ctx, tx)
	if _, err = tx.Exec(ctx, SchemaDefinition().SQL); err != nil {
		return err
	}
	if err = maildirectory.Migrate(ctx, tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Options supplies dependencies that vary between installations.
type Options struct{ SenderDNS TXTResolver }

func New(db *pgxpool.Pool, mail *receiptstore.Store, lifetime time.Duration) (*Service, error) {
	return NewWithOptions(db, mail, lifetime, Options{})
}

// NewWithOptions constructs administration with explicit external dependencies.
func NewWithOptions(
	db *pgxpool.Pool,
	mail *receiptstore.Store,
	lifetime time.Duration,
	opts Options,
) (*Service, error) {
	if db == nil || mail == nil || lifetime < time.Minute || lifetime > 24*time.Hour {
		return nil, ErrInvalid
	}
	return &Service{
		senderDNS: opts.SenderDNS,
		db:        db,
		mail:      mail,
		lifetime:  lifetime,
		passwords: make(chan struct{}, 4),
	}, nil
}

func rollback(ctx context.Context, tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		// Database error details can contain query values; keep them out of logs.
		slog.ErrorContext(ctx, "admin transaction rollback failed")
	}
}
func identifier() (string, error) { id, err := hotstore.NewID(); return id.String(), err }
func opaque() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
func digest(token string) []byte { sum := sha256.Sum256([]byte(token)); return sum[:] }
func validID(v string) bool {
	b, err := hex.DecodeString(v)
	return err == nil && len(b) == 16 && v == strings.ToLower(v) && v != strings.Repeat("0", 32)
}

func mailID(v string) (receiptstore.ID, error) {
	var id receiptstore.ID
	if !validID(v) {
		return id, ErrInvalid
	}
	b, err := hex.DecodeString(v)
	if err != nil {
		return id, ErrInvalid
	}
	copy(id[:], b)
	return id, nil
}

func validName(v string) bool {
	return strings.TrimSpace(v) == v && len(v) > 0 && len(v) <= 128 && !strings.ContainsAny(v, "\x00\r\n")
}

func (s *Service) begin(ctx context.Context, exclusive bool) (pgx.Tx, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	if _, err = dbgen.New(tx).RequireSynchronousCommit(ctx); err == nil {
		if exclusive {
			err = dbgen.New(tx).LockPolicyExclusive(ctx)
		} else {
			err = dbgen.New(tx).LockPolicyShared(ctx)
		}
	}
	if err != nil {
		rollback(ctx, tx)
		return nil, err
	}
	return tx, nil
}

func audit(ctx context.Context, tx pgx.Tx, actor, action, org, target string) error {
	_, err := dbgen.New(tx).
		CreateAudit(ctx, dbgen.CreateAuditParams{Actor: actor, Action: action, Organization: org, Target: target})
	return err
}

func (s *Service) passwordSlot(ctx context.Context) (func(), error) {
	select {
	case s.passwords <- struct{}{}:
		return func() { <-s.passwords }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Bootstrap never resets an existing installation from an environment variable.
// The policy guard serializes concurrent first starts. Returns whether created.
func (s *Service) Bootstrap(ctx context.Context, username, password string, content bool) (bool, error) {
	if !validName(username) {
		return false, ErrInvalid
	}
	release, err := s.passwordSlot(ctx)
	if err != nil {
		return false, err
	}
	defer release()
	hash, err := credentials.HashPassword(password)
	if err != nil {
		return false, ErrInvalid
	}
	tx, err := s.begin(ctx, true)
	if err != nil {
		return false, err
	}
	defer rollback(ctx, tx)
	var exists bool
	if exists, err = dbgen.New(tx).HasAdminUsers(ctx); err != nil {
		return false, err
	}
	if exists {
		return false, nil
	}
	if err = bootstrapIdentity(ctx, tx, username, hash, content); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

func grants(ctx context.Context, tx pgx.Tx, user string) ([]Grant, error) {
	rows, err := dbgen.New(tx).ListAdminGrants(ctx, user)
	if err != nil {
		return nil, err
	}
	result := []Grant{}
	for _, row := range rows {
		var g Grant
		g.Scope = row.Scope
		g.Content = row.Content
		g.Source = row.Source
		result = append(result, g)
	}
	return result, nil
}

func allowed(who Identity, scope string, content bool) bool {
	for _, g := range who.Grants {
		if (g.Scope == "*" || g.Scope == scope) && (!content || g.Content) {
			return true
		}
	}
	return false
}

func authorize(ctx context.Context, tx pgx.Tx, token string) (Identity, error) {
	var who Identity
	if len(token) != 43 {
		return who, ErrUnauthorized
	}
	session, err := dbgen.New(tx).GetSessionIdentity(ctx, digest(token))
	if err == nil {
		who.ID = session.ID
		who.Name = *session.Name
		who.CSRF = session.Csrf
		who.Expires = session.ExpiresAt
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return who, ErrUnauthorized
	}
	if err != nil {
		return who, err
	}
	who.Grants, err = grants(ctx, tx, who.ID)
	if err == nil && len(who.Grants) == 0 {
		err = ErrForbidden
	}
	return who, err
}

func (s *Service) Identity(ctx context.Context, token string) (Identity, error) {
	tx, err := s.begin(ctx, false)
	if err != nil {
		return Identity{}, err
	}
	defer rollback(ctx, tx)
	return authorize(ctx, tx, token)
}

func (s *Service) issue(ctx context.Context, tx pgx.Tx, id, method string, generation *int64) (Session, error) {
	var result Session
	token, err := opaque()
	if err != nil {
		return result, err
	}
	csrf, err := opaque()
	if err != nil {
		return result, err
	}
	// Bound unused session accumulation for this identity; sessions also have an absolute lifetime.
	if _, err = dbgen.New(tx).PruneAdminSessions(ctx, id); err != nil {
		return result, err
	}
	if _, err = dbgen.New(tx).
		CreateAdminSession(
			ctx,
			dbgen.CreateAdminSessionParams{
				TokenHash:       digest(token),
				ID:              id,
				Method:          method,
				OidcGeneration:  generation,
				Csrf:            csrf,
				LifetimeSeconds: int64(s.lifetime / time.Second),
			},
		); err != nil {
		return result, err
	}
	result.Token = token
	result.Identity, err = authorize(ctx, tx, token)
	return result, err
}

func (s *Service) Login(ctx context.Context, username, password string) (Session, error) {
	var result Session
	release, err := s.passwordSlot(ctx)
	if err != nil {
		return result, err
	}
	defer release()
	account, err := s.localLogin(ctx, username, password)
	if err != nil {
		return result, err
	}
	id, encoded, version := account.ID, *account.PasswordHash, account.Version

	tx, err := s.begin(ctx, false)
	if err != nil {
		return result, err
	}
	defer rollback(ctx, tx)
	var valid bool
	if valid, err = dbgen.New(tx).
		ValidateLocalLogin(
			ctx,
			dbgen.ValidateLocalLoginParams{ID: id, Version: version, PasswordHash: &encoded},
		); err != nil {
		return result, err
	}
	if !valid {
		return result, ErrUnauthorized
	}
	result, err = s.issue(ctx, tx, id, "local", nil)
	if err != nil {
		return result, err
	}
	if err = audit(ctx, tx, id, "login-local", "", id); err != nil {
		return Session{}, err
	}
	err = tx.Commit(ctx)
	return result, err
}

func (s *Service) Logout(ctx context.Context, token string) error {
	_, err := dbgen.New(s.db).DeleteAdminSession(ctx, digest(token))
	return err
}

// PutUser creates a local account or resets its password and enabled status.
// Every update invalidates all existing sessions. Only global operators may do it.
func (s *Service) PutUser(ctx context.Context, token, username, password string, enabled bool) (string, error) {
	if !validName(username) {
		return "", ErrInvalid
	}
	// Authorize before doing expensive password work, then check again in the mutation transaction.
	who, err := s.Identity(ctx, token)
	if err != nil {
		return "", err
	}
	if !allowed(who, "*", false) {
		return "", ErrForbidden
	}
	release, err := s.passwordSlot(ctx)
	if err != nil {
		return "", err
	}
	defer release()
	encoded, err := credentials.HashPassword(password)
	if err != nil {
		return "", ErrInvalid
	}
	tx, err := s.begin(ctx, true)
	if err != nil {
		return "", err
	}
	defer rollback(ctx, tx)
	who, err = authorizeScope(ctx, tx, token, "*", false)
	if err != nil {
		return "", err
	}
	id, err := putLocalUser(ctx, tx, who.ID, username, encoded, enabled)
	if err != nil {
		return "", err
	}

	return id, tx.Commit(ctx)
}

// ReplaceGrants replaces only operator-managed grants. OIDC provenance is separate.
func (s *Service) ReplaceGrants(ctx context.Context, token, user string, want []Grant) error {
	if err := validateGrantReplacement(user, want); err != nil {
		return err
	}
	tx, err := s.begin(ctx, true)
	if err != nil {
		return err
	}
	defer rollback(ctx, tx)
	who, err := authorizeScope(ctx, tx, token, "*", false)
	if err != nil {
		return err
	}
	if err = validateGrantTargets(ctx, tx, user, want); err != nil {
		return err
	}
	if err = replaceLocalGrants(ctx, tx, who.ID, user, want); err != nil {
		return err
	}

	return tx.Commit(ctx)
}

type Organization struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (s *Service) Organizations(ctx context.Context, token string) ([]Organization, error) {
	tx, err := s.begin(ctx, false)
	if err != nil {
		return nil, err
	}
	defer rollback(ctx, tx)
	who, err := authorize(ctx, tx, token)
	if err != nil {
		return nil, err
	}
	rows, err := dbgen.New(tx).
		ListOrganizations(ctx, dbgen.ListOrganizationsParams{UserID: who.ID, GlobalAccess: allowed(who, "*", false)})
	if err != nil {
		return nil, err
	}
	result := []Organization{}
	for _, row := range rows {
		var o Organization
		o.ID = row.ID
		o.Name = row.Name
		result = append(result, o)
	}
	return result, nil
}

func (s *Service) PutOrganization(ctx context.Context, token string, o Organization) error {
	if !validID(o.ID) || !validName(o.Name) {
		return ErrInvalid
	}
	tx, err := s.begin(ctx, false)
	if err != nil {
		return err
	}
	defer rollback(ctx, tx)
	who, err := authorize(ctx, tx, token)
	if err != nil {
		return err
	}
	if !allowed(who, o.ID, false) {
		return ErrForbidden
	}
	if err = putOrganization(ctx, tx, who, o); err != nil {
		return err
	}
	if err = audit(ctx, tx, who.ID, "organization-update", o.ID, o.ID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// RegisterConfiguredOrganizations adds names for trusted startup configuration.
// It neither creates users nor grants access, and never overwrites edited names.
func (s *Service) RegisterConfiguredOrganizations(ctx context.Context) error {
	return RegisterConfiguredOrganizations(ctx, s.db)
}

// RegisterConfiguredOrganizations discovers organizations from trusted inbox
// configuration. Existing organization names and administrator access persist.
func RegisterConfiguredOrganizations(ctx context.Context, db *pgxpool.Pool) error {
	_, err := dbgen.New(db).RegisterConfiguredOrganizations(ctx)
	return err
}

// RegisterSubmissionOrganization makes a trusted send-only organization visible
// to administration without requiring an inbox or resetting an edited name.
func RegisterSubmissionOrganization(ctx context.Context, db *pgxpool.Pool, organization receiptstore.ID) error {
	_, err := dbgen.New(db).RegisterSubmissionOrganization(ctx, organization.String())
	return err
}

// authorizeScope checks authority on the caller's policy-locked transaction.
// It never commits or opens another connection.
func authorizeScope(ctx context.Context, tx pgx.Tx, token, scope string, content bool) (Identity, error) {
	who, err := authorize(ctx, tx, token)
	if err != nil {
		return who, err
	}
	if !allowed(who, scope, content) {
		return who, ErrForbidden
	}
	return who, nil
}

func bootstrapIdentity(ctx context.Context, tx pgx.Tx, username, hash string, content bool) error {
	id, err := identifier()
	if err != nil {
		return err
	}
	if _, err = dbgen.New(tx).
		CreateBootstrapUser(
			ctx,
			dbgen.CreateBootstrapUserParams{
				ID:           id,
				Username:     &username,
				PasswordHash: &hash,
			},
		); err != nil {
		return err
	}
	if _, err = dbgen.New(tx).
		GrantBootstrapAccess(ctx, dbgen.GrantBootstrapAccessParams{UserID: id, Content: content}); err != nil {
		return err
	}
	if err = audit(ctx, tx, id, "bootstrap", "", id); err != nil {
		return err
	}
	return nil
}

func (s *Service) localLogin(ctx context.Context, username, password string) (dbgen.GetLocalLoginRow, error) {
	account, err := dbgen.New(s.db).GetLocalLogin(ctx, &username)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return account, err
	}
	encoded := ""
	if err == nil {
		encoded = *account.PasswordHash
	}
	if !credentials.VerifyHash(encoded, password) {
		return account, ErrUnauthorized
	}
	return account, nil
}

func putLocalUser(ctx context.Context, tx pgx.Tx, actor, username, encoded string, enabled bool) (string, error) {
	id, err := identifier()
	if err != nil {
		return "", err
	}
	id, err = dbgen.New(tx).
		UpsertLocalAdminUser(
			ctx,
			dbgen.UpsertLocalAdminUserParams{
				ID:           id,
				Username:     &username,
				PasswordHash: &encoded,
				Enabled:      enabled,
			},
		)
	if err != nil {
		return "", err
	}
	if err = retainOperator(ctx, tx); err != nil {
		return "", err
	}
	if err = audit(ctx, tx, actor, "account-update", "", id); err != nil {
		return "", err
	}
	return id, nil
}

func validateGrantReplacement(user string, want []Grant) error {
	if !validID(user) || len(want) > 256 {
		return ErrInvalid
	}
	seen := map[string]bool{}
	for _, g := range want {
		if (g.Scope != "*" && !validID(g.Scope)) || seen[g.Scope] {
			return ErrInvalid
		}
		seen[g.Scope] = true
	}
	return nil
}

func validateGrantTargets(ctx context.Context, tx pgx.Tx, user string, want []Grant) error {
	var err error
	var exists bool
	if exists, err = dbgen.New(tx).AdminUserExists(ctx, user); err != nil {
		return err
	}
	if !exists {
		return ErrInvalid
	}

	for _, g := range want {
		if g.Scope != "*" {
			var exists bool
			if exists, err = dbgen.New(tx).OrganizationExistsForGrant(ctx, g.Scope); err != nil {
				return err
			}
			if !exists {
				return ErrInvalid
			}
		}
	}
	return nil
}

func replaceLocalGrants(ctx context.Context, tx pgx.Tx, actor, user string, want []Grant) error {
	if _, err := dbgen.New(tx).DeleteLocalGrants(ctx, user); err != nil {
		return err
	}
	for _, g := range want {
		if _, err := dbgen.New(tx).
			CreateLocalGrant(
				ctx,
				dbgen.CreateLocalGrantParams{UserID: user, Scope: g.Scope, Content: g.Content},
			); err != nil {
			return err
		}
	}
	if err := retainOperator(ctx, tx); err != nil {
		return err
	}
	if err := audit(ctx, tx, actor, "grants-replace", "", user); err != nil {
		return err
	}
	return nil
}

func putOrganization(ctx context.Context, tx pgx.Tx, who Identity, o Organization) error {
	var err error
	// Organization managers may rename an assigned organization, never create one.
	if !allowed(who, "*", false) {
		tag, e := dbgen.New(tx).UpdateOrganization(ctx, dbgen.UpdateOrganizationParams{ID: o.ID, Name: o.Name})
		err = e
		if err == nil && tag != 1 {
			return ErrForbidden
		}
	} else {
		_, err = dbgen.New(tx).UpsertOrganization(ctx, dbgen.UpsertOrganizationParams{ID: o.ID, Name: o.Name})
	}
	return err
}
