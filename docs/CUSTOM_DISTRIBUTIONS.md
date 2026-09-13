<!--
SPDX-FileCopyrightText: 2026 Scitrera LLC
SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
SPDX-License-Identifier: AGPL-3.0-only
-->

# Building a custom SparkRoute distribution

SparkRoute exposes its routing engine, HTTP lifecycle and integration interfaces
as public Go packages. A downstream distribution can pin this module and assemble
its own command without copying the engine or changing standalone defaults.

## Runtime composition

- Build provider credential sources with `pkg/credentials/builtin`. Existing file,
  Kubernetes Secret, environment and AWS workload sources remain reusable.
- Supply authenticated `pkg/identity.Principal` values and bounded attribution at
  the request boundary. Keep application-specific admission/header policy downstream.
- Use `pkg/modelcatalog.Directory` for cached tenant-specific model lookup. Provide
  its resolver and egress/credential policy, then pass both the directory and
  dynamic credential source to `gateway.DataOptions.ModelCatalog` and
  `ModelCatalogCredentials`. Native platform catalog protocols belong downstream.
- Pass a usage recorder and optional durable response state to
  `gateway.NewDataPlane`. Its handler owns protocol conversion, streaming, errors,
  cancellation, model routing and accounting. Wrap it with the distribution's
  authentication middleware before exposing it.
- Use `gateway.ServerGroup` for listener binding and graceful request drain.
  Flush asynchronous usage/traces after handlers finish, then close stores.
  `gateway.NewHealth` and the operations handler provide health/status primitives.
- A configured runtime store alone does not wire distributed activation: dynamic
  deployments need a lifecycle coordinator/controller passed to the data plane.
  Keep readiness, fencing and replica guarantees explicit and tested.

These public interfaces already support an external composition; no private Go
imports or command-package fork is required. The standalone command continues to
support its existing local configuration/storage and sparkrun/provider-auth/UI
features. PostgreSQL packages below are library backends, not newly enabled CLI
flags in the standalone executable.

## PostgreSQL backends

| Package | Persistent contract |
| --- | --- |
| `pkg/ledger/postgres` | Usage/attempt ledger, response/resource/file state, prompt-cache affinity and PII mapping storage; optional TimescaleDB specialization. |
| `pkg/runtime/postgres` | Endpoint registry and lifecycle coordination/fencing store. |
| `pkg/config/postgres` | Configuration revisions, activation history, optimistic updates and watch/reconciliation. |
| `pkg/clientcredentials/postgres` | Managed caller credential issuance/revocation persistence. |
| `pkg/savedtrace/postgres` | Durable trace storage, capture sessions and dataset queries/export. |

Each backend exposes `Open(ctx, Options{URL: ..., AutoMigrate: ...})`. PostgreSQL 17
is the default tested server major. Use a dedicated migration phase before normal
startup; startup without auto-migration validates the expected schema. The ledger
uses ordinary PostgreSQL unless `BackendTimescaleDB` is explicitly selected.
TimescaleDB requires its extension and a separate deployment/test environment.
Migration names and SQL bytes are retained to preserve installed-schema compatibility.

The PII backend implements the existing public PII types; the engine is shared.
A downstream executable decides which stores/features to wire and must retain
third-party notices for the packages it actually distributes.

Run store tests against disposable PostgreSQL 17 databases with:

```sh
python3 scripts/test-postgres.py --go go
```

This creates its own loopback-only container and a database per backend, runs real
migration/persistence tests, and removes only that container afterward. Ordinary
`go test ./...` still runs non-database tests without external services. To target
an independently prepared disposable server, the tests accept
`SPARKROUTE_TEST_POSTGRES_URL`, `SPARKROUTE_TEST_TRACE_POSTGRES_URL`, and optional
`SPARKROUTE_TEST_POSTGRES_BACKEND=timescaledb`. Use fresh databases for those tests.
