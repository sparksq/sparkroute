# Provider-owned state selector contract

Status: implemented in the native OpenAI Chat Completions, Responses, and
Responses Compact request path.

Provider-owned IDs are meaningful only to the provider target that created
them. A request which consumes one of those IDs therefore has a stronger
continuity requirement than an adaptive logical-model selector such as `auto`.

## Contract

1. The gateway resolves every referenced response, Conversation, provider item,
   and provider file in the authenticated caller scope before invoking a model
   selector. No prompt or provider resource ID is disclosed to the selector.
2. Every resolved reference must identify the same canonical virtual model,
   provider, deployment, and upstream model. Missing, deleted, expired,
   cross-model, or cross-target state fails closed before provider credentials
   or traffic leave the gateway.
3. When the request model is a selector, the gateway supplies a bounded
   `ProviderStatePin` containing only the canonical virtual-model name and a
   low-cardinality state kind. A router must select exactly that model or fail.
   The gateway validates the returned decision again so a noncompliant external
   router cannot escape the pin.
4. The native router treats the pinned model as an explicit choice. It does not
   advance round-robin counters, evaluate keyword selection, or adaptively
   reselect. Current model-level policy still applies. Selector-default policy
   is not inherited because the affinity record intentionally stores no
   selector-policy snapshot.
5. The execution layer re-resolves provider state after projection and
   pre-guardrails, applies the provider/deployment/upstream-model hard pin, and
   uses the existing single-attempt rule. Health, capability, admission, and
   configuration checks remain authoritative; the pin cannot restore an
   ineligible target or fail over opaque state to another provider.
6. The caller-facing selector spelling remains the response model and
   `requested_model`; the canonical pinned model remains `virtual_model` in
   ledger and saved-trace records. OpenTelemetry records
   `llm.gateway.routing.state_affinity.pinned` and the bounded
   `llm.gateway.routing.state_affinity.kind` without recording resource IDs.

Concrete configured model names and aliases keep their existing semantics. A
concrete model which disagrees with provider-owned state is rejected as a model
mismatch. The affinity intentionally records the canonical model rather than a
create-time selector spelling, so a later selector name is presentation and
telemetry input, not a way to rebind the chain.

## Failure behavior

- state lookup unavailable: `503 state_affinity_unavailable`;
- missing/deleted/expired state: `409 state_affinity_not_found`;
- references from different logical models: `409
  state_affinity_model_mismatch`;
- references from different provider targets: `409 state_affinity_conflict`;
- pinned logical model no longer selectable/configured, or a router attempts to
  escape it: `503 state_affinity_model_unavailable`; and
- pinned target unavailable or changed: the existing
  `state_affinity_target_unavailable` / `state_affinity_target_changed`
  execution errors.

This contract is deliberately storage-neutral. Memory, SQLite, and PostgreSQL
affinity implementations all expose the same content-free routing identity.
