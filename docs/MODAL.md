# Modal proxy inference

OpenAI-compatible Modal endpoints can authenticate through provider header
references for `Modal-Key` and `Modal-Secret`. Both values must use `value_from`;
do not put credentials directly in configuration.

A long-running request can return HTTP 303 while Modal retains its result.
For authenticated Modal inference, SparkRoute follows up to eight HTTPS 303
continuations on the original host and port. Continuations use GET with no
inference body, preserve the configured credentials and request deadline, and
can return either a buffered response or an SSE stream. Other redirects retain
the gateway's no-follow policy; credentials never follow a different origin.

After a continuation is received, retrieval failure is terminal for that routing
attempt: SparkRoute returns an error without submitting the original POST again.
Continuation URLs can contain signed tokens and are not exposed in error messages
or returned as downstream Location headers. An operator should treat an accepted
but unretrieved request as potentially executed when deciding whether to retry.
