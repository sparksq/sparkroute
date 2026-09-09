<!--
SPDX-FileCopyrightText: 2026 Scitrera LLC
SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
SPDX-License-Identifier: AGPL-3.0-only
-->

# SparkRoute OSS releases

The public repository is `sparksq/sparkroute`. Build and release from its root;
`GOWORK=off` keeps builds independent of a surrounding Go workspace.

`versions.yaml` is authoritative for the Go binary and embedded web console.
Use `python scripts/update-versions.py --check` and
`python scripts/generate-ci-gha.py --check` to verify the pinned repo-tools
outputs. Only version checks, Go checks, and web checks are generated. Binary
and container publication workflows are maintained in this repository because
they also enforce the source, notices, and publication identity contract.

A `vX.Y.Z` tag must match the catalog. Release publication requires Go vet,
race tests, lint/security checks, the web tests/type-check/build, embedded UI
freshness, and archive packaging checks. A manual Release run produces candidate
artifacts; it publishes only when run on a matching tag in the exact public
repository. Forks and renamed repositories cannot publish through these jobs.

Binary targets are Linux, macOS (Darwin), and Windows, each on amd64 and arm64. With the embedded UI current,
run `python scripts/build-release.py` from a clean public checkout. It produces:

- `sparkroute_<version>_<os>_<arch>.tar.gz` (`.zip` on Windows) containing the executable, license
  and notices (including retained dependency license texts), and `build-info.json`;
- `sparkroute_<version>_source.tar.gz` containing precisely the corresponding
  committed source, build instructions, and dependency manifests;
- `checksums.txt` with the SHA-256 digest of each archive.

`./sparkroute --build-info` and the authenticated console identify the exact
public source commit. Archives use stable ordering, ownership, modes, and gzip
timestamps. Release builds stamp both version and commit. Reproducing a binary
also requires the same Go toolchain and embedded frontend assets.

The container is published with an immutable version tag after the same gates.
It includes `/licenses`, source/revision labels, and the stamped executable.
Its minimal runtime does not include Python or SparkRun: use host execution for
the SparkRun subprocess integration. Do not assume GPU workload lifecycle is
available just because the standalone routing container is running.

SparkRun's plugin verifies a pinned archive digest before using any release.
Keep the archive with its notices when distributing the extracted executable.
Development overrides are explicit and do not qualify a published release pin.

## Public release qualification

For v0.0.1, first run the Release workflow manually on the intended `main`
commit. Require successful Go/web, license/notice, archive, and native-platform
jobs before creating the tag. A job that never starts is not a passing check.
Changing repository visibility and creating the release tag are separate manual
decisions; neither happens during a source audit.

File licensing follows [REUSE 3.3](https://reuse.software/spec-3.3/). Commentable
source has SPDX headers; `REUSE.toml` covers generated data and immutable fixtures.
The BSD-3-Clause helper scripts retain their original license. Run:

```sh
(cd web && npm ci)
python3 scripts/collect-third-party-notices.py --check
uvx --from reuse==6.2.0 reuse lint
gitleaks git . --log-opts=--all --redact=100
```

Use Gitleaks 8.30.1 or a reviewed newer version. Scan all history intended for
publication, not just the working tree. Review findings before publishing;
do not commit scan reports containing secrets. Also review tracked fixtures,
documentation, and Git author metadata for material unsuitable for publication.

Dependency updates require a fresh `govulncheck ./...` and `npm audit`, notice
regeneration, and a console rebuild. The notice collector covers packages linked
into `cmd/sparkroute` with `CGO_ENABLED=0` on all six platforms and production npm
dependencies. It retains upstream license and notice texts with SHA-256 hashes;
review newly introduced components and license terms rather than assuming that
successful generation alone establishes compatibility. Go's toolchain license
is included. Container base-image contents retain their upstream licensing;
the runtime uses the pinned Debian 12 distroless image family.

The console exposes the full notices at `/admin/legal.html`; the same dependency
bundle ships beside the executable in every archive and under `/licenses` in the
container. Keep the exact corresponding source available at the commit reported
by `--build-info` and the console's Source link, including for modified builds.
