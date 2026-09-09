// SPDX-FileCopyrightText: 2026 Scitrera LLC
// SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
// SPDX-License-Identifier: AGPL-3.0-only

package responsesstate

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	DefaultFileListLimit = 100
	MaxFileListLimit     = 1_000
	MaxFilenameBytes     = 1_024
	MaxPurposeBytes      = 256
)

var ErrInvalidFileCursor = errors.New("invalid file list cursor")

type FileOrder string

const (
	FileOrderAscending  FileOrder = "asc"
	FileOrderDescending FileOrder = "desc"
)

// FileRecord is the bounded, content-free metadata retained by the gateway
// after a successful provider upload. Scope is storage-only and is never
// serialized to a caller.
type FileRecord struct {
	Scope     string `json:"-"`
	ID        string `json:"id"`
	Object    string `json:"object"`
	Bytes     int64  `json:"bytes"`
	CreatedAt int64  `json:"created_at"`
	ExpiresAt int64  `json:"expires_at,omitempty"`
	Filename  string `json:"filename"`
	Purpose   string `json:"purpose"`
}

func (r FileRecord) Validate() error {
	if err := ValidateScope(r.Scope); err != nil {
		return err
	}
	if err := ValidateResourceID(r.ID); err != nil {
		return err
	}
	if r.Object != "file" {
		return fmt.Errorf("file object must be %q", "file")
	}
	if r.Bytes < 0 {
		return fmt.Errorf("file bytes must not be negative")
	}
	if r.CreatedAt < 0 {
		return fmt.Errorf("file creation time must not be negative")
	}
	if r.ExpiresAt != 0 && r.ExpiresAt <= r.CreatedAt {
		return fmt.Errorf("file expiry must be after creation time")
	}
	if err := validateFileString("filename", r.Filename, MaxFilenameBytes); err != nil {
		return err
	}
	return validateFileString("purpose", r.Purpose, MaxPurposeBytes)
}

type FileQuery struct {
	Scope   string
	Purpose string
	Limit   int
	Order   FileOrder
	After   string
}

func NormalizeFileQuery(query FileQuery) (FileQuery, error) {
	if err := ValidateScope(query.Scope); err != nil {
		return FileQuery{}, err
	}
	if err := validateFileString("purpose", query.Purpose, MaxPurposeBytes); err != nil {
		return FileQuery{}, err
	}
	if query.Limit == 0 {
		query.Limit = DefaultFileListLimit
	}
	if query.Limit < 1 || query.Limit > MaxFileListLimit {
		return FileQuery{}, fmt.Errorf(
			"file list limit must be between 1 and %d",
			MaxFileListLimit,
		)
	}
	if query.Order == "" {
		query.Order = FileOrderDescending
	}
	if query.Order != FileOrderAscending && query.Order != FileOrderDescending {
		return FileQuery{}, fmt.Errorf("file list order must be asc or desc")
	}
	if query.After != "" {
		if err := ValidateResourceID(query.After); err != nil {
			return FileQuery{}, fmt.Errorf("%w: %v", ErrInvalidFileCursor, err)
		}
	}
	return query, nil
}

type FilePage struct {
	Records []FileRecord
	HasMore bool
}

// FileStore atomically binds file routing affinity and list metadata. Lists
// are caller-scoped and ordered by provider creation time plus file ID.
type FileStore interface {
	ResourceStore
	BindFile(context.Context, ResourceAffinity, FileRecord) error
	ListFiles(context.Context, FileQuery) (FilePage, error)
}

func (s *MemoryStore) BindFile(
	ctx context.Context,
	affinity ResourceAffinity,
	record FileRecord,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := affinity.Validate(); err != nil {
		return err
	}
	if affinity.Kind != ResourceFile {
		return fmt.Errorf("file affinity must use resource kind %q", ResourceFile)
	}
	if err := record.Validate(); err != nil {
		return err
	}
	if record.Scope != affinity.Scope || record.ID != affinity.ResourceID {
		return fmt.Errorf("file metadata does not match routing affinity")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	key := memoryKey(affinity.ResourceKey)
	if existing, ok := s.files[key]; ok && existing != record {
		return ErrConflict
	}
	if err := s.bindResourceLocked(affinity); err != nil {
		return err
	}
	s.files[key] = record
	return nil
}

func (s *MemoryStore) ListFiles(
	ctx context.Context,
	query FileQuery,
) (FilePage, error) {
	if err := ctx.Err(); err != nil {
		return FilePage{}, err
	}
	query, err := NormalizeFileQuery(query)
	if err != nil {
		return FilePage{}, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	var cursor FileRecord
	if query.After != "" {
		cursorKey := memoryKey(ResourceKey{
			Scope: query.Scope, Kind: ResourceFile, ResourceID: query.After,
		})
		var ok bool
		cursor, ok = s.files[cursorKey]
		if !ok || query.Purpose != "" && cursor.Purpose != query.Purpose {
			return FilePage{}, ErrInvalidFileCursor
		}
	}

	records := make([]FileRecord, 0)
	for key, entry := range s.entries {
		if entry.affinity.Scope != query.Scope ||
			entry.affinity.Kind != ResourceFile ||
			entry.affinity.Deleted() ||
			resourceExpired(entry.affinity, s.now()) {
			continue
		}
		record, ok := s.files[key]
		if !ok || query.Purpose != "" && record.Purpose != query.Purpose {
			continue
		}
		if record.ExpiresAt != 0 && record.ExpiresAt <= s.now().Unix() {
			continue
		}
		if query.After != "" && !fileAfter(record, cursor, query.Order) {
			continue
		}
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool {
		return fileBefore(records[i], records[j], query.Order)
	})
	page := FilePage{Records: records}
	if len(page.Records) > query.Limit {
		page.HasMore = true
		page.Records = page.Records[:query.Limit]
	}
	return page, nil
}

func fileBefore(left, right FileRecord, order FileOrder) bool {
	if left.CreatedAt == right.CreatedAt {
		if order == FileOrderAscending {
			return left.ID < right.ID
		}
		return left.ID > right.ID
	}
	if order == FileOrderAscending {
		return left.CreatedAt < right.CreatedAt
	}
	return left.CreatedAt > right.CreatedAt
}

func fileAfter(record, cursor FileRecord, order FileOrder) bool {
	return fileBefore(cursor, record, order)
}

func validateFileString(name, value string, maxBytes int) error {
	if !utf8.ValidString(value) || len(value) > maxBytes {
		return fmt.Errorf("%s must be valid UTF-8 and at most %d bytes", name, maxBytes)
	}
	if strings.IndexByte(value, 0) >= 0 {
		return fmt.Errorf("%s must not contain NUL", name)
	}
	return nil
}

var _ FileStore = (*MemoryStore)(nil)
