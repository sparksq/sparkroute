package gateway

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestRewriteJSONResponseModel(t *testing.T) {
	t.Parallel()

	body, err := rewriteJSONResponseModel(
		[]byte(`{"id":"chatcmpl-1","model":"upstream","future":{"field":true}}`),
		"virtual",
	)
	if err != nil {
		t.Fatalf("rewriteJSONResponseModel() error = %v", err)
	}
	var response map[string]json.RawMessage
	if err := json.Unmarshal(body, &response); err != nil {
		t.Fatalf("decode rewritten response: %v", err)
	}
	if string(response["model"]) != `"virtual"` {
		t.Fatalf("model = %s", response["model"])
	}
	if string(response["future"]) != `{"field":true}` {
		t.Fatalf("future = %s", response["future"])
	}
}

func TestRewriteSSEModelsAcrossSmallReads(t *testing.T) {
	t.Parallel()

	input := strings.Join([]string{
		": keepalive\n",
		"event: message\n",
		"data: {\"id\":\"chunk-1\",\n",
		"data: \"model\":\"upstream\"}\n",
		"\n",
		"data: [DONE]\n",
		"\n",
	}, "")
	var output bytes.Buffer
	err := rewriteSSEModels(&output, oneByteReader{source: strings.NewReader(input)}, "virtual")
	if err != nil {
		t.Fatalf("rewriteSSEModels() error = %v", err)
	}
	got := output.String()
	if !strings.Contains(got, ": keepalive\n") || !strings.Contains(got, "event: message\n") {
		t.Fatalf("metadata fields not preserved: %q", got)
	}
	if !strings.Contains(got, `"model":"virtual"`) || !strings.Contains(got, `"id":"chunk-1"`) {
		t.Fatalf("JSON event not rewritten: %q", got)
	}
	if !strings.Contains(got, "data: [DONE]\n\n") {
		t.Fatalf("terminal event not preserved: %q", got)
	}
}

func TestRewriteSSEModelsPreservesUnknownData(t *testing.T) {
	t.Parallel()

	input := "data: provider-specific-control\n\n"
	var output bytes.Buffer
	if err := rewriteSSEModels(&output, strings.NewReader(input), "virtual"); err != nil {
		t.Fatalf("rewriteSSEModels() error = %v", err)
	}
	if output.String() != input {
		t.Fatalf("output = %q, want %q", output.String(), input)
	}
}

func TestRewriteSSEModelsSupportsEveryEventLineEnding(t *testing.T) {
	t.Parallel()

	for name, ending := range map[string]string{
		"LF":   "\n",
		"CRLF": "\r\n",
		"CR":   "\r",
	} {
		name := name
		ending := ending
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			input := strings.Join([]string{
				`data: {"model":"upstream","part":1}`,
				"",
				`data: {"model":"upstream","part":2}`,
			}, ending)
			input += ending
			var output bytes.Buffer
			if err := rewriteSSEModels(
				&output,
				oneByteReader{source: strings.NewReader(input)},
				"virtual",
			); err != nil {
				t.Fatalf("rewriteSSEModels() error = %v", err)
			}
			got := output.String()
			if strings.Count(got, `"model":"virtual"`) != 2 ||
				!strings.Contains(got, `"part":1`) ||
				!strings.Contains(got, `"part":2`) ||
				strings.Count(got, ending+ending) != 1 ||
				!strings.HasSuffix(got, ending) {
				t.Fatalf("output = %q", got)
			}
		})
	}
}

func TestRewriteSSEModelsRejectsOversizedEvent(t *testing.T) {
	t.Parallel()

	input := "data: " + strings.Repeat("x", maxSSEEventBytes) + "\n\n"
	var output bytes.Buffer
	if err := rewriteSSEModels(
		&output,
		strings.NewReader(input),
		"virtual",
	); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("rewriteSSEModels() error = %v", err)
	}
}

type oneByteReader struct {
	source *strings.Reader
}

func (r oneByteReader) Read(value []byte) (int, error) {
	if len(value) > 1 {
		value = value[:1]
	}
	return r.source.Read(value)
}
