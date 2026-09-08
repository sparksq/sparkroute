# Trace capture and dataset export

SparkRoute keeps optional payload capture separate from its content-free usage
ledger and operational telemetry. The canonical saved record is the only
capture path; training formats are derived during export.

## Durability evidence

Every asynchronous recorder instance creates a random capture-session ID and
persists a session record in the same trace backend. Trace rows carry that ID.
The session contains start/update/completion timestamps and accepted,
persisted, pending, invalid, queue-full, closed-recorder, and store-failure
counters. PostgreSQL makes this accounting visible across replicas; SQLite and
filesystem storage provide the same contract for single-process deployments.

Trace batches are transactional in SQLite and PostgreSQL and group-committed in
the filesystem journal. Capture-session checkpoints are throttled to one per
second by default, with a forced start, failure, and graceful-close checkpoint.
This avoids a second database transaction or filesystem sync for every trace
batch. An unclosed or stale session remains visible as uncertainty rather than
being silently treated as complete.

The default `block` overflow policy applies bounded backpressure. Explicit
`drop` mode records loss counters. Neither mode claims that caller success waits
for the trace commit; request-path `strict`/WAL durability remains future work.

## Capture outcomes and correlation

Each record declares `capture_outcome` as `complete`, `truncated`, or
`incomplete`. Size truncation is explicit. A successful caller-visible SSE
stream without a recognized terminal event is incomplete; non-SSE streams are
complete when the proxy observed their normal end.

Records also expose conversation ID, response ID, parent response ID, caller
session ID, and recorder capture-session ID. OpenAI request/response bodies,
SSE response events, Conversations resource paths, and trusted thread/session
attribution supply these values where available. Every field is an exact
server-side export filter in addition to tenant and allowlisted metadata.

## Dataset ZIP

`format=dataset` / `-format dataset` writes:

- `manifest.json`;
- `records.jsonl` for the canonical projection; or
- `dataset.jsonl` for DeepSpec and MLflow projections.

Canonical rows retain the complete versioned trace. DeepSpec rows contain a
conversation plus physical target provenance for successful non-streaming Chat
Completions or Responses records that can be represented safely. MLflow rows
contain `inputs`, `outputs`, target expectations, and correlation metadata for
complete JSON payloads. Projection exclusions are counted by reason; they are
never silently coerced.

The manifest freezes an otherwise-open end time at export start and declares
the exact filters, limits, record/time/config-revision range, projection,
capture outcomes, exclusions, overlapping capture sessions, completeness
reasons, and content-policy digest. `require_complete` rejects before writing
any archive bytes unless all matching rows are representable, no limit omitted
matches, every row is fully captured and session-attributed, and overlapping
session counters prove persistence and coverage through the requested end.

The content-policy digest currently identifies raw caller-visible captured
content. It does not assert application-level redaction or encryption.

## Bounded filesystem export snapshots

The optional `RecordSnapshotReader` contract gives an exporter one ordered,
replayable view of its query. Raw JSONL visits that view once. Dataset export
visits the same view twice—first for completeness and manifest preflight, then
for archive rows—so records appended between those phases cannot change the
artifact after it passes preflight.

The filesystem implementation captures each journal's completed byte length,
then scans and validates one full record at a time. Matching records become
fixed 128-byte references containing only ordering and journal-location fields.
References are sorted in 64 MiB memory chunks and merged through owner-only
temporary files with an 8 GiB default disk ceiling, 32-way fan-in, and capped
source-file cache. Full payloads remain in their journals and are decoded one at
a time during replay. `List` uses a bounded newest-entry set instead of the
external sorter because its page size is capped.

Disk exhaustion, malformed or incompatibly changed journals, cancellation, and
configured budget exhaustion fail the export instead of silently omitting
records. Snapshot work is removed on close or error. Store startup removes only
recognized abandoned snapshot directories older than seven days, leaving
recent work—which may belong to another export process—untouched. This does not
make filesystem trace storage a shared-RWX multi-writer backend; PostgreSQL
remains the supported multi-replica store.

Dataset manifest aggregation is separately capped at 65,536 distinct config
revision/capture-session values and 64 MiB of their string data. Exceeding that
bound also fails before archive rows are written.

## Standalone UI configuration

Configuration → Advanced Options saves `observability.saved_traces` and
`observability.otlp_traces` in the operator-managed set. Omitted signals inherit
startup flags/environment; explicit `enabled: false` overrides them. Saved trace
settings include `storage` (`filesystem` or `database`), an absolute gateway-host
`path`, `max_body_bytes` (default 64 MiB), `queue_capacity` (default 4096), and
`overflow` (`block` or `drop`). Disabling capture preserves existing files.

OTLP settings include the full HTTP trace `endpoint`, `service_name` (default
`sparkroute`), `sample_ratio` (0–1, parent-based), and `header_env`: HTTP header
names mapped to environment variable names. Header values must exist on the
gateway host; resolved values never enter configuration or bootstrap responses.
Sampling only affects operational spans, not saved payload capture. UI header
settings replace startup OTLP headers, including when empty.

Validate then Save applies settings without restart. New requests use the new
configuration; old generations finish and drain their exporters. Store handles
are shared across overlapping generations for the same path. Filesystem and
SQLite continue enforcing private owner permissions. A storage-open failure
keeps the previous serving generation active and appears as a stored/serving
revision mismatch; fix the configured path/permissions and save again.

The standalone `/v1/saved-traces/export` route supports JSONL and dataset ZIP
filters and requires `trace_read_all`, separate from config/status privileges.
The default local administrator receives that role. Export is unavailable while
capture is disabled; existing data can still be exported through the CLI.
