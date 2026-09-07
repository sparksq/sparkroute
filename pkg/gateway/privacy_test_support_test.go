package gateway

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/sparksq/sparkroute/pkg/privacy"
)

// These deliberately small doubles exercise the OSS protocol extension
// plumbing without shipping a production PII detector or substitution engine.
type testPrivacyProvider struct{}

func (testPrivacyProvider) NewSession(
	_ context.Context,
	entities []privacy.Entity,
	_ privacy.Scope,
) (privacy.Session, error) {
	for _, entity := range entities {
		if entity != privacy.EntityEmail {
			return nil, fmt.Errorf("test privacy provider does not support %q", entity)
		}
	}
	return &testPrivacySession{
		byOriginal: make(map[string]string),
		byToken:    make(map[string]string),
	}, nil
}

func (testPrivacyProvider) Supports(entity privacy.Entity) bool {
	return entity == privacy.EntityEmail
}

func (testPrivacyProvider) RecordInspection() {}
func (testPrivacyProvider) RecordFailure()    {}
func (testPrivacyProvider) PrivacyStatus(context.Context) privacy.Status {
	return privacy.Status{State: privacy.StatusHealthy, Enabled: true, Backend: "test"}
}

var testEmailPattern = regexp.MustCompile(
	`(?i)[a-z0-9.!#$%&'*+/=?^_` + "`" + `{|}~-]+@[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)+`,
)

type testPrivacySession struct {
	mu         sync.Mutex
	byOriginal map[string]string
	byToken    map[string]string
}

func (s *testPrivacySession) Substitute(_ context.Context, text string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return testEmailPattern.ReplaceAllStringFunc(text, func(original string) string {
		if token := s.byOriginal[original]; token != "" {
			return token
		}
		token := "[[SPARKROUTE_PII_EMAIL_TOKEN]]"
		s.byOriginal[original] = token
		s.byToken[token] = original
		return token
	}), nil
}

func (s *testPrivacySession) Redact(_ context.Context, text string) (string, error) {
	return testEmailPattern.ReplaceAllString(text, "[[SPARKROUTE_PII_EMAIL_REDACTED]]"), nil
}

func (s *testPrivacySession) Restore(text string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for token, original := range s.byToken {
		text = strings.ReplaceAll(text, token, original)
	}
	return text
}

func (s *testPrivacySession) MappingCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.byToken)
}

func (*testPrivacySession) SupportsStreaming() bool { return true }

func (s *testPrivacySession) NewStreamRedactor(
	options privacy.StreamOptions,
) (privacy.StreamRedactor, error) {
	maximum := options.MaximumPendingBytes
	if maximum == 0 {
		maximum = 64 << 10
	}
	if maximum < 1 {
		return nil, fmt.Errorf("PII stream pending-byte limit must be positive")
	}
	return &testPrivacyStreamRedactor{session: s, maximum: maximum}, nil
}

type testPrivacyStreamRedactor struct {
	session *testPrivacySession
	maximum int
	pending string
}

func (r *testPrivacyStreamRedactor) Transform(
	ctx context.Context,
	fragment string,
	final bool,
) (string, error) {
	combined := r.pending + fragment
	safe := len(combined)
	if !final {
		safe = testTrailingEmailStart(combined)
		if tokenSafe := testOpaqueTokenPrefix(combined); tokenSafe < safe {
			safe = tokenSafe
		}
	}
	if safe < 0 || safe > len(combined) ||
		(safe < len(combined) && !utf8.RuneStart(combined[safe])) {
		return "", fmt.Errorf("invalid test PII stream prefix")
	}
	if len(combined)-safe > r.maximum {
		return "", fmt.Errorf("test PII stream pending suffix exceeds %d bytes", r.maximum)
	}
	masked, err := r.session.Redact(ctx, combined[:safe])
	if err != nil {
		return "", err
	}
	r.pending = combined[safe:]
	return masked, nil
}

func (r *testPrivacyStreamRedactor) Pending() string { return r.pending }

func (r *testPrivacyStreamRedactor) Abort(fragment string) string {
	result := r.pending + fragment
	r.pending = ""
	return result
}

func testTrailingEmailStart(text string) int {
	start := len(text)
	for start > 0 {
		value := text[start-1]
		allowed := value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' ||
			value >= '0' && value <= '9' || strings.ContainsRune(".!#$%&'*+/=?^_`{|}~@-", rune(value))
		if !allowed {
			break
		}
		start--
	}
	return start
}

func testOpaqueTokenPrefix(text string) int {
	const prefix = "[[SPARKROUTE_PII_"
	safe := len(text)
	maximum := len(prefix) - 1
	if len(text) < maximum {
		maximum = len(text)
	}
	for length := 1; length <= maximum; length++ {
		if strings.HasSuffix(text, prefix[:length]) {
			safe = len(text) - length
		}
	}
	if start := strings.LastIndex(text, prefix); start >= 0 &&
		!strings.Contains(text[start+len(prefix):], "]]") && start < safe {
		safe = start
	}
	return safe
}

func newTestPrivacySession() privacy.Session {
	session, _ := (testPrivacyProvider{}).NewSession(
		context.Background(), []privacy.Entity{privacy.EntityEmail}, privacy.Scope{},
	)
	return session
}
