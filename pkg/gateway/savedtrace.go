package gateway

import (
	"encoding/base64"
	"encoding/json"
	"mime"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/sparksq/sparkroute/pkg/ledger"
	"github.com/sparksq/sparkroute/pkg/savedtrace"
)

type savedTraceResponseWriter struct {
	http.ResponseWriter
	maxBytes   int64
	body       []byte
	truncated  bool
	overridden bool
}

func newSavedTraceResponseWriter(
	writer http.ResponseWriter,
	recorder savedtrace.Recorder,
	maxBytes int64,
) *savedTraceResponseWriter {
	if recorder == nil {
		return nil
	}
	return &savedTraceResponseWriter{
		ResponseWriter: writer,
		maxBytes:       effectiveMaxSavedTraceBytes(maxBytes),
	}
}

func effectiveMaxSavedTraceBytes(value int64) int64 {
	if value <= 0 {
		return savedtrace.DefaultMaxBodyBytes
	}
	if value > savedtrace.MaxBodyBytes {
		return savedtrace.MaxBodyBytes
	}
	return value
}

func (w *savedTraceResponseWriter) Write(payload []byte) (int, error) {
	written, err := w.ResponseWriter.Write(payload)
	w.capture(payload[:written])
	return written, err
}

func (w *savedTraceResponseWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *savedTraceResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *savedTraceResponseWriter) capture(payload []byte) {
	if w.overridden {
		return
	}
	remaining := w.maxBytes - int64(len(w.body))
	if remaining <= 0 {
		if len(payload) != 0 {
			w.truncated = true
		}
		return
	}
	if int64(len(payload)) > remaining {
		w.body = append(w.body, payload[:remaining]...)
		w.truncated = true
		return
	}
	w.body = append(w.body, payload...)
}

// overrideCapture records a privacy-transformed representation while the
// wrapped writer continues sending the caller-visible representation.
func (w *savedTraceResponseWriter) overrideCapture(payload []byte) {
	if w == nil {
		return
	}
	w.body = w.body[:0]
	w.truncated = false
	w.overridden = true
	w.captureOverride(payload)
}

// appendOverrideCapture incrementally captures a privacy-safe streaming
// representation after overrideCapture has selected it over caller-visible
// writes.
func (w *savedTraceResponseWriter) appendOverrideCapture(payload []byte) {
	if w == nil {
		return
	}
	if !w.overridden {
		w.overrideCapture(nil)
	}
	w.captureOverride(payload)
}

func (w *savedTraceResponseWriter) captureOverride(payload []byte) {
	remaining := w.maxBytes - int64(len(w.body))
	if remaining <= 0 {
		if len(payload) != 0 {
			w.truncated = true
		}
		return
	}
	if int64(len(payload)) > remaining {
		w.body = append(w.body, payload[:remaining]...)
		w.truncated = true
		return
	}
	w.body = append(w.body, payload...)
}

func (w *savedTraceResponseWriter) payload() savedtrace.Payload {
	body := w.body
	contentType := savedTraceContentType(
		w.Header().Get("Content-Type"),
	)
	if contentType == "application/vnd.amazon.eventstream" {
		return savedtrace.Payload{
			ContentType: contentType,
			Body: savedtrace.Base64BodyPrefix +
				base64.StdEncoding.EncodeToString(body),
			Truncated: w.truncated,
		}
	}
	if w.truncated {
		body = validUTF8Prefix(body)
	}
	return savedtrace.Payload{
		ContentType: contentType,
		Body:        string(body),
		Truncated:   w.truncated,
	}
}

func savedTraceContentType(value string) string {
	mediaType, _, err := mime.ParseMediaType(value)
	if err != nil {
		return ""
	}
	return strings.ToLower(mediaType)
}

func boundedTraceBody(body []byte, maximum int64) (string, bool) {
	maximum = effectiveMaxSavedTraceBytes(maximum)
	if int64(len(body)) <= maximum {
		return string(body), false
	}
	return string(validUTF8Prefix(body[:maximum])), true
}

func validUTF8Prefix(value []byte) []byte {
	if utf8.Valid(value) {
		return value
	}
	for offset := 0; offset < len(value); {
		current, size := utf8.DecodeRune(value[offset:])
		if current == utf8.RuneError && size == 1 {
			return value[:offset]
		}
		offset += size
	}
	return value
}

func savedTraceCaptureOutcome(stream bool, outcome ledger.Outcome, request, response savedtrace.Payload) savedtrace.CaptureOutcome {
	if request.Truncated || response.Truncated {
		return savedtrace.CaptureTruncated
	}
	if stream && outcome == ledger.OutcomeSuccess &&
		response.ContentType == "text/event-stream" &&
		!savedTraceStreamTerminal(response.Body) {
		return savedtrace.CaptureIncomplete
	}
	return savedtrace.CaptureComplete
}

func savedTraceStreamTerminal(body string) bool {
	if strings.Contains(body, "data: [DONE]") {
		return true
	}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var event map[string]any
		if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &event) != nil {
			continue
		}
		if event["type"] == "response.completed" {
			return true
		}
	}
	return false
}

func savedTraceCorrelation(request, response savedtrace.Payload) (string, string, string) {
	var requestObject, responseObject map[string]any
	_ = json.Unmarshal([]byte(request.Body), &requestObject)
	if strings.Contains(response.ContentType, "event-stream") {
		responseObject = savedTraceLastSSEObject(response.Body)
	} else {
		_ = json.Unmarshal([]byte(response.Body), &responseObject)
	}
	conversationID := objectIdentifier(requestObject, "conversation_id", "conversation")
	if value := objectIdentifier(responseObject, "conversation_id", "conversation"); value != "" {
		conversationID = value
	}
	responseID := stringField(responseObject, "id")
	if nested, ok := responseObject["response"].(map[string]any); ok {
		if value := stringField(nested, "id"); value != "" {
			responseID = value
		}
		if value := objectIdentifier(nested, "conversation_id", "conversation"); value != "" {
			conversationID = value
		}
	}
	return conversationID, responseID, stringField(requestObject, "previous_response_id")
}

func savedTraceLastSSEObject(body string) map[string]any {
	result := make(map[string]any)
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var current map[string]any
		if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &current) == nil {
			result = current
		}
	}
	return result
}

func objectIdentifier(object map[string]any, fields ...string) string {
	for _, field := range fields {
		switch value := object[field].(type) {
		case string:
			if value != "" {
				return value
			}
		case map[string]any:
			if identifier := stringField(value, "id"); identifier != "" {
				return identifier
			}
		}
	}
	return ""
}

func stringField(object map[string]any, field string) string {
	value, _ := object[field].(string)
	return value
}
