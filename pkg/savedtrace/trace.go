// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

// Package savedtrace defines explicit, opt-in inference payload capture and
// filtered export independently from the content-free usage ledger and OTLP
// operational telemetry.
package savedtrace

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	SchemaVersion       = 1
	DefaultPageSize     = 10
	MaxPageSize         = 10
	DefaultMaxBodyBytes = 64 << 20
	MaxBodyBytes        = 256 << 20
	MaxMetadataBytes    = 16 << 10
	Base64BodyPrefix    = "base64:"

	maxCursorBytes = 2048
)

var (
	ErrInvalidCursor = errors.New("invalid saved trace cursor")
)

// Payload preserves one caller-facing wire payload. JSON and SSE bodies remain
// direct strings. Binary bodies use Base64BodyPrefix followed by standard
// padded base64 so the existing text storage schema remains lossless.
type Payload struct {
	ContentType string `json:"content_type,omitempty"`
	Body        string `json:"body,omitempty"`
	Truncated   bool   `json:"truncated,omitempty"`
}

// Record is one logical gateway request and its caller-visible result.
type Record struct {
	Version                int               `json:"version"`
	RequestID              string            `json:"request_id"`
	StartedAt              time.Time         `json:"started_at"`
	CompletedAt            time.Time         `json:"completed_at"`
	TenantID               string            `json:"tenant_id,omitempty"`
	PrincipalID            string            `json:"principal_id,omitempty"`
	PrincipalType          string            `json:"principal_type,omitempty"`
	PrincipalSubject       string            `json:"principal_subject,omitempty"`
	ConversationID         string            `json:"conversation_id,omitempty"`
	ResponseID             string            `json:"response_id,omitempty"`
	ParentResponseID       string            `json:"parent_response_id,omitempty"`
	SessionID              string            `json:"session_id,omitempty"`
	CaptureSessionID       string            `json:"capture_session_id,omitempty"`
	Metadata               map[string]string `json:"metadata,omitempty"`
	Protocol               string            `json:"protocol"`
	Operation              string            `json:"operation"`
	Stream                 bool              `json:"stream"`
	RequestedModel         string            `json:"requested_model,omitempty"`
	VirtualModel           string            `json:"virtual_model,omitempty"`
	ResponsePresentedModel string            `json:"response_presented_model,omitempty"`
	ConfigRevision         string            `json:"config_revision,omitempty"`
	FinalProvider          string            `json:"final_provider,omitempty"`
	FinalDeployment        string            `json:"final_deployment,omitempty"`
	FinalUpstreamModel     string            `json:"final_upstream_model,omitempty"`
	AttemptCount           int               `json:"attempt_count"`
	HTTPStatus             int               `json:"http_status,omitempty"`
	Outcome                string            `json:"outcome"`
	FailureClass           string            `json:"failure_class,omitempty"`
	CaptureOutcome         CaptureOutcome    `json:"capture_outcome"`
	Request                Payload           `json:"request"`
	Response               Payload           `json:"response"`
}

func (r Record) Validate() error {
	if r.Version != SchemaVersion {
		return fmt.Errorf("saved trace version must be %d", SchemaVersion)
	}
	if r.RequestID == "" {
		return fmt.Errorf("request_id is required")
	}
	if r.StartedAt.IsZero() || r.CompletedAt.IsZero() {
		return fmt.Errorf("saved trace timestamps are required")
	}
	if r.CompletedAt.Before(r.StartedAt) {
		return fmt.Errorf("saved trace completion precedes start")
	}
	if err := validateString("request_id", r.RequestID, 96); err != nil {
		return err
	}
	for name, value := range map[string]string{
		"tenant_id":                r.TenantID,
		"principal_id":             r.PrincipalID,
		"principal_type":           r.PrincipalType,
		"principal_subject":        r.PrincipalSubject,
		"conversation_id":          r.ConversationID,
		"response_id":              r.ResponseID,
		"parent_response_id":       r.ParentResponseID,
		"session_id":               r.SessionID,
		"capture_session_id":       r.CaptureSessionID,
		"protocol":                 r.Protocol,
		"operation":                r.Operation,
		"requested_model":          r.RequestedModel,
		"virtual_model":            r.VirtualModel,
		"response_presented_model": r.ResponsePresentedModel,
		"config_revision":          r.ConfigRevision,
		"final_provider":           r.FinalProvider,
		"final_deployment":         r.FinalDeployment,
		"final_upstream_model":     r.FinalUpstreamModel,
		"outcome":                  r.Outcome,
		"failure_class":            r.FailureClass,
		"capture_outcome":          string(r.CaptureOutcome),
	} {
		if err := validateString(name, value, 1024); err != nil {
			return err
		}
	}
	if r.Protocol == "" || r.Operation == "" || r.Outcome == "" {
		return fmt.Errorf("protocol, operation, and outcome are required")
	}
	if r.CaptureOutcome != "" &&
		r.CaptureOutcome != CaptureComplete &&
		r.CaptureOutcome != CaptureTruncated &&
		r.CaptureOutcome != CaptureIncomplete {
		return fmt.Errorf("invalid capture_outcome %q", r.CaptureOutcome)
	}
	if r.AttemptCount < 0 {
		return fmt.Errorf("attempt_count must not be negative")
	}
	if r.HTTPStatus < 0 || r.HTTPStatus > 999 {
		return fmt.Errorf("http_status is invalid")
	}
	if err := validateMetadata(r.Metadata); err != nil {
		return err
	}
	if err := validatePayload("request", r.Request); err != nil {
		return err
	}
	return validatePayload("response", r.Response)
}

func validatePayload(name string, payload Payload) error {
	if err := validateString(name+" content_type", payload.ContentType, 1024); err != nil {
		return err
	}
	if payload.ContentType == "application/vnd.amazon.eventstream" {
		if !strings.HasPrefix(payload.Body, Base64BodyPrefix) {
			return fmt.Errorf(
				"%s AWS event-stream body must use base64 encoding",
				name,
			)
		}
		encodedBody := strings.TrimPrefix(
			payload.Body,
			Base64BodyPrefix,
		)
		if len(encodedBody) >
			base64.StdEncoding.EncodedLen(MaxBodyBytes) {
			return fmt.Errorf(
				"%s body exceeds %d encoded bytes",
				name,
				base64.StdEncoding.EncodedLen(MaxBodyBytes),
			)
		}
		decoder := base64.NewDecoder(
			base64.StdEncoding,
			strings.NewReader(encodedBody),
		)
		decodedBytes, err := io.Copy(
			io.Discard,
			io.LimitReader(decoder, MaxBodyBytes+1),
		)
		if err != nil {
			return fmt.Errorf("%s body has invalid base64 encoding", name)
		}
		if decodedBytes > MaxBodyBytes {
			return fmt.Errorf(
				"%s body exceeds %d decoded bytes",
				name,
				MaxBodyBytes,
			)
		}
		return nil
	}
	if len(payload.Body) > MaxBodyBytes {
		return fmt.Errorf("%s body exceeds %d bytes", name, MaxBodyBytes)
	}
	if !utf8.ValidString(payload.Body) {
		return fmt.Errorf("%s body is not valid UTF-8", name)
	}
	return nil
}

func validateMetadata(metadata map[string]string) error {
	if len(metadata) > 64 {
		return fmt.Errorf("too many saved trace metadata fields")
	}
	total := 0
	for key, value := range metadata {
		if key == "" {
			return fmt.Errorf("saved trace metadata key is empty")
		}
		if err := validateString("metadata key", key, 128); err != nil {
			return err
		}
		if err := validateString("metadata value", value, 1024); err != nil {
			return err
		}
		total += len(key) + len(value)
		if total > MaxMetadataBytes {
			return fmt.Errorf("saved trace metadata exceeds %d bytes", MaxMetadataBytes)
		}
	}
	return nil
}

func validateString(name, value string, maximum int) error {
	if len(value) > maximum {
		return fmt.Errorf("%s exceeds %d bytes", name, maximum)
	}
	if !utf8.ValidString(value) {
		return fmt.Errorf("%s is not valid UTF-8", name)
	}
	return nil
}

// Query applies conjunctive exact-match filters. StartedAtOrAfter is inclusive
// and StartedAtBefore is exclusive.
type Query struct {
	StartedAtOrAfter time.Time
	StartedAtBefore  time.Time
	TenantID         string
	ConversationID   string
	ResponseID       string
	ParentResponseID string
	SessionID        string
	CaptureSessionID string
	Protocol         string
	Operation        string
	RequestedModel   string
	VirtualModel     string
	Provider         string
	Deployment       string
	Outcome          string
	CaptureOutcome   CaptureOutcome
	Metadata         map[string]string
	Limit            int
	Cursor           string
}

type Page struct {
	Records    []Record `json:"records"`
	NextCursor string   `json:"next_cursor,omitempty"`
}

type Reader interface {
	List(ctx context.Context, query Query) (Page, error)
}

// RecordVisitor is an optional store optimization for visiting an entire
// filtered result without repeatedly rebuilding seek pages. Implementations
// must preserve List ordering and filtering. The boolean result reports that a
// maximum omitted one or more additional matching records.
type RecordVisitor interface {
	VisitRecords(context.Context, Query, int, func(Record) error) (bool, error)
}

// RecordSnapshot is one replayable, ordered view of a filtered result. Visit
// may be called more than once and must return the same records in List order.
// Limited reports whether the requested maximum omitted additional matches.
// Close releases any temporary resources and must be idempotent. Visits are
// sequential; concurrent Visit and Close calls are not supported.
type RecordSnapshot interface {
	Visit(context.Context, func(Record) error) error
	Limited() bool
	Close() error
}

// RecordSnapshotReader optionally lets exporters materialize an ordered view
// once and replay it for completeness preflight plus output. A maximum of zero
// means no record-count limit.
type RecordSnapshotReader interface {
	OpenRecordSnapshot(context.Context, Query, int) (RecordSnapshot, error)
}

type Store interface {
	Reader
	Append(ctx context.Context, record Record) error
	Close() error
}

// BatchStore persists a group of records with one storage commit. Implementers
// must preserve each record's validation, ordering, and all-or-observable-error
// contract. AsyncRecorder detects this optional optimization automatically.
type BatchStore interface {
	Store
	AppendTraceBatch(ctx context.Context, records []Record) error
}

type Recorder interface {
	Record(record Record)
}

type DiscardRecorder struct{}

func (DiscardRecorder) Record(Record) {}

type QueryPlan struct {
	Limit           int
	CursorStartedAt time.Time
	CursorRequestID string
}

func PlanQuery(query *Query) (QueryPlan, error) {
	if query == nil {
		return QueryPlan{}, fmt.Errorf("saved trace query is required")
	}
	if !query.StartedAtOrAfter.IsZero() {
		query.StartedAtOrAfter = query.StartedAtOrAfter.UTC()
	}
	if !query.StartedAtBefore.IsZero() {
		query.StartedAtBefore = query.StartedAtBefore.UTC()
	}
	if !query.StartedAtOrAfter.IsZero() &&
		!query.StartedAtBefore.IsZero() &&
		!query.StartedAtOrAfter.Before(query.StartedAtBefore) {
		return QueryPlan{}, fmt.Errorf("saved trace start range is invalid")
	}
	for name, value := range map[string]string{
		"tenant_id":          query.TenantID,
		"conversation_id":    query.ConversationID,
		"response_id":        query.ResponseID,
		"parent_response_id": query.ParentResponseID,
		"session_id":         query.SessionID,
		"capture_session_id": query.CaptureSessionID,
		"protocol":           query.Protocol,
		"operation":          query.Operation,
		"requested_model":    query.RequestedModel,
		"virtual_model":      query.VirtualModel,
		"provider":           query.Provider,
		"deployment":         query.Deployment,
		"outcome":            query.Outcome,
		"capture_outcome":    string(query.CaptureOutcome),
	} {
		if err := validateString(name, value, 1024); err != nil {
			return QueryPlan{}, err
		}
	}
	if err := validateMetadata(query.Metadata); err != nil {
		return QueryPlan{}, err
	}
	if query.CaptureOutcome != "" &&
		query.CaptureOutcome != CaptureComplete &&
		query.CaptureOutcome != CaptureTruncated &&
		query.CaptureOutcome != CaptureIncomplete {
		return QueryPlan{}, fmt.Errorf("invalid capture_outcome %q", query.CaptureOutcome)
	}
	switch {
	case query.Limit < 0:
		return QueryPlan{}, fmt.Errorf("saved trace page limit must not be negative")
	case query.Limit == 0:
		query.Limit = DefaultPageSize
	case query.Limit > MaxPageSize:
		return QueryPlan{}, fmt.Errorf("saved trace page limit exceeds %d", MaxPageSize)
	}
	plan := QueryPlan{Limit: query.Limit}
	if query.Cursor == "" {
		return plan, nil
	}
	cursor, err := decodeCursor(query.Cursor)
	if err != nil {
		return QueryPlan{}, err
	}
	plan.CursorStartedAt = time.Unix(0, cursor.StartedNS).UTC()
	plan.CursorRequestID = cursor.RequestID
	return plan, nil
}

func NextCursor(record Record) (string, error) {
	if record.RequestID == "" || record.StartedAt.IsZero() {
		return "", fmt.Errorf("saved trace cursor requires request_id and started_at")
	}
	raw, err := json.Marshal(cursor{
		Version:   SchemaVersion,
		StartedNS: record.StartedAt.UnixNano(),
		RequestID: record.RequestID,
	})
	if err != nil {
		return "", fmt.Errorf("encode saved trace cursor: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

type cursor struct {
	Version   int    `json:"v"`
	StartedNS int64  `json:"t"`
	RequestID string `json:"r"`
}

func decodeCursor(value string) (cursor, error) {
	if len(value) > maxCursorBytes {
		return cursor{}, fmt.Errorf("%w: cursor is too large", ErrInvalidCursor)
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return cursor{}, fmt.Errorf("%w: malformed encoding", ErrInvalidCursor)
	}
	var result cursor
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return cursor{}, fmt.Errorf("%w: malformed payload", ErrInvalidCursor)
	}
	if err := requireEOF(decoder); err != nil ||
		result.Version != SchemaVersion ||
		result.StartedNS <= 0 ||
		result.RequestID == "" {
		return cursor{}, fmt.Errorf("%w: invalid payload", ErrInvalidCursor)
	}
	if err := validateString("cursor request_id", result.RequestID, 96); err != nil {
		return cursor{}, fmt.Errorf("%w: invalid request ID", ErrInvalidCursor)
	}
	return result, nil
}

func requireEOF(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return fmt.Errorf("trailing JSON data")
	}
	return err
}

func Matches(record Record, query Query, plan QueryPlan) bool {
	if !query.StartedAtOrAfter.IsZero() && record.StartedAt.Before(query.StartedAtOrAfter) {
		return false
	}
	if !query.StartedAtBefore.IsZero() && !record.StartedAt.Before(query.StartedAtBefore) {
		return false
	}
	if plan.CursorRequestID != "" {
		if record.StartedAt.After(plan.CursorStartedAt) ||
			record.StartedAt.Equal(plan.CursorStartedAt) &&
				record.RequestID >= plan.CursorRequestID {
			return false
		}
	}
	for _, values := range [][2]string{
		{query.TenantID, record.TenantID},
		{query.ConversationID, record.ConversationID},
		{query.ResponseID, record.ResponseID},
		{query.ParentResponseID, record.ParentResponseID},
		{query.SessionID, record.SessionID},
		{query.CaptureSessionID, record.CaptureSessionID},
		{query.Protocol, record.Protocol},
		{query.Operation, record.Operation},
		{query.RequestedModel, record.RequestedModel},
		{query.VirtualModel, record.VirtualModel},
		{query.Provider, record.FinalProvider},
		{query.Deployment, record.FinalDeployment},
		{query.Outcome, record.Outcome},
		{string(query.CaptureOutcome), string(record.EffectiveCaptureOutcome())},
	} {
		if values[0] != "" && values[1] != values[0] {
			return false
		}
	}
	for key, expected := range query.Metadata {
		if record.Metadata[key] != expected {
			return false
		}
	}
	return true
}

// ParseMetadataFilter parses a repeated CLI/API key=value filter.
func ParseMetadataFilter(value string) (string, string, error) {
	key, expected, found := strings.Cut(value, "=")
	key = strings.TrimSpace(key)
	if !found || key == "" {
		return "", "", fmt.Errorf("metadata filter %q must be key=value", value)
	}
	if err := validateString("metadata key", key, 128); err != nil {
		return "", "", err
	}
	if err := validateString("metadata value", expected, 1024); err != nil {
		return "", "", err
	}
	return key, expected, nil
}

// ExportJSONL streams every matching record without accumulating the export in
// memory. A maximum of zero means no record-count limit.
func ExportJSONL(
	ctx context.Context,
	store Reader,
	query Query,
	output io.Writer,
	maximum int,
) (int, error) {
	if store == nil || output == nil {
		return 0, fmt.Errorf("saved trace store and output are required")
	}
	if maximum < 0 {
		return 0, fmt.Errorf("saved trace export limit must not be negative")
	}
	if snapshotReader, ok := store.(RecordSnapshotReader); ok {
		snapshot, err := snapshotReader.OpenRecordSnapshot(ctx, query, maximum)
		if err != nil {
			return 0, err
		}
		encoder := json.NewEncoder(output)
		encoder.SetEscapeHTML(false)
		exported := 0
		visitErr := snapshot.Visit(ctx, func(record Record) error {
			if err := encoder.Encode(record); err != nil {
				return fmt.Errorf("write saved trace export: %w", err)
			}
			exported++
			return nil
		})
		return exported, errors.Join(visitErr, snapshot.Close())
	}
	if visitor, ok := store.(RecordVisitor); ok {
		encoder := json.NewEncoder(output)
		encoder.SetEscapeHTML(false)
		exported := 0
		_, err := visitor.VisitRecords(ctx, query, maximum, func(record Record) error {
			if err := encoder.Encode(record); err != nil {
				return fmt.Errorf("write saved trace export: %w", err)
			}
			exported++
			return nil
		})
		return exported, err
	}
	// Payload records can each be large. Export one at a time so the paging
	// layer never aggregates multiple maximum-sized request/response pairs.
	query.Limit = 1
	query.Cursor = ""
	encoder := json.NewEncoder(output)
	encoder.SetEscapeHTML(false)
	exported := 0
	for {
		if err := ctx.Err(); err != nil {
			return exported, err
		}
		page, err := store.List(ctx, query)
		if err != nil {
			return exported, err
		}
		for _, record := range page.Records {
			if maximum > 0 && exported >= maximum {
				return exported, nil
			}
			if err := encoder.Encode(record); err != nil {
				return exported, fmt.Errorf("write saved trace export: %w", err)
			}
			exported++
		}
		if page.NextCursor == "" || maximum > 0 && exported >= maximum {
			return exported, nil
		}
		query.Cursor = page.NextCursor
	}
}
