<!--
SPDX-FileCopyrightText: 2026 Scitrera LLC
SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
SPDX-License-Identifier: AGPL-3.0-only
-->

# Security

For a suspected vulnerability, use the repository's private
[Report a vulnerability](https://github.com/sparksq/sparkroute/security/advisories/new)
form when available. If private reporting is unavailable, open an issue asking
for a private reporting channel without including exploit details, credentials,
prompts, traces, or personal data. Include affected versions and reproduction
steps in the private report.

Before the first release, fixes target `main`. After v0.0.1 is released, security
fixes target the latest release; older development snapshots have no separate
maintenance branch or response-time guarantee.

The default listeners bind to loopback. Review the authentication modes in the
[README](README.md#admin-authentication) before exposing a listener. In particular,
admin `token-file` mode allows local writes when its file is missing or empty.
Usage traces can contain request and response content; keep their storage private
and configure recording and retention deliberately.
