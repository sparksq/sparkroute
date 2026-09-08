// Package privacy defines the optional privacy-provider contract used by the
// gateway. The standalone implementation lives in pkg/pii; downstream
// distributions may supply additional detectors and persistence backends.
package privacy

import (
	"context"
	"time"
)

type Entity string

const (
	EntityEmail      Entity = "email"
	EntityPhone      Entity = "phone"
	EntitySSN        Entity = "ssn"
	EntityCreditCard Entity = "credit_card"
	EntityIPv4       Entity = "ipv4"
	EntityPerson     Entity = "person"
	EntityAddress    Entity = "address"
)

// Scope partitions mappings using authenticated identity and trusted
// attribution. Conversation is empty for request-local sessions.
type Scope struct {
	Tenant       string
	Principal    string
	Conversation string
}

type StreamOptions struct {
	MaximumPendingBytes int
}

// StreamRedactor incrementally redacts novel sensitive content while retaining
// only a bounded suffix that may be completed by a later stream fragment.
type StreamRedactor interface {
	Transform(context.Context, string, bool) (string, error)
	Pending() string
	Abort(string) string
}

// Session is one isolated substitution namespace. Implementations must never
// share request-local mappings between sessions.
type Session interface {
	Substitute(context.Context, string) (string, error)
	Redact(context.Context, string) (string, error)
	Restore(string) string
	MappingCount() int
	SupportsStreaming() bool
	NewStreamRedactor(StreamOptions) (StreamRedactor, error)
}

// Provider creates request-local or authenticated conversation-stable
// sessions. It is the only PII implementation seam consumed by the OSS data
// plane; pkg/pii supplies the standalone implementation.
type Provider interface {
	NewSession(context.Context, []Entity, Scope) (Session, error)
	Supports(Entity) bool
	RecordInspection()
	RecordFailure()
	PrivacyStatus(context.Context) Status
}

const (
	StatusDisabled    = "disabled"
	StatusHealthy     = "healthy"
	StatusUnavailable = "unavailable"
)

// MappingStoreStatus is a content-free aggregate projection. It deliberately
// excludes partition values, tokens, ciphertext, key identifiers, and errors.
type MappingStoreStatus struct {
	Backend             string `json:"backend"`
	LiveMappings        int64  `json:"live_mappings"`
	LiveConversations   int64  `json:"live_conversations"`
	LiveOriginalBytes   int64  `json:"live_original_bytes"`
	ExpiredPendingPrune int64  `json:"expired_pending_prune"`
}

// Status is the safe privacy projection returned by the admin status API.
type Status struct {
	ObservedAt           time.Time `json:"observed_at"`
	State                string    `json:"state"`
	Enabled              bool      `json:"enabled"`
	ConfiguredModels     int       `json:"configured_models"`
	ConversationModels   int       `json:"conversation_models"`
	MediaRequiredModels  int       `json:"media_required_models"`
	FileInspectionModels int       `json:"file_inspection_models"`
	DetectorEntities     []Entity  `json:"detector_entities"`
	Backend              string    `json:"backend"`
	LiveMappings         int64     `json:"live_mappings"`
	LiveConversations    int64     `json:"live_conversations"`
	LiveOriginalBytes    int64     `json:"live_original_bytes"`
	ExpiredPendingPrune  int64     `json:"expired_pending_prune"`
	MaximumMappings      int       `json:"maximum_mappings"`
	MaximumOriginalBytes int       `json:"maximum_original_bytes"`
	MaximumConversations int       `json:"maximum_conversations"`
	SlidingTTLSeconds    int64     `json:"sliding_ttl_seconds"`
	AbsoluteTTLSeconds   int64     `json:"absolute_ttl_seconds"`
	Inspections          uint64    `json:"inspections"`
	Failures             uint64    `json:"failures"`
}

type StatusSource interface {
	PrivacyStatus(context.Context) Status
}

// DisabledStatus returns the canonical content-free projection for a build or
// runtime with no privacy provider.
func DisabledStatus() Status {
	return Status{
		ObservedAt:       time.Now().UTC(),
		State:            StatusDisabled,
		Backend:          "disabled",
		DetectorEntities: []Entity{},
	}
}
