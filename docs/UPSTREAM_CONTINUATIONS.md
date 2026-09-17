<!--
SPDX-FileCopyrightText: 2026 Scitrera LLC
SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
SPDX-License-Identifier: AGPL-3.0-only
-->

# Upstream inference result continuations

Some inference hosts return HTTP 303 with a result URL while a request is still
running. SparkRoute can retrieve that result before decoding or translating the
provider's response. This transport policy is independent of provider type,
native API protocol, and authentication.

Set **Inference result continuations → Result continuations** in the provider
editor, or configure the provider's `continuations` field:

```json
{
  "name": "hosted-models",
  "type": "anthropic",
  "base_url": "https://inference.example/v1",
  "continuations": "same_origin_303"
}
```

| Value | Behavior |
| --- | --- |
| Omitted or empty | Preserve legacy behavior: follow only `openai_compatible` inference requests carrying both `Modal-Key` and `Modal-Secret`. |
| `none` | Disable continuations, including legacy Modal detection. |
| `same_origin_303` | Follow bounded HTTPS 303 result continuations for any inference provider, including public endpoints without credentials. |

Enable the explicit policy only for endpoints that use 303 to return an inference
result URL. Ordinary HTTP 303 redirects can also point to other resources, such as
login pages; the policy does not establish that the target serves an inference
result.

## Retrieval contract

- Follow at most eight 303 hops, all on the original HTTPS host and port.
- Resolve relative locations against the response that supplied them.
- Use GET without replaying the inference body or its content headers.
- Preserve configured authentication and protocol headers. AWS SigV4 requests
  are signed again for each GET, using the deployment credential override when
  present and the empty body hash.
- Preserve the original attempt context, cancellation, and deadline. Hops do not
  extend timeouts or consume additional routing attempts.
- Decode the final successful response through the normal buffered or streaming
  protocol path, including protocol translation.
- Reject foreign origins, HTTP downgrades, userinfo, fragments, invalid locations,
  excessive hops, and unsuccessful retrieval responses. Other redirect status
  codes retain the no-follow policy.
- Suppress continuation URLs in retrieval errors and downstream Location headers;
  result URLs may contain signed tokens.

Once a continuation is received under an enabled policy, retrieval failure is
terminal: SparkRoute does not retry or fall back by submitting the inference
again. This includes failed GET authentication and timeouts. Initial subscription
authentication recovery can still replay an initial 401 rejection before any
continuation has been received. An accepted but unretrieved request may already
have executed; operators must account for that when deciding whether to retry.

## API scope

The shared inference path covers Chat Completions, Responses creation and compact,
embeddings, Anthropic Messages and token counting, Gemini generation and token
counting (including translated embeddings), and Bedrock Converse. Buffered and
streaming responses use the same continuation policy.

Provider-owned Responses resource operations (retrieve, delete, cancel, and
input items), Files, and Conversations use separate request paths and retain
their no-follow policy. Health checks, credential exchange, and metadata requests
are also outside this inference policy.

See [Modal configuration](MODAL.md) for the existing compatibility behavior.
