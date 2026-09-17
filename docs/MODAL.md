<!--
SPDX-FileCopyrightText: 2026 Scitrera LLC
SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
SPDX-License-Identifier: AGPL-3.0-only
-->

# Modal proxy inference

Modal endpoints can authenticate through provider header
references for `Modal-Key` and `Modal-Secret`. Both values must use `value_from`;
do not put credentials directly in configuration.

A long-running request can return HTTP 303 while Modal retains its result.
By default, SparkRoute follows continuations for `openai_compatible` providers
whose inference requests include both Modal headers. To enable retrieval for
public endpoints, other authentication methods, or other API protocols, set the
provider's `continuations` to `same_origin_303`. Set it to `none` to disable
retrieval explicitly. See [upstream result continuations](UPSTREAM_CONTINUATIONS.md).

When enabled, SparkRoute follows up to eight HTTPS 303
continuations on the original host and port. Continuations use GET with no
inference body, preserve the configured credentials and request deadline, and
can return either a buffered response or an SSE stream. Other redirects retain
the gateway's no-follow policy; credentials never follow a different origin.

After a continuation is received, retrieval failure is terminal for that routing
attempt: SparkRoute returns an error without submitting the original POST again.
Continuation URLs can contain signed tokens and are not exposed in error messages
or returned as downstream Location headers. An operator should treat an accepted
but unretrieved request as potentially executed when deciding whether to retry.
