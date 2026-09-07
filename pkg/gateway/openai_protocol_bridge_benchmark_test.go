// SPDX-License-Identifier: AGPL-3.0-only
// Copyright 2026 Scitrera LLC
// Copyright 2026 Fox Engine Ltd.

package gateway

import (
	"bytes"
	"io"
	"testing"
)

var benchmarkResponsesRequest = []byte(`{
  "model":"logical-model",
  "input":[{"role":"user","content":[{"type":"input_text","text":"Use the weather tool for Chicago."}]}],
  "tools":[{"type":"function","name":"weather","description":"Look up weather","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}],
  "store":false,
  "stream":true
}`)

var benchmarkChatStream = []byte("data: {\"id\":\"chat_1\",\"model\":\"served\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"},\"finish_reason\":null}]}\n\n" +
	"data: {\"id\":\"chat_1\",\"model\":\"served\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"},\"finish_reason\":null}]}\n\n" +
	"data: {\"id\":\"chat_1\",\"model\":\"served\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
	"data: {\"id\":\"chat_1\",\"model\":\"served\",\"choices\":[],\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":1,\"total_tokens\":5}}\n\n" +
	"data: [DONE]\n\n")

func BenchmarkResponsesChatFallbackRequest(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(int64(len(benchmarkResponsesRequest)))
	for i := 0; i < b.N; i++ {
		body, err := translateResponsesRequestToChat(benchmarkResponsesRequest)
		if err != nil || len(body) == 0 {
			b.Fatalf("translateResponsesRequestToChat() body=%d error=%v", len(body), err)
		}
	}
}

func BenchmarkChatResponsesStreamFallback(b *testing.B) {
	b.ReportAllocs()
	b.SetBytes(int64(len(benchmarkChatStream)))
	for i := 0; i < b.N; i++ {
		if err := proxyChatStreamToResponses(io.Discard, bytes.NewReader(benchmarkChatStream), "logical-model", nil); err != nil {
			b.Fatal(err)
		}
	}
}
