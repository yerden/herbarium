#include "shared_utils.h"

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
