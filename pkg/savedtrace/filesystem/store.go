// Package filesystem implements owner-only saved trace persistence for
// standalone, single-node deployments.
package filesystem

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sparksq/sparkroute/pkg/savedtrace"
)

type Store struct {
	root          string
	mu            sync.Mutex
	exportOptions exportOptions
}

const journalName = "saved-traces.jsonl"
const captureSessionDirectory = ".capture-sessions"

// Options bounds temporary resources used to order filesystem exports. Zero
// values select conservative defaults. The limits exclude the one trace record
// currently being decoded because a caller may configure bodies larger than
// the index-memory budget itself.
type Options struct {
	ExportMemoryBytes int64
	ExportDiskBytes   int64
	ExportMergeFanIn  int
	ExportOpenFiles   int
}

func Open(root string) (*Store, error) {
	return OpenWithOptions(root, Options{})
}

func OpenWithOptions(root string, options Options) (*Store, error) {
	if strings.TrimSpace(root) == "" {
		return nil, fmt.Errorf("saved trace filesystem directory is required")
	}
	exportOptions, err := normalizeExportOptions(options)
	if err != nil {
		return nil, err
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve saved trace directory: %w", err)
	}
	if err := os.MkdirAll(absolute, 0o700); err != nil {
		return nil, fmt.Errorf("create saved trace directory: %w", err)
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return nil, fmt.Errorf("inspect saved trace directory: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("saved trace path %q is not a directory", absolute)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf(
			"saved trace directory %q must not grant group or other permissions",
			absolute,
		)
	}
	store := &Store{root: absolute, exportOptions: exportOptions}
	if err := store.prepareExportWork(); err != nil {
		return nil, err
	}
	if err := store.repairJournalTails(); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *Store) repairJournalTails() error {
	return filepath.WalkDir(s.root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() && path == s.exportRoot() {
			return filepath.SkipDir
		}
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".jsonl" {
			return nil
		}
		if err := repairJournalTail(path); err != nil {
			return fmt.Errorf("repair saved trace journal %q: %w", path, err)
		}
		return nil
	})
}

func repairJournalTail(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("journal must be an owner-only regular file")
	}
	if info.Size() == 0 {
		return nil
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	last := []byte{0}
	if _, err := file.ReadAt(last, info.Size()-1); err != nil {
		return err
	}
	if last[0] == '\n' {
		return nil
	}
	const chunkSize int64 = 64 << 10
	buffer := make([]byte, chunkSize)
	position := info.Size()
	truncateAt := int64(0)
	for position > 0 {
		start := max64(0, position-chunkSize)
		chunk := buffer[:position-start]
		if _, err := file.ReadAt(chunk, start); err != nil {
			return err
		}
		if newline := bytes.LastIndexByte(chunk, '\n'); newline >= 0 {
			truncateAt = start + int64(newline) + 1
			break
		}
		position = start
	}
	if err := file.Truncate(truncateAt); err != nil {
		return err
	}
	return file.Sync()
}

func (s *Store) Append(ctx context.Context, record savedtrace.Record) error {
	return s.AppendBatch(ctx, []savedtrace.Record{record})
}

func (s *Store) AppendTraceBatch(ctx context.Context, records []savedtrace.Record) error {
	return s.AppendBatch(ctx, records)
}

func (s *Store) AppendBatch(ctx context.Context, records []savedtrace.Record) error {
	if len(records) == 0 {
		return nil
	}
	partitions := make(map[string]*bytes.Buffer)
	for _, record := range records {
		if err := record.Validate(); err != nil {
			return fmt.Errorf("validate filesystem saved trace: %w", err)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		directory := filepath.Join(
			s.root,
			record.StartedAt.UTC().Format("2006"),
			record.StartedAt.UTC().Format("01"),
			record.StartedAt.UTC().Format("02"),
		)
		buffer := partitions[directory]
		if buffer == nil {
			buffer = &bytes.Buffer{}
			partitions[directory] = buffer
		}
		encoder := json.NewEncoder(buffer)
		encoder.SetEscapeHTML(false)
		if err := encoder.Encode(record); err != nil {
			return fmt.Errorf("encode saved trace: %w", err)
		}
	}
	directories := make([]string, 0, len(partitions))
	for directory := range partitions {
		directories = append(directories, directory)
	}
	sort.Strings(directories)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, directory := range directories {
		if err := s.appendJournal(ctx, directory, partitions[directory].Bytes()); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) appendJournal(ctx context.Context, directory string, payload []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create saved trace date directory: %w", err)
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return fmt.Errorf("secure saved trace date directory: %w", err)
	}
	target := filepath.Join(directory, journalName)
	_, statErr := os.Lstat(target)
	created := errors.Is(statErr, os.ErrNotExist)
	if statErr != nil && !created {
		return fmt.Errorf("inspect saved trace journal: %w", statErr)
	}
	file, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open saved trace journal: %w", err)
	}
	cleanup := func() error {
		return file.Close()
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		_ = cleanup()
		if err != nil {
			return fmt.Errorf("inspect saved trace journal: %w", err)
		}
		return fmt.Errorf("saved trace journal must be an owner-only regular file")
	}
	start, err := file.Seek(0, io.SeekEnd)
	if err != nil {
		_ = cleanup()
		return fmt.Errorf("seek saved trace journal: %w", err)
	}
	rollback := func(cause error) error {
		truncateErr := file.Truncate(start)
		syncErr := file.Sync()
		closeErr := cleanup()
		return errors.Join(cause, truncateErr, syncErr, closeErr)
	}
	written := 0
	for written < len(payload) {
		count, writeErr := file.Write(payload[written:])
		written += count
		if writeErr != nil {
			return fmt.Errorf("write saved trace journal: %w", rollback(writeErr))
		}
		if count == 0 {
			return fmt.Errorf("write saved trace journal: %w", rollback(io.ErrShortWrite))
		}
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync saved trace journal: %w", rollback(err))
	}
	if err := cleanup(); err != nil {
		return fmt.Errorf("close saved trace journal: %w", err)
	}
	if created {
		directoryFile, err := os.Open(directory)
		if err != nil {
			return fmt.Errorf("open saved trace journal directory: %w", err)
		}
		syncErr := directoryFile.Sync()
		closeErr := directoryFile.Close()
		if err := errors.Join(syncErr, closeErr); err != nil {
			return fmt.Errorf("sync saved trace journal directory: %w", err)
		}
	}
	return nil
}

func (s *Store) List(ctx context.Context, query savedtrace.Query) (savedtrace.Page, error) {
	plan, err := savedtrace.PlanQuery(&query)
	if err != nil {
		return savedtrace.Page{}, err
	}
	sources, err := s.snapshotSources(ctx)
	if err != nil {
		return savedtrace.Page{}, err
	}
	entries, err := collectNewestEntries(
		ctx,
		sources,
		query,
		plan,
		plan.Limit+1,
	)
	if err != nil {
		return savedtrace.Page{}, err
	}
	hasMore := len(entries) > plan.Limit
	if hasMore {
		entries = entries[:plan.Limit]
	}
	records := make([]savedtrace.Record, 0, len(entries))
	cache := newSourceFileCache(sources, s.exportOptions.openFiles)
	defer cache.Close()
	for _, entry := range entries {
		record, loadErr := cache.Load(entry)
		if loadErr != nil {
			return savedtrace.Page{}, loadErr
		}
		records = append(records, record)
	}
	page := savedtrace.Page{Records: records}
	if hasMore {
		page.NextCursor, err = savedtrace.NextCursor(records[len(records)-1])
		if err != nil {
			return savedtrace.Page{}, err
		}
	}
	return page, nil
}

// VisitRecords preserves the legacy visitor contract while using one bounded,
// replayable ordered snapshot internally.
func (s *Store) VisitRecords(
	ctx context.Context,
	query savedtrace.Query,
	maximum int,
	visit func(savedtrace.Record) error,
) (bool, error) {
	if visit == nil {
		return false, fmt.Errorf("saved trace visitor is required")
	}
	if maximum < 0 {
		return false, fmt.Errorf("saved trace visit limit must not be negative")
	}
	snapshot, err := s.OpenRecordSnapshot(ctx, query, maximum)
	if err != nil {
		return false, err
	}
	visitErr := snapshot.Visit(ctx, visit)
	return snapshot.Limited(), errors.Join(visitErr, snapshot.Close())
}

func fileReadLimit(path string) int64 {
	if filepath.Ext(path) == ".jsonl" {
		return 1 << 40
	}
	return int64(2*savedtrace.MaxBodyBytes + savedtrace.MaxMetadataBytes + (1 << 20))
}

func max64(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
}

func (s *Store) Close() error {
	return nil
}

func (s *Store) UpsertCaptureSession(ctx context.Context, session savedtrace.CaptureSession) error {
	if err := session.Validate(); err != nil {
		return fmt.Errorf("validate filesystem capture session: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	payload, err := json.Marshal(session)
	if err != nil {
		return fmt.Errorf("encode filesystem capture session: %w", err)
	}
	payload = append(payload, '\n')
	directory := filepath.Join(s.root, captureSessionDirectory)
	digest := sha256.Sum256([]byte(session.ID))
	target := filepath.Join(directory, fmt.Sprintf("%x.session", digest))
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".session-*")
	if err != nil {
		return fmt.Errorf("create capture session temporary file: %w", err)
	}
	temporaryName := temporary.Name()
	cleanup := func() { _ = temporary.Close(); _ = os.Remove(temporaryName) }
	if err := temporary.Chmod(0o600); err != nil {
		cleanup()
		return err
	}
	if _, err := temporary.Write(payload); err != nil {
		cleanup()
		return err
	}
	if err := temporary.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := temporary.Close(); err != nil {
		_ = os.Remove(temporaryName)
		return err
	}
	if err := os.Rename(temporaryName, target); err != nil {
		_ = os.Remove(temporaryName)
		return err
	}
	directoryFile, err := os.Open(directory)
	if err != nil {
		return err
	}
	err = errors.Join(directoryFile.Sync(), directoryFile.Close())
	if err != nil {
		return fmt.Errorf("sync capture session directory: %w", err)
	}
	return nil
}

func (s *Store) ListCaptureSessions(ctx context.Context, query savedtrace.CaptureSessionQuery) ([]savedtrace.CaptureSession, error) {
	directory := filepath.Join(s.root, captureSessionDirectory)
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list capture session directory: %w", err)
	}
	sessions := make([]savedtrace.CaptureSession, 0, len(entries))
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".session" {
			continue
		}
		file, err := os.Open(filepath.Join(directory, entry.Name()))
		if err != nil {
			return nil, err
		}
		var session savedtrace.CaptureSession
		decodeErr := json.NewDecoder(io.LimitReader(file, 1<<20)).Decode(&session)
		closeErr := file.Close()
		if err := errors.Join(decodeErr, closeErr); err != nil {
			return nil, err
		}
		if !query.StartedBefore.IsZero() && !session.StartedAt.Before(query.StartedBefore) {
			continue
		}
		if !query.UpdatedAfter.IsZero() && session.UpdatedAt.Before(query.UpdatedAfter) {
			continue
		}
		sessions = append(sessions, session)
	}
	sort.Slice(sessions, func(i, j int) bool {
		if sessions[i].StartedAt.Equal(sessions[j].StartedAt) {
			return sessions[i].ID < sessions[j].ID
		}
		return sessions[i].StartedAt.Before(sessions[j].StartedAt)
	})
	return sessions, nil
}

var _ savedtrace.CaptureSessionStore = (*Store)(nil)

var _ savedtrace.Store = (*Store)(nil)
var _ savedtrace.BatchStore = (*Store)(nil)
var _ savedtrace.RecordVisitor = (*Store)(nil)
var _ savedtrace.RecordSnapshotReader = (*Store)(nil)

var _ = time.Time{}
