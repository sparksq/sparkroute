# sparkrun on-demand configuration and lifecycle

The executable enables sparkrun only with `-sparkrun`; the plugin passes it
automatically. Setting `-sparkrun-command` alone does not opt in. Without the
flag, no catalog/controller is constructed and the UI omits sparkrun creation
controls. Configuring a sparkrun endpoint source then returns an enablement
error, including during config checks and managed configuration validation.

The Model Deployments editor offers Standard and sparkrun as creation types.
Standard uses the normal provider form. sparkrun uses recipe selection and a
recipe-specific saved-deployment view. Recipe edits use the existing draft
endpoint with a deployment ID; IDs and virtual-model aliases are preserved.

UI-created models live in the operator managed set. Providers, deployments,
virtual model names, and aliases are prepared as a single draft and committed
using the existing revision-checked Validate/Save API. Generated entries remain
read-only and can be reused without copying their lifecycle policy. Overlapping
bindings for the same recipe and cluster are rejected; use another alias of the
existing deployment instead.

The shared-listener admin path classifier includes `/v1/sparkrun`, so catalog
and draft calls reach the admin handler on both shared and separate ports.

The browser calls `POST /v1/sparkrun/catalog` for cached registry/cluster search,
recipe resolution, uploads, and explicit refresh progress. The handler only
allows catalog operations, never arbitrary subprocess or lifecycle commands.
`POST /v1/sparkrun/recipe-draft` resolves and pins the selection and derives the
provider, deployment, binding revision, and virtual-model entries. Validate and
Save recheck the recipe revision and named cluster. None of those paths launch
models. Existing configuration-read and operator-write roles apply; uploads and
registry refresh require write permission. Catalog failures do not block cloud
configurations without sparkrun bindings.

Selections are exact controller-local `catalog:<id>` references. sparkrun owns
registry resolution, normalized overrides, fingerprints, recipe/plugin checks,
and trust. Paths refer to the control node. Imports are untrusted single-file
YAML; abandoned imports expire after seven days and saved imports are retained.

Bridge schema v4 adds catalog operations, persisted operation status, endpoint
ownership, and asynchronous activation. The Go client starts `ensure_ready`
with `wait: false`, polls the operation, and exposes phase/job progress through
`GET /v1/runtime/status`. The detached Python worker continues if its requesting
gateway disconnects. Retry reconciles recorded placement and persisted jobs
before launching; ambiguous/unreachable prior launches require resolution.
Normal sparkrun plan/run, automatic ports, post-launch steps, and readiness are
shared with the CLI. Recipe revisions and authoritative cluster metadata scope
adoption and registration; unknown legacy clusters are not guessed.

A gateway-process workload manager shares physical-job request leases and idle
policy across runtime configuration generations. A draining generation cannot
stop a job serving requests in its replacement. Only owned jobs get idle timers;
stops check ownership again through sparkrun. Idle intervals start anew after a
gateway process restart and observation. Removing a route disables its idle
policy without stopping its job. This is one local gateway's coordination, not
multi-replica distributed ownership.

## Recipe management and native APIs

The recipe public name defaults to `defaults.served_model_name`, falling back to
the Hugging Face model name. Advanced settings include ordered cluster fallback,
explicit capacity checks, and optional idle sleep for ColdSnap recipes. Catalog
metadata filters use declared values and preserve unknowns. Registry controls
require explicit trust acknowledgement; browsing never refreshes automatically.

The `sparkrun` provider delegates endpoint management to the integration. Native
APIs are per deployment/runtime: vLLM offers Chat Completions, Responses, and
Anthropic Messages by default, including nightly/custom images. Recognized vLLM
versions older than 0.12.0 default to Chat Completions only. Recipe
`metadata.native_apis` can narrow or override these defaults and also constrains
which API sparkrun can select for its launch readiness probe.
UI selection persists the wire families plus the native Responses declaration.
This deployment override affects gateway routing; recipe YAML controls readiness.

Virtual Models / Aliases supports explicit request profiles such as `coding:xhigh`.
Only parent models appear in the virtual-model list; their variants stay in the
Request profiles table between general model fields and routing pools.
Use **Add profile** to create a selector; use the pencil to expand its parameters
beneath the row or the X to remove it from the draft. JSON previews stay on one
line and truncate to fit. The expanded editor selects the request API and values.
`request_overrides` is keyed by ingress operation (`chat_completions`, `responses`,
`messages`, etc.); values replace caller parameters before protocol translation.
Profiles share the deployment and lifecycle; the colon has no implicit parser.

Runtime sleep/wake requires actual ColdSnap use, an enabled lifecycle API,
SparkRoute ownership, and zero active request leases. ColdSnap verifies the job,
hosts, and capture identity. Idle time starts after the final lease ends; an
uncertain transition blocks serving until reconciled. Coordination remains
within one local gateway and its durable bridge workers.
