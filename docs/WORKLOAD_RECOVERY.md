<!--
SPDX-FileCopyrightText: 2026 Scitrera LLC
SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
SPDX-License-Identifier: AGPL-3.0-only
-->

# Automatic workload recovery (v0.0.2)

Recovery is disabled by default. Enable it in **Edit recipe settings → Automatic
recovery**, or add this object to an activatable Sparkrun deployment's
`endpoint_source`:

```json
"recovery": {
  "action": "restart",
  "failed_probes": 3,
  "unhealthy_for": "2m",
  "drain_timeout": "30s",
  "backoff": "1m",
  "max_backoff": "15m",
  "max_restarts": 3
}
```

Only `action` is required; the remaining values shown are defaults. Omit the
object to disable recovery. `restart` stops the exact owned job and immediately
asks Sparkrun to launch a replacement and verify serving readiness. `stop` shuts
down the job and leaves subsequent activation to the normal policy: an eligible
request starts it with `cold_start: "wait"`; `cold_start: "reject"` requires
explicit Start. Recovery always stops the process, even when idle management
normally uses ColdSnap sleep.

## Detection

Both the failed half-open probe threshold and minimum duration since a qualifying
circuit opening must be satisfied. The initial opening is not a failed probe.
A successful applicable inference result clears the evidence and cancels pending
recovery. More failures do not move the unhealthy deadline forward. Probes are
real requests admitted by the circuit; there is no synthetic background inference.

Qualifying failures are transport errors, per-try timeouts, upstream stream
read/idle failures, and HTTP 5xx except 503 (treated as overload). HTTP 429,
other 4xx responses, client cancellation, gateway configuration errors, overall
request-budget expiration, and local transformations do not trigger recovery.
Some can still open the routing circuit. Successful responses with unusable
model output need an additional quality signal and are not detected here.

Circuit protection continues through recovery. A replacement passes normal
endpoint authorization and fencing before serving. Its next successful half-open
request closes the circuit; a restart does not blindly reset routing health or
replay failed requests. A ready, prewarmed replacement can also obtain a fresh
serving registration with `cold_start: "reject"`. That path cannot launch a
model: if its prepared readiness receipt expires or idle shutdown wins the race,
the request fails instead of implicitly cold-starting it.

## Idle and concurrency

Ordinary idle time starts when the last physical-job request lease ends, including
failed requests. Half-open probes can keep resetting that interval. Scheduled
recovery uses an independent timer and takes precedence over idle stop/sleep,
so it works with idle management disabled or with a long idle TTL.

Recovery shares the activation/manual-control workload gate and blocks new
admission while draining. Leases across aliases and configuration generations
must finish before stop. Exceeding `drain_timeout` fails visibly without killing
an active workload. Borrowed jobs are never automatically stopped or restarted;
Sparkrun rechecks ownership and the exact job at stop time.

## Retry budget and status

`max_restarts` bounds recovery attempts per recipe revision and actual cluster.
The initial stop/start consumes one attempt; startup retries and later failing
replacements consume the remaining budget. Startup retries use exponential
backoff capped by `max_backoff` and reconcile durable Sparkrun operations before
launching. Readiness success and configuration-generation changes do not refill
the budget. An uncertain stop reply or invalid replacement receipt fails closed
without launching another process.

Overview details and `GET /v1/runtime/status` expose `recovery`: phase, action,
failed probes, attempts, limit, next attempt time, and a bounded failure reason.
`failed` or `exhausted` blocks automatic admission. After inspecting the cause,
explicit **Start** resets the budget and retries normal readiness reconciliation.
**Stop** resets it after confirmed shutdown. Active request leases and ownership
checks still apply.

Timers and budgets are process-local: they survive configuration generations and
job replacement, but reset on gateway process restart. Independent gateways do
not coordinate recovery. Removing a binding or changing its effective recovery
policy cancels pending recovery. An already-admitted detached Sparkrun launch
can continue; later activation reconciles its receipt rather than duplicating it.
