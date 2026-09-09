// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

// Package clientcredentials manages gateway-issued bearer credentials.
//
// The package is profile-neutral. An empty tenant is the standalone
// single-tenant scope; the cluster composition enables explicit tenant binding.
package clientcredentials

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/sparksq/sparkroute/pkg/identity"
)

const (
	RoleRead      = "credentials_read"
	RoleWrite     = "credentials_write"
	RoleInference = "inference"

	defaultListLimit = 100
	maxListLimit     = 500
	maxNameBytes     = 128
	maxIdentityBytes = 256
	maxRoleCount     = 64
	maxMapEntries    = 64
)

type State string

const (
	StateActive   State = "active"
	StateDisabled State = "disabled"
	StateRevoked  State = "revoked"
)

var (
	ErrNotFound          = errors.New("client credential not found")
	ErrConflict          = errors.New("client credential conflict")
	ErrRevoked           = errors.New("client credential is revoked")
	ErrInvalidCredential = errors.New("client credential is invalid")

	identifierPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@/-]*$`)
	rolePattern       = regexp.MustCompile(`^[a-z][a-z0-9_:-]*$`)
)

// Credential is the presentation-safe credential metadata. Secret material
// and its digest are intentionally absent.
type Credential struct {
	ID                 string            `json:"id"`
	Name               string            `json:"name"`
	TenantID           string            `json:"tenant_id,omitempty"`
	PrincipalID        string            `json:"principal_id"`
	PrincipalType      string            `json:"principal_type,omitempty"`
	PrincipalSubject   string            `json:"principal_subject,omitempty"`
	Roles              []string          `json:"roles"`
	AllowedAttribution []string          `json:"allowed_attribution,omitempty"`
	FixedAttribution   map[string]string `json:"fixed_attribution,omitempty"`
	State              State             `json:"state"`
	CreatedAt          time.Time         `json:"created_at"`
	CreatedBy          string            `json:"created_by"`
	UpdatedAt          time.Time         `json:"updated_at"`
	UpdatedBy          string            `json:"updated_by"`
	RotatedAt          *time.Time        `json:"rotated_at,omitempty"`
	ExpiresAt          *time.Time        `json:"expires_at,omitempty"`
	RevokedAt          *time.Time        `json:"revoked_at,omitempty"`
}

// Record is the storage representation. SecretSHA256 is never serialized.
type Record struct {
	Credential
	SecretSHA256 [sha256.Size]byte `json:"-"`
}

type CreateInput struct {
	Name               string            `json:"name"`
	TenantID           string            `json:"tenant_id,omitempty"`
	PrincipalID        string            `json:"principal_id"`
	PrincipalType      string            `json:"principal_type,omitempty"`
	PrincipalSubject   string            `json:"principal_subject,omitempty"`
	Roles              []string          `json:"roles"`
	AllowedAttribution []string          `json:"allowed_attribution,omitempty"`
	FixedAttribution   map[string]string `json:"fixed_attribution,omitempty"`
	ExpiresAt          *time.Time        `json:"expires_at,omitempty"`
}

type IssuedCredential struct {
	Credential Credential `json:"credential"`
	APIKey     string     `json:"api_key"`
}

type ListQuery struct {
	TenantID    string
	PrincipalID string
	State       State
	Limit       int
}

type Page struct {
	Credentials []Credential `json:"credentials"`
}

type AuditEvent struct {
	ID           int64          `json:"id"`
	CredentialID string         `json:"credential_id"`
	TenantID     string         `json:"tenant_id,omitempty"`
	Action       string         `json:"action"`
	OccurredAt   time.Time      `json:"occurred_at"`
	Actor        string         `json:"actor"`
	Details      map[string]any `json:"details,omitempty"`
}

type AuditQuery struct {
	TenantID     string
	CredentialID string
	Action       string
	Limit        int
}

type AuditPage struct {
	Events []AuditEvent `json:"events"`
}

// Store owns atomic credential mutations and their corresponding audit event.
type Store interface {
	Create(context.Context, Record, string) error
	Get(context.Context, string) (Record, error)
	List(context.Context, ListQuery) (Page, error)
	Rotate(context.Context, string, [sha256.Size]byte, string) (Credential, error)
	SetState(context.Context, string, State, string) (Credential, error)
	ListAudit(context.Context, AuditQuery) (AuditPage, error)
	Close() error
}

type Manager struct {
	store       Store
	now         func() time.Time
	inputPolicy func(CreateInput) (CreateInput, error)
}

func NewManager(store Store) (*Manager, error) {
	if store == nil {
		return nil, fmt.Errorf("client credential store is required")
	}
	return &Manager{store: store, now: time.Now}, nil
}

// NewManagerWithInputPolicy adds an edition-specific principal policy while
// keeping generation, persistence, lifecycle, and authentication shared.
func NewManagerWithInputPolicy(
	store Store,
	policy func(CreateInput) (CreateInput, error),
) (*Manager, error) {
	manager, err := NewManager(store)
	if err != nil {
		return nil, err
	}
	manager.inputPolicy = policy
	return manager, nil
}

func (m *Manager) Create(
	ctx context.Context,
	input CreateInput,
	actor string,
) (IssuedCredential, error) {
	if strings.TrimSpace(input.PrincipalType) == "" {
		input.PrincipalType = "machine"
	}
	if strings.TrimSpace(input.PrincipalSubject) == "" {
		input.PrincipalSubject = strings.TrimSpace(input.PrincipalID)
	}
	if m.inputPolicy != nil {
		var err error
		input, err = m.inputPolicy(input)
		if err != nil {
			return IssuedCredential{}, fmt.Errorf("%w: %v", ErrInvalidCredential, err)
		}
	}
	if err := validateCreateInput(input, actor, m.now().UTC()); err != nil {
		return IssuedCredential{}, fmt.Errorf("%w: %v", ErrInvalidCredential, err)
	}
	id, err := randomComponent(18)
	if err != nil {
		return IssuedCredential{}, fmt.Errorf("generate client credential ID: %w", err)
	}
	secret, err := randomComponent(32)
	if err != nil {
		return IssuedCredential{}, fmt.Errorf("generate client credential secret: %w", err)
	}
	id = "cc_" + id
	apiKey := "llmgw_v1." + id + "." + secret
	now := m.now().UTC()
	record := Record{
		Credential: Credential{
			ID:                 id,
			Name:               strings.TrimSpace(input.Name),
			TenantID:           strings.TrimSpace(input.TenantID),
			PrincipalID:        strings.TrimSpace(input.PrincipalID),
			PrincipalType:      strings.TrimSpace(input.PrincipalType),
			PrincipalSubject:   strings.TrimSpace(input.PrincipalSubject),
			Roles:              normalizedStrings(input.Roles),
			AllowedAttribution: normalizedStrings(input.AllowedAttribution),
			FixedAttribution:   cloneStringMap(input.FixedAttribution),
			State:              StateActive,
			CreatedAt:          now,
			CreatedBy:          actor,
			UpdatedAt:          now,
			UpdatedBy:          actor,
			ExpiresAt:          cloneTime(input.ExpiresAt),
		},
		SecretSHA256: sha256.Sum256([]byte(apiKey)),
	}
	if err := m.store.Create(ctx, record, actor); err != nil {
		return IssuedCredential{}, err
	}
	return IssuedCredential{Credential: cloneCredential(record.Credential), APIKey: apiKey}, nil
}

func (m *Manager) List(ctx context.Context, query ListQuery) (Page, error) {
	if err := validateListQuery(query); err != nil {
		return Page{}, fmt.Errorf("%w: %v", ErrInvalidCredential, err)
	}
	return m.store.List(ctx, query)
}

func (m *Manager) Get(ctx context.Context, id string) (Credential, error) {
	if err := validateCredentialID(id); err != nil {
		return Credential{}, fmt.Errorf("%w: %v", ErrInvalidCredential, err)
	}
	record, err := m.store.Get(ctx, id)
	if err != nil {
		return Credential{}, err
	}
	return cloneCredential(record.Credential), nil
}

func (m *Manager) Rotate(
	ctx context.Context,
	id string,
	actor string,
) (IssuedCredential, error) {
	if err := validateMutation(id, actor); err != nil {
		return IssuedCredential{}, fmt.Errorf("%w: %v", ErrInvalidCredential, err)
	}
	secret, err := randomComponent(32)
	if err != nil {
		return IssuedCredential{}, fmt.Errorf("generate client credential secret: %w", err)
	}
	apiKey := "llmgw_v1." + id + "." + secret
	credential, err := m.store.Rotate(
		ctx,
		id,
		sha256.Sum256([]byte(apiKey)),
		actor,
	)
	if err != nil {
		return IssuedCredential{}, err
	}
	return IssuedCredential{Credential: credential, APIKey: apiKey}, nil
}

func (m *Manager) SetState(
	ctx context.Context,
	id string,
	state State,
	actor string,
) (Credential, error) {
	if err := validateMutation(id, actor); err != nil {
		return Credential{}, fmt.Errorf("%w: %v", ErrInvalidCredential, err)
	}
	if state != StateActive && state != StateDisabled && state != StateRevoked {
		return Credential{}, fmt.Errorf("%w: unsupported client credential state %q", ErrInvalidCredential, state)
	}
	return m.store.SetState(ctx, id, state, actor)
}

func (m *Manager) ListAudit(
	ctx context.Context,
	query AuditQuery,
) (AuditPage, error) {
	if query.Limit == 0 {
		query.Limit = defaultListLimit
	}
	if query.Limit < 1 || query.Limit > maxListLimit {
		return AuditPage{}, fmt.Errorf("%w: audit limit must be between 1 and %d", ErrInvalidCredential, maxListLimit)
	}
	if query.CredentialID != "" {
		if err := validateCredentialID(query.CredentialID); err != nil {
			return AuditPage{}, fmt.Errorf("%w: %v", ErrInvalidCredential, err)
		}
	}
	if len(query.TenantID) > maxIdentityBytes || len(query.Action) > 64 {
		return AuditPage{}, fmt.Errorf("%w: client credential audit filter is too long", ErrInvalidCredential)
	}
	return m.store.ListAudit(ctx, query)
}

// AuthenticateBearer verifies a gateway-issued API key and returns its bound
// principal. It performs a constant-time digest comparison after indexed ID
// lookup and never returns credential metadata on failure.
func (m *Manager) AuthenticateBearer(
	ctx context.Context,
	apiKey string,
) (identity.Principal, error) {
	id, ok := credentialIDFromAPIKey(apiKey)
	if !ok {
		return identity.Principal{}, identity.ErrInvalidCredentials
	}
	record, err := m.store.Get(ctx, id)
	if err != nil {
		return identity.Principal{}, identity.ErrInvalidCredentials
	}
	digest := sha256.Sum256([]byte(apiKey))
	if subtle.ConstantTimeCompare(digest[:], record.SecretSHA256[:]) != 1 ||
		record.State != StateActive ||
		record.ExpiresAt != nil && !record.ExpiresAt.After(m.now().UTC()) {
		return identity.Principal{}, identity.ErrInvalidCredentials
	}
	return identity.Principal{
		ID:                 record.PrincipalID,
		Type:               record.PrincipalType,
		Tenant:             record.TenantID,
		Subject:            record.PrincipalSubject,
		Roles:              append([]string(nil), record.Roles...),
		AllowedAttribution: append([]string(nil), record.AllowedAttribution...),
		FixedAttribution:   cloneStringMap(record.FixedAttribution),
	}, nil
}

// Authenticator adapts a Manager to the edition-neutral identity contract.
// RequiredRole is useful for the data plane; route-level admin authorization
// should leave it empty and enforce roles in each handler.
type Authenticator struct {
	Manager      *Manager
	RequiredRole string
}

func (a Authenticator) Authenticate(
	ctx context.Context,
	request *http.Request,
) (identity.Principal, error) {
	if a.Manager == nil {
		return identity.Principal{}, identity.ErrInvalidCredentials
	}
	apiKey, err := bearerToken(request)
	if err != nil {
		return identity.Principal{}, err
	}
	principal, err := a.Manager.AuthenticateBearer(ctx, apiKey)
	if err != nil {
		return identity.Principal{}, err
	}
	if a.RequiredRole != "" && !HasRole(principal, a.RequiredRole) {
		return identity.Principal{}, identity.ErrInvalidCredentials
	}
	return principal, nil
}

func (Authenticator) AuthenticationChallenge(realm string) string {
	return fmt.Sprintf(`Bearer realm=%q`, realm)
}

func HasRole(principal identity.Principal, role string) bool {
	for _, current := range principal.Roles {
		if current == role {
			return true
		}
	}
	return false
}

func bearerToken(request *http.Request) (string, error) {
	values := request.Header.Values("Authorization")
	if len(values) == 0 {
		return "", identity.ErrMissingCredentials
	}
	if len(values) != 1 {
		return "", identity.ErrInvalidCredentials
	}
	parts := strings.Fields(values[0])
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || parts[1] == "" {
		return "", identity.ErrInvalidCredentials
	}
	return parts[1], nil
}

func credentialIDFromAPIKey(apiKey string) (string, bool) {
	parts := strings.Split(apiKey, ".")
	if len(parts) != 3 || parts[0] != "llmgw_v1" || parts[2] == "" {
		return "", false
	}
	if err := validateCredentialID(parts[1]); err != nil {
		return "", false
	}
	if decoded, err := base64.RawURLEncoding.DecodeString(parts[2]); err != nil || len(decoded) != 32 {
		return "", false
	}
	return parts[1], true
}

func randomComponent(size int) (string, error) {
	raw := make([]byte, size)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func validateCreateInput(input CreateInput, actor string, now time.Time) error {
	if err := validateActor(actor); err != nil {
		return err
	}
	if err := validateBounded("name", input.Name, maxNameBytes, true); err != nil {
		return err
	}
	if err := validateIdentifier("principal_id", input.PrincipalID, true); err != nil {
		return err
	}
	if err := validateIdentifier("principal_type", input.PrincipalType, false); err != nil {
		return err
	}
	if err := validateBounded("principal_subject", input.PrincipalSubject, maxIdentityBytes, false); err != nil {
		return err
	}
	if err := validateBounded("tenant_id", input.TenantID, maxIdentityBytes, false); err != nil {
		return err
	}
	roles := normalizedStrings(input.Roles)
	if len(roles) == 0 || len(roles) > maxRoleCount {
		return fmt.Errorf("roles must contain between 1 and %d unique entries", maxRoleCount)
	}
	for _, role := range roles {
		if len(role) > 64 || !rolePattern.MatchString(role) {
			return fmt.Errorf("invalid role %q", role)
		}
	}
	allowed := normalizedStrings(input.AllowedAttribution)
	if len(allowed) > maxMapEntries {
		return fmt.Errorf("allowed_attribution contains more than %d entries", maxMapEntries)
	}
	for _, name := range allowed {
		if err := validateIdentifier("allowed attribution", name, true); err != nil {
			return err
		}
	}
	if len(input.FixedAttribution) > maxMapEntries {
		return fmt.Errorf("fixed_attribution contains more than %d entries", maxMapEntries)
	}
	for key, value := range input.FixedAttribution {
		if err := validateIdentifier("fixed attribution key", key, true); err != nil {
			return err
		}
		if err := validateBounded("fixed attribution value", value, maxIdentityBytes, true); err != nil {
			return err
		}
	}
	if input.ExpiresAt != nil && !input.ExpiresAt.After(now) {
		return fmt.Errorf("expires_at must be in the future")
	}
	return nil
}

func validateListQuery(query ListQuery) error {
	if query.Limit != 0 && (query.Limit < 1 || query.Limit > maxListLimit) {
		return fmt.Errorf("credential limit must be between 1 and %d", maxListLimit)
	}
	if query.State != "" && query.State != StateActive && query.State != StateDisabled && query.State != StateRevoked {
		return fmt.Errorf("invalid client credential state %q", query.State)
	}
	if len(query.TenantID) > maxIdentityBytes || len(query.PrincipalID) > maxIdentityBytes {
		return fmt.Errorf("client credential filter is too long")
	}
	return nil
}

func validateMutation(id, actor string) error {
	if err := validateCredentialID(id); err != nil {
		return err
	}
	return validateActor(actor)
}

func validateCredentialID(id string) error {
	if !strings.HasPrefix(id, "cc_") || len(id) < 10 || len(id) > 64 || !identifierPattern.MatchString(id) {
		return fmt.Errorf("invalid client credential ID")
	}
	return nil
}

func validateActor(actor string) error {
	return validateIdentifier("actor", actor, true)
}

func validateIdentifier(name, value string, required bool) error {
	value = strings.TrimSpace(value)
	if err := validateBounded(name, value, maxIdentityBytes, required); err != nil {
		return err
	}
	if value != "" && !identifierPattern.MatchString(value) {
		return fmt.Errorf("%s contains unsupported characters", name)
	}
	return nil
}

func validateBounded(name, value string, maximum int, required bool) error {
	value = strings.TrimSpace(value)
	if required && value == "" {
		return fmt.Errorf("%s is required", name)
	}
	if len(value) > maximum {
		return fmt.Errorf("%s exceeds %d bytes", name, maximum)
	}
	return nil
}

func normalizedStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func cloneCredential(value Credential) Credential {
	value.Roles = append([]string(nil), value.Roles...)
	value.AllowedAttribution = append([]string(nil), value.AllowedAttribution...)
	value.FixedAttribution = cloneStringMap(value.FixedAttribution)
	value.RotatedAt = cloneTime(value.RotatedAt)
	value.ExpiresAt = cloneTime(value.ExpiresAt)
	value.RevokedAt = cloneTime(value.RevokedAt)
	return value
}

func cloneStringMap(source map[string]string) map[string]string {
	if source == nil {
		return nil
	}
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func cloneTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	cloned := value.UTC()
	return &cloned
}
