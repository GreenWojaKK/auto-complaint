// Linear layers on int4 weights that stay packed: y = x W^T for a few rows of x, read straight from the checkpoint's
// packed words. The weight a column holds is what the checkpoint's own layer unpacks: (nibble - zero) * scale, rounded
// to BF16. Products of BF16 pairs are summed in FP32 and the sums rounded to BF16.
//
// Layout of one layer, as compressed-tensors stores it, with groups of 32 columns:
//   packed [out, in / 8]    one word holds 8 columns of a row, 4 bits each, the first column in the lowest bits
//   scale  [out, in / 32]   BF16, one per group of a row
//   zero   [out / 8, in / 32]   one word holds the zero points of 8 rows, 4 bits each; absent for symmetric weights
//
// A thread takes one row of the weight and all B rows of x over a run of columns, so each pair of weights it decodes
// meets B pairs of x. It reads memory a whole cache line at a time: 16 packed words are 128 columns, and the scales
// and zero points of 512 columns are loaded together. Reading a word here and a word there fetches each line many
// times over.
//
// Layers with few rows are also split along the columns to keep the device busy, and the pieces are summed afterwards
// in a fixed order, so the result is the same from run to run.

#include <hip/hip_runtime.h>

#include <cstddef>
#include <cstdint>

namespace {

constexpr int kBlock = 256;
constexpr int kGroup = 32;        // Columns that share a scale and a zero point.
constexpr int kUnit = 512;        // Columns whose scales and zero points a thread loads together.
constexpr int kLines = 4;         // Cache lines of packed words in a unit.
constexpr int kLineWords = 16;    // Packed words in a cache line: 128 columns.
constexpr int kUnitScales = 8;    // Words that hold the 16 scales of a unit, two to a word.
constexpr int kUnitZeros = 16;    // Zero point words of a unit, one to a group.
constexpr int kThreads = 16384;   // Threads a launch aims for at least.
constexpr int kPassRows = 20;     // Rows of x one pass takes. With more, a thread's sums no longer stay in registers.
constexpr int kRows = 32;         // Rows of x one call takes, in at most two passes.

typedef __bf16 bf16x2 __attribute__((ext_vector_type(2)));

__device__ inline bf16x2 as_pair(uint32_t bits) { return __builtin_bit_cast(bf16x2, bits); }

__device__ inline float from_bits(uint32_t bits) { return __builtin_bit_cast(float, bits); }

__device__ inline uint32_t to_bits(float value) { return __builtin_bit_cast(uint32_t, value); }

// The bits of a float rounded to BF16, to nearest and ties to even, in the upper half of the word. The lower half is
// not cleared.
__device__ inline uint32_t rounded(float value) {
    const uint32_t bits = to_bits(value);
    return bits + 0x7FFFu + ((bits >> 16) & 1u);
}

// The weights of two neighbouring columns as a pair of BF16: byte `index` of a packed word, with the zero point and
// the scale of its row and group.
__device__ inline uint32_t weight_pair(uint32_t word, int index, int zero, float scale) {
    const int low = static_cast<int>((word >> (8 * index)) & 15u) - zero;
    const int high = static_cast<int>((word >> (8 * index + 4)) & 15u) - zero;
    return (rounded(static_cast<float>(low) * scale) >> 16) | (rounded(static_cast<float>(high) * scale) & 0xFFFF0000u);
}

template <int B>
__global__ void row_kernel(const uint32_t* __restrict__ packed, const uint32_t* __restrict__ scale,
                           const uint32_t* __restrict__ zero, const uint32_t* __restrict__ x, float* __restrict__ partial,
                           int out, int in, int units, int segments) {
    const int thread = blockIdx.x * blockDim.x + threadIdx.x;
    const int segment = thread / out;
    if (segment >= segments) {
        return;
    }
    const int row = thread - segment * out;
    // Segments differ by at most one unit in length.
    const int first = static_cast<int>(static_cast<int64_t>(segment) * units / segments);
    const int last = static_cast<int>(static_cast<int64_t>(segment + 1) * units / segments);

    const uint32_t* words = packed + static_cast<size_t>(row) * (in / 8) + first * (kUnit / 8);
    const uint32_t* scales = scale + static_cast<size_t>(row) * (in / kGroup / 2) + first * kUnitScales;
    const uint32_t* zeros = zero ? zero + static_cast<size_t>(row >> 3) * (in / kGroup) + first * kUnitZeros : nullptr;
    const int shift = (row & 7) * 4;  // Where this row's zero point sits in a word.
    const uint32_t* values[B];
#pragma unroll
    for (int b = 0; b < B; ++b) {
        values[b] = x + static_cast<size_t>(b) * (in / 2) + first * (kUnit / 2);
    }

    float sum[B];
#pragma unroll
    for (int b = 0; b < B; ++b) {
        sum[b] = 0.0f;
    }
    for (int unit = first; unit < last; ++unit) {
        uint32_t s[kUnitScales];
        uint32_t z[kUnitZeros];
#pragma unroll
        for (int i = 0; i < kUnitScales; ++i) {
            s[i] = scales[i];
        }
        scales += kUnitScales;
#pragma unroll
        for (int i = 0; i < kUnitZeros; ++i) {
            // A symmetric layer stores its values offset by 8, which is a zero point of 8 in every row.
            z[i] = zeros ? zeros[i] : 0x88888888u;
        }
        if (zeros) {
            zeros += kUnitZeros;
        }
        for (int line = 0; line < kLines; ++line) {
            uint32_t w[kLineWords];
#pragma unroll
            for (int i = 0; i < kLineWords; ++i) {
                w[i] = words[i];
            }
            words += kLineWords;
            // The four groups of this line use the first two scale words and the first four zero point words. The
            // rest move down afterwards, which keeps every index here a constant.
#pragma unroll
            for (int group = 0; group < 4; ++group) {
                const float scale_here = from_bits(group % 2 ? s[group / 2] & 0xFFFF0000u : s[group / 2] << 16);
                const int zero_here = static_cast<int>((z[group] >> shift) & 15u);
#pragma unroll
                for (int word = 0; word < 4; ++word) {
#pragma unroll
                    for (int index = 0; index < 4; ++index) {
                        const bf16x2 weights = as_pair(weight_pair(w[group * 4 + word], index, zero_here, scale_here));
                        const int at = (group * 4 + word) * 4 + index;  // The pair of columns within the line.
#pragma unroll
                        for (int b = 0; b < B; ++b) {
                            sum[b] = __builtin_amdgcn_fdot2_f32_bf16(weights, as_pair(values[b][at]), sum[b], false);
                        }
                    }
                }
            }
#pragma unroll
            for (int b = 0; b < B; ++b) {
                values[b] += kLineWords * 4;
            }
#pragma unroll
            for (int i = 0; i + 2 < kUnitScales; ++i) {
                s[i] = s[i + 2];
            }
#pragma unroll
            for (int i = 0; i + 4 < kUnitZeros; ++i) {
                z[i] = z[i + 4];
            }
        }
    }
#pragma unroll
    for (int b = 0; b < B; ++b) {
        partial[(static_cast<size_t>(segment) * B + b) * out + row] = sum[b];
    }
}

// Sums the segments of every output in order and rounds to BF16.
__global__ void reduce_kernel(const float* __restrict__ partial, uint16_t* __restrict__ y, int count, int segments) {
    const int index = blockIdx.x * blockDim.x + threadIdx.x;
    if (index >= count) {
        return;
    }
    float sum = 0.0f;
    for (int segment = 0; segment < segments; ++segment) {
        sum += partial[static_cast<size_t>(segment) * count + index];
    }
    y[index] = static_cast<uint16_t>(rounded(sum) >> 16);
}

float* scratch = nullptr;
size_t scratch_floats = 0;

#define DPLUS_CASE(B) \
    case B: \
        hipLaunchKernelGGL((row_kernel<B>), dim3((threads + kBlock - 1) / kBlock), dim3(kBlock), 0, 0, packed, scale, \
                           zero, x, scratch, out, in, units, segments); \
        break;

// One pass: the sums of every segment into the scratch buffer, then their reduction into y.
int pass(const uint32_t* packed, const uint32_t* scale, const uint32_t* zero, const uint32_t* x, uint16_t* y, int out,
         int in, int rows) {
    const int units = in / kUnit;
    int segments = (kThreads + out - 1) / out;
    if (segments > units) {
        segments = units;
    }
    const size_t needed = static_cast<size_t>(segments) * rows * out;
    if (needed > scratch_floats) {
        if (scratch) {
            // What still reads the old buffer has to finish before it goes.
            (void)hipDeviceSynchronize();
            (void)hipFree(scratch);
        }
        scratch = nullptr;
        scratch_floats = 0;
        if (hipMalloc(&scratch, needed * sizeof(float)) != hipSuccess) {
            return -2;
        }
        scratch_floats = needed;
    }
    const int threads = out * segments;
    switch (rows) {
        DPLUS_CASE(1) DPLUS_CASE(2) DPLUS_CASE(3) DPLUS_CASE(4) DPLUS_CASE(5) DPLUS_CASE(6) DPLUS_CASE(7)
        DPLUS_CASE(8) DPLUS_CASE(9) DPLUS_CASE(10) DPLUS_CASE(11) DPLUS_CASE(12) DPLUS_CASE(13) DPLUS_CASE(14)
        DPLUS_CASE(15) DPLUS_CASE(16) DPLUS_CASE(17) DPLUS_CASE(18) DPLUS_CASE(19) DPLUS_CASE(20)
        default:
            return -1;
    }
    const int count = rows * out;
    hipLaunchKernelGGL(reduce_kernel, dim3((count + kBlock - 1) / kBlock), dim3(kBlock), 0, 0, scratch, y, count,
                       segments);
    return hipGetLastError() == hipSuccess ? 0 : -3;
}

}  // namespace

extern "C" {

// y [rows, out] = x [rows, in] times the transposed weight, both BF16, on the current device and its default stream.
// Returns 0, or a negative number when the layer or the number of rows is not one this kernel takes or a launch
// failed, in which case y holds nothing to use.
int dplus_int4_linear(const uint32_t* packed, const uint32_t* scale, const uint32_t* zero, const uint32_t* x,
                      uint16_t* y, int out, int in, int group, int rows) {
    if (rows < 1 || rows > kRows || group != kGroup || in % kUnit != 0 || out % 8 != 0) {
        return -1;
    }
    if (rows <= kPassRows) {
        return pass(packed, scale, zero, x, y, out, in, rows);
    }
    const int head = (rows + 1) / 2;
    const int code = pass(packed, scale, zero, x, y, out, in, head);
    if (code != 0) {
        return code;
    }
    return pass(packed, scale, zero, x + static_cast<size_t>(head) * (in / 2), y + static_cast<size_t>(head) * out, out,
                in, rows - head);
}

}  // extern "C"
