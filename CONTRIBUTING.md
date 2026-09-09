<!--
SPDX-FileCopyrightText: 2026 Scitrera LLC
SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
SPDX-License-Identifier: AGPL-3.0-only
-->

# Contributing

Open an issue or pull request in [sparksq/sparkroute](https://github.com/sparksq/sparkroute).
Describe the behavior you want to change and include a focused reproduction or
validation result. See [SECURITY.md](SECURITY.md) for vulnerability reports.

Use the Go toolchain in `versions.yaml` and Node 24. Before submitting code, run:

```sh
GOWORK=off go vet ./...
GOWORK=off go test -race ./...
(cd web && npm ci && npm run check && npm test && npm run build)
python3 -m unittest discover -s scripts -p 'test_*.py'
```

Commit rebuilt `pkg/adminui/ui` assets with frontend changes. Keep source headers
and upstream notices intact. New project files should carry the existing
SPDX copyright and `AGPL-3.0-only` headers; formats that cannot take comments
use narrowly scoped entries in `REUSE.toml`. Do not attribute imported code to
this project or change its license. Record its origin and retain its notices.

When updating shipped dependencies, run `python3 scripts/collect-third-party-notices.py`
after `npm ci`, review license changes, and rebuild the console. Check file
licensing with `uvx --from reuse==6.2.0 reuse lint`. See
[release qualification](docs/RELEASES.md) for the complete release checks.

Contributions are distributed under the applicable file licenses and the
project's AGPL-3.0-only distribution license. Submit only work you have the
right to contribute under those terms.
