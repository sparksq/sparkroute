#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Scitrera LLC
# SPDX-FileCopyrightText: 2026 Fox Engine Ltd.
# SPDX-License-Identifier: AGPL-3.0-only


set -euo pipefail

requests=()
traces=""
output="-"
allow_extra="false"

usage() {
  echo "usage: $0 --requests REQUESTS.jsonl --traces TRACES.jsonl [--output PATH|-] [--allow-extra]"
}

while (($# > 0)); do
  case "$1" in
    --requests) requests+=("$2"); shift 2 ;;
    --traces) traces="$2"; shift 2 ;;
    --output) output="$2"; shift 2 ;;
    --allow-extra) allow_extra="true"; shift ;;
    --help|-h) usage; exit 0 ;;
    *) echo "unknown argument: $1" >&2; usage >&2; exit 2 ;;
  esac
done

if ((${#requests[@]} == 0)) || [[ -z "$traces" ]]; then
  usage >&2
  exit 2
fi

repository="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$repository"
args=(-traces "$traces" -output "$output")
for requests_path in "${requests[@]}"; do
  args+=(-requests "$requests_path")
done
if [[ "$allow_extra" == "true" ]]; then
  args+=(-allow-extra)
fi
"${GO_COMMAND:-go}" run ./benchmarks/blackbox/traceverify "${args[@]}"
