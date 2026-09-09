// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package promptcache

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

type blockingBackend struct{}

func (blockingBackend) Lookup(
	ctx context.Context,
	_ Query,
) (Match, bool, error) {
	<-ctx.Done()
	return Match{}, false, ctx.Err()
}

func (blockingBackend) Record(context.Context, Observation) error { return nil }

func TestFingerprinterMatchesLongestCommonMessagePrefix(t *testing.T) {
	t.Parallel()

	fingerprinter, err := NewFingerprinter([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatalf("NewFingerprinter() error = %v", err)
	}
	first := map[string]json.RawMessage{
		"model":       json.RawMessage(`"virtual"`),
		"temperature": json.RawMessage(`0.2`),
		"tools":       json.RawMessage(`[{"type":"function","function":{"name":"lookup"}}]`),
		"messages": json.RawMessage(`[
			{"role":"system","content":"shared"},
			{"role":"user","content":"first"},
			{"role":"assistant","content":"answer"}
		]`),
	}
	second := map[string]json.RawMessage{
		"model":       json.RawMessage(`"alias"`),
		"temperature": json.RawMessage(`0.9`),
		"tools":       first["tools"],
		"messages": json.RawMessage(`[
			{"content":"shared","role":"system"},
			{"role":"user","content":"first"},
			{"role":"assistant","content":"different"}
		]`),
	}
	options := FingerprintOptions{
		ScopeDigest: "scope", VirtualModel: "virtual", Operation: "chat.completions",
		ConfigRevision: "revision", SequenceField: "messages",
		MinPrefixBytes: 1, MaxPrefixes: 8,
	}
	firstPrefixes, err := fingerprinter.Prefixes(first, options)
	if err != nil {
		t.Fatalf("Prefixes(first) error = %v", err)
	}
	secondPrefixes, err := fingerprinter.Prefixes(second, options)
	if err != nil {
		t.Fatalf("Prefixes(second) error = %v", err)
	}
	if len(firstPrefixes) != 3 || len(secondPrefixes) != 3 {
		t.Fatalf("prefix lengths = %d, %d", len(firstPrefixes), len(secondPrefixes))
	}
	if firstPrefixes[1].Digest != secondPrefixes[1].Digest ||
		firstPrefixes[0].Digest == secondPrefixes[0].Digest {
		t.Fatalf("common-prefix fingerprints = %#v vs %#v", firstPrefixes, secondPrefixes)
	}
	for _, prefix := range firstPrefixes {
		if strings.Contains(prefix.Digest, "shared") || strings.Contains(prefix.Digest, "first") {
			t.Fatalf("fingerprint exposed content: %q", prefix.Digest)
		}
	}
}

func TestMemoryStorePrefersLongestPrefixThenMostRecentRoute(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_800_000_000, 0)
	store := NewMemoryStore(MemoryOptions{})
	base := Observation{
		ScopeDigest: "scope", VirtualModel: "model", Operation: "chat",
		ConfigRevision: "revision", LastSuccess: now, ExpiresAt: now.Add(time.Minute),
	}
	first := base
	first.Route = Route{Provider: "p", Deployment: "a", UpstreamModel: "m", UpstreamProtocol: "openai"}
	first.Prefixes = []Prefix{{Digest: "short", Segments: 1}, {Digest: "long", Segments: 2}}
	if err := store.Record(context.Background(), first); err != nil {
		t.Fatalf("Record(first) error = %v", err)
	}
	second := base
	second.LastSuccess = now.Add(10 * time.Second)
	second.ExpiresAt = now.Add(time.Minute)
	second.Route = Route{Provider: "p", Deployment: "b", UpstreamModel: "m", UpstreamProtocol: "openai"}
	second.Prefixes = []Prefix{{Digest: "short", Segments: 1}}
	if err := store.Record(context.Background(), second); err != nil {
		t.Fatalf("Record(second) error = %v", err)
	}
	match, found, err := store.Lookup(context.Background(), Query{
		ScopeDigest: "scope", VirtualModel: "model", Operation: "chat",
		ConfigRevision: "revision", Now: now.Add(20 * time.Second),
		Prefixes:   []Prefix{{Digest: "long", Segments: 2}, {Digest: "short", Segments: 1}},
		Candidates: []Route{first.Route, second.Route},
	})
	if err != nil || !found {
		t.Fatalf("Lookup() = %#v, %v, %v", match, found, err)
	}
	if match.Route.Deployment != "a" || match.Prefix.Digest != "long" {
		t.Fatalf("Lookup() = %#v, want longest prefix on a", match)
	}

	_, found, err = store.Lookup(context.Background(), Query{
		ScopeDigest: "scope", VirtualModel: "model", Operation: "chat",
		ConfigRevision: "revision", Now: now.Add(2 * time.Minute),
		Prefixes: []Prefix{{Digest: "long"}}, Candidates: []Route{first.Route},
	})
	if err != nil || found {
		t.Fatalf("expired Lookup() found = %v, err = %v", found, err)
	}
}

func TestDirectoryBoundsLookupAndFailsOpen(t *testing.T) {
	t.Parallel()

	errors := make(chan error, 1)
	directory, err := NewDirectory(blockingBackend{}, DirectoryOptions{
		LookupTimeout: 5 * time.Millisecond,
		OnError:       func(err error) { errors <- err },
	})
	if err != nil {
		t.Fatalf("NewDirectory() error = %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if closeErr := directory.Close(ctx); closeErr != nil {
			t.Errorf("Close() error = %v", closeErr)
		}
	}()
	started := time.Now()
	_, found := directory.Lookup(context.Background(), Query{
		Prefixes:   []Prefix{{Digest: "prefix"}},
		Candidates: []Route{{Deployment: "deployment"}},
	})
	if found || time.Since(started) > 250*time.Millisecond {
		t.Fatalf("Lookup() found = %v, elapsed = %s", found, time.Since(started))
	}
	select {
	case <-errors:
	case <-time.After(time.Second):
		t.Fatal("lookup timeout was not observed")
	}
}
