# SparkRun on-demand configuration and lifecycle

UI-created models live in the operator managed set. Providers, deployments,
virtual model names, and aliases are prepared as a single draft and committed
using the existing revision-checked Validate/Save API. Generated entries remain
read-only and can be reused without copying their lifecycle policy. Overlapping
bindings for the same recipe and cluster are rejected; use another alias of the
existing deployment instead.

The browser calls `POST /v1/sparkrun/catalog` for cached registry/cluster search,
recipe resolution, uploads, and explicit refresh progress. The handler only
allows catalog operations, never arbitrary subprocess or lifecycle commands.
`POST /v1/sparkrun/recipe-draft` resolves and pins the selection and derives the
provider, deployment, binding revision, and virtual-model entries. Validate and
Save recheck the recipe revision and named cluster. None of those paths launch
models. Existing configuration-read and operator-write roles apply; uploads and
registry refresh require write permission. Catalog failures do not block cloud
configurations without SparkRun bindings.

Selections are exact controller-local `catalog:<id>` references. SparkRun owns
registry resolution, normalized overrides, fingerprints, recipe/plugin checks,
and trust. Paths refer to the control node. Imports are untrusted single-file
YAML; abandoned imports expire after seven days and saved imports are retained.

Bridge schema v3 adds catalog operations, persisted operation status, endpoint
ownership, and asynchronous activation. The Go client starts `ensure_ready`
with `wait: false`, polls the operation, and exposes phase/job progress through
`GET /v1/runtime/status`. The detached Python worker continues if its requesting
gateway disconnects. Retry reconciles recorded placement and persisted jobs
before launching; ambiguous/unreachable prior launches require resolution.
Normal SparkRun plan/run, automatic ports, post-launch steps, and readiness are
shared with the CLI. Recipe revisions and authoritative cluster metadata scope
adoption and registration; unknown legacy clusters are not guessed.

A gateway-process workload manager shares physical-job request leases and idle
policy across runtime configuration generations. A draining generation cannot
stop a job serving requests in its replacement. Only owned jobs get idle timers;
stops check ownership again through SparkRun. Idle intervals start anew after a
gateway process restart and observation. Removing a route disables its idle
policy without stopping its job. This is one local gateway's coordination, not
multi-replica distributed ownership.

Request profiles such as `coding:xhigh`, richer recipe facets, multiple fallback
clusters in the wizard, and ColdSnap sleep/wake controls are subsequent work.
Recipe preview reports registered recipe extensions and those required by the
recipe; it does not claim an installed plugin is actively managing a live job.
