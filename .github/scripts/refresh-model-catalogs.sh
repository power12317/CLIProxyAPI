#!/usr/bin/env bash
set -euo pipefail

models_repository="${MODELS_REPOSITORY_URL:-https://github.com/router-for-me/models.git}"
models_ref="${MODELS_REPOSITORY_REF:-main}"
catalog_dir="${MODEL_CATALOG_DIR:-internal/registry/models}"
codex_catalog="$catalog_dir/codex_client_models.json"
build_catalog="$catalog_dir/codex_client_models.build.json"
codex_candidate="$(mktemp)"
trap 'rm -f "$codex_candidate"' EXIT

git fetch --depth 1 "$models_repository" "$models_ref"
git show FETCH_HEAD:models.json > "$catalog_dir/models.json"

if git show FETCH_HEAD:codex_client_models.json > "$codex_candidate" &&
  go run ./cmd/validate_codex_models --file "$codex_candidate"; then
	mv "$codex_candidate" "$build_catalog"
	printf 'Refreshed validated Codex client model catalog.\n'
else
	rm -f "$build_catalog"
	printf '::warning::Remote Codex client model catalog is missing or invalid; using embedded fallback.\n'
fi

go run ./cmd/validate_codex_models --file "$codex_catalog"
