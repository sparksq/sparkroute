// Package sqlite implements the single-process standalone ledger profile.
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

	"github.com/sparksq/sparkroute/pkg/ledger"
	"github.com/sparksq/sparkroute/pkg/responsesstate"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

type Options struct {
	// Path may be ":memory:" for tests and ephemeral standalone runs.
	Path        string
	BusyTimeout time.Duration
}

type Store struct {
	db        *sql.DB
	writeMu   sync.Mutex
	closeOnce sync.Once
	closeErr  error
}

func Open(ctx context.Context, options Options) (*Store, error) {
	if options.Path == "" {
		return nil, fmt.Errorf("SQLite ledger path is required")
	}
	if options.BusyTimeout <= 0 {
		options.BusyTimeout = 5 * time.Second
	}
	path := options.Path
	if path != ":memory:" {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return nil, fmt.Errorf("resolve SQLite ledger path: %w", err)
		}
		if err := ensurePrivateFile(absolute); err != nil {
			return nil, err
		}
		path = absolute
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open SQLite ledger: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	store := &Store{db: db}
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
			return closeOnError(fmt.Errorf("configure SQLite ledger: %w", err))
		}
	}
	if err := runMigrations(ctx, db); err != nil {
		return closeOnError(err)
	}
	return store, nil
}

func ensurePrivateFile(path string) error {
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return fmt.Errorf("create SQLite ledger directory: %w", err)
	}
	parentInfo, err := os.Stat(parent)
	if err != nil {
		return fmt.Errorf("inspect SQLite ledger directory: %w", err)
	}
	if permissionErr := privatepath.Check(parent, parentInfo.Mode()); permissionErr != nil {
		return fmt.Errorf("SQLite ledger directory %q: %w", parent, permissionErr)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("create SQLite ledger file: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close SQLite ledger file: %w", err)
	}
	fileInfo, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("inspect SQLite ledger file: %w", err)
	}
	if permissionErr := privatepath.Check(path, fileInfo.Mode()); permissionErr != nil {
		return fmt.Errorf("SQLite ledger file %q: %w", path, permissionErr)
	}
	return nil
}

func runMigrations(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS ledger_schema_migrations (
			version TEXT PRIMARY KEY,
			applied_at TEXT NOT NULL
		)
	`); err != nil {
		return fmt.Errorf("create SQLite migration table: %w", err)
	}
	entries, err := migrationFiles.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("read SQLite ledger migrations: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".sql" {
			continue
		}
		version := strings.TrimSuffix(entry.Name(), ".sql")
		var exists int
		if err := db.QueryRowContext(
			ctx,
			"SELECT COUNT(*) FROM ledger_schema_migrations WHERE version = ?",
			version,
		).Scan(&exists); err != nil {
			return fmt.Errorf("check SQLite migration %s: %w", version, err)
		}
		if exists > 0 {
			continue
		}
		content, err := migrationFiles.ReadFile("migrations/" + entry.Name())
		if err != nil {
			return fmt.Errorf("read SQLite migration %s: %w", version, err)
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin SQLite migration %s: %w", version, err)
		}
		if _, err := tx.ExecContext(ctx, string(content)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("execute SQLite migration %s: %w", version, err)
		}
		if _, err := tx.ExecContext(
			ctx,
			"INSERT INTO ledger_schema_migrations (version, applied_at) VALUES (?, ?)",
			version,
			formatTime(time.Now()),
		); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record SQLite migration %s: %w", version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit SQLite migration %s: %w", version, err)
		}
	}
	return nil
}

func (s *Store) AppendBatch(ctx context.Context, records []ledger.Record) error {
	if len(records) == 0 {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin SQLite ledger batch: %w", err)
	}
	for _, record := range records {
		if err := record.Validate(); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("validate SQLite ledger record: %w", err)
		}
		switch record.Kind {
		case ledger.KindRequest:
			err = insertRequest(ctx, tx, *record.Request)
		case ledger.KindAttempt:
			err = insertAttempt(ctx, tx, *record.Attempt)
		case ledger.KindRuntimeEvent:
			err = insertRuntimeEvent(ctx, tx, *record.RuntimeEvent)
		default:
			err = fmt.Errorf("unsupported ledger record kind %q", record.Kind)
		}
		if err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit SQLite ledger batch: %w", err)
	}
	return nil
}

func (s *Store) Health(ctx context.Context) error {
	var value int
	if err := s.db.QueryRowContext(ctx, "SELECT 1").Scan(&value); err != nil {
		return fmt.Errorf("SQLite ledger health: %w", err)
	}
	return nil
}

func (s *Store) Resolve(
	ctx context.Context,
	scope string,
	responseID string,
) (responsesstate.Affinity, bool, error) {
	if err := responsesstate.ValidateScope(scope); err != nil {
		return responsesstate.Affinity{}, false, err
	}
	if err := responsesstate.ValidateResponseID(responseID); err != nil {
		return responsesstate.Affinity{}, false, err
	}
	var affinity responsesstate.Affinity
	var boundAt, expiresAt string
	err := s.db.QueryRowContext(ctx, `
		SELECT owner_scope, response_id, virtual_model, provider, deployment, upstream_model,
		       bound_at, expires_at
		FROM llm_response_affinities
		WHERE owner_scope = ? AND response_id = ? AND expires_at_unix_ns > ?
	`, scope, responseID, time.Now().UTC().UnixNano()).Scan(
		&affinity.Scope,
		&affinity.ResponseID,
		&affinity.VirtualModel,
		&affinity.Provider,
		&affinity.Deployment,
		&affinity.UpstreamModel,
		&boundAt,
		&expiresAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return responsesstate.Affinity{}, false, nil
	}
	if err != nil {
		return responsesstate.Affinity{}, false, fmt.Errorf(
			"resolve SQLite response affinity: %w",
			err,
		)
	}
	affinity.BoundAt, err = time.Parse(time.RFC3339Nano, boundAt)
	if err != nil {
		return responsesstate.Affinity{}, false, fmt.Errorf(
			"parse SQLite response affinity bound time: %w",
			err,
		)
	}
	affinity.ExpiresAt, err = time.Parse(time.RFC3339Nano, expiresAt)
	if err != nil {
		return responsesstate.Affinity{}, false, fmt.Errorf(
			"parse SQLite response affinity expiry: %w",
			err,
		)
	}
	return affinity, true, nil
}

func (s *Store) Bind(ctx context.Context, affinity responsesstate.Affinity) error {
	if err := affinity.Validate(); err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin SQLite response affinity bind: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()
	if _, err := tx.ExecContext(
		ctx,
		"DELETE FROM llm_response_affinities WHERE expires_at_unix_ns <= ?",
		time.Now().UTC().UnixNano(),
	); err != nil {
		return fmt.Errorf("expire SQLite response affinities: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO llm_response_affinities (
			owner_scope, response_id, virtual_model, provider, deployment, upstream_model,
			bound_at, bound_at_unix_ns, expires_at, expires_at_unix_ns
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (owner_scope, response_id) DO NOTHING
	`,
		affinity.Scope,
		affinity.ResponseID,
		affinity.VirtualModel,
		affinity.Provider,
		affinity.Deployment,
		affinity.UpstreamModel,
		formatTime(affinity.BoundAt),
		affinity.BoundAt.UTC().UnixNano(),
		formatTime(affinity.ExpiresAt),
		affinity.ExpiresAt.UTC().UnixNano(),
	); err != nil {
		return fmt.Errorf("insert SQLite response affinity: %w", err)
	}
	var existing responsesstate.Affinity
	if err := tx.QueryRowContext(ctx, `
		SELECT owner_scope, response_id, virtual_model, provider, deployment, upstream_model
		FROM llm_response_affinities
		WHERE owner_scope = ? AND response_id = ?
	`, affinity.Scope, affinity.ResponseID).Scan(
		&existing.Scope,
		&existing.ResponseID,
		&existing.VirtualModel,
		&existing.Provider,
		&existing.Deployment,
		&existing.UpstreamModel,
	); err != nil {
		return fmt.Errorf("verify SQLite response affinity: %w", err)
	}
	if !responsesstate.SameRoute(existing, affinity) {
		return responsesstate.ErrConflict
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit SQLite response affinity bind: %w", err)
	}
	return nil
}

func (s *Store) ResolveResource(
	ctx context.Context,
	key responsesstate.ResourceKey,
) (responsesstate.ResourceAffinity, bool, error) {
	if err := key.Validate(); err != nil {
		return responsesstate.ResourceAffinity{}, false, err
	}
	var affinity responsesstate.ResourceAffinity
	var kind string
	var boundAt string
	var deletedAt, expiresAt sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT owner_scope, resource_kind, resource_id, virtual_model, provider,
		       deployment, upstream_model, bound_at, deleted_at, expires_at
		FROM llm_resource_affinities
		WHERE owner_scope = ? AND resource_kind = ? AND resource_id = ?
		  AND (expires_at_unix_ns IS NULL OR expires_at_unix_ns > ?)
	`, key.Scope, string(key.Kind), key.ResourceID, time.Now().UTC().UnixNano()).Scan(
		&affinity.Scope,
		&kind,
		&affinity.ResourceID,
		&affinity.VirtualModel,
		&affinity.Provider,
		&affinity.Deployment,
		&affinity.UpstreamModel,
		&boundAt,
		&deletedAt,
		&expiresAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return responsesstate.ResourceAffinity{}, false, nil
	}
	if err != nil {
		return responsesstate.ResourceAffinity{}, false, fmt.Errorf(
			"resolve SQLite resource affinity: %w",
			err,
		)
	}
	affinity.Kind = responsesstate.ResourceKind(kind)
	affinity.BoundAt, err = time.Parse(time.RFC3339Nano, boundAt)
	if err != nil {
		return responsesstate.ResourceAffinity{}, false, fmt.Errorf(
			"parse SQLite resource affinity bound time: %w",
			err,
		)
	}
	if deletedAt.Valid {
		affinity.DeletedAt, err = time.Parse(time.RFC3339Nano, deletedAt.String)
		if err != nil {
			return responsesstate.ResourceAffinity{}, false, fmt.Errorf(
				"parse SQLite resource affinity deleted time: %w",
				err,
			)
		}
	}
	if expiresAt.Valid {
		affinity.ExpiresAt, err = time.Parse(time.RFC3339Nano, expiresAt.String)
		if err != nil {
			return responsesstate.ResourceAffinity{}, false, fmt.Errorf(
				"parse SQLite resource affinity expiry: %w",
				err,
			)
		}
	}
	return affinity, true, nil
}

func (s *Store) BindResource(
	ctx context.Context,
	affinity responsesstate.ResourceAffinity,
) error {
	if err := affinity.Validate(); err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin SQLite resource affinity bind: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()
	if err := bindResourceTx(ctx, tx, affinity); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit SQLite resource affinity bind: %w", err)
	}
	return nil
}

func bindResourceTx(
	ctx context.Context,
	tx *sql.Tx,
	affinity responsesstate.ResourceAffinity,
) error {
	if _, err := tx.ExecContext(
		ctx,
		`DELETE FROM llm_resource_affinities
		 WHERE expires_at_unix_ns IS NOT NULL AND expires_at_unix_ns <= ?`,
		time.Now().UTC().UnixNano(),
	); err != nil {
		return fmt.Errorf("expire SQLite resource affinities: %w", err)
	}
	var deletedAt, deletedAtNS, expiresAt, expiresAtNS any
	if !affinity.DeletedAt.IsZero() {
		deletedAt = formatTime(affinity.DeletedAt)
		deletedAtNS = affinity.DeletedAt.UTC().UnixNano()
	}
	if !affinity.ExpiresAt.IsZero() {
		expiresAt = formatTime(affinity.ExpiresAt)
		expiresAtNS = affinity.ExpiresAt.UTC().UnixNano()
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO llm_resource_affinities (
			owner_scope, resource_kind, resource_id, virtual_model, provider,
			deployment, upstream_model, bound_at, bound_at_unix_ns,
			deleted_at, deleted_at_unix_ns, expires_at, expires_at_unix_ns
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (owner_scope, resource_kind, resource_id) DO NOTHING
	`,
		affinity.Scope,
		string(affinity.Kind),
		affinity.ResourceID,
		affinity.VirtualModel,
		affinity.Provider,
		affinity.Deployment,
		affinity.UpstreamModel,
		formatTime(affinity.BoundAt),
		affinity.BoundAt.UTC().UnixNano(),
		deletedAt,
		deletedAtNS,
		expiresAt,
		expiresAtNS,
	); err != nil {
		return fmt.Errorf("insert SQLite resource affinity: %w", err)
	}
	var existing responsesstate.ResourceAffinity
	var kind string
	var existingDeletedAt, existingExpiresAt sql.NullString
	if err := tx.QueryRowContext(ctx, `
		SELECT owner_scope, resource_kind, resource_id, virtual_model, provider,
		       deployment, upstream_model, deleted_at, expires_at
		FROM llm_resource_affinities
		WHERE owner_scope = ? AND resource_kind = ? AND resource_id = ?
	`, affinity.Scope, string(affinity.Kind), affinity.ResourceID).Scan(
		&existing.Scope,
		&kind,
		&existing.ResourceID,
		&existing.VirtualModel,
		&existing.Provider,
		&existing.Deployment,
		&existing.UpstreamModel,
		&existingDeletedAt,
		&existingExpiresAt,
	); err != nil {
		return fmt.Errorf("verify SQLite resource affinity: %w", err)
	}
	existing.Kind = responsesstate.ResourceKind(kind)
	if existingDeletedAt.Valid ||
		!responsesstate.SameResourceRoute(existing, affinity) {
		return responsesstate.ErrConflict
	}
	if existingExpiresAt.Valid {
		parsedExpiry, err := time.Parse(
			time.RFC3339Nano,
			existingExpiresAt.String,
		)
		if err != nil {
			return fmt.Errorf("parse SQLite resource affinity expiry: %w", err)
		}
		existing.ExpiresAt = parsedExpiry
	}
	mergedExpiry := responsesstate.MergeResourceExpiry(
		existing.ExpiresAt,
		affinity.ExpiresAt,
	)
	var mergedExpiryValue, mergedExpiryNS any
	if !mergedExpiry.IsZero() {
		mergedExpiryValue = formatTime(mergedExpiry)
		mergedExpiryNS = mergedExpiry.UTC().UnixNano()
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE llm_resource_affinities
		SET expires_at = ?, expires_at_unix_ns = ?
		WHERE owner_scope = ? AND resource_kind = ? AND resource_id = ?
		  AND deleted_at IS NULL
	`,
		mergedExpiryValue,
		mergedExpiryNS,
		affinity.Scope,
		string(affinity.Kind),
		affinity.ResourceID,
	); err != nil {
		return fmt.Errorf("extend SQLite resource affinity: %w", err)
	}
	return nil
}

func (s *Store) TombstoneResource(
	ctx context.Context,
	key responsesstate.ResourceKey,
	deletedAt time.Time,
	expiresAt time.Time,
) error {
	if err := key.Validate(); err != nil {
		return err
	}
	if deletedAt.IsZero() || expiresAt.IsZero() || !expiresAt.After(deletedAt) {
		return fmt.Errorf("tombstone expiry must be after deleted time")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	result, err := s.db.ExecContext(ctx, `
		UPDATE llm_resource_affinities
		SET deleted_at = COALESCE(deleted_at, ?),
		    deleted_at_unix_ns = COALESCE(deleted_at_unix_ns, ?),
		    expires_at = CASE WHEN deleted_at IS NULL THEN ? ELSE expires_at END,
		    expires_at_unix_ns = CASE
		        WHEN deleted_at IS NULL THEN ? ELSE expires_at_unix_ns END
		WHERE owner_scope = ? AND resource_kind = ? AND resource_id = ?
		  AND (expires_at_unix_ns IS NULL OR expires_at_unix_ns > ?)
	`,
		formatTime(deletedAt),
		deletedAt.UTC().UnixNano(),
		formatTime(expiresAt),
		expiresAt.UTC().UnixNano(),
		key.Scope,
		string(key.Kind),
		key.ResourceID,
		time.Now().UTC().UnixNano(),
	)
	if err != nil {
		return fmt.Errorf("tombstone SQLite resource affinity: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect SQLite resource tombstone result: %w", err)
	}
	if affected == 0 {
		return responsesstate.ErrNotFound
	}
	return nil
}

func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		s.closeErr = s.db.Close()
	})
	return s.closeErr
}

func insertRequest(ctx context.Context, tx *sql.Tx, record ledger.RequestRecord) error {
	usage, err := usageArgs(record.Usage)
	if err != nil {
		return err
	}
	principalRoles, err := jsonText(record.PrincipalRoles)
	if err != nil {
		return fmt.Errorf("encode SQLite principal roles: %w", err)
	}
	attribution, err := jsonText(record.Attribution)
	if err != nil {
		return fmt.Errorf("encode SQLite attribution: %w", err)
	}
	_, err = tx.ExecContext(ctx, insertRequestSQL,
		record.RequestID,
		formatTime(record.StartedAt),
		record.StartedAt.UTC().UnixNano(),
		formatTime(record.CompletedAt),
		nullString(record.PrincipalID),
		nullString(record.PrincipalType),
		nullString(record.PrincipalSubject),
		principalRoles,
		nullString(record.TenantID),
		attribution,
		record.Protocol,
		record.Operation,
		boolInt(record.Stream),
		nullString(record.RequestedModel),
		nullString(record.VirtualModel),
		nullString(record.ResponsePresentedModel),
		nullString(record.ConfigRevision),
		nullString(record.FinalProvider),
		nullString(record.FinalDeployment),
		nullString(record.FinalUpstreamModel),
		record.AttemptCount,
		nullInt(record.HTTPStatus),
		string(record.Outcome),
		nullString(record.FailureClass),
		record.Latency.Nanoseconds(),
		nullDuration(record.TimeToFirstByte),
		usage[0], usage[1], usage[2], usage[3], usage[4],
		usage[5], usage[6], usage[7], usage[8], usage[9],
		usage[10], string(record.Usage.Completeness),
		nullString(record.Usage.NormalizationVersion),
	)
	if err != nil {
		return fmt.Errorf("insert SQLite request record: %w", err)
	}
	return nil
}

func jsonText(value any) (any, error) {
	switch typed := value.(type) {
	case []string:
		if len(typed) == 0 {
			return nil, nil
		}
	case map[string]string:
		if len(typed) == 0 {
			return nil, nil
		}
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return string(encoded), nil
}

func insertAttempt(ctx context.Context, tx *sql.Tx, record ledger.AttemptRecord) error {
	usage, err := usageArgs(record.Usage)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, insertAttemptSQL,
		record.RequestID,
		record.Attempt,
		formatTime(record.StartedAt),
		record.StartedAt.UTC().UnixNano(),
		nullTime(record.FirstByteAt),
		formatTime(record.CompletedAt),
		record.Provider,
		record.Deployment,
		record.UpstreamModel,
		record.PoolPriority,
		nullInt(record.HTTPStatus),
		nullString(record.UpstreamRequestID),
		string(record.Outcome),
		nullString(record.FailureClass),
		boolInt(record.Retried),
		record.Latency.Nanoseconds(),
		usage[0], usage[1], usage[2], usage[3], usage[4],
		usage[5], usage[6], usage[7], usage[8], usage[9],
		usage[10], string(record.Usage.Completeness),
		nullString(record.Usage.NormalizationVersion),
	)
	if err != nil {
		return fmt.Errorf("insert SQLite attempt record: %w", err)
	}
	return nil
}

func insertRuntimeEvent(
	ctx context.Context,
	tx *sql.Tx,
	record ledger.RuntimeEventRecord,
) error {
	_, err := tx.ExecContext(ctx, insertRuntimeEventSQL,
		record.EventID,
		formatTime(record.OccurredAt),
		record.OccurredAt.UTC().UnixNano(),
		record.Controller,
		nullString(record.BindingRevision),
		nullString(record.VirtualModel),
		nullString(record.Deployment),
		nullString(record.EndpointInstance),
		nullString(record.ClusterID),
		nullString(record.JobID),
		nullString(record.RecipeRevision),
		nullString(record.PriorState),
		record.NewState,
		nullString(record.Reason),
		nullDuration(record.Latency),
		record.QueueDepth,
		record.WaiterCount,
		string(record.Outcome),
	)
	if err != nil {
		return fmt.Errorf("insert SQLite runtime event: %w", err)
	}
	return nil
}

func usageArgs(usage ledger.TokenUsage) ([11]any, error) {
	var result [11]any
	result[0] = nullToken(usage.InputTokens)
	result[1] = nullToken(usage.OutputTokens)
	result[2] = nullToken(usage.TotalTokens)
	result[3] = nullToken(usage.CachedInputTokens)
	result[4] = nullToken(usage.CacheCreationTokens)
	result[5] = nullToken(usage.ReasoningTokens)
	result[6] = nullToken(usage.ToolUsePromptTokens)
	result[7] = nullToken(usage.AcceptedPredictionTokens)
	result[8] = nullToken(usage.RejectedPredictionTokens)
	if len(usage.ProviderComponents) > 0 {
		encoded, err := json.Marshal(usage.ProviderComponents)
		if err != nil {
			return result, fmt.Errorf("encode provider usage components: %w", err)
		}
		result[9] = string(encoded)
	}
	if len(usage.Raw) > 0 {
		result[10] = string(usage.Raw)
	}
	return result, nil
}

func formatTime(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}

func nullTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return formatTime(*value)
}

func nullDuration(value *time.Duration) any {
	if value == nil {
		return nil
	}
	return value.Nanoseconds()
}

func nullToken(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

func nullInt(value int) any {
	if value == 0 {
		return nil
	}
	return value
}

func nullString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

const insertRequestSQL = `
	INSERT OR IGNORE INTO llm_requests (
		request_id, started_at, started_at_unix_ns, completed_at,
		principal_id, principal_type, principal_subject, principal_roles_json,
		tenant_id, attribution_json,
		protocol, operation, stream,
		requested_model, virtual_model, response_presented_model, config_revision,
		final_provider, final_deployment, final_upstream_model, attempt_count,
		http_status, outcome, failure_class, latency_ns, time_to_first_byte_ns,
		input_tokens, output_tokens, total_tokens, cached_input_tokens,
		cache_creation_tokens, reasoning_tokens, tool_use_prompt_tokens,
		accepted_prediction_tokens, rejected_prediction_tokens,
		provider_components_json, raw_usage_json, usage_completeness,
		normalization_version
	) VALUES (
		?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
		?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?
	)
`

const insertAttemptSQL = `
	INSERT OR IGNORE INTO llm_attempts (
		request_id, attempt_no, started_at, started_at_unix_ns,
		first_byte_at, completed_at,
		provider, deployment, upstream_model, pool_priority, http_status,
		upstream_request_id, outcome, failure_class, retried, latency_ns,
		input_tokens, output_tokens, total_tokens, cached_input_tokens,
		cache_creation_tokens, reasoning_tokens, tool_use_prompt_tokens,
		accepted_prediction_tokens, rejected_prediction_tokens,
		provider_components_json, raw_usage_json, usage_completeness,
		normalization_version
	) VALUES (
		?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
		?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?
	)
`

const insertRuntimeEventSQL = `
	INSERT OR IGNORE INTO model_runtime_events (
		event_id, occurred_at, occurred_at_unix_ns, controller,
		binding_revision, virtual_model, deployment, endpoint_instance,
		cluster_id, job_id, recipe_revision, prior_state, new_state, reason,
		latency_ns, queue_depth, waiter_count, outcome
	) VALUES (
		?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?
	)
`

var _ ledger.Store = (*Store)(nil)
