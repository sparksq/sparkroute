// Package endpointregistry defines dynamic serving endpoint registration.
package endpointregistry

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"sort"
	"sync"
	"time"
)

type State string

const (
	StateOffline      State = "offline"
	StateActivating   State = "activating"
	StateReady        State = "ready"
	StateDraining     State = "draining"
	StateDeactivating State = "deactivating"
	StateFailed       State = "failed"
	StateUnknown      State = "unknown"
)

const (
	DefaultPageSize = 100
	MaxPageSize     = 500
	maxCursorBytes  = 2048
	cursorVersion   = 1
)

type Endpoint struct {
	ID              string
	Target          string
	BaseURL         string
	Protocol        string
	ServedModels    []string
	Controller      string
	ClusterID       string
	JobID           string
	BindingRevision string
	RecipeRevision  string
	FencingToken    int64
	State           State
	ActiveRequests  int
	MaxConcurrency  int
	RegisteredAt    time.Time
	HeartbeatAt     time.Time
	ExpiresAt       time.Time
	Metadata        map[string]string
}

type Registry interface {
	Register(ctx context.Context, endpoint Endpoint) error
	Remove(ctx context.Context, endpointID string) error
	Ready(ctx context.Context, target string) ([]Endpoint, error)
}

// Query selects current endpoint registrations in stable endpoint-ID order.
type Query struct {
	Target     string
	Controller string
	State      State
	Limit      int
	Cursor     string
}

// Page is one bounded page of current endpoint registrations.
type Page struct {
	Endpoints  []Endpoint `json:"endpoints"`
	NextCursor string     `json:"next_cursor,omitempty"`
}

// Inspector is separate from Registry so routing implementations need not
// expose an administrative inventory unless they intentionally support it.
type Inspector interface {
	List(ctx context.Context, query Query) (Page, error)
}

// Memory is the single-process registry used by the standalone composition.
type Memory struct {
	mu        sync.RWMutex
	endpoints map[string]Endpoint
	now       func() time.Time
}

func NewMemory() *Memory {
	return &Memory{
		endpoints: make(map[string]Endpoint),
		now:       time.Now,
	}
}

func (r *Memory) Register(ctx context.Context, endpoint Endpoint) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ValidateEndpoint(endpoint); err != nil {
		return err
	}
	if endpoint.State == "" {
		endpoint.State = StateUnknown
	}
	endpoint = cloneEndpoint(endpoint)
	r.mu.Lock()
	if current, exists := r.endpoints[endpoint.ID]; exists &&
		current.FencingToken > endpoint.FencingToken {
		r.mu.Unlock()
		return fmt.Errorf("endpoint registration has a stale fencing token")
	}
	r.endpoints[endpoint.ID] = endpoint
	r.mu.Unlock()
	return nil
}

// List returns a bounded, cloned snapshot. Expired registrations remain visible
// to operators until their owner removes them, but Ready never routes to them.
func (r *Memory) List(ctx context.Context, query Query) (Page, error) {
	if err := ctx.Err(); err != nil {
		return Page{}, err
	}
	query, afterID, err := validateQuery(query)
	if err != nil {
		return Page{}, err
	}
	r.mu.RLock()
	endpoints := make([]Endpoint, 0, len(r.endpoints))
	for _, endpoint := range r.endpoints {
		if query.Target != "" && endpoint.Target != query.Target ||
			query.Controller != "" && endpoint.Controller != query.Controller ||
			query.State != "" && endpoint.State != query.State {
			continue
		}
		endpoints = append(endpoints, cloneEndpoint(endpoint))
	}
	r.mu.RUnlock()
	sort.Slice(endpoints, func(i, j int) bool { return endpoints[i].ID < endpoints[j].ID })
	if afterID != "" {
		index := sort.Search(len(endpoints), func(index int) bool {
			return endpoints[index].ID > afterID
		})
		endpoints = endpoints[index:]
	}
	page := Page{Endpoints: endpoints}
	if len(endpoints) > query.Limit {
		page.Endpoints = endpoints[:query.Limit]
		page.NextCursor, err = encodeCursor(page.Endpoints[len(page.Endpoints)-1].ID)
		if err != nil {
			return Page{}, err
		}
	}
	return page, nil
}

func (r *Memory) Remove(ctx context.Context, endpointID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	delete(r.endpoints, endpointID)
	r.mu.Unlock()
	return nil
}

func (r *Memory) Ready(ctx context.Context, target string) ([]Endpoint, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	now := r.now()
	r.mu.RLock()
	ready := make([]Endpoint, 0)
	for _, endpoint := range r.endpoints {
		if endpoint.Target != target || endpoint.State != StateReady {
			continue
		}
		if !endpoint.ExpiresAt.IsZero() && !endpoint.ExpiresAt.After(now) {
			continue
		}
		ready = append(ready, cloneEndpoint(endpoint))
	}
	r.mu.RUnlock()
	sort.Slice(ready, func(i, j int) bool {
		return ready[i].ID < ready[j].ID
	})
	return ready, nil
}

func cloneEndpoint(endpoint Endpoint) Endpoint {
	endpoint.ServedModels = append([]string(nil), endpoint.ServedModels...)
	if endpoint.Metadata != nil {
		metadata := make(map[string]string, len(endpoint.Metadata))
		for key, value := range endpoint.Metadata {
			metadata[key] = value
		}
		endpoint.Metadata = metadata
	}
	return endpoint
}

func validateEndpoint(endpoint Endpoint) error {
	if endpoint.ID == "" {
		return fmt.Errorf("endpoint ID is required")
	}
	if endpoint.Target == "" {
		return fmt.Errorf("endpoint target is required")
	}
	for name, value := range map[string]string{
		"endpoint ID":               endpoint.ID,
		"endpoint target":           endpoint.Target,
		"endpoint protocol":         endpoint.Protocol,
		"endpoint controller":       endpoint.Controller,
		"endpoint cluster ID":       endpoint.ClusterID,
		"endpoint job ID":           endpoint.JobID,
		"endpoint binding revision": endpoint.BindingRevision,
		"endpoint recipe revision":  endpoint.RecipeRevision,
	} {
		if len(value) > 1024 {
			return fmt.Errorf("%s exceeds 1024 bytes", name)
		}
	}
	if err := validateState(endpoint.State); err != nil {
		return err
	}
	if endpoint.ActiveRequests < 0 || endpoint.MaxConcurrency < 0 {
		return fmt.Errorf("endpoint concurrency values must not be negative")
	}
	if endpoint.FencingToken < 0 {
		return fmt.Errorf("endpoint fencing token must not be negative")
	}
	if endpoint.FencingToken > 0 && (endpoint.Controller == "" || endpoint.BindingRevision == "") {
		return fmt.Errorf("fenced endpoint requires controller and binding revision")
	}
	if len(endpoint.ServedModels) > 256 {
		return fmt.Errorf("endpoint advertises too many served models")
	}
	for _, model := range endpoint.ServedModels {
		if model == "" || len(model) > 1024 {
			return fmt.Errorf("endpoint served model must be between 1 and 1024 bytes")
		}
	}
	if len(endpoint.Metadata) > 64 {
		return fmt.Errorf("endpoint metadata has too many fields")
	}
	metadataBytes := 0
	for key, value := range endpoint.Metadata {
		if key == "" || len(key) > 128 || len(value) > 1024 {
			return fmt.Errorf("endpoint metadata key/value is invalid")
		}
		metadataBytes += len(key) + len(value)
		if metadataBytes > 16<<10 {
			return fmt.Errorf("endpoint metadata exceeds 16384 bytes")
		}
	}
	return nil
}

// ValidateEndpoint checks an endpoint without registering it. Controllers and
// data-plane coordinators use this before trusting a returned endpoint lease.
func ValidateEndpoint(endpoint Endpoint) error {
	if err := validateEndpoint(endpoint); err != nil {
		return err
	}
	parsed, err := url.Parse(endpoint.BaseURL)
	if err != nil || parsed.Host == "" || parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("endpoint base URL must be an absolute HTTP(S) URL")
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("endpoint base URL must not contain userinfo, query, or fragment")
	}
	return nil
}

func validateQuery(query Query) (Query, string, error) {
	for name, value := range map[string]string{
		"target":     query.Target,
		"controller": query.Controller,
	} {
		if len(value) > 1024 {
			return Query{}, "", fmt.Errorf("endpoint query %s exceeds 1024 bytes", name)
		}
	}
	if query.State != "" {
		if err := validateState(query.State); err != nil {
			return Query{}, "", err
		}
	}
	switch {
	case query.Limit < 0:
		return Query{}, "", fmt.Errorf("endpoint page limit must not be negative")
	case query.Limit == 0:
		query.Limit = DefaultPageSize
	case query.Limit > MaxPageSize:
		return Query{}, "", fmt.Errorf("endpoint page limit exceeds %d", MaxPageSize)
	}
	afterID := ""
	if query.Cursor != "" {
		var err error
		afterID, err = decodeCursor(query.Cursor)
		if err != nil {
			return Query{}, "", err
		}
	}
	return query, afterID, nil
}

// ValidateQuery checks public inventory filters and pagination bounds. The
// cursor remains opaque to callers and is decoded again by the inspector.
func ValidateQuery(query Query) (Query, error) {
	validated, _, err := validateQuery(query)
	return validated, err
}

func validateState(state State) error {
	if state == "" {
		return nil
	}
	switch state {
	case StateOffline, StateActivating, StateReady, StateDraining,
		StateDeactivating, StateFailed, StateUnknown:
		return nil
	default:
		return fmt.Errorf("unsupported endpoint state %q", state)
	}
}

type listCursor struct {
	Version int    `json:"v"`
	AfterID string `json:"after_id"`
}

func encodeCursor(endpointID string) (string, error) {
	raw, err := json.Marshal(listCursor{Version: cursorVersion, AfterID: endpointID})
	if err != nil {
		return "", fmt.Errorf("encode endpoint cursor: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func decodeCursor(value string) (string, error) {
	if len(value) > maxCursorBytes {
		return "", fmt.Errorf("invalid endpoint cursor: cursor is too large")
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return "", fmt.Errorf("invalid endpoint cursor: malformed encoding")
	}
	var cursor listCursor
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cursor); err != nil {
		return "", fmt.Errorf("invalid endpoint cursor: malformed payload")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return "", fmt.Errorf("invalid endpoint cursor: trailing payload")
	}
	if cursor.Version != cursorVersion || cursor.AfterID == "" || len(cursor.AfterID) > 1024 {
		return "", fmt.Errorf("invalid endpoint cursor: unsupported payload")
	}
	return cursor.AfterID, nil
}

var _ Registry = (*Memory)(nil)
var _ Inspector = (*Memory)(nil)
