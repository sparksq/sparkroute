# Third-party and upstream notices

SparkRoute is distributed as a combined work under the GNU Affero General
Public License, version 3 only (`AGPL-3.0-only`).

The initial codebase incorporates and extends:

- Scitrera LLM Gateway, revision `91b1891`, originally published for its OSS
  core under Apache License 2.0. Copyright 2026 Scitrera LLC.
- Fox SparkRoute, revision `c752b02`, licensed under AGPL-3.0-only. Copyright
  Fox Engine Ltd.
- Scitrera LLM Protocol for Go, licensed under Apache License 2.0. Copyright
  2026 Scitrera LLC.
- Scitrera LLM Client for Go, licensed under Apache License 2.0. Copyright
  2026 Scitrera LLC.
- NVIDIA NeMo Switchyard, licensed under Apache License 2.0. Copyright 2026
  NVIDIA CORPORATION & AFFILIATES. SparkRoute's independently implemented
  stage-router scoring and tool-signal design were informed by that work.

The retained Apache-2.0 and AGPL-3.0-only texts are in `LICENSES/`. The root
`LICENSE` is the license for the combined SparkRoute distribution.

Go and npm dependencies remain under their respective licenses. Their exact
versions are recorded in `go.mod`, `go.sum`, and the frontend lockfiles.
