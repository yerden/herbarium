#include "shared_utils.h"

/* Two functions written differently but semantically identical at
   the gimple/RTL level. GCC's -fipa-icf pass at -O2 should fold them
   into a single winning symbol. Herbarium's list_icf_groups will
   report the folded pair once the ICF-persistence path is wired up. */

int icf_add_one(int x) {
    return x + 1;
}

int icf_bump_by_one(int x) {
    int y = x;
    y += 1;
    return y;
}
