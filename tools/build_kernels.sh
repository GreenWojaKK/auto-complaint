#!/bin/bash
# Builds the device kernels into libdllio_kernels.so with the HIP compiler of the ROCm SDK that the model worker's
# Python environment holds. The library goes next to the dllio binary: the first argument names that folder,
# ../Dllio-artifact/bin unless given. DLLIO_GPU_ARCH names the device architecture, gfx1201 unless set.
set -euo pipefail
root=$(cd "$(dirname "$0")/.." && pwd)
out="${1:-$root/../Dllio-artifact/bin}"
python="${DLLIO_PYTHON:-$HOME/.venvs/vllm-rocm10/bin/python}"
sdk=$("$python" -I -c 'import os, _rocm_sdk_core; print(os.path.dirname(_rocm_sdk_core.__file__))')
arch="${DLLIO_GPU_ARCH:-gfx1201}"
mkdir -p "$out"
object=$(mktemp --suffix=.o)
trap 'rm -f "$object"' EXIT
export ROCM_PATH="$sdk" HIP_DEVICE_LIB_PATH="$sdk/lib/llvm/amdgcn/bitcode" DEVICE_LIB_PATH="$sdk/lib/llvm/amdgcn/bitcode"
"$sdk/bin/hipcc" -O3 -std=c++17 -fPIC -fno-gpu-rdc --offload-arch="$arch" -I"$sdk/include" \
  -c "$root/kernels/int4_linear.hip" -o "$object"
# The SDK holds the runtime under its versioned name only.
c++ -shared "$object" "$sdk"/lib/libamdhip64.so.[0-9]* -o "$out/libdllio_kernels.so"
