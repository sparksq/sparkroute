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
read-only and can be reused without copying their lifecycle policy. Operator
exclusions can remove generated entries from the effective configuration. Overlapping
bindings for the same recipe and cluster are rejected; use another alias of the
existing deployment instead.

## Removing generated entries

Choose **Remove from sparkroute** on a generated virtual model or deployment,
review the affected names, and confirm. Then **Validate** and **Save**. A stopped
recipe binding can be removed this way even though its generated model fields
are read-only. Generated aliases and request profiles disappear with their last
deployment target; generated models backed by another deployment keep that
target. Operator-created models and routing are not silently rewritten. The
preview identifies references that need repairing, and merged validation checks
all remaining references, including effective guardrail policies.

Removal saves `sparkrun_overrides.excluded_deployments` in the operator fragment,
using the generated deployment's stable ID. The SQLite managed configuration
merge applies exclusions before validation and runtime construction, in the same
revision-checked transaction as other operator edits. No edit to `proxy.yaml`
is required. The raw sparkrun set remains its source declaration; subsequent
syncs, disappearance/reappearance, and restarts cannot override the exclusion.
It applies only to generated deployments, never operator-created deployments.

**Excluded sparkrun deployments** at the bottom of Model Deployments provides
**Restore**, followed by Validate and Save. Restore uses the latest reported
source settings. A binding with the same deployment ID remains excluded until
restored; a new recipe fingerprint produces a distinct deployment ID.

Exclusion removes the deployment from routing and automatic activation. It does
not stop a running workload. The normal generation switch removes its idle
policy; explicit workload stop/sleep operations are separate. This is removal
from sparkroute, not the future promote/demote availability-mode control.

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


## Recipe-provided capabilities and request profiles

The sparkrun recipe may contain an optional top-level `sparkroute` mapping:

```yaml
sparkroute:
  capabilities: [vision]
  request_profiles:
    low:
      chat_completions:
        temperature: 0.2
      responses:
        reasoning: {effort: low}
```

The recipe picker previews these defaults. Adding the public model `coding`
imports `coding:low` (and suffixed aliases) into the same draft, pointing to the
same deployment. The existing Request profiles table under `coding` edits these
parameters. Recipe/lifecycle edits preserve existing operator profiles; defaults
are imported when creating a public model, not continuously overlaid onto edits.

`capabilities` describes support, not a requirement for every request. Profiles
map selectors to ingress operations and JSON parameter objects, using the same
protected-field rules and recursive override behavior as manually configured
profiles. Invalid selectors, protected request structure, and existing
model/alias collisions are rejected before the draft can be saved. Runtime API
declarations remain separate from these optional model features.

The generated sparkrun set follows the recipe of a loaded binding on sync, or
its saved launch recipe for discovery-only jobs. Those profiles are read-only.
Recipe settings do not alter workload identity or create additional workloads.
