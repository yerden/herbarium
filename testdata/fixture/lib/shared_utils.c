#include "shared_utils.h"
#include "hdr_inline.h"

int add_ints(int a, int b) {
    return a + b;
}

int mul_ints(int a, int b) {
    return a * b;
}

int compute(int a, int b) {
    return add_ints(a, b) + mul_ints(a, b);
}

void never_called(void) {
}

/* always_inline is handled by GCC's early inliner, which fires before any
   IPA pass runs — so this fold is invisible to the .inline dump and shows
   up only in the optimization record and in DWARF. Nothing calls
   scaled_compute, so the fold also leaves no trace in the linked binary. */
static inline __attribute__((always_inline)) int scale_by_two(int v) {
    return v * 2;
}

int scaled_compute(int a, int b) {
    return hdr_clamp(scale_by_two(compute(a, b))) + hdr_clamp(a);
}
