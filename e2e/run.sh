#!/usr/bin/env bash
# CMA-facade e2e: native Anthropic SDK against `ahsir start --cma-listen` (echo provider).
set -euo pipefail
cd "$(dirname "$0")/.."
python3 -m pip install -q -r e2e/requirements.txt
exec python3 -m pytest e2e/ -q "$@"
