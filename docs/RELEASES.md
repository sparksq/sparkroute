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
