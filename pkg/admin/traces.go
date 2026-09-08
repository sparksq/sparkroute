package admin

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/sparksq/sparkroute/pkg/savedtrace"
)

// Payload export is a separate privilege from configuration and status access.
const RoleTraceReadAll = "trace_read_all"

func (h *handler) exportSavedTraces(w http.ResponseWriter, r *http.Request) {
	if !allowRead(w, r) || !requireRole(w, r, RoleTraceReadAll) {
		return
	}
	v := r.URL.Query()
	q := savedtrace.Query{}
	fields := map[string]*string{"tenant_id": &q.TenantID, "conversation_id": &q.ConversationID, "response_id": &q.ResponseID, "parent_response_id": &q.ParentResponseID, "session_id": &q.SessionID, "capture_session_id": &q.CaptureSessionID, "protocol": &q.Protocol, "operation": &q.Operation, "requested_model": &q.RequestedModel, "virtual_model": &q.VirtualModel, "provider": &q.Provider, "deployment": &q.Deployment, "outcome": &q.Outcome}
	invalid := func(err error) { writeError(w, http.StatusBadRequest, "invalid_query", err.Error()) }
	for key, values := range v {
		if key != "metadata" && len(values) != 1 {
			invalid(fmt.Errorf("duplicate query parameter %s", key))
			return
		}
		if field, ok := fields[key]; ok {
			*field = values[0]
			continue
		}
		switch key {
		case "started_at_or_after", "started_at_before", "capture_outcome", "metadata", "max_records", "format", "projection", "require_complete":
		default:
			invalid(fmt.Errorf("unknown query parameter %s", key))
			return
		}
	}
	for name, field := range map[string]*time.Time{"started_at_or_after": &q.StartedAtOrAfter, "started_at_before": &q.StartedAtBefore} {
		if v.Get(name) != "" {
			parsed, err := time.Parse(time.RFC3339Nano, v.Get(name))
			if err != nil {
				invalid(fmt.Errorf("%s must be an RFC3339 timestamp", name))
				return
			}
			*field = parsed
		}
	}
	q.CaptureOutcome = savedtrace.CaptureOutcome(v.Get("capture_outcome"))
	q.Metadata = map[string]string{}
	for _, value := range v["metadata"] {
		key, expected, err := savedtrace.ParseMetadataFilter(value)
		if err != nil {
			invalid(err)
			return
		}
		if _, ok := q.Metadata[key]; ok {
			invalid(fmt.Errorf("duplicate metadata key"))
			return
		}
		q.Metadata[key] = expected
	}
	if _, err := savedtrace.PlanQuery(&q); err != nil {
		invalid(err)
		return
	}
	maximum := 100
	if v.Get("max_records") != "" {
		n, err := strconv.Atoi(v.Get("max_records"))
		if err != nil || n < 0 {
			invalid(fmt.Errorf("max_records must be a nonnegative integer"))
			return
		}
		maximum = n
	}
	format := v.Get("format")
	if format == "" {
		format = "jsonl"
	}
	if format != "jsonl" && format != "dataset" {
		invalid(fmt.Errorf("format must be jsonl or dataset"))
		return
	}
	projection := savedtrace.ProjectionCanonical
	if v.Get("projection") != "" {
		parsed, err := savedtrace.ParseDatasetProjection(v.Get("projection"))
		if err != nil {
			invalid(err)
			return
		}
		projection = parsed
	}
	complete := false
	if v.Get("require_complete") != "" {
		parsed, err := strconv.ParseBool(v.Get("require_complete"))
		if err != nil {
			invalid(err)
			return
		}
		complete = parsed
	}
	if format == "jsonl" && (projection != savedtrace.ProjectionCanonical || complete) {
		invalid(fmt.Errorf("projection and require_complete require dataset format"))
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// Stream errors abort the download rather than producing an apparently valid
	// partial dataset or appending an API error to captured payloads.
	output := &traceExportWriter{ResponseWriter: w}
	failed := func(err error) {
		if output.wrote {
			panic(http.ErrAbortHandler)
		}
		w.Header().Del("Content-Disposition")
		w.Header().Del("Content-Type")
		if errors.Is(err, savedtrace.ErrIncompleteDataset) {
			writeError(w, http.StatusConflict, "incomplete_dataset", "Matching traces do not form a complete dataset")
		} else {
			writeError(w, http.StatusInternalServerError, "trace_export_failed", "Could not read the saved trace store")
		}
	}
	if format == "dataset" {
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", `attachment; filename="sparkroute-trace-dataset.zip"`)
		if _, err := savedtrace.ExportDataset(r.Context(), h.options.TraceReader, q, output, savedtrace.DatasetOptions{Projection: projection, MaximumRecords: maximum, RequireComplete: complete}); err != nil {
			failed(err)
		}
	} else {
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("Content-Disposition", `attachment; filename="sparkroute-traces.jsonl"`)
		if _, err := savedtrace.ExportJSONL(r.Context(), h.options.TraceReader, q, output, maximum); err != nil {
			failed(err)
		}
	}
}

type traceExportWriter struct {
	http.ResponseWriter
	wrote bool
}

func (w *traceExportWriter) Write(data []byte) (int, error) {
	w.wrote = true
	return w.ResponseWriter.Write(data)
}
