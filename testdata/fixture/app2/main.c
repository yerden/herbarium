#include <stdio.h>
#include "shared_utils.h"

int main(int argc, char **argv) {
    (void)argv;
    int r = compute(argc, 7);
    r += hook(r);
    r += icf_bump_by_one(r);
    printf("app2: %d\n", r);
    return 0;
}
