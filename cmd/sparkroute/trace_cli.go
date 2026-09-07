package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	ledgersqlite "github.com/sparksq/sparkroute/pkg/ledger/sqlite"
	"github.com/sparksq/sparkroute/pkg/savedtrace"
	tracefilesystem "github.com/sparksq/sparkroute/pkg/savedtrace/filesystem"
)

func runTraceExport(ctx context.Context, args []string, stdout io.Writer) (resultErr error) {
	flags := flag.NewFlagSet("sparkroute traces export", flag.ContinueOnError)
	flags.SetOutput(stdout)
	storage := flags.String(
		"storage",
		env("SPARKROUTE_TRACE_STORAGE", ""),
		"trace storage to read: database or filesystem",
	)
	database := flags.String(
		"database",
		firstNonEmpty(
			env("SPARKROUTE_TRACE_DATABASE", ""),
			env("SPARKROUTE_LEDGER_SQLITE", ""),
		),
		"SQLite saved-trace database path",
	)
	filesystem := flags.String(
		"filesystem",
		env("SPARKROUTE_TRACE_FILESYSTEM", ""),
		"saved-trace filesystem directory",
	)
	outputPath := flags.String("output", "-", "output JSONL or dataset zip path; - writes stdout")
	format := flags.String("format", "jsonl", "export format: jsonl or dataset")
	projection := flags.String("projection", "canonical", "dataset projection: canonical, deepspec, or mlflow")
	requireComplete := flags.Bool("require-complete", false, "reject a dataset unless capture completeness is proven")
	after := flags.String("started-at-or-after", "", "inclusive RFC3339 start time")
	before := flags.String("started-at-before", "", "exclusive RFC3339 end time")
	tenant := flags.String("tenant", "", "exact tenant ID")
	conversationID := flags.String("conversation-id", "", "exact conversation ID")
	responseID := flags.String("response-id", "", "exact response ID")
	parentResponseID := flags.String("parent-response-id", "", "exact parent response ID")
	sessionID := flags.String("session-id", "", "exact caller session ID")
	protocol := flags.String("protocol", "", "exact ingress protocol")
	operation := flags.String("operation", "", "exact operation")
	requestedModel := flags.String("requested-model", "", "exact requested model")
	virtualModel := flags.String("virtual-model", "", "exact virtual model")
	provider := flags.String("provider", "", "exact final provider")
	deployment := flags.String("deployment", "", "exact final deployment")
	outcome := flags.String("outcome", "", "exact outcome")
	captureOutcome := flags.String("capture-outcome", "", "exact capture outcome")
	limit := flags.Int("limit", 0, "maximum records; zero exports all matches")
	var metadata metadataFlags
	flags.Var(&metadata, "metadata", "exact metadata key=value; repeatable")
	if err := flags.Parse(args); err != nil {
		return fmt.Errorf("parse trace export flags: %w", err)
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("trace export does not accept positional arguments")
	}
	query, err := traceExportQuery(
		*after,
		*before,
		*tenant,
		*protocol,
		*operation,
		*requestedModel,
		*virtualModel,
		*provider,
		*deployment,
		*outcome,
		metadata,
	)
	if err != nil {
		return err
	}
	query.ConversationID = *conversationID
	query.ResponseID = *responseID
	query.ParentResponseID = *parentResponseID
	query.SessionID = *sessionID
	query.CaptureOutcome = savedtrace.CaptureOutcome(*captureOutcome)
	if _, err := savedtrace.PlanQuery(&query); err != nil {
		return err
	}
	var store savedtrace.Store
	switch strings.ToLower(strings.TrimSpace(*storage)) {
	case "database":
		if *database == "" {
			return fmt.Errorf("-database is required for database trace export")
		}
		store, err = ledgersqlite.Open(ctx, ledgersqlite.Options{Path: *database})
	case "filesystem":
		store, err = tracefilesystem.Open(*filesystem)
	default:
		return fmt.Errorf("-storage must be database or filesystem")
	}
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	output, closeOutput, err := traceExportOutput(*outputPath, stdout)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, closeOutput()) }()
	switch strings.ToLower(strings.TrimSpace(*format)) {
	case "jsonl":
		if *requireComplete || *projection != "canonical" {
			return fmt.Errorf("-projection and -require-complete require -format dataset")
		}
		_, err = savedtrace.ExportJSONL(ctx, store, query, output, *limit)
	case "dataset":
		selected, parseErr := savedtrace.ParseDatasetProjection(*projection)
		if parseErr != nil {
			return parseErr
		}
		_, err = savedtrace.ExportDataset(ctx, store, query, output, savedtrace.DatasetOptions{
			Projection: selected, MaximumRecords: *limit, RequireComplete: *requireComplete,
		})
	default:
		return fmt.Errorf("-format must be jsonl or dataset")
	}
	return err
}

type metadataFlags map[string]string

func (f *metadataFlags) String() string {
	if f == nil {
		return ""
	}
	return fmt.Sprint(map[string]string(*f))
}

func (f *metadataFlags) Set(value string) error {
	key, expected, err := savedtrace.ParseMetadataFilter(value)
	if err != nil {
		return err
	}
	if *f == nil {
		*f = make(map[string]string)
	}
	if _, exists := (*f)[key]; exists {
		return fmt.Errorf("metadata key %q was supplied more than once", key)
	}
	(*f)[key] = expected
	return nil
}

func traceExportQuery(
	after string,
	before string,
	tenant string,
	protocol string,
	operation string,
	requestedModel string,
	virtualModel string,
	provider string,
	deployment string,
	outcome string,
	metadata map[string]string,
) (savedtrace.Query, error) {
	startedAfter, err := optionalTraceTime(after)
	if err != nil {
		return savedtrace.Query{}, err
	}
	startedBefore, err := optionalTraceTime(before)
	if err != nil {
		return savedtrace.Query{}, err
	}
	query := savedtrace.Query{
		StartedAtOrAfter: startedAfter,
		StartedAtBefore:  startedBefore,
		TenantID:         tenant,
		Protocol:         protocol,
		Operation:        operation,
		RequestedModel:   requestedModel,
		VirtualModel:     virtualModel,
		Provider:         provider,
		Deployment:       deployment,
		Outcome:          outcome,
		Metadata:         metadata,
	}
	_, err = savedtrace.PlanQuery(&query)
	return query, err
}

func optionalTraceTime(value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("timestamp %q must be RFC3339", value)
	}
	return parsed.UTC(), nil
}

func traceExportOutput(
	path string,
	stdout io.Writer,
) (io.Writer, func() error, error) {
	if path == "" || path == "-" {
		return stdout, func() error { return nil }, nil
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("create trace export %q: %w", path, err)
	}
	return file, file.Close, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
