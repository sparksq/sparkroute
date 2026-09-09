// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package pii

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"
)

const defaultMaximumStreamPendingBytes = 64 << 10

// StreamingDetector identifies the prefix of an incomplete text stream that
// cannot participate in an entity completed by a later chunk. The returned
// byte offset must be a UTF-8 boundary. Detectors that do not implement this
// contract remain usable for unary requests and responses only.
type StreamingDetector interface {
	Detector
	SafePrefix(context.Context, string, []Entity) (int, error)
}

type StreamOptions struct {
	MaximumPendingBytes int
}

// StreamRedactor incrementally redacts novel PII while retaining only a
// bounded suffix that could be completed by a future chunk. Known substitution
// tokens remain opaque; restoration belongs to the caller-facing layer.
type StreamRedactor struct {
	session             *Session
	detector            StreamingDetector
	maximumPendingBytes int
	pending             string
}

func (s *Session) SupportsStreaming() bool {
	if s == nil {
		return false
	}
	_, ok := s.detector.(StreamingDetector)
	return ok
}

func (s *Session) NewStreamRedactor(options StreamOptions) (*StreamRedactor, error) {
	if s == nil {
		return nil, fmt.Errorf("PII session is required")
	}
	detector, ok := s.detector.(StreamingDetector)
	if !ok {
		return nil, fmt.Errorf("PII detector does not support bounded streaming")
	}
	maximum := options.MaximumPendingBytes
	if maximum == 0 {
		maximum = defaultMaximumStreamPendingBytes
	}
	if maximum < 1 {
		return nil, fmt.Errorf("PII stream pending-byte limit must be positive")
	}
	return &StreamRedactor{
		session:             s,
		detector:            detector,
		maximumPendingBytes: maximum,
	}, nil
}

// Transform accepts the next semantic string fragment. final releases and
// screens the complete remaining suffix. On error, no internal state changes.
func (r *StreamRedactor) Transform(
	ctx context.Context,
	fragment string,
	final bool,
) (string, error) {
	if r == nil {
		return "", fmt.Errorf("PII stream redactor is required")
	}
	combined := r.pending + fragment
	safe := len(combined)
	if !final {
		var err error
		safe, err = r.detector.SafePrefix(ctx, combined, r.session.entities)
		if err != nil {
			return "", err
		}
		tokenSafe := safeOpaqueTokenPrefix(combined)
		if tokenSafe < safe {
			safe = tokenSafe
		}
	}
	if safe < 0 || safe > len(combined) ||
		(safe < len(combined) && !utf8.RuneStart(combined[safe])) {
		return "", fmt.Errorf("PII streaming detector returned an invalid UTF-8 prefix")
	}
	if len(combined)-safe > r.maximumPendingBytes {
		return "", fmt.Errorf(
			"PII stream pending suffix exceeds %d bytes",
			r.maximumPendingBytes,
		)
	}
	masked, err := r.session.Redact(ctx, combined[:safe])
	if err != nil {
		return "", err
	}
	r.pending = combined[safe:]
	return masked, nil
}

func (r *StreamRedactor) Pending() string {
	if r == nil {
		return ""
	}
	return r.pending
}

// Abort returns the untransformed retained suffix plus the current fragment and
// clears the redactor. It is used only by an explicit fail-open policy.
func (r *StreamRedactor) Abort(fragment string) string {
	if r == nil {
		return fragment
	}
	result := r.pending + fragment
	r.pending = ""
	return result
}

const opaqueTokenPrefix = "[[SPARKROUTE_PII_"

func safeOpaqueTokenPrefix(text string) int {
	safe := len(text)
	maximumPartial := len(opaqueTokenPrefix) - 1
	if maximumPartial > len(text) {
		maximumPartial = len(text)
	}
	for length := 1; length <= maximumPartial; length++ {
		if strings.HasSuffix(text, opaqueTokenPrefix[:length]) {
			safe = len(text) - length
		}
	}
	if start := strings.LastIndex(text, opaqueTokenPrefix); start >= 0 &&
		!strings.Contains(text[start+len(opaqueTokenPrefix):], "]]") {
		if start < safe {
			safe = start
		}
	}
	return safe
}

// SafePrefix implements StreamingDetector for the bounded built-in entity
// grammar. It retains the trailing lexical candidate for each enabled entity;
// a delimiter in a later chunk makes that candidate safe to inspect/release.
func (d *BuiltinDetector) SafePrefix(
	ctx context.Context,
	text string,
	entities []Entity,
) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	safe := len(text)
	for _, entity := range entities {
		var allowed func(byte) bool
		switch entity {
		case EntityEmail:
			allowed = emailStreamByte
		case EntityPhone:
			allowed = phoneStreamByte
		case EntitySSN:
			allowed = ssnStreamByte
		case EntityCreditCard:
			allowed = creditCardStreamByte
		case EntityIPv4:
			allowed = ipv4StreamByte
		default:
			return 0, fmt.Errorf("PII entity %q has no built-in streaming grammar", entity)
		}
		start := trailingAllowedStart(text, allowed)
		if start < safe {
			safe = start
		}
	}
	return safe, nil
}

func trailingAllowedStart(text string, allowed func(byte) bool) int {
	start := len(text)
	for start > 0 && allowed(text[start-1]) {
		start--
	}
	return start
}

func asciiLetterOrDigit(value byte) bool {
	return value >= 'a' && value <= 'z' ||
		value >= 'A' && value <= 'Z' ||
		value >= '0' && value <= '9'
}

func emailStreamByte(value byte) bool {
	return asciiLetterOrDigit(value) || strings.ContainsRune(".!#$%&'*+/=?^_`{|}~@-", rune(value))
}

func phoneStreamByte(value byte) bool {
	return value >= '0' && value <= '9' || strings.ContainsRune("+() .-", rune(value))
}

func ssnStreamByte(value byte) bool {
	return value >= '0' && value <= '9' || value == '-'
}

func creditCardStreamByte(value byte) bool {
	return value >= '0' && value <= '9' || value == ' ' || value == '-'
}

func ipv4StreamByte(value byte) bool {
	return value >= '0' && value <= '9' || value == '.'
}
