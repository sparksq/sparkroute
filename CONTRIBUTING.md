<!--
SPDX-FileCopyrightText: 2026 Scitrera LLC
SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
SPDX-License-Identifier: AGPL-3.0-only
-->

# Contributing to SparkRoute

Open an issue or pull request in [sparksq/sparkroute](https://github.com/sparksq/sparkroute).
Describe the behavior you want to change and include a focused reproduction or
validation result. See [SECURITY.md](SECURITY.md) for vulnerability reports.

## Contributor License Agreement

Before a contribution can be merged, its contributor must accept the
[SparkRoute Contributor License Agreement](CLA.md) through a contribution process
designated by the Project Owners, Scitrera LLC and Fox Engine Ltd. You retain
ownership of your contributions; the CLA grants the rights described in the
agreement, including rights for open-source, commercial, and proprietary licensing.

The repository does not yet designate a particular electronic CLA acceptance
service. Until one is documented, coordinate acceptance with the Project Owners.
General discussion, feature requests, bug reports, and ideas that are not intended
as submissions of copyrightable material do not require CLA acceptance.

Submit only work you authored or are authorized to contribute under the applicable
file licenses and the CLA, including any necessary employer authorization.
Identify third-party material and its source and license when submitting it.

## Development and validation

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

Contributions incorporated into the publicly available project remain available
under the applicable Project License as described in the CLA. The CLA also grants
the Project Owners the licensing rights described in that agreement.
