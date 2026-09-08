# MMBridge projection configuration

In the standalone gateway with SQLite configuration, open **Configuration →
Advanced Options → MMBridge projection**. Select **Enabled**, enter the MMBridge
`/v1` base URL and a shared bearer-token credential reference, and optionally set
the default analyzer virtual model and timeout. Choose **Validate**, then **Save**.
The gateway applies the connection without restarting; requests already in
progress finish with their original settings.

For example, the operator configuration can contain:

```json
{
  "mm_projection": {
    "enabled": true,
    "url": "http://localhost:8100/v1",
    "token_ref": "env://MMBRIDGE_TOKEN",
    "analyzer_model": "vision-analyzer",
    "timeout_ms": 600000
  }
}
```

The URL is resolved from the gateway host. The token reference uses the existing
credential sources, such as an environment variable or an allowed credential
file. With `env://MMBRIDGE_TOKEN`, set that variable in the gateway process's
environment to the shared bearer token. Restart the process if changing its
environment. Store only the reference in configuration. Do not embed credentials
in the URL. Validation checks the configuration and credential source setup;
**Test bridge** resolves the credential and checks the bridge's model discovery
and projection capability contract. It tests the saved, active connection and
does not perform inference. A successful test does not validate the analyzer's
ability to process a particular input.

The connection alone does not enable projection for every model. Under **Model
Routing**, enable multimedia projection for the appropriate selector or candidate
model. Choose an analyzer virtual model capable of handling the media and a
failure behavior: fall back to the original request or reject the request when
projection fails. A routing policy can override the connection's analyzer and
timeout. The connection timeout defaults to 10 minutes; an explicit value must be
between 1 second and 15 minutes.

The modes have these meanings:

| Mode | Stored configuration | Result |
| --- | --- | --- |
| Use startup settings | `mm_projection` omitted | Uses `-mm-projection-*` flags or corresponding environment variables; disabled when no connection is supplied. |
| Disabled | `"enabled": false` | Overrides a startup connection and disables projection execution. Previously entered fields can be retained for later re-enabling. |
| Enabled | `"enabled": true` plus connection fields | Replaces the startup connection settings as a unit. |

The connection belongs to the operator set. sparkrun-generated configuration
cannot change it. The same `mm_projection` object is accepted by standalone file
configuration, where changes require a gateway restart.
The cluster profile continues to use its deployment-level connection settings.
