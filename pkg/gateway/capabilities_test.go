// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package gateway

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/sparksq/sparkroute/pkg/config"
)

func TestDetectChatCapabilities(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		request   string
		streaming bool
		want      []config.Capability
	}{
		{
			name:    "ordinary text",
			request: `{"messages":[{"role":"user","content":"hello"}]}`,
		},
		{
			name: "tools",
			request: `{
				"messages":[{"role":"tool","tool_call_id":"call-1","content":"ok"}],
				"tools":[{"type":"function","function":{"name":"lookup"}}],
				"parallel_tool_calls":true
			}`,
			want: []config.Capability{
				config.CapabilityParallelTools,
				config.CapabilityTools,
			},
		},
		{
			name: "multimodal and developer",
			request: `{"messages":[
				{"role":"developer","content":"help"},
				{"role":"user","content":[
					{"type":"text","text":"inspect"},
					{"type":"image_url","image_url":{"url":"data:image/png;base64,a"}},
					{"type":"input_video","input_video":{"data":"a","format":"mp4"}},
					{"type":"input_audio","input_audio":{"data":"a","format":"wav"}},
					{"type":"input_file","file_id":"file-1"}
				]}
			]}`,
			want: []config.Capability{
				config.CapabilityAudioInput,
				config.CapabilityDeveloperMessages,
				config.CapabilityFileInput,
				config.CapabilityVision,
			},
		},
		{
			name: "generation controls",
			request: `{
				"response_format":{"type":"json_schema","json_schema":{"name":"answer"}},
				"reasoning_effort":"high",
				"logprobs":true,
				"seed":0,
				"n":2,
				"prediction":{"type":"content","content":"answer"},
				"service_tier":"priority",
				"store":true,
				"modalities":["text","audio"],
				"web_search_options":{}
			}`,
			want: []config.Capability{
				config.CapabilityAudioOutput,
				config.CapabilityLogprobs,
				config.CapabilityMultipleChoices,
				config.CapabilityPrediction,
				config.CapabilityProviderTools,
				config.CapabilityReasoning,
				config.CapabilitySeed,
				config.CapabilityServiceTier,
				config.CapabilityStoredCompletion,
				config.CapabilityStructuredOutputs,
			},
		},
		{
			name:      "stream usage",
			request:   `{"stream_options":{"include_usage":true}}`,
			streaming: true,
			want:      []config.Capability{config.CapabilityStreamUsage},
		},
		{
			name:    "inactive optional fields",
			request: `{"tools":[],"parallel_tool_calls":false,"tool_choice":"none","n":1,"store":false}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var envelope map[string]json.RawMessage
			if err := json.Unmarshal([]byte(test.request), &envelope); err != nil {
				t.Fatalf("unmarshal fixture: %v", err)
			}
			got, err := detectChatCapabilities(envelope, test.streaming)
			if err != nil {
				t.Fatalf("detectChatCapabilities() error = %v", err)
			}
			if fmt.Sprint(got) != fmt.Sprint(test.want) {
				t.Fatalf("capabilities = %v, want %v", got, test.want)
			}
		})
	}
}

func TestDetectChatCapabilitiesRejectsMalformedKnownShapes(t *testing.T) {
	t.Parallel()

	for _, request := range []string{
		`{"messages":"not-an-array"}`,
		`{"messages":[{"role":17}]}`,
		`{"response_format":"json"}`,
		`{"modalities":"audio"}`,
	} {
		t.Run(request, func(t *testing.T) {
			var envelope map[string]json.RawMessage
			if err := json.Unmarshal([]byte(request), &envelope); err != nil {
				t.Fatalf("unmarshal fixture: %v", err)
			}
			if _, err := detectChatCapabilities(envelope, false); err == nil {
				t.Fatalf("detectChatCapabilities(%s) unexpectedly succeeded", request)
			}
		})
	}
}
