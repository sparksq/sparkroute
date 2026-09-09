// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package sqlite

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	privacy "github.com/sparksq/sparkroute/pkg/pii"
)

func TestPIIConversationMappingsStableEncryptedRotatableAndDeletable(t *testing.T) {
	store, err := Open(t.Context(), Options{Path: testPIISQLitePath(t)})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = store.Close() }()

	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	scope := privacy.ConversationScope{
		Tenant: "tenant-a", Principal: "principal-a", Conversation: "thread-a",
	}
	directory := testPIIDirectory(t, store, testPIIKeyring(t, "v1"), &now, "FIRST")
	first, err := directory.NewSession(
		t.Context(), privacy.NewBuiltinDetector(), []privacy.Entity{privacy.EntityEmail}, scope,
	)
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	masked, err := first.Substitute(t.Context(), "contact drew@example.com")
	if err != nil {
		t.Fatalf("Substitute() error = %v", err)
	}
	if strings.Contains(masked, "drew@example.com") || !strings.Contains(masked, "FIRST") {
		t.Fatalf("masked value = %q", masked)
	}

	second, err := directory.NewSession(
		t.Context(), privacy.NewBuiltinDetector(), []privacy.Entity{privacy.EntityEmail}, scope,
	)
	if err != nil {
		t.Fatalf("second NewSession() error = %v", err)
	}
	if restored := second.Restore(masked); restored != "contact drew@example.com" {
		t.Fatalf("Restore() = %q", restored)
	}
	reused, err := second.Substitute(t.Context(), "again drew@example.com")
	if err != nil || !strings.Contains(reused, "FIRST") {
		t.Fatalf("stable Substitute() = %q, %v", reused, err)
	}

	var keyID string
	var ciphertext []byte
	if err := store.db.QueryRowContext(t.Context(), `
		SELECT key_id, ciphertext FROM llm_pii_conversation_mappings
	`).Scan(&keyID, &ciphertext); err != nil {
		t.Fatalf("inspect encrypted row: %v", err)
	}
	if keyID != "v1" || strings.Contains(string(ciphertext), "drew@example.com") {
		t.Fatalf("stored key/ciphertext = %q, %x", keyID, ciphertext)
	}

	rotated := testPIIDirectory(t, store, testPIIRotatedKeyring(t), &now, "SECOND")
	rotationSession, err := rotated.NewSession(
		t.Context(), privacy.NewBuiltinDetector(), []privacy.Entity{privacy.EntityEmail}, scope,
	)
	if err != nil || rotationSession.Restore(masked) != "contact drew@example.com" {
		t.Fatalf("rotated NewSession()/Restore() = %q, %v", rotationSession.Restore(masked), err)
	}
	if err := store.db.QueryRowContext(t.Context(), `
		SELECT key_id FROM llm_pii_conversation_mappings
	`).Scan(&keyID); err != nil || keyID != "v2" {
		t.Fatalf("rotated key ID = %q, %v", keyID, err)
	}

	if err := rotated.DeleteConversation(t.Context(), scope); err != nil {
		t.Fatalf("DeleteConversation() error = %v", err)
	}
	var rows int
	if err := store.db.QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM llm_pii_conversation_mappings
	`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("rows after delete = %d, %v", rows, err)
	}
}

func TestPIIConversationMappingsConvergeAcrossConcurrentSessions(t *testing.T) {
	store, err := Open(t.Context(), Options{Path: testPIISQLitePath(t)})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = store.Close() }()
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	keyring := testPIIKeyring(t, "v1")
	scope := privacy.ConversationScope{Tenant: "t", Principal: "p", Conversation: "c"}

	const workers = 24
	tokens := make(chan string, workers)
	errorsSeen := make(chan error, workers)
	var wait sync.WaitGroup
	for index := 0; index < workers; index++ {
		index := index
		wait.Add(1)
		go func() {
			defer wait.Done()
			directory := testPIIDirectory(t, store, keyring, &now, fmt.Sprintf("TOKEN%d", index))
			session, sessionErr := directory.NewSession(
				context.Background(), privacy.NewBuiltinDetector(), []privacy.Entity{privacy.EntityEmail}, scope,
			)
			if sessionErr != nil {
				errorsSeen <- sessionErr
				return
			}
			masked, substituteErr := session.Substitute(context.Background(), "drew@example.com")
			if substituteErr != nil {
				errorsSeen <- substituteErr
				return
			}
			tokens <- masked
		}()
	}
	wait.Wait()
	close(tokens)
	close(errorsSeen)
	for err := range errorsSeen {
		t.Fatalf("concurrent mapping error = %v", err)
	}
	want := ""
	for token := range tokens {
		if want == "" {
			want = token
		}
		if token != want {
			t.Fatalf("concurrent token = %q, want %q", token, want)
		}
	}
	var count int
	if err := store.db.QueryRowContext(t.Context(), `
		SELECT COUNT(*) FROM llm_pii_conversation_mappings
	`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("stored mappings = %d, %v", count, err)
	}
}

func TestPIIConversationMappingsExpirePruneAndEnforceBounds(t *testing.T) {
	store, err := Open(t.Context(), Options{Path: testPIISQLitePath(t)})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer func() { _ = store.Close() }()
	now := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	keyring := testPIIKeyring(t, "v1")
	scope := privacy.ConversationScope{Tenant: "t", Principal: "p", Conversation: "c"}
	directory, err := privacy.NewDirectory(store, keyring, privacy.DirectoryOptions{
		Now: func() time.Time { return now },
		Limits: privacy.ConversationLimits{
			MaximumMappings: 1, MaximumOriginalBytes: 64, MaximumConversations: 1,
			SlidingTTL: time.Minute, AbsoluteTTL: 2 * time.Minute,
		},
		TokenSource: func() (string, error) { return "ONE", nil },
	})
	if err != nil {
		t.Fatalf("NewDirectory() error = %v", err)
	}
	session, err := directory.NewSession(
		t.Context(), privacy.NewBuiltinDetector(), []privacy.Entity{privacy.EntityEmail}, scope,
	)
	if err != nil {
		t.Fatalf("NewSession() error = %v", err)
	}
	if _, err := session.Substitute(t.Context(), "a@example.com b@example.com"); !errorsIsConversationLimit(err) {
		t.Fatalf("mapping bound error = %v", err)
	}

	now = now.Add(3 * time.Minute)
	pruned, err := directory.Prune(t.Context(), 10)
	if err != nil || pruned != 1 {
		t.Fatalf("Prune() = %d, %v", pruned, err)
	}
}

func errorsIsConversationLimit(err error) bool {
	return err != nil && (strings.Contains(err.Error(), privacy.ErrConversationLimit.Error()) ||
		strings.Contains(err.Error(), privacy.ErrMappingLimit.Error()))
}

func testPIISQLitePath(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatalf("secure temporary SQLite directory: %v", err)
	}
	return filepath.Join(directory, "ledger.sqlite")
}

func testPIIDirectory(
	t *testing.T,
	store *Store,
	keyring *privacy.Keyring,
	now *time.Time,
	token string,
) *privacy.Directory {
	t.Helper()
	directory, err := privacy.NewDirectory(store, keyring, privacy.DirectoryOptions{
		Now:         func() time.Time { return *now },
		TokenSource: func() (string, error) { return token, nil },
	})
	if err != nil {
		t.Fatalf("NewDirectory() error = %v", err)
	}
	return directory
}

func testPIIKeyring(t *testing.T, active string) *privacy.Keyring {
	t.Helper()
	key := strings.Repeat(active, 32)
	document := fmt.Sprintf(
		`{"active":%q,"keys":{%q:%q}}`,
		active, active, base64.StdEncoding.EncodeToString([]byte(key)),
	)
	keyring, err := privacy.ParseKeyring([]byte(document))
	if err != nil {
		t.Fatalf("ParseKeyring() error = %v", err)
	}
	return keyring
}

func testPIIRotatedKeyring(t *testing.T) *privacy.Keyring {
	t.Helper()
	document := fmt.Sprintf(
		`{"active":"v2","lookup_key":%q,"keys":{"v1":%q,"v2":%q}}`,
		base64.StdEncoding.EncodeToString([]byte(strings.Repeat("v1", 32))),
		base64.StdEncoding.EncodeToString([]byte(strings.Repeat("v1", 32))),
		base64.StdEncoding.EncodeToString([]byte(strings.Repeat("v2", 32))),
	)
	keyring, err := privacy.ParseKeyring([]byte(document))
	if err != nil {
		t.Fatalf("ParseKeyring() error = %v", err)
	}
	return keyring
}
