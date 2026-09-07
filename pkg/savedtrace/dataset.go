package savedtrace

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

const (
	DatasetManifestVersion           = 1
	maximumDatasetManifestValues     = 65_536
	maximumDatasetManifestValueBytes = 64 << 20
)

type DatasetProjection string

const (
	ProjectionCanonical DatasetProjection = "canonical"
	ProjectionDeepSpec  DatasetProjection = "deepspec"
	ProjectionMLflow    DatasetProjection = "mlflow"
)

var ErrIncompleteDataset = errors.New("saved trace dataset is not complete")

func ParseDatasetProjection(value string) (DatasetProjection, error) {
	switch projection := DatasetProjection(strings.ToLower(strings.TrimSpace(value))); projection {
	case ProjectionCanonical, ProjectionDeepSpec, ProjectionMLflow:
		return projection, nil
	default:
		return "", fmt.Errorf("dataset projection must be canonical, deepspec, or mlflow")
	}
}

type DatasetOptions struct {
	Projection      DatasetProjection
	MaximumRecords  int
	RequireComplete bool
	CreatedAt       time.Time
}

type DatasetFilter struct {
	StartedAtOrAfter time.Time         `json:"started_at_or_after,omitempty"`
	StartedAtBefore  time.Time         `json:"started_at_before,omitempty"`
	TenantID         string            `json:"tenant_id,omitempty"`
	ConversationID   string            `json:"conversation_id,omitempty"`
	ResponseID       string            `json:"response_id,omitempty"`
	ParentResponseID string            `json:"parent_response_id,omitempty"`
	SessionID        string            `json:"session_id,omitempty"`
	Protocol         string            `json:"protocol,omitempty"`
	Operation        string            `json:"operation,omitempty"`
	RequestedModel   string            `json:"requested_model,omitempty"`
	VirtualModel     string            `json:"virtual_model,omitempty"`
	Provider         string            `json:"provider,omitempty"`
	Deployment       string            `json:"deployment,omitempty"`
	Outcome          string            `json:"outcome,omitempty"`
	CaptureOutcome   CaptureOutcome    `json:"capture_outcome,omitempty"`
	Metadata         map[string]string `json:"metadata,omitempty"`
}

type DatasetManifest struct {
	Version             int               `json:"version"`
	CreatedAt           time.Time         `json:"created_at"`
	Projection          DatasetProjection `json:"projection"`
	DataFile            string            `json:"data_file"`
	Filter              DatasetFilter     `json:"filter"`
	MatchingRecords     int               `json:"matching_records"`
	DatasetRecords      int               `json:"dataset_records"`
	RecordLimitApplied  bool              `json:"record_limit_applied"`
	FirstRecordAt       *time.Time        `json:"first_record_at,omitempty"`
	LastRecordAt        *time.Time        `json:"last_record_at,omitempty"`
	ConfigRevisions     []string          `json:"config_revisions,omitempty"`
	CaptureComplete     bool              `json:"capture_complete"`
	CompletenessReasons []string          `json:"completeness_reasons,omitempty"`
	CaptureSessions     []CaptureSession  `json:"capture_sessions,omitempty"`
	CaptureOutcomes     map[string]int    `json:"capture_outcomes"`
	ExcludedByReason    map[string]int    `json:"excluded_by_reason,omitempty"`
	ContentPolicyDigest string            `json:"content_policy_digest"`
}

type datasetScan struct {
	manifest          DatasetManifest
	captureSessionIDs map[string]struct{}
}

type recordTraversal struct {
	reader   Reader
	query    Query
	maximum  int
	snapshot RecordSnapshot
}

func openRecordTraversal(
	ctx context.Context,
	reader Reader,
	query Query,
	maximum int,
) (*recordTraversal, error) {
	traversal := &recordTraversal{reader: reader, query: query, maximum: maximum}
	if snapshotReader, ok := reader.(RecordSnapshotReader); ok {
		snapshot, err := snapshotReader.OpenRecordSnapshot(ctx, query, maximum)
		if err != nil {
			return nil, err
		}
		traversal.snapshot = snapshot
	}
	return traversal, nil
}

func (t *recordTraversal) Visit(
	ctx context.Context,
	visit func(Record) error,
) (bool, error) {
	if t.snapshot != nil {
		return t.snapshot.Limited(), t.snapshot.Visit(ctx, visit)
	}
	return visitRecords(ctx, t.reader, t.query, t.maximum, visit)
}

func (t *recordTraversal) Close() error {
	if t == nil || t.snapshot == nil {
		return nil
	}
	return t.snapshot.Close()
}

// ExportDataset writes a self-describing zip archive. It performs a bounded
// first pass so require-complete can fail before any response bytes are sent.
func ExportDataset(
	ctx context.Context,
	reader Reader,
	query Query,
	output io.Writer,
	options DatasetOptions,
) (manifest DatasetManifest, resultErr error) {
	if reader == nil || output == nil {
		return DatasetManifest{}, fmt.Errorf("saved trace store and output are required")
	}
	if options.MaximumRecords < 0 {
		return DatasetManifest{}, fmt.Errorf("saved trace export limit must not be negative")
	}
	if options.Projection == "" {
		options.Projection = ProjectionCanonical
	}
	if _, err := ParseDatasetProjection(string(options.Projection)); err != nil {
		return DatasetManifest{}, err
	}
	createdAt := options.CreatedAt.UTC()
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	if query.StartedAtBefore.IsZero() {
		query.StartedAtBefore = createdAt
	}
	if _, err := PlanQuery(&query); err != nil {
		return DatasetManifest{}, err
	}
	traversal, err := openRecordTraversal(
		ctx,
		reader,
		query,
		options.MaximumRecords,
	)
	if err != nil {
		return DatasetManifest{}, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, traversal.Close())
	}()

	scan, err := scanDataset(ctx, traversal, query, options)
	if err != nil {
		return DatasetManifest{}, err
	}
	scan.manifest.CreatedAt = createdAt
	if err := attachCaptureSessions(ctx, reader, query, &scan); err != nil {
		return DatasetManifest{}, err
	}
	if options.RequireComplete && !scan.manifest.CaptureComplete {
		return scan.manifest, fmt.Errorf("%w: %s", ErrIncompleteDataset, strings.Join(scan.manifest.CompletenessReasons, "; "))
	}

	archive := zip.NewWriter(output)
	manifestFile, err := archive.Create("manifest.json")
	if err != nil {
		return scan.manifest, err
	}
	encoder := json.NewEncoder(manifestFile)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(scan.manifest); err != nil {
		return scan.manifest, err
	}
	dataFile, err := archive.Create(scan.manifest.DataFile)
	if err != nil {
		return scan.manifest, err
	}
	if err := writeDatasetRows(ctx, traversal, dataFile, options); err != nil {
		return scan.manifest, err
	}
	if err := archive.Close(); err != nil {
		return scan.manifest, fmt.Errorf("close saved trace dataset: %w", err)
	}
	return scan.manifest, nil
}

func scanDataset(
	ctx context.Context,
	traversal *recordTraversal,
	query Query,
	options DatasetOptions,
) (datasetScan, error) {
	manifest := DatasetManifest{
		Version: DatasetManifestVersion, Projection: options.Projection,
		DataFile: datasetFileName(options.Projection), Filter: datasetFilter(query),
		CaptureOutcomes: make(map[string]int), ExcludedByReason: make(map[string]int),
		ContentPolicyDigest: contentPolicyDigest(),
	}
	revisions := make(map[string]struct{})
	sessionIDs := make(map[string]struct{})
	manifestValueCount := 0
	manifestValueBytes := 0
	limited, err := traversal.Visit(ctx, func(record Record) error {
		manifest.MatchingRecords++
		capture := record.EffectiveCaptureOutcome()
		manifest.CaptureOutcomes[string(capture)]++
		if record.CaptureSessionID == "" {
			appendReason(&manifest.CompletenessReasons, "record missing capture session attribution")
		} else {
			if err := addDatasetManifestValue(
				sessionIDs,
				record.CaptureSessionID,
				&manifestValueCount,
				&manifestValueBytes,
			); err != nil {
				return err
			}
		}
		if capture != CaptureComplete {
			appendReason(&manifest.CompletenessReasons, "one or more records have incomplete payload capture")
		}
		if record.ConfigRevision != "" {
			if err := addDatasetManifestValue(
				revisions,
				record.ConfigRevision,
				&manifestValueCount,
				&manifestValueBytes,
			); err != nil {
				return err
			}
		}
		started := record.StartedAt.UTC()
		if manifest.FirstRecordAt == nil || started.Before(*manifest.FirstRecordAt) {
			value := started
			manifest.FirstRecordAt = &value
		}
		if manifest.LastRecordAt == nil || started.After(*manifest.LastRecordAt) {
			value := started
			manifest.LastRecordAt = &value
		}
		if _, reason, err := projectRecord(record, options.Projection); err != nil {
			return err
		} else if reason != "" {
			manifest.ExcludedByReason[reason]++
			appendReason(&manifest.CompletenessReasons, "one or more matching records are not representable in the selected projection")
		} else {
			manifest.DatasetRecords++
		}
		return nil
	})
	if err != nil {
		return datasetScan{}, err
	}
	manifest.RecordLimitApplied = limited
	if limited {
		appendReason(&manifest.CompletenessReasons, "maximum record limit omitted additional matches")
	}
	if manifest.MatchingRecords == 0 {
		appendReason(&manifest.CompletenessReasons, "no matching records")
	}
	for revision := range revisions {
		manifest.ConfigRevisions = append(manifest.ConfigRevisions, revision)
	}
	sort.Strings(manifest.ConfigRevisions)
	if len(manifest.ExcludedByReason) == 0 {
		manifest.ExcludedByReason = nil
	}
	return datasetScan{manifest: manifest, captureSessionIDs: sessionIDs}, nil
}

func addDatasetManifestValue(
	values map[string]struct{},
	value string,
	count *int,
	byteCount *int,
) error {
	if _, exists := values[value]; exists {
		return nil
	}
	if *count >= maximumDatasetManifestValues ||
		*byteCount > maximumDatasetManifestValueBytes ||
		len(value) > maximumDatasetManifestValueBytes-*byteCount {
		return fmt.Errorf(
			"saved trace dataset manifest exceeds its %d-value or %d-byte metadata limit",
			maximumDatasetManifestValues,
			maximumDatasetManifestValueBytes,
		)
	}
	values[value] = struct{}{}
	(*count)++
	*byteCount += len(value)
	return nil
}

func attachCaptureSessions(ctx context.Context, reader Reader, query Query, scan *datasetScan) error {
	store, ok := reader.(CaptureSessionStore)
	if !ok {
		appendReason(&scan.manifest.CompletenessReasons, "trace store does not expose capture sessions")
		scan.manifest.CaptureComplete = false
		return nil
	}
	start := query.StartedAtOrAfter
	if start.IsZero() && scan.manifest.FirstRecordAt != nil {
		start = *scan.manifest.FirstRecordAt
	}
	sessions, err := store.ListCaptureSessions(ctx, CaptureSessionQuery{
		StartedBefore: query.StartedAtBefore, UpdatedAfter: start,
	})
	if err != nil {
		return err
	}
	scan.manifest.CaptureSessions = sessions
	found := make(map[string]struct{}, len(sessions))
	for _, session := range sessions {
		found[session.ID] = struct{}{}
		if session.Lost != 0 || session.Invalid != 0 || session.QueueFull != 0 || session.Closed != 0 || session.StoreFailures != 0 {
			appendReason(&scan.manifest.CompletenessReasons, "capture session reports lost records")
		}
		if session.Pending != 0 || session.Accepted != session.Persisted {
			appendReason(&scan.manifest.CompletenessReasons, "capture session is not fully persisted")
		}
		coveredThroughEnd := session.CompletedAt != nil && !session.CompletedAt.Before(query.StartedAtBefore) || !session.UpdatedAt.Before(query.StartedAtBefore)
		if !coveredThroughEnd {
			appendReason(&scan.manifest.CompletenessReasons, "capture session does not prove coverage through the export end")
		}
	}
	for identifier := range scan.captureSessionIDs {
		if _, ok := found[identifier]; !ok {
			appendReason(&scan.manifest.CompletenessReasons, "referenced capture session is missing")
		}
	}
	if len(sessions) == 0 {
		appendReason(&scan.manifest.CompletenessReasons, "no capture sessions overlap the export window")
	}
	scan.manifest.CaptureComplete = len(scan.manifest.CompletenessReasons) == 0
	return nil
}

func writeDatasetRows(
	ctx context.Context,
	traversal *recordTraversal,
	output io.Writer,
	options DatasetOptions,
) error {
	encoder := json.NewEncoder(output)
	encoder.SetEscapeHTML(false)
	_, err := traversal.Visit(ctx, func(record Record) error {
		row, reason, err := projectRecord(record, options.Projection)
		if err != nil {
			return err
		}
		if reason != "" {
			return nil
		}
		if err := encoder.Encode(row); err != nil {
			return fmt.Errorf("write saved trace dataset row: %w", err)
		}
		return nil
	})
	return err
}

func visitRecords(ctx context.Context, reader Reader, query Query, maximum int, visit func(Record) error) (bool, error) {
	if visitor, ok := reader.(RecordVisitor); ok {
		return visitor.VisitRecords(ctx, query, maximum, visit)
	}
	query.Limit, query.Cursor = 1, ""
	count := 0
	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		page, err := reader.List(ctx, query)
		if err != nil {
			return false, err
		}
		for _, record := range page.Records {
			if maximum > 0 && count >= maximum {
				return true, nil
			}
			if err := visit(record); err != nil {
				return false, err
			}
			count++
		}
		if page.NextCursor == "" {
			return false, nil
		}
		query.Cursor = page.NextCursor
	}
}

func projectRecord(record Record, projection DatasetProjection) (any, string, error) {
	switch projection {
	case ProjectionCanonical:
		return record, "", nil
	case ProjectionMLflow:
		return projectMLflow(record)
	case ProjectionDeepSpec:
		return projectDeepSpec(record)
	default:
		return nil, "", fmt.Errorf("unsupported dataset projection %q", projection)
	}
}

func projectMLflow(record Record) (any, string, error) {
	if record.EffectiveCaptureOutcome() != CaptureComplete {
		return nil, "capture_incomplete", nil
	}
	var request, response any
	if err := json.Unmarshal([]byte(record.Request.Body), &request); err != nil {
		return nil, "request_not_json", nil
	}
	if err := json.Unmarshal([]byte(record.Response.Body), &response); err != nil {
		return nil, "response_not_json", nil
	}
	return map[string]any{
		"inputs": request, "outputs": response,
		"expectations": map[string]any{"provider": record.FinalProvider, "deployment": record.FinalDeployment, "model": record.FinalUpstreamModel},
		"metadata":     projectionMetadata(record),
	}, "", nil
}

func projectDeepSpec(record Record) (any, string, error) {
	if record.EffectiveCaptureOutcome() != CaptureComplete {
		return nil, "capture_incomplete", nil
	}
	if record.Outcome != "success" || record.HTTPStatus < 200 || record.HTTPStatus >= 300 {
		return nil, "unsuccessful_request", nil
	}
	if record.Stream {
		return nil, "streaming_not_supported", nil
	}
	var request, response map[string]any
	if err := json.Unmarshal([]byte(record.Request.Body), &request); err != nil {
		return nil, "request_not_json", nil
	}
	if err := json.Unmarshal([]byte(record.Response.Body), &response); err != nil {
		return nil, "response_not_json", nil
	}
	conversations := make([]any, 0)
	if messages, ok := request["messages"].([]any); ok {
		conversations = append(conversations, messages...)
		choices, ok := response["choices"].([]any)
		if !ok || len(choices) == 0 {
			return nil, "missing_output", nil
		}
		choice, ok := choices[0].(map[string]any)
		if !ok || choice["message"] == nil {
			return nil, "missing_output", nil
		}
		conversations = append(conversations, choice["message"])
	} else if input, exists := request["input"]; exists {
		switch value := input.(type) {
		case string:
			conversations = append(conversations, map[string]any{"role": "user", "content": value})
		case []any:
			conversations = append(conversations, value...)
		default:
			return nil, "unsupported_input", nil
		}
		output, ok := response["output"].([]any)
		if !ok || len(output) == 0 {
			return nil, "missing_output", nil
		}
		conversations = append(conversations, output...)
	} else {
		return nil, "unsupported_operation", nil
	}
	return map[string]any{
		"id": record.RequestID, "conversations": conversations,
		"_router": map[string]any{
			"target_identity": map[string]any{"provider": record.FinalProvider, "deployment": record.FinalDeployment, "model": record.FinalUpstreamModel},
			"metadata":        projectionMetadata(record),
		},
	}, "", nil
}

func projectionMetadata(record Record) map[string]any {
	return map[string]any{
		"request_id": record.RequestID, "tenant_id": record.TenantID,
		"conversation_id": record.ConversationID, "response_id": record.ResponseID,
		"parent_response_id": record.ParentResponseID, "session_id": record.SessionID,
		"virtual_model": record.VirtualModel, "config_revision": record.ConfigRevision,
		"started_at": record.StartedAt,
	}
}

func datasetFilter(query Query) DatasetFilter {
	return DatasetFilter{
		StartedAtOrAfter: query.StartedAtOrAfter, StartedAtBefore: query.StartedAtBefore,
		TenantID: query.TenantID, ConversationID: query.ConversationID,
		ResponseID: query.ResponseID, ParentResponseID: query.ParentResponseID,
		SessionID: query.SessionID, Protocol: query.Protocol, Operation: query.Operation,
		RequestedModel: query.RequestedModel, VirtualModel: query.VirtualModel,
		Provider: query.Provider, Deployment: query.Deployment, Outcome: query.Outcome,
		CaptureOutcome: query.CaptureOutcome, Metadata: cloneMap(query.Metadata),
	}
}

func datasetFileName(projection DatasetProjection) string {
	if projection == ProjectionCanonical {
		return "records.jsonl"
	}
	return "dataset.jsonl"
}

func contentPolicyDigest() string {
	digest := sha256.Sum256([]byte("sparkroute:saved-trace-export:raw-captured-content:v1"))
	return "sha256:" + hex.EncodeToString(digest[:])
}

func appendReason(reasons *[]string, reason string) {
	for _, existing := range *reasons {
		if existing == reason {
			return
		}
	}
	*reasons = append(*reasons, reason)
}
