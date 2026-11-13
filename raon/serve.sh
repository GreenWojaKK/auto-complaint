#!/bin/bash
# Starts the Raon generation server on the WSL AMD device: takes the engine environment from the rdna4-mxfp6a8
# repository (its raon/env.sh assembles the vLLM tree with the RDNA4 kernels) and runs serve.py in one process.
# RDNA4_MXFP6A8 names that repository, the folder next to this repository unless set. serve.py reads the checkpoint,
# context length, KV cache size and port from its own variables.
set -euo pipefail
here=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
source "${RDNA4_MXFP6A8:-$here/../../rdna4-mxfp6a8}/raon/env.sh"
cd -- "$HOME"
exec "$RAON_PYTHON" -u "$here/serve.py"
