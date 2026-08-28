#include "dispatch.h"
#include "shared_utils.h"

const struct ops g_ops = {
    .add = add_ints,
    .mul = mul_ints,
    .name = "default_ops",
};
