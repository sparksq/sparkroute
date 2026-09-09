// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestVerifyExactTraceSet(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	requests := filepath.Join(directory, "requests.jsonl")
	traces := filepath.Join(directory, "traces.jsonl")
	if err := os.WriteFile(requests, []byte(
		"{\"request_id\":\"request-a\"}\n{\"request_id\":\"request-b\"}\n",
	), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(traces, []byte(
		"{\"request_id\":\"request-b\"}\n{\"request_id\":\"request-a\"}\n",
	), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := verify([]string{requests}, traces, false)
	if err != nil {
		t.Fatalf("verify() error = %v", err)
	}
	if !got.Complete || got.MatchedRequests != 2 || got.ObservedTraces != 2 {
		t.Fatalf("verify() = %#v", got)
	}
}

func TestVerifyFindsMissingDuplicateAndUnexpected(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	requests := filepath.Join(directory, "requests.jsonl")
	traces := filepath.Join(directory, "traces.jsonl")
	if err := os.WriteFile(requests, []byte(
		"{\"request_id\":\"request-a\"}\n{\"request_id\":\"request-b\"}\n",
	), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(traces, []byte(
		"{\"request_id\":\"request-a\"}\n{\"request_id\":\"request-a\"}\n{\"request_id\":\"request-c\"}\n",
	), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := verify([]string{requests}, traces, false)
	if err != nil {
		t.Fatalf("verify() error = %v", err)
	}
	if got.Complete || got.MissingRequestIDs != 1 || got.DuplicateRequestID != 1 || got.UnexpectedTraces != 1 {
		t.Fatalf("verify() = %#v", got)
	}
}

func TestVerifyRejectsGatewayEvidenceWithoutUniqueRequestIDs(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	requests := filepath.Join(directory, "requests.jsonl")
	traces := filepath.Join(directory, "traces.jsonl")
	if err := os.WriteFile(requests, []byte(
		"{\"request_id\":\"request-a\",\"status\":200}\n"+
			"{\"request_id\":\"request-a\",\"status\":200}\n"+
			"{\"status\":500}\n",
	), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(traces, []byte("{\"request_id\":\"request-a\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := verify([]string{requests}, traces, false)
	if err != nil {
		t.Fatalf("verify() error = %v", err)
	}
	if got.Complete || got.DuplicateEvidence != 1 || got.EvidenceWithoutID != 1 {
		t.Fatalf("verify() = %#v", got)
	}
}

func TestVerifyMergesRepeatedRequestEvidence(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	requestsA := filepath.Join(directory, "requests-a.jsonl")
	requestsB := filepath.Join(directory, "requests-b.jsonl")
	traces := filepath.Join(directory, "traces.jsonl")
	if err := os.WriteFile(requestsA, []byte("{\"request_id\":\"request-a\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(requestsB, []byte("{\"request_id\":\"request-b\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(traces, []byte(
		"{\"request_id\":\"request-b\"}\n{\"request_id\":\"request-a\"}\n",
	), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := verify([]string{requestsA, requestsB}, traces, false)
	if err != nil {
		t.Fatalf("verify() error = %v", err)
	}
	if !got.Complete || got.ExpectedRequests != 2 || got.MatchedRequests != 2 {
		t.Fatalf("verify() = %#v", got)
	}
}
