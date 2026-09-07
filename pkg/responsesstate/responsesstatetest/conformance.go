// Package responsesstatetest provides storage conformance checks for
// provider-owned resource state backends.
package responsesstatetest

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/sparksq/sparkroute/pkg/responsesstate"
)

// RunFileStoreConformance verifies caller isolation, deterministic ordering,
// filtering, cursor behavior, conflict handling, and tombstone visibility.
func RunFileStoreConformance(t *testing.T, store responsesstate.FileStore) {
	t.Helper()
	ctx := t.Context()
	prefix := fmt.Sprintf("file-conformance-%d-", time.Now().UnixNano())
	scope := prefix + "scope-a"
	otherScope := prefix + "scope-b"
	boundAt := time.Now().UTC()

	bind := func(
		scope, id, purpose string,
		createdAt int64,
		expiresAt ...int64,
	) responsesstate.FileRecord {
		t.Helper()
		affinity := responsesstate.ResourceAffinity{
			ResourceKey: responsesstate.ResourceKey{
				Scope: scope, Kind: responsesstate.ResourceFile, ResourceID: id,
			},
			VirtualModel: "public", Provider: "provider",
			Deployment: "deployment", UpstreamModel: "upstream",
			BoundAt: boundAt,
		}
		record := responsesstate.FileRecord{
			Scope: scope, ID: id, Object: "file", Bytes: 10,
			CreatedAt: createdAt, Filename: id + ".txt", Purpose: purpose,
		}
		if len(expiresAt) > 0 {
			record.ExpiresAt = expiresAt[0]
		}
		if err := store.BindFile(ctx, affinity, record); err != nil {
			t.Fatalf("BindFile(%q) error = %v", id, err)
		}
		return record
	}

	oldest := bind(scope, prefix+"a", "user_data", 10)
	middle := bind(scope, prefix+"b", "fine-tune", 20)
	newest := bind(scope, prefix+"c", "user_data", 20)
	other := bind(otherScope, prefix+"other", "user_data", 30)
	_ = bind(scope, prefix+"expired", "user_data", 1, 2)

	page, err := store.ListFiles(ctx, responsesstate.FileQuery{
		Scope: scope, Limit: 2,
	})
	if err != nil {
		t.Fatalf("ListFiles() first page error = %v", err)
	}
	assertFileIDs(t, page.Records, newest.ID, middle.ID)
	if !page.HasMore {
		t.Fatal("ListFiles() first page HasMore = false")
	}
	page, err = store.ListFiles(ctx, responsesstate.FileQuery{
		Scope: scope, Limit: 2, After: middle.ID,
	})
	if err != nil {
		t.Fatalf("ListFiles() second page error = %v", err)
	}
	assertFileIDs(t, page.Records, oldest.ID)
	if page.HasMore {
		t.Fatal("ListFiles() terminal page HasMore = true")
	}

	page, err = store.ListFiles(ctx, responsesstate.FileQuery{
		Scope: scope, Purpose: "user_data",
	})
	if err != nil {
		t.Fatalf("ListFiles() purpose error = %v", err)
	}
	assertFileIDs(t, page.Records, newest.ID, oldest.ID)
	page, err = store.ListFiles(ctx, responsesstate.FileQuery{
		Scope: scope, Order: responsesstate.FileOrderAscending,
	})
	if err != nil {
		t.Fatalf("ListFiles() ascending error = %v", err)
	}
	assertFileIDs(t, page.Records, oldest.ID, middle.ID, newest.ID)
	page, err = store.ListFiles(ctx, responsesstate.FileQuery{Scope: otherScope})
	if err != nil {
		t.Fatalf("ListFiles() other scope error = %v", err)
	}
	assertFileIDs(t, page.Records, other.ID)

	for _, query := range []responsesstate.FileQuery{
		{Scope: scope, After: other.ID},
		{Scope: scope, Purpose: "user_data", After: middle.ID},
	} {
		if _, err := store.ListFiles(ctx, query); !errors.Is(err, responsesstate.ErrInvalidFileCursor) {
			t.Fatalf("ListFiles(%#v) error = %v, want invalid cursor", query, err)
		}
	}

	conflicting := middle
	conflicting.Filename = "different.txt"
	affinity, found, err := store.ResolveResource(ctx, responsesstate.ResourceKey{
		Scope: scope, Kind: responsesstate.ResourceFile, ResourceID: middle.ID,
	})
	if err != nil || !found {
		t.Fatalf("ResolveResource() = %#v, %v, %v", affinity, found, err)
	}
	if err := store.BindFile(ctx, affinity, conflicting); !errors.Is(err, responsesstate.ErrConflict) {
		t.Fatalf("BindFile() metadata conflict error = %v", err)
	}

	deletedAt := time.Now().UTC()
	if err := store.TombstoneResource(
		ctx,
		responsesstate.ResourceKey{
			Scope: scope, Kind: responsesstate.ResourceFile, ResourceID: middle.ID,
		},
		deletedAt,
		deletedAt.Add(time.Hour),
	); err != nil {
		t.Fatalf("TombstoneResource() error = %v", err)
	}
	page, err = store.ListFiles(ctx, responsesstate.FileQuery{Scope: scope})
	if err != nil {
		t.Fatalf("ListFiles() after delete error = %v", err)
	}
	assertFileIDs(t, page.Records, newest.ID, oldest.ID)
	page, err = store.ListFiles(ctx, responsesstate.FileQuery{
		Scope: scope, After: middle.ID,
	})
	if err != nil {
		t.Fatalf("ListFiles() deleted cursor error = %v", err)
	}
	assertFileIDs(t, page.Records, oldest.ID)
}

func assertFileIDs(
	t *testing.T,
	records []responsesstate.FileRecord,
	want ...string,
) {
	t.Helper()
	if len(records) != len(want) {
		t.Fatalf("file count = %d, want %d: %#v", len(records), len(want), records)
	}
	for index := range want {
		if records[index].ID != want[index] {
			t.Fatalf("file %d ID = %q, want %q", index, records[index].ID, want[index])
		}
	}
}
