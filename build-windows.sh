#!/usr/bin/env bash
set -euo pipefail

repo_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
output_path="${1:-${repo_dir}/dist/taskswitcher-toolbar.exe}"
version="$(tr -d '\r\n' < "${repo_dir}/version.txt")"

mkdir -p "$(dirname -- "${output_path}")"
cd "${repo_dir}"

GOOS=windows GOARCH=amd64 go build \
  -trimpath \
  -ldflags "-H=windowsgui -X main.AppVersion=${version}" \
  -o "${output_path}" \
  ./cmd/taskswitcher-toolbar

printf 'Built %s (version %s)\n' "${output_path}" "${version}"
