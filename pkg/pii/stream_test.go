// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package pii

import (
	"context"
	"strings"
	"testing"
)

func TestStreamRedactorHoldsSplitPIIAndOpaqueTokens(t *testing.T) {
	t.Parallel()

	session, err := NewSession(
		NewBuiltinDetector(),
		[]Entity{EntityEmail},
		SessionOptions{TokenSource: func() (string, error) { return "TOKEN", nil }},
	)
	if err != nil {
		t.Fatal(err)
	}
	token, err := session.Substitute(context.Background(), "known@example.com")
	if err != nil {
		t.Fatal(err)
	}
	redactor, err := session.NewStreamRedactor(StreamOptions{})
	if err != nil {
		t.Fatal(err)
	}
	fragments := []string{
		"hello dr",
		"ew@example",
		".com and " + token[:12],
		token[12:] + " done",
	}
	var output strings.Builder
	for _, fragment := range fragments {
		masked, transformErr := redactor.Transform(
			context.Background(), fragment, false,
		)
		if transformErr != nil {
			t.Fatal(transformErr)
		}
		output.WriteString(masked)
	}
	final, err := redactor.Transform(context.Background(), "", true)
	if err != nil {
		t.Fatal(err)
	}
	output.WriteString(final)
	got := output.String()
	if got != "hello [[SPARKROUTE_PII_EMAIL_REDACTED]] and "+token+" done" {
		t.Fatalf("stream output = %q", got)
	}
	if restored := session.Restore(got); restored !=
		"hello [[SPARKROUTE_PII_EMAIL_REDACTED]] and known@example.com done" {
		t.Fatalf("restored output = %q", restored)
	}
}

func TestStreamRedactorFailsAtBoundWithoutReleasingCandidate(t *testing.T) {
	t.Parallel()

	session, err := NewSession(NewBuiltinDetector(), []Entity{EntityEmail}, SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	redactor, err := session.NewStreamRedactor(StreamOptions{MaximumPendingBytes: 8})
	if err != nil {
		t.Fatal(err)
	}
	if output, err := redactor.Transform(context.Background(), "abcdefgh", false); err != nil || output != "" {
		t.Fatalf("first Transform() = %q, %v", output, err)
	}
	if output, err := redactor.Transform(context.Background(), "i", false); err == nil || output != "" {
		t.Fatalf("second Transform() = %q, %v", output, err)
	}
	if got := redactor.Pending(); got != "abcdefgh" {
		t.Fatalf("pending = %q", got)
	}
	if got := redactor.Abort("i"); got != "abcdefghi" || redactor.Pending() != "" {
		t.Fatalf("Abort() = %q; pending = %q", got, redactor.Pending())
	}
}

type unaryOnlyDetector struct{ delegate *BuiltinDetector }

func (d unaryOnlyDetector) Supports(entity Entity) bool {
	return d.delegate.Supports(entity)
}

func (d unaryOnlyDetector) Detect(
	ctx context.Context,
	text string,
	entities []Entity,
) ([]Span, error) {
	return d.delegate.Detect(ctx, text, entities)
}

func TestSessionReportsStreamingDetectorSupport(t *testing.T) {
	t.Parallel()

	session, err := NewSession(
		unaryOnlyDetector{delegate: NewBuiltinDetector()},
		[]Entity{EntityEmail},
		SessionOptions{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if session.SupportsStreaming() {
		t.Fatal("SupportsStreaming() = true")
	}
	if _, err := session.NewStreamRedactor(StreamOptions{}); err == nil {
		t.Fatal("NewStreamRedactor() error = nil")
	}
}
