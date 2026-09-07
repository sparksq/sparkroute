# SparkRoute OSS

SparkRoute OSS is a standalone, provider-neutral AI gateway distributed under
AGPL-3.0-only. It is the local/Sparkrun distribution of SparkRoute and the base
module used by the enterprise edition.

The Go module is `github.com/sparksq/sparkroute`. Provider protocol clients and
intermediate request/response types come from the independently reusable
Apache-2.0 `github.com/scitrera/go-llm v0.3.0` module.

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

File contents are flushed on all platforms. Windows filesystem trace storage
does not flush directory entries, so a sudden power loss can lose recently
created or renamed journal/session files. Ordinary process restart and incomplete
journal recovery are tested separately from power-loss durability.

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

## Configuration in the console

The Configuration sidebar contains **Providers**, **Model Deployments**,
**Virtual Models / Aliases**, and **Model Routing** for the operator-managed set.
These sections share a draft; navigating between them or other console pages
preserves edits. Saving validates and replaces the whole operator set, and the
stored/serving indicator follows runtime application. JSON edits apply to the
whole selected set. Reloading the page or using Refresh reloads stored configuration.

**SparkRun Generated** is a separate read-only view. Operator virtual models can
target deployments from either owner; generated targets are labeled in the picker.
Model-routing selectors can use virtual models from both owners. These references
do not copy or modify generated entities. Names and references are validated
against the merged configuration when saving.

Deployment capability controls show only optional **Vision** and **Files (file
inputs)** declarations. An unchecked option leaves support unspecified, so the
default try-first policy still applies unless overridden. Native protocol defaults
come from the provider. Existing advanced capabilities and explicit protocol
overrides are preserved and remain accessible in JSON. File inputs (`file_input`)
are distinct from a provider's Files resource API (`files`). Concurrency limits and
passive circuit health settings are grouped under **Concurrency and circuit policy**.

## Native provider configuration

The console separates the provider's native API from its authentication:

| Provider type | Default native protocol | API capability |
| --- | --- | --- |
| OpenAI (Chat), `openai` | OpenAI | Baseline Chat Completions |
| OpenAI (Responses), `openai_responses` | OpenAI | Responses, automatically enabled |
| Anthropic, `anthropic` | Anthropic Messages | Baseline Messages |
| OpenAI compatible (custom), `openai_compatible` | OpenAI | Existing explicit capability behavior |
| Google Gemini, `gemini` | Gemini | Existing explicit capability behavior |
| Amazon Bedrock, `bedrock` | Bedrock Converse | Existing explicit capability behavior |

Provider selection sets the native protocol default. Responses providers imply
`responses` for every associated deployment, including configuration supplied
as JSON. An explicitly unsupported Responses capability conflicts with that
provider type and is rejected. Other capabilities remain model-specific.
Choose a deployment's provider in Model Deployments; its native defaults and other
capability declarations are preserved. A Responses-native provider is not sent Chat
Completions requests; Chat-to-Responses translation is not implemented.

AWS region appears only for Bedrock. Its Base URL control is hidden; new
Bedrock configurations derive the runtime endpoint from the region and use
AWS SigV4 with a workload credential reference. Existing custom endpoints in
JSON remain preserved.

## Codex subscription providers

Select **Codex Subscription** in **Authentication type** for an OpenAI provider,
then choose **Sign in with ChatGPT**. The native API becomes OpenAI (Responses).
Changing authentication back to None, Bearer, or Custom header leaves subscription
mode; endpoint and credential-reference drafts are restored during editing.
Switching authentication does not sign out a shared subscription profile.
Already saved subscription providers switch back to the standard OpenAI endpoint
and need their ordinary API credential reference configured again.
Choose a credential profile name, request a one-time device code, open the OpenAI
sign-in link, and complete authentication. Enable device-code login in your
ChatGPT security settings or workspace permissions if necessary. The console
shows pending, expired, failed, and connected states and provides cancellation,
retry, and sign-out. See [OpenAI authentication](https://learn.chatgpt.com/docs/auth#login-on-headless-devices).

Subscription sign-in uses the account's Codex entitlement and limits. API-key
providers retain their separate configuration. Subscription providers currently
support `/v1/responses` with full transcript input, including streaming and
buffered replies. They require `store: false` (the default for this provider),
and do not support background execution, previous-response IDs, Conversations,
Chat Completions, embeddings, or provider-side response resources. Reasoning
settings and Codex custom `apply_patch` requests use go-llm's subscription profile.

A minimal provider/deployment pair is:

```json
{
  "providers": [{"name": "codex", "type": "openai_subscription", "subscription_profile": "personal-codex"}],
  "deployments": [{"name": "codex-model", "provider": "codex", "model": "YOUR_SUBSCRIPTION_MODEL", "capabilities": ["responses"]}],
  "virtual_models": []
}
```

Add a virtual model targeting that deployment, then validate and save the
configuration. Changing the profile name selects a different account binding;
renaming a provider does not move or rename its credentials. Providers that name
the same profile share the account, token refresh, and sign-out. Signing in does
not automatically save an edited configuration or change a route.

Managed SQLite configuration automatically uses
`<config-sqlite>.provider-auth.db` for provider credentials. File configuration
can enable sign-in with `-provider-auth-sqlite /private/path/provider-auth.db`.
This store must have a private parent directory and private file permissions
(or equivalent Windows ACLs). It contains unencrypted renewable credentials and
must be protected like a password; it is separate from configuration exports,
client API keys, and Codex CLI credentials. No OAuth token or device-auth ID is
returned to the browser, logs, or configuration. Backups of this database contain
secrets. The adjacent `.lock` database prevents concurrent SparkRoute processes
from sharing refresh-token ownership; use local disk, not network storage.

Sign-in administration requires `config_write` on an authenticated admin
connection, or the explicitly enabled loopback-only insecure admin mode. Device
codes are shown only to the administrator who started that sign-in. The fixed
OpenAI issuer and Codex endpoint cannot be overridden through configuration.
Credentials refresh before expiry; an upstream 401 permits one refresh and
replay within the existing request deadline. Logout first joins pending login
work and removes local credentials even if remote revocation fails; the UI
reports that failure. Pending sign-ins expire after ten minutes and are
cancelled on gateway shutdown. Stored credentials survive restarts.
