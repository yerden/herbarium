#include "shared_utils.h"

__attribute__((weak))
int hook(int x) {
    return x + 1;
}
