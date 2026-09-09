<!--
SPDX-FileCopyrightText: 2026 Scitrera LLC
SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
SPDX-License-Identifier: AGPL-3.0-only
-->

# PII and guardrails

Create reusable profiles under **Configuration → PII Privacy** and
**Configuration → Guardrails**, then use **Policy profiles** under
**Virtual Models / Aliases** to assign them. You can also assign models directly
from either profile page. Use **Validate**, then **Save**, to publish changes.
Both policies are available in the standalone AGPL build. Enterprise uses the
same PII engine, with PostgreSQL storage remaining in enterprise.

Profile definitions and assignments belong to operator configuration, including
assignments to sparkrun-generated virtual models. Generated model/routing
settings stay read-only; their policy selectors are editable. Assignments survive
sparkrun refreshes and remain dormant when a model disappears, applying again if
the same canonical name returns. Changing a shared profile updates every assigned
model on configuration reload. Renaming a profile updates its references; remove
assignments before deleting a profile.

Profiles use these top-level configuration fields (a partial document):

```json
{
  "pii_profiles": {
    "personal": {"mode": "substitute", "entities": ["email", "phone"]}
  },
  "guardrail_profiles": {
    "safe": {"pre": [{"name": "safety", "model": "guard", "prompt": "Block secrets."}]}
  },
  "model_policies": {
    "coding": {"pii_profile": "personal", "guardrail_profile": "safe"},
    "coding:low": {"pii_profile": ""}
  }
}
```

Assignment keys are canonical virtual-model names; aliases use their canonical
model's policy. Request-profile variants inherit each policy assignment from
their parent unless overridden. An omitted reference retains existing inline
settings (or an inherited assignment); an empty string explicitly disables that
policy. In this example, `coding:low` disables PII but inherits the guardrail.
A selected profile replaces that kind of inline policy, rather than appending
checks or merging fields. The UI can copy existing inline settings into a reusable
profile without discarding the original settings.

Unknown profile references are rejected even for dormant models. Unused
profiles may refer to temporarily unavailable guardrail models; active policies
require their guardrail targets to exist. A guardrail is a call to the selected
model, so stopping a model used by an active guardrail requires updating that
policy first. Profile limits are 256 of each kind and 4096 model assignments.

## PII substitution

Enable **PII privacy** to replace detected values with opaque tokens before
provider delivery. Choose restoration for the caller or keep tokens masked.
Saved trace content defaults to masked; choosing caller-visible trace content
can retain personal information. Detection is best effort. The built-in detector
supports email, phone, US SSN, credit-card numbers, and IPv4 addresses. Names
and street addresses require an additional detector and are not offered as
built-in choices.

Request scope keeps mappings in memory for that request. Conversation scope
shares encrypted mappings across requests and process restarts. In standalone,
enable caller authentication (`-caller-auth-mode managed` or `token-file`) and
send a stable `X-SparkRoute-Thread-Id` header. Mappings are partitioned by the
authenticated caller and conversation; a conversation ID alone grants no access
to another caller's mappings. Conversation policies fail closed by default when
that identity is unavailable.

Conversation storage opens lazily when a policy needs it:

- Default SQLite path: `<config-file-or-config-sqlite-path>.pii/mappings.sqlite`.
- Default encryption key: `keyring.json` beside that database, generated for a
  fresh store and restricted to its owner. Despite the filename, generated keys
  use the compact base64 key format accepted by `pii.ParseKeyring`.
- Override with `-pii-sqlite` / `SPARKROUTE_PII_SQLITE` and
  `-pii-keyring-file` / `SPARKROUTE_PII_KEYRING_FILE`.
- An explicitly supplied key file must already exist. Back up the database and
  its key together. SparkRoute refuses to regenerate a lost key for an existing
  database. Keyring JSON supports rotation; retain old decryption keys and a
  stable lookup key when rotating, then restart SparkRoute.

Mappings use AES-GCM encryption and keyed lookup digests. Defaults expire idle
conversations after 24 hours and cap absolute lifetime at 30 days. The directory
prunes expired records periodically. File text and metadata inspection require
conversation scope, which the editor selects automatically. Multimedia text
inspection covers extracted text, not arbitrary pixels or audio content.

## Guardrails

Add ordered request and response checks, selecting a virtual model or alias and
optional policy instructions. SparkRoute supplies the JSON verdict format.
Checks can allow or block content; optional replacement is available for
non-streaming checks. The selected guardrail model's provider receives the
content being evaluated. Choose a suitable model and provider for that policy.

The failure setting controls errors and invalid verdicts. An explicit block
always blocks, even when failures are configured to continue. Streaming screening
requires at least one response check and buffers output before releasing it.
Streaming checks allow or block; they cannot replace content. The editor exposes
window and approved-context sizes with their supported bounds.
