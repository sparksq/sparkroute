package pii

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestSessionSubstitutesRepeatedEntitiesAndRestoresExactTokens(t *testing.T) {
	t.Parallel()
	sequence := 0
	session, err := NewSession(NewBuiltinDetector(), []Entity{EntityEmail, EntityPhone}, SessionOptions{
		TokenSource: func() (string, error) {
			sequence++
			return "TOKEN" + string(rune('0'+sequence)), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	original := "Email drew@example.com twice: drew@example.com; call +1 (312) 555-0199."
	masked, err := session.Substitute(context.Background(), original)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(masked, "[[SPARKROUTE_PII_EMAIL_TOKEN1]]") != 2 ||
		!strings.Contains(masked, "[[SPARKROUTE_PII_PHONE_TOKEN2]]") {
		t.Fatalf("masked text = %q", masked)
	}
	if got := session.Restore(masked); got != original {
		t.Fatalf("restored text = %q, want %q", got, original)
	}
	if got := session.Restore("[[SPARKROUTE_PII_EMAIL_UNKNOWN]]"); got != "[[SPARKROUTE_PII_EMAIL_UNKNOWN]]" {
		t.Fatalf("unknown token restored as %q", got)
	}
	if session.MappingCount() != 2 {
		t.Fatalf("mapping count = %d, want 2", session.MappingCount())
	}
}

func TestBuiltinDetectorPrefersCreditCardAndValidatesCandidates(t *testing.T) {
	t.Parallel()
	detector := NewBuiltinDetector()
	spans, err := detector.Detect(context.Background(), "card 4242 4242 4242 4242, bad 4242 4242 4242 4241, ip 10.2.3.4", DefaultEntities())
	if err != nil {
		t.Fatal(err)
	}
	if len(spans) != 2 || spans[0].Entity != EntityCreditCard || spans[1].Entity != EntityIPv4 {
		t.Fatalf("spans = %#v", spans)
	}
}

func TestSessionRedactionDoesNotCreateRestorableMapping(t *testing.T) {
	t.Parallel()
	session, err := NewSession(NewBuiltinDetector(), []Entity{EntityEmail}, SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	redacted, err := session.Redact(context.Background(), "drew@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if redacted != "[[SPARKROUTE_PII_EMAIL_REDACTED]]" || session.MappingCount() != 0 {
		t.Fatalf("redacted = %q, mappings = %d", redacted, session.MappingCount())
	}
}

func TestSessionFailsClosedAtMappingLimits(t *testing.T) {
	t.Parallel()
	session, err := NewSession(NewBuiltinDetector(), []Entity{EntityEmail}, SessionOptions{
		MaximumMappings:      1,
		MaximumOriginalBytes: 1024,
		TokenSource:          func() (string, error) { return "ONE", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = session.Substitute(context.Background(), "a@example.com b@example.com")
	if !errors.Is(err, ErrMappingLimit) {
		t.Fatalf("Substitute() error = %v, want ErrMappingLimit", err)
	}
}

func TestSessionRestoresPersistedEntityOutsideCurrentDetectionPolicy(t *testing.T) {
	t.Parallel()
	session, err := NewSession(NewBuiltinDetector(), []Entity{EntityEmail}, SessionOptions{
		InitialMappings: []Mapping{{
			Entity: EntityPerson, Original: "Ada Lovelace",
			Token: "[[SPARKROUTE_PII_PERSON_PERSISTED]]",
		}},
	})
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	if got := session.Restore("hello [[SPARKROUTE_PII_PERSON_PERSISTED]]"); got != "hello Ada Lovelace" {
		t.Fatalf("Restore() = %q", got)
	}
}

func TestParseKeyringAcceptsRawThirtyTwoByteKey(t *testing.T) {
	t.Parallel()
	if _, err := ParseKeyring([]byte("0123456789abcdef0123456789abcdef")); err != nil {
		t.Fatalf("ParseKeyring() error = %v", err)
	}
}

func TestParseKeyringRequiresStableLookupKeyForRotation(t *testing.T) {
	t.Parallel()
	_, err := ParseKeyring([]byte(`{
		"active":"v2",
		"keys":{
			"v1":"MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=",
			"v2":"YWJjZGVmMDEyMzQ1Njc4OWFiY2RlZmFiY2RlZjAxMjM0NTY3ODk="
		}
	}`))
	if err == nil || !strings.Contains(err.Error(), "lookup_key") {
		t.Fatalf("ParseKeyring() error = %v", err)
	}
}

type invalidSpanDetector struct{}

func (invalidSpanDetector) Supports(Entity) bool { return true }
func (invalidSpanDetector) Detect(context.Context, string, []Entity) ([]Span, error) {
	return []Span{{Start: 1, End: 3, Entity: EntityPerson}}, nil
}

func TestSessionRejectsDetectorSpansThatSplitUTF8(t *testing.T) {
	t.Parallel()
	session, err := NewSession(invalidSpanDetector{}, []Entity{EntityPerson}, SessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.Substitute(context.Background(), "éx"); err == nil || !strings.Contains(err.Error(), "UTF-8") {
		t.Fatalf("Substitute() error = %v", err)
	}
}
