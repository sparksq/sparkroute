# SparkRoute OSS

SparkRoute OSS is a standalone, provider-neutral AI gateway distributed under
AGPL-3.0-only. It is the local/Sparkrun distribution of SparkRoute and the base
module used by the enterprise edition.

The Go module is `github.com/sparksq/sparkroute`. Provider protocol clients and
intermediate request/response types come from the independently reusable
Apache-2.0 `github.com/scitrera/go-llm v0.2.0` module.

## Included

- OpenAI Chat Completions, Responses, Conversations, Files, and Embeddings;
- Anthropic Messages, Gemini, Amazon Bedrock, and compatible-provider routing;
- virtual models, capability-aware retries/fallback, routing policies,
  guardrails, prefix affinity, and protocol translation;
- optional Sparkrun lifecycle/discovery through its hidden gateway bridge;
- optional MMBridge multimedia projection, including the authenticated bridge
  client and analyzer callback used by local/Sparkrun deployments;
- file configuration or writable, current-state-only single-process SQLite managed configuration;
- optional memory/SQLite usage accounting;
- bounded filesystem or SQLite saved traces and filtered dataset export;
- a basic embedded React admin/configuration/credential console;
- co-located or split data, admin, and operations listeners;
- local managed bearer credentials and explicit insecure-development admin
  mode; and
- exported storage, identity, routing, lifecycle, telemetry, model-catalog,
  and PII-provider extension seams used by downstream compositions.

PostgreSQL/TimescaleDB, cross-tenant trusted-network authentication, Aether,
MLflow relay, concrete PII detection/reversible substitution/persistence,
replicated coordination, and the Kubernetes chart are not OSS runtime
dependencies. An OSS data plane rejects an enabled `privacy.pii` policy unless
a downstream `privacy.Provider` is injected.

## Run

```sh
go run ./cmd/sparkroute -config ./examples/config.yaml
```

By default the data listener is `127.0.0.1:8080`. When `-admin-address` and
`-operations-address` are empty, `/admin`, `/metrics`, and health routes share
the data listener. Explicit listener addresses remove those routes from the
data listener.

For writable configuration suitable for Sparkrun:

```sh
go run ./cmd/sparkroute \
  -config-source sqlite \
  -config-sqlite ./sparkroute-config.db \
  -config-bootstrap ./examples/config.yaml \
  -admin-auth-mode token-file \
  -admin-token-file ./state/admin-token.secret \
  -data-address 127.0.0.1:4000
```

Sparkrun-owned configuration is isolated from operator-owned configuration in
the managed store. SparkRoute periodically reconciles activatable targets via
the `sparkrun gateway-bridge` JSON contract.

## Admin authentication

Local read-only admin access is the safe default. Managed mode uses the SQLite
client-credential store. The standalone `token-file` mode supports the
convenience-focused Sparkrun profile: a missing or empty admin token file grants
writable local admin access; creating or replacing it requires that bearer;
removing it opens access again. The file is read for every request, so those
changes need no listener restart. Since this mode can become unauthenticated at
runtime, exposing it on a non-loopback listener requires
`-allow-insecure-admin-nonloopback` even while a token currently exists.

Caller `token-file` mode is fail-closed and is used for Sparkrun master keys.

Use `sparkroute -help` for the current flags. Sparkrun normally owns token
creation/retrieval through its `proxy admin-token` commands.

## Saved traces and export

On Windows, SQLite databases and filesystem traces must reside in directories
whose ACLs restrict access to the running user, SYSTEM, and local Administrators.
Inheritable permissions are checked as well; existing ACLs are never rewritten.
On Linux and macOS, directories use mode 0700 and files use mode 0600.

Filesystem recording is owner-only, bounded, and intended for one gateway
writer. It is not a multi-replica RWX-PVC backend. SQLite may share the usage
ledger database or use a dedicated trace database.

```sh
sparkroute \
  -config ./examples/config.yaml \
  -trace-storage filesystem \
  -trace-filesystem ./traces

sparkroute traces export \
  -storage filesystem \
  -filesystem ./traces \
  -output ./training.jsonl
```

Export supports time, principal, protocol/operation, virtual-model, outcome,
request, trace, and allowlisted metadata filters. Tenant remains an empty,
fixed scope in OSS.

## Development

```sh
go test ./...
cd web && npm ci && npm test && npm run build
docker build -t sparkroute:dev .
```

The Dockerfile builds the React application and Go binary in multi-stage,
multi-architecture-safe stages.

See [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md),
[`docs/SPARKRUN_INTEGRATION_READINESS.md`](docs/SPARKRUN_INTEGRATION_READINESS.md),
and [`docs/TRACE_DATASET_EXPORT.md`](docs/TRACE_DATASET_EXPORT.md).
