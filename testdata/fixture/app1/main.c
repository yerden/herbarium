#include <stdio.h>
#include "dispatch.h"
#include "shared_utils.h"

static int use_dispatch(int a, int b) {
    int s = g_ops.add(a, b);
    int p = g_ops.mul(a, b);
    return s + p;
}

int main(int argc, char **argv) {
    (void)argv;
    int r = compute(argc, 3);
    r += use_dispatch(2, 5);
    r += hook(r);
    r += icf_add_one(r);
    r += icf_bump_by_one(r);
    printf("app1: %d\n", r);
    return 0;
}
