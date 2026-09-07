package clientcredentials

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/sparksq/sparkroute/pkg/identity"
)

const maxAdminBodyBytes = 1 << 20

type AdminOptions struct {
	Manager     *Manager
	MultiTenant bool
}

// NewAdminHandler returns the shared credential administration API. The
// parent admin surface must authenticate requests and install identity context;
// this handler remains authoritative for credential roles and tenant scope.
func NewAdminHandler(options AdminOptions) (http.Handler, error) {
	if options.Manager == nil {
		return nil, fmt.Errorf("client credential manager is required")
	}
	return &adminHandler{options: options}, nil
}

type adminHandler struct {
	options AdminOptions
}

func (h *adminHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	principal, ok := principalFromRequest(request)
	if !ok {
		writeAdminError(writer, http.StatusUnauthorized, "authentication_required", "admin authentication is required")
		return
	}
	path := strings.TrimPrefix(request.URL.Path, "/v1/client-credentials")
	switch {
	case path == "":
		h.collection(writer, request, principal)
	case path == "/audit":
		h.audit(writer, request, principal)
	case strings.HasPrefix(path, "/"):
		h.mutation(writer, request, principal, strings.TrimPrefix(path, "/"))
	default:
		writeAdminError(writer, http.StatusNotFound, "not_found", "route not found")
	}
}

func (h *adminHandler) collection(
	writer http.ResponseWriter,
	request *http.Request,
	principal identityPrincipal,
) {
	switch request.Method {
	case http.MethodGet:
		if !requireAdminRole(writer, principal, RoleRead) {
			return
		}
		values, ok := strictAdminQuery(
			writer,
			request.URL.Query(),
			"tenant_id",
			"principal_id",
			"state",
			"limit",
		)
		if !ok {
			return
		}
		tenant, ok := h.authorizedTenant(writer, principal, values.Get("tenant_id"), false)
		if !ok {
			return
		}
		limit, err := optionalAdminInt(values.Get("limit"))
		if err != nil {
			writeAdminError(writer, http.StatusBadRequest, "invalid_query", err.Error())
			return
		}
		page, err := h.options.Manager.List(request.Context(), ListQuery{
			TenantID:    tenant,
			PrincipalID: values.Get("principal_id"),
			State:       State(values.Get("state")),
			Limit:       limit,
		})
		if err != nil {
			writeManagerError(writer, err)
			return
		}
		writeAdminJSON(writer, http.StatusOK, page)
	case http.MethodPost:
		if !requireAdminRole(writer, principal, RoleWrite) {
			return
		}
		if _, ok := strictAdminQuery(writer, request.URL.Query()); !ok {
			return
		}
		var input CreateInput
		if !decodeAdminJSON(writer, request, &input) {
			return
		}
		tenant, ok := h.authorizedTenant(writer, principal, input.TenantID, true)
		if !ok {
			return
		}
		input.TenantID = tenant
		issued, err := h.options.Manager.Create(request.Context(), input, principal.ID)
		if err != nil {
			writeManagerError(writer, err)
			return
		}
		writer.Header().Set("Cache-Control", "no-store")
		writeAdminJSON(writer, http.StatusCreated, issued)
	default:
		writer.Header().Set("Allow", "GET, POST")
		writeAdminError(writer, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
	}
}

func (h *adminHandler) audit(
	writer http.ResponseWriter,
	request *http.Request,
	principal identityPrincipal,
) {
	if request.Method != http.MethodGet {
		writer.Header().Set("Allow", http.MethodGet)
		writeAdminError(writer, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	if !requireAdminRole(writer, principal, RoleRead) {
		return
	}
	values, ok := strictAdminQuery(
		writer,
		request.URL.Query(),
		"tenant_id",
		"credential_id",
		"action",
		"limit",
	)
	if !ok {
		return
	}
	tenant, ok := h.authorizedTenant(writer, principal, values.Get("tenant_id"), false)
	if !ok {
		return
	}
	limit, err := optionalAdminInt(values.Get("limit"))
	if err != nil {
		writeAdminError(writer, http.StatusBadRequest, "invalid_query", err.Error())
		return
	}
	page, err := h.options.Manager.ListAudit(request.Context(), AuditQuery{
		TenantID:     tenant,
		CredentialID: values.Get("credential_id"),
		Action:       values.Get("action"),
		Limit:        limit,
	})
	if err != nil {
		writeManagerError(writer, err)
		return
	}
	writeAdminJSON(writer, http.StatusOK, page)
}

func (h *adminHandler) mutation(
	writer http.ResponseWriter,
	request *http.Request,
	principal identityPrincipal,
	path string,
) {
	if request.Method != http.MethodPost {
		writer.Header().Set("Allow", http.MethodPost)
		writeAdminError(writer, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
		return
	}
	if !requireAdminRole(writer, principal, RoleWrite) {
		return
	}
	if _, ok := strictAdminQuery(writer, request.URL.Query()); !ok {
		return
	}
	parts := strings.Split(path, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		writeAdminError(writer, http.StatusNotFound, "not_found", "route not found")
		return
	}
	if !decodeEmptyAdminBody(writer, request) {
		return
	}
	current, err := h.options.Manager.Get(request.Context(), parts[0])
	if err != nil {
		writeManagerError(writer, err)
		return
	}
	if !h.credentialVisible(principal, current) {
		writeAdminError(writer, http.StatusNotFound, "not_found", "client credential not found")
		return
	}
	switch parts[1] {
	case "rotate":
		issued, err := h.options.Manager.Rotate(request.Context(), current.ID, principal.ID)
		if err != nil {
			writeManagerError(writer, err)
			return
		}
		writer.Header().Set("Cache-Control", "no-store")
		writeAdminJSON(writer, http.StatusOK, issued)
	case "enable":
		h.setState(writer, request, current.ID, StateActive, principal.ID)
	case "disable":
		h.setState(writer, request, current.ID, StateDisabled, principal.ID)
	case "revoke":
		h.setState(writer, request, current.ID, StateRevoked, principal.ID)
	default:
		writeAdminError(writer, http.StatusNotFound, "not_found", "route not found")
	}
}

func (h *adminHandler) setState(
	writer http.ResponseWriter,
	request *http.Request,
	id string,
	state State,
	actor string,
) {
	credential, err := h.options.Manager.SetState(request.Context(), id, state, actor)
	if err != nil {
		writeManagerError(writer, err)
		return
	}
	writeAdminJSON(writer, http.StatusOK, map[string]Credential{"credential": credential})
}

// identityPrincipal is deliberately the subset used by this handler, making
// tenant authorization tests independent from request authentication details.
type identityPrincipal struct {
	ID     string
	Tenant string
	Roles  []string
}

func principalFromRequest(request *http.Request) (identityPrincipal, bool) {
	value, ok := identity.FromContext(request.Context())
	if !ok {
		return identityPrincipal{}, false
	}
	return identityPrincipal{
		ID: value.Principal.ID, Tenant: value.Principal.Tenant,
		Roles: append([]string(nil), value.Principal.Roles...),
	}, true
}

func (h *adminHandler) authorizedTenant(
	writer http.ResponseWriter,
	principal identityPrincipal,
	requested string,
	required bool,
) (string, bool) {
	requested = strings.TrimSpace(requested)
	if !h.options.MultiTenant {
		if requested != "" {
			writeAdminError(writer, http.StatusBadRequest, "tenant_not_supported", "tenant_id is not supported by the standalone single-tenant profile")
			return "", false
		}
		return "", true
	}
	if principal.Tenant != "" {
		if requested != "" && requested != principal.Tenant {
			writeAdminError(writer, http.StatusForbidden, "tenant_forbidden", "client credential tenant is outside the authenticated scope")
			return "", false
		}
		return principal.Tenant, true
	}
	if required && requested == "" {
		writeAdminError(writer, http.StatusBadRequest, "tenant_required", "tenant_id is required for a global administrator")
		return "", false
	}
	return requested, true
}

func (h *adminHandler) credentialVisible(principal identityPrincipal, credential Credential) bool {
	return !h.options.MultiTenant || principal.Tenant == "" || principal.Tenant == credential.TenantID
}

func requireAdminRole(writer http.ResponseWriter, principal identityPrincipal, role string) bool {
	for _, current := range principal.Roles {
		if current == role {
			return true
		}
	}
	writeAdminError(writer, http.StatusForbidden, "forbidden", "the authenticated principal is not authorized for this operation")
	return false
}

func strictAdminQuery(writer http.ResponseWriter, values url.Values, allowed ...string) (url.Values, bool) {
	allow := make(map[string]struct{}, len(allowed))
	for _, name := range allowed {
		allow[name] = struct{}{}
	}
	for name, current := range values {
		if _, ok := allow[name]; !ok || len(current) != 1 {
			writeAdminError(writer, http.StatusBadRequest, "invalid_query", "query parameters are unknown or repeated")
			return nil, false
		}
	}
	return values, true
}

func optionalAdminInt(value string) (int, error) {
	if value == "" {
		return 0, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("limit must be an integer")
	}
	return parsed, nil
}

func decodeAdminJSON(writer http.ResponseWriter, request *http.Request, target any) bool {
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || !strings.EqualFold(mediaType, "application/json") {
		writeAdminError(writer, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json")
		return false
	}
	request.Body = http.MaxBytesReader(writer, request.Body, maxAdminBodyBytes)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeAdminError(writer, http.StatusBadRequest, "invalid_json", "request body must contain one valid JSON object")
		return false
	}
	var trailer any
	if err := decoder.Decode(&trailer); !errors.Is(err, io.EOF) {
		writeAdminError(writer, http.StatusBadRequest, "invalid_json", "request body must contain one valid JSON object")
		return false
	}
	return true
}

func decodeEmptyAdminBody(writer http.ResponseWriter, request *http.Request) bool {
	if request.Body == nil || request.Body == http.NoBody || request.ContentLength == 0 {
		return true
	}
	request.Body = http.MaxBytesReader(writer, request.Body, 1)
	var value [1]byte
	count, err := request.Body.Read(value[:])
	if count == 0 && errors.Is(err, io.EOF) {
		return true
	}
	writeAdminError(writer, http.StatusBadRequest, "body_not_allowed", "request body is not supported")
	return false
}

func writeManagerError(writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound):
		writeAdminError(writer, http.StatusNotFound, "not_found", "client credential not found")
	case errors.Is(err, ErrConflict), errors.Is(err, ErrRevoked):
		writeAdminError(writer, http.StatusConflict, "credential_conflict", err.Error())
	case errors.Is(err, ErrInvalidCredential):
		writeAdminError(writer, http.StatusBadRequest, "invalid_request", err.Error())
	default:
		writeAdminError(writer, http.StatusInternalServerError, "internal_error", "client credential operation failed")
	}
}

func writeAdminJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func writeAdminError(writer http.ResponseWriter, status int, code, message string) {
	writeAdminJSON(writer, status, map[string]any{
		"error": map[string]string{"code": code, "message": message},
	})
}
