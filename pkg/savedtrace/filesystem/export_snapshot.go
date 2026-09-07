package filesystem

import (
	"bufio"
	"bytes"
	"container/heap"
	"context"
	"encoding/binary"
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

	"github.com/sparksq/sparkroute/internal/privatepath"

	"github.com/sparksq/sparkroute/pkg/savedtrace"
)

const (
	exportWorkDirectory      = ".export-work"
	exportSnapshotPrefix     = "snapshot-"
	indexEntrySize           = 128
	indexRequestIDBytes      = 96
	defaultExportMemoryBytes = 64 << 20
	defaultExportDiskBytes   = 8 << 30
	defaultExportMergeFanIn  = 32
	defaultExportOpenFiles   = 32
	maximumExportMemoryBytes = 1 << 30
	maximumExportMergeFanIn  = 256
	maximumExportOpenFiles   = 256
	maximumExportSourceFiles = 65_536
	indexReaderBufferBytes   = 64 << 10
	indexWriterBufferBytes   = 256 << 10
	staleExportWorkAge       = 7 * 24 * time.Hour
)

var (
	ErrExportDiskBudget = errors.New("filesystem saved trace export disk budget exceeded")
	ErrSnapshotClosed   = errors.New("filesystem saved trace snapshot is closed")
)

type exportOptions struct {
	memoryBytes int64
	diskBytes   int64
	mergeFanIn  int
	openFiles   int
}

func normalizeExportOptions(options Options) (exportOptions, error) {
	if options.ExportMemoryBytes < 0 || options.ExportDiskBytes < 0 ||
		options.ExportMergeFanIn < 0 || options.ExportOpenFiles < 0 {
		return exportOptions{}, fmt.Errorf("filesystem trace export limits must not be negative")
	}
	result := exportOptions{
		memoryBytes: options.ExportMemoryBytes,
		diskBytes:   options.ExportDiskBytes,
		mergeFanIn:  options.ExportMergeFanIn,
		openFiles:   options.ExportOpenFiles,
	}
	if result.memoryBytes == 0 {
		result.memoryBytes = defaultExportMemoryBytes
	}
	if result.diskBytes == 0 {
		result.diskBytes = defaultExportDiskBytes
	}
	if result.mergeFanIn == 0 {
		result.mergeFanIn = defaultExportMergeFanIn
	}
	if result.openFiles == 0 {
		result.openFiles = defaultExportOpenFiles
	}
	if result.memoryBytes < indexEntrySize || result.memoryBytes > maximumExportMemoryBytes {
		return exportOptions{}, fmt.Errorf(
			"filesystem trace export memory must be between %d and %d bytes",
			indexEntrySize,
			maximumExportMemoryBytes,
		)
	}
	if result.diskBytes < indexEntrySize {
		return exportOptions{}, fmt.Errorf(
			"filesystem trace export disk budget must be at least %d bytes",
			indexEntrySize,
		)
	}
	if result.mergeFanIn < 2 || result.mergeFanIn > maximumExportMergeFanIn {
		return exportOptions{}, fmt.Errorf(
			"filesystem trace export merge fan-in must be between 2 and %d",
			maximumExportMergeFanIn,
		)
	}
	if result.openFiles < 1 || result.openFiles > maximumExportOpenFiles {
		return exportOptions{}, fmt.Errorf(
			"filesystem trace export open-file limit must be between 1 and %d",
			maximumExportOpenFiles,
		)
	}
	return result, nil
}

func (s *Store) exportRoot() string {
	return filepath.Join(s.root, exportWorkDirectory)
}

func (s *Store) prepareExportWork() error {
	root := s.exportRoot()
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(root, 0o700); err != nil {
			return fmt.Errorf("create saved trace export work directory: %w", err)
		}
		info, err = os.Lstat(root)
	}
	if err != nil {
		return fmt.Errorf("inspect saved trace export work directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || privatepath.Check(root, info.Mode()) != nil {
		return fmt.Errorf("saved trace export work path must be an owner-only directory")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return fmt.Errorf("list saved trace export work directory: %w", err)
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), exportSnapshotPrefix) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("inspect stale saved trace export work: %w", err)
		}
		if time.Since(info.ModTime()) < staleExportWorkAge {
			continue
		}
		if err := os.RemoveAll(filepath.Join(root, entry.Name())); err != nil {
			return fmt.Errorf("remove stale saved trace export work: %w", err)
		}
	}
	return nil
}

type snapshotSource struct {
	path string
	size int64
}

func (s *Store) snapshotSources(ctx context.Context) ([]snapshotSource, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	exportRoot := s.exportRoot()
	captureRoot := filepath.Join(s.root, captureSessionDirectory)
	sources := make([]snapshotSource, 0, 32)
	err := filepath.WalkDir(s.root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() && (path == exportRoot || path == captureRoot) {
			return filepath.SkipDir
		}
		if entry.IsDir() || (filepath.Ext(entry.Name()) != ".json" && filepath.Ext(entry.Name()) != ".jsonl") {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || privatepath.Check(path, info.Mode()) != nil {
			return fmt.Errorf("saved trace journal %q must be an owner-only regular file", path)
		}
		if info.Size() > fileReadLimit(path) {
			return fmt.Errorf("saved trace journal %q exceeds the supported read limit", path)
		}
		if len(sources) >= maximumExportSourceFiles {
			return fmt.Errorf(
				"saved trace export exceeds the %d source-file limit",
				maximumExportSourceFiles,
			)
		}
		sources = append(sources, snapshotSource{path: path, size: info.Size()})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan saved trace directory: %w", err)
	}
	sort.Slice(sources, func(left, right int) bool {
		return sources[left].path < sources[right].path
	})
	return sources, nil
}

type indexEntry struct {
	startedNS       int64
	offset          int64
	length          int64
	pathID          uint32
	requestIDLength uint16
	requestID       [indexRequestIDBytes]byte
}

func makeIndexEntry(
	record savedtrace.Record,
	pathID int,
	offset int64,
	length int64,
) (indexEntry, error) {
	if len(record.RequestID) > indexRequestIDBytes {
		return indexEntry{}, fmt.Errorf("saved trace request ID exceeds index capacity")
	}
	if pathID < 0 || pathID >= maximumExportSourceFiles || offset < 0 || length <= 0 {
		return indexEntry{}, fmt.Errorf("saved trace journal index location is invalid")
	}
	entry := indexEntry{
		startedNS:       record.StartedAt.UnixNano(),
		offset:          offset,
		length:          length,
		pathID:          uint32(pathID),
		requestIDLength: uint16(len(record.RequestID)),
	}
	copy(entry.requestID[:], record.RequestID)
	return entry, nil
}

func (e indexEntry) requestIDBytes() []byte {
	return e.requestID[:e.requestIDLength]
}

func (e indexEntry) requestIDString() string {
	return string(e.requestIDBytes())
}

func indexEntryBefore(left, right indexEntry) bool {
	if left.startedNS != right.startedNS {
		return left.startedNS > right.startedNS
	}
	if compared := bytes.Compare(left.requestIDBytes(), right.requestIDBytes()); compared != 0 {
		return compared > 0
	}
	if left.pathID != right.pathID {
		return left.pathID < right.pathID
	}
	return left.offset < right.offset
}

func scanSnapshotSources(
	ctx context.Context,
	sources []snapshotSource,
	query savedtrace.Query,
	plan savedtrace.QueryPlan,
	visit func(indexEntry) error,
) error {
	for pathID, source := range sources {
		if err := ctx.Err(); err != nil {
			return err
		}
		file, err := os.Open(source.path)
		if err != nil {
			return fmt.Errorf("open saved trace %q: %w", source.path, err)
		}
		decoder := json.NewDecoder(bufio.NewReaderSize(
			io.NewSectionReader(file, 0, source.size),
			indexReaderBufferBytes,
		))
		for {
			if err := ctx.Err(); err != nil {
				_ = file.Close()
				return err
			}
			start := decoder.InputOffset()
			var record savedtrace.Record
			if err := decoder.Decode(&record); err != nil {
				if errors.Is(err, io.EOF) {
					break
				}
				_ = file.Close()
				return fmt.Errorf("decode saved trace %q: %w", source.path, err)
			}
			end := decoder.InputOffset()
			if err := record.Validate(); err != nil {
				_ = file.Close()
				return fmt.Errorf("validate saved trace %q: %w", source.path, err)
			}
			if !savedtrace.Matches(record, query, plan) {
				continue
			}
			entry, err := makeIndexEntry(record, pathID, start, end-start)
			if err != nil {
				_ = file.Close()
				return err
			}
			if err := visit(entry); err != nil {
				_ = file.Close()
				return err
			}
		}
		if err := file.Close(); err != nil {
			return fmt.Errorf("close saved trace %q: %w", source.path, err)
		}
	}
	return nil
}

func collectNewestEntries(
	ctx context.Context,
	sources []snapshotSource,
	query savedtrace.Query,
	plan savedtrace.QueryPlan,
	limit int,
) ([]indexEntry, error) {
	entries := make([]indexEntry, 0, limit)
	err := scanSnapshotSources(ctx, sources, query, plan, func(entry indexEntry) error {
		position := sort.Search(len(entries), func(index int) bool {
			return indexEntryBefore(entry, entries[index])
		})
		if position >= limit {
			return nil
		}
		entries = append(entries, indexEntry{})
		copy(entries[position+1:], entries[position:])
		entries[position] = entry
		if len(entries) > limit {
			entries = entries[:limit]
		}
		return nil
	})
	return entries, err
}

type indexRun struct {
	path  string
	size  int64
	count int64
}

type indexBuilder struct {
	ctx       context.Context
	workspace string
	options   exportOptions
	diskUsed  int64
	sequence  int
}

func (b *indexBuilder) reserveDisk(bytes int64) error {
	if bytes < 0 || bytes > b.options.diskBytes || b.diskUsed > b.options.diskBytes-bytes {
		return fmt.Errorf(
			"%w: need %d additional bytes with %d of %d already used",
			ErrExportDiskBudget,
			bytes,
			b.diskUsed,
			b.options.diskBytes,
		)
	}
	return nil
}

func (b *indexBuilder) nextPath(label string) string {
	b.sequence++
	return filepath.Join(b.workspace, fmt.Sprintf("%s-%06d.idx", label, b.sequence))
}

func (b *indexBuilder) writeRun(entries []indexEntry) (indexRun, error) {
	if err := b.ctx.Err(); err != nil {
		return indexRun{}, err
	}
	sort.Slice(entries, func(left, right int) bool {
		return indexEntryBefore(entries[left], entries[right])
	})
	size := int64(len(entries)) * indexEntrySize
	if err := b.reserveDisk(size); err != nil {
		return indexRun{}, err
	}
	path := b.nextPath("run")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return indexRun{}, fmt.Errorf("create saved trace export index: %w", err)
	}
	writer := bufio.NewWriterSize(file, indexWriterBufferBytes)
	writeErr := writeIndexEntries(b.ctx, writer, entries)
	flushErr := writer.Flush()
	closeErr := file.Close()
	if err := errors.Join(writeErr, flushErr, closeErr); err != nil {
		_ = os.Remove(path)
		return indexRun{}, fmt.Errorf("write saved trace export index: %w", err)
	}
	b.diskUsed += size
	return indexRun{path: path, size: size, count: int64(len(entries))}, nil
}

func (b *indexBuilder) emptyRun() (indexRun, error) {
	path := b.nextPath("empty")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return indexRun{}, fmt.Errorf("create empty saved trace export index: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return indexRun{}, err
	}
	return indexRun{path: path}, nil
}

func writeIndexEntries(ctx context.Context, writer io.Writer, entries []indexEntry) error {
	var encoded [indexEntrySize]byte
	for index, entry := range entries {
		if index%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		encodeIndexEntry(encoded[:], entry)
		if _, err := writer.Write(encoded[:]); err != nil {
			return err
		}
	}
	return nil
}

func encodeIndexEntry(target []byte, entry indexEntry) {
	clear(target)
	binary.LittleEndian.PutUint64(target[0:8], uint64(entry.startedNS))
	binary.LittleEndian.PutUint64(target[8:16], uint64(entry.offset))
	binary.LittleEndian.PutUint64(target[16:24], uint64(entry.length))
	binary.LittleEndian.PutUint32(target[24:28], entry.pathID)
	binary.LittleEndian.PutUint16(target[28:30], entry.requestIDLength)
	copy(target[30:126], entry.requestID[:])
}

func decodeIndexEntry(source []byte) (indexEntry, error) {
	if len(source) != indexEntrySize {
		return indexEntry{}, fmt.Errorf("saved trace export index entry has invalid size")
	}
	requestIDLength := binary.LittleEndian.Uint16(source[28:30])
	if requestIDLength > indexRequestIDBytes {
		return indexEntry{}, fmt.Errorf("saved trace export index request ID is invalid")
	}
	entry := indexEntry{
		startedNS:       int64(binary.LittleEndian.Uint64(source[0:8])),
		offset:          int64(binary.LittleEndian.Uint64(source[8:16])),
		length:          int64(binary.LittleEndian.Uint64(source[16:24])),
		pathID:          binary.LittleEndian.Uint32(source[24:28]),
		requestIDLength: requestIDLength,
	}
	copy(entry.requestID[:], source[30:126])
	if entry.offset < 0 || entry.length <= 0 {
		return indexEntry{}, fmt.Errorf("saved trace export index location is invalid")
	}
	return entry, nil
}

type runReader struct {
	file      *os.File
	reader    *bufio.Reader
	remaining int64
}

func openRunReader(run indexRun) (*runReader, error) {
	if run.size%indexEntrySize != 0 || run.count != run.size/indexEntrySize {
		return nil, fmt.Errorf("saved trace export index run metadata is invalid")
	}
	file, err := os.Open(run.path)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || privatepath.Check(run.path, info.Mode()) != nil || info.Size() != run.size {
		_ = file.Close()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("saved trace export index run is not an owner-only regular file")
	}
	return &runReader{
		file: file, reader: bufio.NewReaderSize(file, indexReaderBufferBytes),
		remaining: run.count,
	}, nil
}

func (r *runReader) next() (indexEntry, bool, error) {
	if r.remaining == 0 {
		return indexEntry{}, false, nil
	}
	var encoded [indexEntrySize]byte
	if _, err := io.ReadFull(r.reader, encoded[:]); err != nil {
		return indexEntry{}, false, err
	}
	entry, err := decodeIndexEntry(encoded[:])
	if err != nil {
		return indexEntry{}, false, err
	}
	r.remaining--
	return entry, true, nil
}

func (r *runReader) close() error {
	return r.file.Close()
}

type mergeItem struct {
	entry  indexEntry
	reader int
}

type mergeHeap []mergeItem

func (h mergeHeap) Len() int { return len(h) }
func (h mergeHeap) Less(left, right int) bool {
	return indexEntryBefore(h[left].entry, h[right].entry)
}
func (h mergeHeap) Swap(left, right int) { h[left], h[right] = h[right], h[left] }
func (h *mergeHeap) Push(value any)      { *h = append(*h, value.(mergeItem)) }
func (h *mergeHeap) Pop() any {
	old := *h
	last := old[len(old)-1]
	*h = old[:len(old)-1]
	return last
}

func (b *indexBuilder) mergeGroup(runs []indexRun) (result indexRun, resultErr error) {
	var size int64
	var count int64
	for _, run := range runs {
		size += run.size
		count += run.count
	}
	if err := b.reserveDisk(size); err != nil {
		return indexRun{}, err
	}
	path := b.nextPath("merge")
	output, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return indexRun{}, fmt.Errorf("create merged saved trace export index: %w", err)
	}
	defer func() {
		if resultErr != nil {
			_ = output.Close()
			_ = os.Remove(path)
		}
	}()
	readers := make([]*runReader, 0, len(runs))
	closeReaders := func() error {
		var closeErr error
		for _, reader := range readers {
			closeErr = errors.Join(closeErr, reader.close())
		}
		readers = nil
		return closeErr
	}
	defer func() {
		resultErr = errors.Join(resultErr, closeReaders())
	}()
	queue := make(mergeHeap, 0, len(runs))
	for _, run := range runs {
		reader, err := openRunReader(run)
		if err != nil {
			return indexRun{}, err
		}
		readers = append(readers, reader)
		entry, found, err := reader.next()
		if err != nil {
			return indexRun{}, err
		}
		if found {
			heap.Push(&queue, mergeItem{entry: entry, reader: len(readers) - 1})
		}
	}
	writer := bufio.NewWriterSize(output, indexWriterBufferBytes)
	var encoded [indexEntrySize]byte
	written := int64(0)
	for queue.Len() != 0 {
		if written%4096 == 0 {
			if err := b.ctx.Err(); err != nil {
				return indexRun{}, err
			}
		}
		item := heap.Pop(&queue).(mergeItem)
		encodeIndexEntry(encoded[:], item.entry)
		if _, err := writer.Write(encoded[:]); err != nil {
			return indexRun{}, err
		}
		written++
		next, found, err := readers[item.reader].next()
		if err != nil {
			return indexRun{}, err
		}
		if found {
			heap.Push(&queue, mergeItem{entry: next, reader: item.reader})
		}
	}
	if written != count {
		return indexRun{}, fmt.Errorf("merged saved trace export index count is invalid")
	}
	if err := errors.Join(writer.Flush(), output.Close()); err != nil {
		return indexRun{}, err
	}
	if err := closeReaders(); err != nil {
		return indexRun{}, err
	}
	b.diskUsed += size
	for _, run := range runs {
		if err := os.Remove(run.path); err != nil {
			return indexRun{}, fmt.Errorf("remove merged saved trace export run: %w", err)
		}
		b.diskUsed -= run.size
	}
	return indexRun{path: path, size: size, count: count}, nil
}

func (b *indexBuilder) mergeAll(runs []indexRun) (indexRun, error) {
	if len(runs) == 0 {
		return b.emptyRun()
	}
	for len(runs) > 1 {
		next := make([]indexRun, 0, (len(runs)+b.options.mergeFanIn-1)/b.options.mergeFanIn)
		for start := 0; start < len(runs); start += b.options.mergeFanIn {
			end := min(start+b.options.mergeFanIn, len(runs))
			if end-start == 1 {
				next = append(next, runs[start])
				continue
			}
			merged, err := b.mergeGroup(runs[start:end])
			if err != nil {
				return indexRun{}, err
			}
			next = append(next, merged)
		}
		runs = next
	}
	return runs[0], nil
}

func (s *Store) OpenRecordSnapshot(
	ctx context.Context,
	query savedtrace.Query,
	maximum int,
) (savedtrace.RecordSnapshot, error) {
	if maximum < 0 {
		return nil, fmt.Errorf("saved trace visit limit must not be negative")
	}
	plan, err := savedtrace.PlanQuery(&query)
	if err != nil {
		return nil, err
	}
	sources, err := s.snapshotSources(ctx)
	if err != nil {
		return nil, err
	}
	workspace, err := os.MkdirTemp(s.exportRoot(), exportSnapshotPrefix)
	if err != nil {
		return nil, fmt.Errorf("create saved trace export snapshot: %w", err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(workspace)
		}
	}()
	if err := os.Chmod(workspace, 0o700); err != nil {
		return nil, err
	}
	builder := &indexBuilder{ctx: ctx, workspace: workspace, options: s.exportOptions}
	maximumEntries := int(s.exportOptions.memoryBytes / indexEntrySize)
	entries := make([]indexEntry, 0, maximumEntries)
	runs := make([]indexRun, 0, 8)
	var matching int64
	if err := scanSnapshotSources(ctx, sources, query, plan, func(entry indexEntry) error {
		entries = append(entries, entry)
		matching++
		if len(entries) < maximumEntries {
			return nil
		}
		run, err := builder.writeRun(entries)
		if err != nil {
			return err
		}
		runs = append(runs, run)
		entries = entries[:0]
		return nil
	}); err != nil {
		return nil, err
	}
	if len(entries) != 0 {
		run, err := builder.writeRun(entries)
		if err != nil {
			return nil, err
		}
		runs = append(runs, run)
	}
	finalRun, err := builder.mergeAll(runs)
	if err != nil {
		return nil, err
	}
	cleanup = false
	return &recordSnapshot{
		workspace: workspace,
		indexPath: finalRun.path,
		sources:   sources, matching: matching, maximum: maximum,
		openFiles: s.exportOptions.openFiles,
	}, nil
}

type recordSnapshot struct {
	mu        sync.Mutex
	workspace string
	indexPath string
	sources   []snapshotSource
	matching  int64
	maximum   int
	openFiles int
	closed    bool
}

func (s *recordSnapshot) Limited() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.maximum > 0 && s.matching > int64(s.maximum)
}

func (s *recordSnapshot) Visit(ctx context.Context, visit func(savedtrace.Record) error) (resultErr error) {
	if visit == nil {
		return fmt.Errorf("saved trace visitor is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrSnapshotClosed
	}
	if err := os.Chtimes(s.workspace, time.Now(), time.Now()); err != nil {
		return fmt.Errorf("refresh saved trace export snapshot lease: %w", err)
	}
	indexFile, err := os.Open(s.indexPath)
	if err != nil {
		return fmt.Errorf("open saved trace export snapshot: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, indexFile.Close()) }()
	info, err := indexFile.Stat()
	if err != nil || !info.Mode().IsRegular() || privatepath.Check(s.indexPath, info.Mode()) != nil || info.Size()%indexEntrySize != 0 {
		if err != nil {
			return err
		}
		return fmt.Errorf("saved trace export snapshot index is invalid")
	}
	cache := newSourceFileCache(s.sources, s.openFiles)
	defer func() { resultErr = errors.Join(resultErr, cache.Close()) }()
	reader := bufio.NewReaderSize(indexFile, indexReaderBufferBytes)
	visited := 0
	for {
		if s.maximum > 0 && visited >= s.maximum {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		var encoded [indexEntrySize]byte
		_, err := io.ReadFull(reader, encoded[:])
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read saved trace export snapshot: %w", err)
		}
		entry, err := decodeIndexEntry(encoded[:])
		if err != nil {
			return err
		}
		record, err := cache.Load(entry)
		if err != nil {
			return err
		}
		if err := visit(record); err != nil {
			return err
		}
		visited++
	}
}

func (s *recordSnapshot) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	if err := os.RemoveAll(s.workspace); err != nil {
		return fmt.Errorf("remove saved trace export snapshot: %w", err)
	}
	return nil
}

type cachedSourceFile struct {
	file     *os.File
	lastUsed uint64
}

type sourceFileCache struct {
	sources []snapshotSource
	limit   int
	clock   uint64
	files   map[uint32]cachedSourceFile
}

func newSourceFileCache(sources []snapshotSource, limit int) *sourceFileCache {
	return &sourceFileCache{
		sources: sources,
		limit:   limit,
		files:   make(map[uint32]cachedSourceFile, min(limit, len(sources))),
	}
}

func (c *sourceFileCache) get(pathID uint32) (*os.File, snapshotSource, error) {
	if int(pathID) >= len(c.sources) {
		return nil, snapshotSource{}, fmt.Errorf("saved trace export source ID is invalid")
	}
	c.clock++
	if cached, ok := c.files[pathID]; ok {
		cached.lastUsed = c.clock
		c.files[pathID] = cached
		return cached.file, c.sources[pathID], nil
	}
	if len(c.files) >= c.limit {
		var oldestID uint32
		oldestClock := ^uint64(0)
		for identifier, cached := range c.files {
			if cached.lastUsed < oldestClock {
				oldestID, oldestClock = identifier, cached.lastUsed
			}
		}
		if err := c.files[oldestID].file.Close(); err != nil {
			return nil, snapshotSource{}, err
		}
		delete(c.files, oldestID)
	}
	source := c.sources[pathID]
	file, err := os.Open(source.path)
	if err != nil {
		return nil, snapshotSource{}, fmt.Errorf("open saved trace snapshot source: %w", err)
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || privatepath.Check(source.path, info.Mode()) != nil || info.Size() < source.size {
		_ = file.Close()
		if err != nil {
			return nil, snapshotSource{}, err
		}
		return nil, snapshotSource{}, fmt.Errorf("saved trace snapshot source changed incompatibly")
	}
	c.files[pathID] = cachedSourceFile{file: file, lastUsed: c.clock}
	return file, source, nil
}

func (c *sourceFileCache) Load(entry indexEntry) (savedtrace.Record, error) {
	file, source, err := c.get(entry.pathID)
	if err != nil {
		return savedtrace.Record{}, err
	}
	if entry.offset > source.size || entry.length > source.size-entry.offset {
		return savedtrace.Record{}, fmt.Errorf("saved trace export source location is out of bounds")
	}
	decoder := json.NewDecoder(io.NewSectionReader(file, entry.offset, entry.length))
	var record savedtrace.Record
	if err := decoder.Decode(&record); err != nil {
		return savedtrace.Record{}, fmt.Errorf("decode saved trace snapshot source: %w", err)
	}
	if err := record.Validate(); err != nil {
		return savedtrace.Record{}, fmt.Errorf("validate saved trace snapshot source: %w", err)
	}
	if record.StartedAt.UnixNano() != entry.startedNS || record.RequestID != entry.requestIDString() {
		return savedtrace.Record{}, fmt.Errorf("saved trace snapshot source no longer matches its index")
	}
	return record, nil
}

func (c *sourceFileCache) Close() error {
	var result error
	for identifier, cached := range c.files {
		result = errors.Join(result, cached.file.Close())
		delete(c.files, identifier)
	}
	return result
}

var _ savedtrace.RecordSnapshot = (*recordSnapshot)(nil)
