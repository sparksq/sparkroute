<!--
SPDX-FileCopyrightText: 2026 Scitrera LLC
SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
SPDX-License-Identifier: AGPL-3.0-only
-->

# Operator configuration presets

The top-right **Configuration preset** selector shows **Default** or the active
named preset. Choose **Save as preset…** to copy the saved operator configuration
and select the new preset. Subsequent configuration saves update that preset.
Save any unsaved draft before creating or managing presets.

Choose a preset to load its operator configuration. Loading checks the merged
configuration against the current generated inventory and revalidates sparkrun
recipe bindings. A failed check leaves the current configuration and selection
intact. An unsaved draft requires confirmation before it is discarded.

**Manage presets…** provides rename and delete actions. Default cannot be renamed
or deleted, and the active preset must be deselected before deletion. Names may
contain 1–64 characters; up to 64 presets, including Default, are supported.

Presets include the entire operator managed set: providers, deployments, virtual
models, routing, profiles, settings, and operator overrides of generated entries.
They exclude the sparkrun generated set, credentials stored separately from
configuration, and runtime job state. Loading a preset applies configuration;
it does not issue workload Start or Stop actions. Workload lifecycle policies
continue to apply normally.

SQLite stores the active preset and operator document together. SparkRoute
resumes that selection and configuration on restart. Existing installations
receive a Default preset containing their current operator configuration.

## API

`GET /v1/config/presets` returns `active_preset`, `active_revision`,
`presets_revision`, and preset metadata. It requires `config_read` or
`config_write`; the sparkrun reconciliation role cannot access presets.

The following POST operations require `config_write`:

| Path | Additional fields | Effect |
| --- | --- | --- |
| `/v1/config/presets/save` | `name` | Copy current operator configuration and select the new preset |
| `/v1/config/presets/activate` | `id` | Validate and load the saved operator document |
| `/v1/config/presets/rename` | `id`, `name` | Rename a named preset |
| `/v1/config/presets/delete` | `id` | Delete an inactive named preset |

Every mutation also requires `expected_active_revision` and
`expected_presets_revision` from the catalog. Stale requests return HTTP 409.
The separate presets counter protects selection changes even when two presets
contain identical configuration documents.

`GET /v1/config/managed-sets` also includes `active_preset` and
`presets_revision` for operator readers and writers. The administration UI sends
`expected_presets_revision` with operator configuration PUT requests, preventing
an older tab from overwriting a different selected preset. Existing API clients
may omit that optional field; their writes update the currently selected preset.
