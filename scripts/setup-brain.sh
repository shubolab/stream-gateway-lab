#!/bin/sh
set -eu
cd "$(dirname "$0")/../brain"
command -v uv >/dev/null 2>&1 || { echo 'Install uv first: https://docs.astral.sh/uv/getting-started/installation/' >&2; exit 1; }
# Do not inherit unrelated user-level package indexes. uv.lock pins the dependency graph.
uv --no-config sync --frozen --python 3.12 --default-index https://pypi.org/simple
if [ ! -f model.local.json ]; then
  (umask 077; cp model.example.json model.local.json)
fi
printf '%s\n' 'Brain ready. Fill base_url, api_key, model in brain/model.local.json; then start Gateway.'
