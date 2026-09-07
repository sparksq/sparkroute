package gateway

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sparksq/sparkroute/pkg/ledger"
)

func TestResponsesStreamingTerminalOutcomes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		terminal    string
		wantOutcome ledger.Outcome
		wantFailure string
	}{
		{
			name: "failed event",
			terminal: "event: response.failed\n" +
				`data: {"type":"response.failed","sequence_number":1,"response":{"id":"resp_1","model":"upstream-model","status":"failed","error":{"code":"server_error","message":"failed"}}}` + "\n\n",
			wantOutcome: ledger.OutcomeUpstreamError,
			wantFailure: "response_failed",
		},
		{
			name:        "EOF before terminal event",
			wantOutcome: ledger.OutcomeStreamError,
			wantFailure: "responses_stream_incomplete",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "event: response.created\n")
				_, _ = io.WriteString(w, `data: {"type":"response.created","sequence_number":0,"response":{"id":"resp_1","model":"upstream-model","status":"in_progress"}}`+"\n\n")
				_, _ = io.WriteString(w, test.terminal)
			}))
			defer upstream.Close()

			recorder := &collectingRecorder{}
			handler, err := NewDataHandler(
				responsesDocument(upstream.URL+"/v1"),
				DataOptions{Ledger: recorder},
			)
			if err != nil {
				t.Fatalf("NewDataHandler() error = %v", err)
			}
			request := httptest.NewRequest(
				http.MethodPost,
				"/v1/responses",
				strings.NewReader(`{"model":"public","input":"hello","store":false,"stream":true}`),
			)
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)

			if response.Code != http.StatusOK {
				t.Fatalf("status = %d; body=%s", response.Code, response.Body)
			}
			records := recorder.snapshot()
			if len(records) != 2 || records[0].Attempt == nil || records[1].Request == nil {
				t.Fatalf("records = %#v", records)
			}
			for _, record := range []struct {
				outcome ledger.Outcome
				failure string
			}{
				{records[0].Attempt.Outcome, records[0].Attempt.FailureClass},
				{records[1].Request.Outcome, records[1].Request.FailureClass},
			} {
				if record.outcome != test.wantOutcome || record.failure != test.wantFailure {
					t.Errorf(
						"outcome/failure = %q/%q, want %q/%q",
						record.outcome,
						record.failure,
						test.wantOutcome,
						test.wantFailure,
					)
				}
			}
		})
	}
}
