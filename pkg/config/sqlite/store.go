// Package sqlite implements the ownership-aware, single-process mutable
// configuration source for the standalone gateway profile.
package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sparksq/sparkroute/internal/privatepath"

	"github.com/sparksq/sparkroute/pkg/config"
	"github.com/sparksq/sparkroute/pkg/config/managed"

	_ "modernc.org/sqlite"
)

var (
	ErrNoActiveConfiguration = errors.New("no active SQLite gateway configuration")
	ErrRevisionConflict      = managed.ErrRevisionConflict
	ErrManagedSetNotFound    = managed.ErrSetNotFound
	ErrInvalidConfiguration  = managed.ErrInvalidConfiguration
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

type Options struct {
	Path        string
	BusyTimeout time.Duration
	Validator   managed.Validator
}

type Store struct {
	db        *sql.DB
	validator managed.Validator
	writeMu   sync.Mutex
	watchMu   sync.Mutex
	watchers  map[uint64]chan config.Version
	nextWatch uint64
	closeOnce sync.Once
	closeErr  error
}

func Open(ctx context.Context, options Options) (*Store, error) {
	if strings.TrimSpace(options.Path) == "" {
		return nil, fmt.Errorf("SQLite configuration path is required")
	}
	if options.BusyTimeout <= 0 {
		options.BusyTimeout = 5 * time.Second
	}
	path := options.Path
	if path != ":memory:" {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return nil, fmt.Errorf("resolve SQLite configuration path: %w", err)
		}
		if err := ensurePrivateFile(absolute); err != nil {
			return nil, err
		}
		path = absolute
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open SQLite configuration store: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	closeOnError := func(err error) (*Store, error) {
		_ = db.Close()
		return nil, err
	}
	pragmas := []string{
		"PRAGMA foreign_keys=ON",
		fmt.Sprintf("PRAGMA busy_timeout=%d", options.BusyTimeout.Milliseconds()),
	}
	if path != ":memory:" {
		pragmas = append(pragmas, "PRAGMA journal_mode=WAL")
	}
	for _, statement := range pragmas {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			return closeOnError(fmt.Errorf("configure SQLite configuration store: %w", err))
		}
	}
	if err := runMigrations(ctx, db); err != nil {
		return closeOnError(err)
	}
	return &Store{
		db: db, validator: options.Validator,
		watchers: make(map[uint64]chan config.Version),
	}, nil
}

func (s *Store) Initialize(
	ctx context.Context,
	operator config.Document,
	actor string,
	reason string,
) (bool, error) {
	if err := validateMetadata(actor, reason); err != nil {
		return false, err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin SQLite configuration initialization: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var exists int
	if err := tx.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM gateway_config_current WHERE singleton = 1",
	).Scan(&exists); err != nil {
		return false, fmt.Errorf("check SQLite configuration initialization: %w", err)
	}
	if exists != 0 {
		return false, nil
	}
	sets := map[managed.Owner]config.Document{
		managed.OwnerOperator: managed.NormalizeFragment(operator),
		managed.OwnerSparkrun: managed.EmptyDocument(),
	}
	_, raw, version, fragments, err := s.compile(sets)
	if err != nil {
		return false, err
	}
	now := time.Now().UTC()
	for _, owner := range []managed.Owner{managed.OwnerOperator, managed.OwnerSparkrun} {
		fragment := fragments[owner]
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO gateway_config_managed_sets (
				owner, revision_id, document_json, updated_at, updated_by, reason
			) VALUES (?, ?, ?, ?, ?, ?)
		`, owner, fragment.version, fragment.raw, formatTime(now), actor, nullString(reason)); err != nil {
			return false, fmt.Errorf("initialize SQLite managed set %s: %w", owner, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO gateway_config_current (
			singleton, revision_id, document_json, updated_at, updated_by, reason
		) VALUES (1, ?, ?, ?, ?, ?)
	`, version, raw, formatTime(now), actor, nullString(reason)); err != nil {
		return false, fmt.Errorf("initialize SQLite configuration state: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit SQLite configuration initialization: %w", err)
	}
	s.notify(version)
	return true, nil
}

func (s *Store) Load(ctx context.Context) (config.Document, config.Version, error) {
	var raw []byte
	var version config.Version
	err := s.db.QueryRowContext(ctx, `
		SELECT document_json, revision_id
		FROM gateway_config_current WHERE singleton = 1
	`).Scan(&raw, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return config.Document{}, "", ErrNoActiveConfiguration
	}
	if err != nil {
		return config.Document{}, "", fmt.Errorf("load active SQLite configuration: %w", err)
	}
	document, err := config.Decode(raw)
	if err != nil {
		return config.Document{}, "", fmt.Errorf("decode active SQLite configuration: %w", err)
	}
	canonical, calculated, err := config.EncodeCanonical(document)
	if err != nil {
		return config.Document{}, "", err
	}
	if calculated != version || string(canonical) != string(raw) {
		return config.Document{}, "", fmt.Errorf("active SQLite configuration failed integrity check")
	}
	sets, err := s.loadSets(ctx, s.db)
	if err != nil {
		return config.Document{}, "", err
	}
	merged, err := managed.Merge(sets)
	if err != nil {
		return config.Document{}, "", fmt.Errorf("merge active SQLite managed sets: %w", err)
	}
	_, mergedVersion, err := config.EncodeCanonical(merged)
	if err != nil || mergedVersion != version {
		return config.Document{}, "", fmt.Errorf("active SQLite managed sets failed integrity check")
	}
	return document, version, nil
}

func (s *Store) Watch(ctx context.Context) (<-chan config.Version, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	channel := make(chan config.Version, 1)
	s.watchMu.Lock()
	s.nextWatch++
	id := s.nextWatch
	s.watchers[id] = channel
	s.watchMu.Unlock()
	go func() {
		<-ctx.Done()
		s.watchMu.Lock()
		if current, exists := s.watchers[id]; exists {
			delete(s.watchers, id)
			close(current)
		}
		s.watchMu.Unlock()
	}()
	return channel, nil
}

func (s *Store) ListSets(ctx context.Context) ([]managed.SetMetadata, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT owner, revision_id, updated_at, updated_by, COALESCE(reason, '')
		FROM gateway_config_managed_sets
		ORDER BY CASE owner WHEN 'operator' THEN 0 WHEN 'sparkrun' THEN 1 ELSE 2 END, owner
	`)
	if err != nil {
		return nil, fmt.Errorf("list SQLite managed sets: %w", err)
	}
	defer func() { _ = rows.Close() }()
	sets := make([]managed.SetMetadata, 0, 2)
	for rows.Next() {
		metadata, err := scanSetMetadata(rows)
		if err != nil {
			return nil, err
		}
		sets = append(sets, metadata)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate SQLite managed sets: %w", err)
	}
	return sets, nil
}

func (s *Store) GetSet(ctx context.Context, owner managed.Owner) (managed.Set, error) {
	if err := owner.Validate(); err != nil {
		return managed.Set{}, err
	}
	var raw []byte
	var updatedAt string
	set := managed.Set{SetMetadata: managed.SetMetadata{Owner: owner}}
	err := s.db.QueryRowContext(ctx, `
		SELECT revision_id, document_json, updated_at, updated_by, COALESCE(reason, '')
		FROM gateway_config_managed_sets WHERE owner = ?
	`, owner).Scan(&set.Revision, &raw, &updatedAt, &set.UpdatedBy, &set.Reason)
	if errors.Is(err, sql.ErrNoRows) {
		return managed.Set{}, fmt.Errorf("%w: %s", ErrManagedSetNotFound, owner)
	}
	if err != nil {
		return managed.Set{}, fmt.Errorf("read SQLite managed set: %w", err)
	}
	set.UpdatedAt, err = parseTime(updatedAt)
	if err != nil {
		return managed.Set{}, err
	}
	set.Document, err = decodeFragment(raw, set.Revision)
	if err != nil {
		return managed.Set{}, err
	}
	return set, nil
}

func (s *Store) ValidateSet(
	ctx context.Context,
	owner managed.Owner,
	document config.Document,
	expectedActive config.Version,
) (managed.Validation, error) {
	_, validation, err := s.BuildCandidate(ctx, owner, document, expectedActive)
	return validation, err
}

// BuildCandidate atomically substitutes one owner fragment into the currently
// active managed-set collection, validates the merged result, and returns it
// without persisting any state. This is the read-side seam used by draft tools
// that must evaluate the same cross-owner configuration ReplaceSet would apply.
func (s *Store) BuildCandidate(
	ctx context.Context,
	owner managed.Owner,
	document config.Document,
	expectedActive config.Version,
) (config.Document, managed.Validation, error) {
	if err := owner.Validate(); err != nil {
		return config.Document{}, managed.Validation{}, err
	}
	if expectedActive == "" {
		return config.Document{}, managed.Validation{}, fmt.Errorf("expected active configuration revision is required")
	}
	document = managed.NormalizeFragment(document)
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return config.Document{}, managed.Validation{}, fmt.Errorf("begin SQLite managed-set candidate: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	current, err := readCurrent(ctx, tx)
	if err != nil {
		return config.Document{}, managed.Validation{}, err
	}
	if current.Version != expectedActive {
		return config.Document{}, managed.Validation{}, fmt.Errorf(
			"%w: expected %q, found %q",
			ErrRevisionConflict, expectedActive, current.Version,
		)
	}
	sets, err := s.loadSets(ctx, tx)
	if err != nil {
		return config.Document{}, managed.Validation{}, err
	}
	sets[owner] = document
	candidate, _, candidateRevision, fragments, err := s.compile(sets)
	if err != nil {
		return config.Document{}, managed.Validation{}, err
	}
	setRevision := fragments[owner].version

	return candidate, managed.Validation{
		Owner: owner, SetRevision: setRevision,
		ActiveRevision:    current.Version,
		CandidateRevision: candidateRevision, Valid: true,
	}, nil
}

func (s *Store) ReplaceSet(
	ctx context.Context,
	owner managed.Owner,
	document config.Document,
	options managed.ReplaceOptions,
) (managed.ReplaceResult, error) {
	if err := owner.Validate(); err != nil {
		return managed.ReplaceResult{}, err
	}
	if options.ExpectedActive == "" {
		return managed.ReplaceResult{}, fmt.Errorf("expected active configuration revision is required")
	}
	if err := validateMetadata(options.Actor, options.Reason); err != nil {
		return managed.ReplaceResult{}, err
	}
	document = managed.NormalizeFragment(document)
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return managed.ReplaceResult{}, fmt.Errorf("begin SQLite managed-set replacement: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	current, err := readCurrent(ctx, tx)
	if err != nil {
		return managed.ReplaceResult{}, err
	}
	if current.Version != options.ExpectedActive {
		return managed.ReplaceResult{}, fmt.Errorf(
			"%w: expected %q, found %q",
			ErrRevisionConflict, options.ExpectedActive, current.Version,
		)
	}
	sets, err := s.loadSets(ctx, tx)
	if err != nil {
		return managed.ReplaceResult{}, err
	}
	previous := make(map[managed.Owner]config.Version, len(sets))
	for setOwner, fragment := range sets {
		_, revision, err := managed.FragmentRevision(fragment)
		if err != nil {
			return managed.ReplaceResult{}, err
		}
		previous[setOwner] = revision
	}
	sets[owner] = document
	_, mergedRaw, mergedVersion, fragments, err := s.compile(sets)
	if err != nil {
		return managed.ReplaceResult{}, err
	}

	// The reserved shared provider is the only normalization that can move
	// configuration across owners. Commit every affected fragment together so
	// readers never see duplicate providers or dangling deployment references.
	now := time.Now().UTC()
	changed := false
	for setOwner, fragment := range fragments {
		if fragment.version == previous[setOwner] {
			continue
		}
		changed = true
		if _, err := tx.ExecContext(ctx, `
			UPDATE gateway_config_managed_sets
			SET revision_id = ?, document_json = ?, updated_at = ?, updated_by = ?, reason = ?
			WHERE owner = ?
		`, fragment.version, fragment.raw, formatTime(now), options.Actor, nullString(options.Reason), setOwner); err != nil {
			return managed.ReplaceResult{}, fmt.Errorf("replace SQLite managed set: %w", err)
		}
	}
	metadata, err := readSetMetadata(ctx, tx, owner)
	if err != nil {
		return managed.ReplaceResult{}, err
	}
	if !changed {
		return managed.ReplaceResult{Set: metadata, Current: current, Changed: false}, nil
	}

	currentState := current
	if mergedVersion != current.Version {
		currentState = managed.Current{
			Version: mergedVersion, UpdatedAt: now,
			UpdatedBy: options.Actor, Reason: options.Reason,
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE gateway_config_current
			SET revision_id = ?, document_json = ?, updated_at = ?, updated_by = ?, reason = ?
			WHERE singleton = 1
		`, currentState.Version, mergedRaw, formatTime(now), options.Actor,
			nullString(options.Reason)); err != nil {
			return managed.ReplaceResult{}, fmt.Errorf("store current SQLite configuration: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return managed.ReplaceResult{}, fmt.Errorf("commit SQLite managed-set replacement: %w", err)
	}
	s.notify(mergedVersion)
	return managed.ReplaceResult{Set: metadata, Current: currentState, Changed: true}, nil
}

func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		s.watchMu.Lock()
		for id, watcher := range s.watchers {
			delete(s.watchers, id)
			close(watcher)
		}
		s.watchMu.Unlock()
		s.closeErr = s.db.Close()
	})
	return s.closeErr
}

type compiledFragment struct {
	raw     []byte
	version config.Version
}

func (s *Store) compile(
	sets map[managed.Owner]config.Document,
) (config.Document, []byte, config.Version, map[managed.Owner]compiledFragment, error) {
	sets, err := managed.NormalizeSparkrunProvider(sets)
	if err != nil {
		return config.Document{}, nil, "", nil, fmt.Errorf("%w: %v", ErrInvalidConfiguration, err)
	}
	document, err := managed.Merge(sets)
	if err != nil {
		return config.Document{}, nil, "", nil, fmt.Errorf("%w: %v", ErrInvalidConfiguration, err)
	}
	if s.validator != nil {
		if err := s.validator(document); err != nil {
			return config.Document{}, nil, "", nil, fmt.Errorf("%w: %v", ErrInvalidConfiguration, err)
		}
	}
	raw, version, err := config.EncodeCanonical(document)
	if err != nil {
		return config.Document{}, nil, "", nil, fmt.Errorf("%w: %v", ErrInvalidConfiguration, err)
	}
	fragments := make(map[managed.Owner]compiledFragment, len(sets))
	for owner, fragment := range sets {
		fragmentRaw, fragmentVersion, err := managed.FragmentRevision(fragment)
		if err != nil {
			return config.Document{}, nil, "", nil, err
		}
		fragments[owner] = compiledFragment{raw: fragmentRaw, version: fragmentVersion}
	}
	return document, raw, version, fragments, nil
}

func readCurrent(ctx context.Context, query interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}) (managed.Current, error) {
	var current managed.Current
	var occurred string
	err := query.QueryRowContext(ctx, `
		SELECT revision_id, updated_at, updated_by, COALESCE(reason, '')
		FROM gateway_config_current WHERE singleton = 1
	`).Scan(
		&current.Version, &occurred, &current.UpdatedBy, &current.Reason,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return managed.Current{}, ErrNoActiveConfiguration
	}
	if err != nil {
		return managed.Current{}, fmt.Errorf("read current SQLite configuration: %w", err)
	}
	current.UpdatedAt, err = parseTime(occurred)
	return current, err
}

type queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func (s *Store) loadSets(ctx context.Context, query queryer) (map[managed.Owner]config.Document, error) {
	rows, err := query.QueryContext(ctx, `
		SELECT owner, revision_id, document_json FROM gateway_config_managed_sets
	`)
	if err != nil {
		return nil, fmt.Errorf("load SQLite managed sets: %w", err)
	}
	defer func() { _ = rows.Close() }()
	sets := make(map[managed.Owner]config.Document, 2)
	for rows.Next() {
		var owner managed.Owner
		var revision config.Version
		var raw []byte
		if err := rows.Scan(&owner, &revision, &raw); err != nil {
			return nil, fmt.Errorf("scan SQLite managed set: %w", err)
		}
		if err := owner.Validate(); err != nil {
			return nil, err
		}
		document, err := decodeFragment(raw, revision)
		if err != nil {
			return nil, err
		}
		sets[owner] = document
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate SQLite managed sets: %w", err)
	}
	for _, owner := range []managed.Owner{managed.OwnerOperator, managed.OwnerSparkrun} {
		if _, exists := sets[owner]; !exists {
			return nil, fmt.Errorf("%w: %s", ErrManagedSetNotFound, owner)
		}
	}
	return sets, nil
}

func decodeFragment(raw []byte, expected config.Version) (config.Document, error) {
	var document config.Document
	if err := json.Unmarshal(raw, &document); err != nil {
		return config.Document{}, fmt.Errorf("decode SQLite managed set: %w", err)
	}
	document = managed.NormalizeFragment(document)
	canonical, calculated, err := managed.FragmentRevision(document)
	if err != nil {
		return config.Document{}, err
	}
	if calculated != expected || string(canonical) != string(raw) {
		return config.Document{}, fmt.Errorf("SQLite managed set failed integrity check")
	}
	return document, nil
}

func readSetMetadata(ctx context.Context, tx *sql.Tx, owner managed.Owner) (managed.SetMetadata, error) {
	var occurred string
	metadata := managed.SetMetadata{Owner: owner}
	err := tx.QueryRowContext(ctx, `
		SELECT revision_id, updated_at, updated_by, COALESCE(reason, '')
		FROM gateway_config_managed_sets WHERE owner = ?
	`, owner).Scan(&metadata.Revision, &occurred, &metadata.UpdatedBy, &metadata.Reason)
	if err != nil {
		return managed.SetMetadata{}, fmt.Errorf("read SQLite managed-set metadata: %w", err)
	}
	metadata.UpdatedAt, err = parseTime(occurred)
	return metadata, err
}

type scanner interface{ Scan(...any) error }

func scanSetMetadata(row scanner) (managed.SetMetadata, error) {
	var metadata managed.SetMetadata
	var occurred string
	if err := row.Scan(
		&metadata.Owner, &metadata.Revision, &occurred,
		&metadata.UpdatedBy, &metadata.Reason,
	); err != nil {
		return managed.SetMetadata{}, fmt.Errorf("scan SQLite managed-set metadata: %w", err)
	}
	var err error
	metadata.UpdatedAt, err = parseTime(occurred)
	return metadata, err
}

func (s *Store) notify(version config.Version) {
	s.watchMu.Lock()
	defer s.watchMu.Unlock()
	for _, watcher := range s.watchers {
		select {
		case watcher <- version:
		default:
		}
	}
}

func runMigrations(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS gateway_config_schema_migrations (
			version TEXT PRIMARY KEY,
			applied_at TEXT NOT NULL
		)
	`); err != nil {
		return fmt.Errorf("create SQLite configuration migration table: %w", err)
	}
	entries, err := migrationFiles.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("read SQLite configuration migrations: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".sql" {
			continue
		}
		version := strings.TrimSuffix(entry.Name(), ".sql")
		var exists int
		if err := db.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM gateway_config_schema_migrations WHERE version = ?", version,
		).Scan(&exists); err != nil {
			return fmt.Errorf("check SQLite configuration migration %s: %w", version, err)
		}
		if exists != 0 {
			continue
		}
		content, err := migrationFiles.ReadFile("migrations/" + entry.Name())
		if err != nil {
			return fmt.Errorf("read SQLite configuration migration %s: %w", version, err)
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin SQLite configuration migration %s: %w", version, err)
		}
		if _, err := tx.ExecContext(ctx, string(content)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("execute SQLite configuration migration %s: %w", version, err)
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO gateway_config_schema_migrations (version, applied_at) VALUES (?, ?)",
			version, formatTime(time.Now().UTC()),
		); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record SQLite configuration migration %s: %w", version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit SQLite configuration migration %s: %w", version, err)
		}
	}
	if err := migrateLegacyCurrentConfiguration(ctx, db); err != nil {
		return err
	}
	return nil
}

func migrateLegacyCurrentConfiguration(ctx context.Context, db *sql.DB) error {
	var legacyState bool
	if err := db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM sqlite_master
			WHERE type = 'table' AND name = 'gateway_config_state'
		)
	`).Scan(&legacyState); err != nil {
		return fmt.Errorf("inspect legacy SQLite configuration state: %w", err)
	}
	if !legacyState {
		return nil
	}
	var legacyRevisions bool
	if err := db.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM sqlite_master
			WHERE type = 'table' AND name = 'gateway_config_revisions'
		)
	`).Scan(&legacyRevisions); err != nil {
		return fmt.Errorf("inspect legacy SQLite configuration revisions: %w", err)
	}
	if !legacyRevisions {
		return fmt.Errorf("legacy SQLite configuration state has no revision table")
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO gateway_config_current (
			singleton, revision_id, document_json, updated_at, updated_by, reason
		)
		SELECT
			state.singleton,
			revision.revision_id,
			revision.document_json,
			state.activated_at,
			state.activated_by,
			state.reason
		FROM gateway_config_state AS state
		JOIN gateway_config_revisions AS revision
		  ON revision.revision_id = state.active_revision_id
		WHERE NOT EXISTS (SELECT 1 FROM gateway_config_current)
	`); err != nil {
		return fmt.Errorf("copy current configuration from legacy SQLite history: %w", err)
	}
	return nil
}

func ensurePrivateFile(path string) error {
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return fmt.Errorf("create SQLite configuration directory: %w", err)
	}
	parentInfo, err := os.Stat(parent)
	if err != nil {
		return fmt.Errorf("inspect SQLite configuration directory: %w", err)
	}
	if permissionErr := privatepath.Check(parent, parentInfo.Mode()); permissionErr != nil {
		return fmt.Errorf("SQLite configuration directory %q: %w", parent, permissionErr)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("create SQLite configuration file: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close SQLite configuration file: %w", err)
	}
	fileInfo, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("inspect SQLite configuration file: %w", err)
	}
	if permissionErr := privatepath.Check(path, fileInfo.Mode()); permissionErr != nil {
		return fmt.Errorf("SQLite configuration file %q: %w", path, permissionErr)
	}
	return nil
}

func validateMetadata(actor, reason string) error {
	if strings.TrimSpace(actor) == "" || len(actor) > 512 {
		return fmt.Errorf("configuration mutation actor is required and must not exceed 512 bytes")
	}
	if len(reason) > 4096 {
		return fmt.Errorf("configuration mutation reason exceeds 4096 bytes")
	}
	return nil
}

func formatTime(value time.Time) string { return value.UTC().Format(time.RFC3339Nano) }

func parseTime(value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse SQLite configuration timestamp: %w", err)
	}
	return parsed.UTC(), nil
}

func nullString(value string) any {
	if value == "" {
		return nil
	}
	return value
}
