<!--

The optional [daemon transport](SPARKRUN_DAEMON.md) uses direct HTTP/SSE for
lifecycle, catalog and profile requests. Bridge details below apply to the
explicit stdio transport.
SPDX-FileCopyrightText: 2026 Scitrera LLC
SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
SPDX-License-Identifier: AGPL-3.0-only
-->

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
Save recheck the recipe revision and named cluster for new or changed bindings.
The comparison uses the saved merged configuration at the expected active
revision. Full structural validation still applies to the entire candidate;
unchanged bindings do not require catalog resolution or import retention. This
also applies when activating a preset. Routing pools, weights, aliases and display
metadata can be edited even if an unchanged recipe has changed or is unavailable.
None of those paths launch models. Existing configuration-read and operator-write
roles apply; uploads and registry refresh require write permission.

## Automatic recipe updates

Recipe bindings follow trusted catalog changes automatically by default in managed
configuration mode (`-sparkrun-auto-update-recipes=false` opts out). The SparkRoute
plugin exposes this as `gateways.sparkroute.auto_update_recipes` in `proxy.yaml`.
Reconciliation checks every minute and waits until the previous workload receipt
is removed, including sleeping and unready workloads. It preserves deployment
IDs, routes, aliases, launch overrides and lifecycle policy; the next successful
cold start uses the updated pin. A request immediately after shutdown may need to
wait for the next reconciliation. Unavailable catalogs/inventory, untrusted
recipes, changed model/API contracts and invalid configurations leave the saved
binding unchanged and emit a gateway log warning. Automatic updates neither
refresh registries nor restart jobs. With the option disabled, Model Deployments
continues to offer explicit recipe review. Generated YAML bindings use the same
update policy, retaining their stable deployment IDs across discovery sweeps.

Model Deployments shows **Automatic recipe update pending** while a changed
recipe waits for the previous workload to be removed. With auto-updates disabled,
it shows **Recipe update available** and **Review recipe update** instead.
**Apply to draft → Validate → Save** explicitly adopts an update while preserving
the routing draft. A recipe changed again during an explicit binding edit returns
`recipe_revision_changed`, with deployment and saved/current revisions, and the
UI offers a direct review action. Unrelated saves never advance pins themselves;
automatic updates are separate audited configuration changes and use revision
checks to preserve concurrent edits.

A saved fingerprint detects drift but does not retain old recipe contents, so a
full historical diff is unavailable. Existing workloads are adopted, observed,
woken and stopped using their recorded identity. New launches still require an
exact match to the current recipe and enforce trust. Incomplete/truncated
inventory, controller operations in progress and unfinished launch receipts
prevent automatic updates from assuming the old workload is absent. The paired
core/plugin inventory exposes this completeness information; older inventories
without it leave automatic updates deferred.

## Prefer running targets

In **Virtual Models / Aliases → Routing pools**, **Prefer running targets
with capacity** is enabled by default for the whole virtual model, including
existing configurations that omit the setting. Uncheck it to save an explicit
`selection.prefer_running: false` opt-out. Linked request profiles inherit it. SparkRoute first
tries ready targets across all priority pools, preserving priorities, protocol
and capability preferences, and relative weights within that group. If no ready
target can serve, remaining targets retain the existing priority and cold-start
rules. Usual retry limits and budgets still apply; an explicit provider selector
preference and hard state-affinity pins retain their precedence.

Capacity means deployment concurrency/circuit admission and, for dynamic
endpoints, reported active requests plus this gateway's active leases against
endpoint concurrency limits. Expired, unauthorized and non-ready registrations
are not preferred. A zero concurrency limit means no declared limit; SparkRoute
does not estimate spare GPU memory or token throughput. Eligible static/hosted
endpoints are considered running without an additional probe.

Readiness is a non-reserving hint and admission rechecks it. A preferred running
target that loses readiness will not cold-start as part of that preferred attempt;
normal fallback and retry rules apply to remaining targets. This option does not
create routes, start jobs while editing, or change any deployment's cold-start
policy.

Selections are exact controller-local `catalog:<id>` references. sparkrun owns
registry resolution, normalized overrides, fingerprints, recipe/plugin checks,
and trust. Paths refer to the control node. Imports are untrusted single-file
YAML; abandoned imports expire after seven days and saved imports are retained.

Bridge schema v5 includes catalog operations, persisted operation status, endpoint
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

SparkRoute v0.0.2 adds opt-in [automatic recovery](WORKLOAD_RECOVERY.md) for
persistently failing owned jobs. Recovery has its own deadline and restart budget,
independent of idle shutdown and circuit ejection timing.

For v0.0.1, use idle intervals of at least one second (normally minutes).
Sub-second intervals can expire between the admission coordinator's 100 ms
readiness polls: an already queued request can miss the ready window and time
out. The default minute-scale settings avoid this known edge case.

## Recipe management and native APIs

The recipe public name defaults to `defaults.served_model_name`, falling back to
the Hugging Face model name. Advanced settings include ordered cluster fallback,
explicit capacity checks, and optional idle sleep for ColdSnap recipes. Catalog
metadata filters use declared values and preserve unknowns. Registry controls
require explicit trust acknowledgement; browsing never refreshes automatically.

Generated and operator-created deployments share the provider named `sparkrun`,
with type `sparkrun` and no provider-level overrides. The managed store keeps
this reserved provider in the read-only sparkrun set. Saving the first recipe
creates it there even without a prior plugin sync; a later sync reuses it, and
removing generated inventory cannot remove a provider still used by an operator
binding. Other providers retain the normal ownership and collision rules.

On the next configuration Save or sync, the store migrates uncustomized legacy
`sparkrun:operator` providers and their lifecycle deployment references to the
shared provider. Both fragments and the merged configuration are committed in
one transaction. Deployment IDs, binding revisions, aliases and lifecycle
settings are preserved; customized providers and non-lifecycle uses are left
alone. Validate previews this normalization without changing stored state.

The shared provider delegates endpoint management to the integration. Native
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

Overview **Start** activates an inactive recipe deployment without an inference
request, including one that has never launched. It uses the same bounded queue,
activation timeout and fencing as inference admission, and releases its lease
as soon as the workload is ready. An explicit Start can also prewarm a binding
whose inference policy rejects cold starts. **Stop** tears down the exact
SparkRoute-owned job after checking the current recipe and cluster receipt and
requiring zero active request leases. It removes serving endpoints and retains
the binding for a later Start or on-demand request. Both controls work without
ColdSnap; adopted workloads cannot be stopped from the gateway.

Overview sleep/wake requires actual ColdSnap use, an enabled lifecycle API,
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
parameters through operator patches. Recipe values follow their source automatically;
recipe/lifecycle edits preserve operator customizations.

`capabilities` describes support, not a requirement for every request. Profiles
are exposed under their parent virtual model, while implementations belong to
each deployment. Different recipes may implement the same public selector with
different parameters. Unsupported deployments/APIs are excluded rather than
silently receiving an unprofiled request.

The shorthand maps ingress operations to overrides. Expanded implementations
support `ingress_overrides`, `upstream_overrides`, and `supported_operations`.
Upstream overrides apply after translation to the named runtime operation.
For example, `supported_operations: [responses]` with
`upstream_overrides.chat_completions.chat_template_kwargs` configures a Responses
request translated to a Chat runtime. Declare every supported incoming operation;
an explicit supported operation without parameters is an intentional no-op.

Recipe-picker imports follow their source automatically. A read-only
`recipe_profiles` managed set persists refreshed definitions independently of
operator patches. Sources reconcile at startup, after operator/catalog updates,
and every 60 seconds; configure `-sparkrun-profile-refresh-interval` to change
that interval. Local recipe/catalog changes are followed; registry pulls remain
under the existing registry refresh policy. Uploaded YAML is an immutable source
until explicitly replaced. Profile-only edits do not change workload identity.
Source errors keep last-known-good settings; workload-changing recipe edits wait
for a deployment update. Confirmed profile deletion removes that implementation
from eligibility without erasing operator customizations.

**Use recipe profile** creates an inherited public profile. Add and Edit open a
modal with a scrollable editor and a fixed action footer. **Cancel** or Escape
discards the current profile edits; **Apply to draft** and **Delete profile**
update only the configuration draft. Save the draft to activate those changes.
Deleting default parameters keeps the base virtual model.
The **API configurations** list shows every incoming/runtime API entry, including
recipe values, common overrides, deployment overrides and suppressed fields.
Select a card to edit it, or choose **Add API configuration**, select an API and
stage, and enter its parameters. Switching entries preserves edits; Apply saves
all entries to the draft. A new empty entry is not persisted as an override.
Choose common or deployment-specific overrides using **Override scope**.
API availability notices identify recipe support and native-protocol/translation
limitations per deployment. They are advisory and do not enable APIs or replace
request-time parameter validation. Recipe support declarations remain in the
recipe; switching to operator-only parameters removes recipe inheritance.
 **Reset to inherited** removes the selected patch. Suppression
accepts one JSON pointer per line, such as `/temperature`; it removes a configured
override so caller parameters can pass through. JSON null is a literal value.
The deployment preview shows effective parameters and source revision/errors.

Profiles inherit parent pools, weights, aliases, policies, and limits; retries
prepare an independent request for each target. Existing flat `request_overrides`
variants remain compatible. **Review migration** previews linking a legacy variant
to its parent and preserves existing values as operator patches. Explicit generated
profiles remain read-only. The plugin and gateway must both support bridge v5.


## Discovery and public-name conflicts

Discovery and public routing are separate. A running workload may be imported as
a deployment without receiving a default virtual model when that public name is
already reserved. Duplicate names remain invalid in managed configuration.

The Overview inventory shows **Running** and **Not routed** independently, with
the conflicting route and an **Edit existing route** link. The deployment is
available in the Virtual Models / Aliases target picker. A user can add or replace
an existing target, or create a differently named route. No automatic correction
changes an operator route. Once referenced, a discovered deployment is retained
even if its workload stops; removing the reference permits discovery cleanup.

`GET /v1/config/managed-sets/sparkrun/reconcile-context` requires configuration
read or Sparkrun reconciliation permission. It returns:

* `active_revision`: the compare-and-swap revision for the complete snapshot;
* `document`: the current Sparkrun-owned fragment;
* `reserved_names`: other owners' public name claims (`name`, `owner`, `model`,
  `kind`), including derived profile aliases;
* `referenced_deployments` and `referenced_models`: incoming references into the
  Sparkrun fragment that a discovery sweep must preserve.

The snapshot is read transactionally and contains no other owner's provider,
credential, recipe, or policy configuration. It does not relax the validation or
permissions on ordinary managed-set replacements. A stale revision is rejected
with `revision_conflict`; the plugin must rebuild using a fresh snapshot.

`/v1/status` targets include `routing_state` (`routed` or `unrouted`) and, when a
discovered deployment's upstream model name is already in use, `routing_conflicts`
with the public `name` and existing canonical `model`. Unreferenced deployments
are included in status even when a routing implementation omits them.

The standalone admin API exposes `GET /v1/runtime/endpoints` when its live
endpoint registry is available. Like lifecycle status, it requires `status_read`
or explicitly enabled insecure admin access. Bootstrap advertises
`endpoint_inventory` only to callers who can read it. The console joins this
inventory with deployments, so an unexpired ready endpoint can show **Running**
even while its deployment remains **Not routed**. Expired or removed endpoints
do not count as running; inspecting them never starts a workload.

The endpoint API supports `target`, `controller`, `state`, `limit` (up to 500),
and opaque `cursor` filters. Responses contain `endpoints` and an optional
`next_cursor`, with placement, state, heartbeat/expiry times, and request counts.
The optional `cluster_name` supplies the display name (for example, `sparks25`),
while `cluster_id` retains its registry identity. Endpoint URLs, credentials,
fencing tokens, and other controller metadata are omitted.

Sparkrun's proxy configuration keeps SparkRoute-specific settings and generated
catalogs under `gateways.sparkroute` in `proxy.yaml`. Only that namespace is
supported; legacy locations are not read or migrated. This is separate from
SparkRoute's managed configuration database and does not rewrite operator routes.
