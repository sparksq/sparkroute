# SparkRoute OSS architecture

SparkRoute OSS is a single-process gateway with immutable runtime generations.
A configuration source loads one validated `config.Document`; routing compiles
it into a snapshot; a new generation is atomically published; and requests
already using the old generation drain before its local lifecycle controller
is closed.

```text
OpenAI / Anthropic / Gemini / Bedrock request
                    |
                    v
 authentication -> normalization -> optional guardrails/model selection
                    |
                    v
 virtual model -> eligible target plan -> retry/fallback execution
                    |
                    +--> static provider endpoint
                    |
                    +--> Sparkrun lifecycle admission -> discovered endpoint
                    |
                    v
 protocol-native or translated upstream request
```

## Local persistence

- Configuration is read-only file data or a writable current-state-only SQLite
- Usage records use memory or SQLite.
- Saved traces use a dedicated/reused SQLite database or an owner-only bounded
  filesystem store.
- Response/conversation/file affinity uses process-local or SQLite-backed
  contracts as configured.

These implementations assume one gateway process owns lifecycle and writes.
They do not claim multi-replica filesystem safety.

## Sparkrun

Lifecycle-bearing deployments use the exported endpoint-registry and
controller contracts. The OSS composition supplies the Sparkrun controller,
which talks to `sparkrun gateway-bridge` using versioned JSON, periodically
discovers running models, refreshes registrations, and performs bounded
cold-start/stop transitions. Static configuration and Sparkrun-managed
configuration remain separate managed sets.

## Extension boundary

OSS defines interfaces for configuration watching, managed mutations,
credentials, identity, ledger/query/retention, saved traces, runtime endpoint
coordination, model routing, and exact-name external model resolution. The
enterprise distribution implements replicated variants without changing the
data-plane contracts.

The `modelcatalog.Resolver` path is deliberately present even when no external
resolver is configured. A validated exact-name miss can be resolved and then
re-enter ordinary capability, credential, lifecycle, routing, tracing, and
ledger handling.

## Administrative surfaces

Data, admin, and operations routes can share one listener for approachable
local use. Supplying a distinct admin or operations address removes that
surface from the data listener. The embedded React console discovers its
edition and authorized modules from the server; the OSS server exposes only
standalone capabilities.

## Dependency boundary

The OSS module directly depends on the Apache-2.0
`github.com/scitrera/go-llm` provider/protocol library and local persistence/
telemetry libraries. It does not depend on PostgreSQL, TimescaleDB, Aether, or
enterprise packages.
