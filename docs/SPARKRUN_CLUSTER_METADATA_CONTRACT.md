# SparkRun named-cluster display metadata

Bridge schema **2** adds optional `cluster_name` to endpoint replies from
`discover`, `status`, and `ensure_ready`. This is the human-readable named cluster
recorded in SparkRun job metadata under `cluster`. `cluster_id` and `job_id` remain
the opaque workload identifiers; the new field never selects placement, authorizes
stop, contributes to endpoint identity, or changes binding revisions/fencing.

Both components require schema **2**. This is the integration's first use, so no
schema-1 response filtering or protocol downgrade path is retained. Install the
plugin and its pinned SparkRoute binary together. Replies must match the request
ID and current schema; mismatched versions, uncorrelated replies, and activation
failures are rejected without retrying the operation. Parse-error replies preserve
recoverable request IDs and the requested version for useful diagnostics.

A named cluster is optional, at most 1024 UTF-8 bytes, with no control characters.
Missing or invalid display metadata is omitted without rejecting an otherwise
usable endpoint. Older jobs may have no cluster field; overlapping configured host
sets cannot establish which named cluster launched such a job, so no host-set
inference or metadata backfill occurs automatically.

SparkRun discovery and its public proxy endpoint type expose `cluster_name` and
`recipe_revision` from the recorded job. The plugin also reads those fields from
job metadata when running with an older compatible SparkRun host. Healthy endpoint
snapshots enrich recipe-backed deployment titles by matching the **launch recipe
fingerprint**, never just the model name. This includes bindings that have no
configured cluster candidates. A local optional display cache retains the snapshot
across CLI calls; model and recipe labels occupy separate namespaces.

Title preference is observed named cluster, then configured cluster candidates,
then `unassigned` for a recipe binding. Discovery-only titles retain the opaque
cluster ID or `discovered` fallback when no name is recorded. Multiple observed
names are sorted and comma-separated. Stopped jobs and other recipes serving the
same upstream model cannot contribute to a recipe binding's observed cluster list.

SparkRoute also retains the bridge's named cluster in runtime endpoint metadata.
Admin status can enrich generated target titles from ready runtime registrations,
including after on-demand activation, without rewriting managed configuration.
The console uses those titles in Overview and in generated deployment forms and
routing choices. Operator draft values, raw stored generated JSON, routing target
IDs, and compare-and-swap revisions remain unchanged by this display overlay.
