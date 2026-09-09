<!--
SPDX-FileCopyrightText: 2026 Scitrera LLC
SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
SPDX-License-Identifier: AGPL-3.0-only
-->

# Deployment overview

Overview combines routing health and workload lifecycle in one inventory. Each
deployment ID has one row, joining target status, its activation binding, and
registered serving endpoints. Separate deployments of the same model stay
separate. An activatable deployment remains visible before its first start and
after stopping; expired orphan endpoints remain in Diagnostics only.

Lifecycle and routing health are independent. A running job can have an open
circuit. A stopped or sleeping activatable workload is a normal inactive state.
Missing lifecycle observations display Unknown rather than implying that a job
has stopped. Hosted deployments are labeled Externally managed rather than
inferring process readiness from the circuit state.

The activity counters filter the inventory. Search covers model names and
aliases, deployment identity/title, provider, and reported clusters. Lifecycle,
management, circuit, availability, and recent-failure filters can be combined.
Request totals use target counters when available, otherwise endpoint counters;
activation leases are shown separately in details and are never added to requests.

Select a deployment to inspect provider/model information, cluster/job identity,
queue limits and usage, activation progress/deadline, circuit failures and
recovery, serving endpoints, and recent lifecycle activity.

Start and Stop work with sparkrun independently of ColdSnap. Sleep and Wake appear
only when supported and appropriate to the reported lifecycle phase. Adopted
jobs expose status inspection but cannot be stopped, slept, or woken by the
gateway. Mutations wait for active requests/leases and other transitions; each
row displays its own progress and errors. Stopping retains the activation
binding. With the default wait policy, a subsequent request can start the job
again; a reject policy requires manual prewarming.

Activity contains the full, filterable lifecycle transition history. Diagnostics
contains controller health and endpoint inventory. Filters and pagination in
these views do not change deployment counts. The former `/admin/runtime` URL
redirects to Overview.

Status projections refresh independently every five seconds while the page is
visible. A failed source retains its last successful snapshot with a stale
notice and retry action; workload mutations are disabled until status recovers.
An older in-flight response cannot replace a newer refresh. Endpoint inventory
is loaded across all API pages before publishing its snapshot. Recent activity
is requested only for an expanded deployment or the Activity view.

`GET /v1/status` includes presentation fields on each target: `provider`, `model`,
`model_names` (configured virtual models and aliases), `endpoint_source`,
`controller`, and `cold_start` for activatable targets. This does not expose
provider credentials, recipe paths, overrides, or raw endpoint metadata.
