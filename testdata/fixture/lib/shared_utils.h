#ifndef HERBARIUM_FIXTURE_SHARED_UTILS_H
#define HERBARIUM_FIXTURE_SHARED_UTILS_H

int add_ints(int a, int b);
int mul_ints(int a, int b);
int compute(int a, int b);
int hook(int x);
void never_called(void);

/* Two functions written differently in source but semantically identical
   at the gimple level after -O2 lowering, so GCC's -fipa-icf pass folds
   them into a single link-time symbol. Exercised by both apps so both
   have a live callsite. */
int icf_add_one(int x);
int icf_bump_by_one(int x);

#endif
