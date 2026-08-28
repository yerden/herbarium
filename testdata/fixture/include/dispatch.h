#ifndef HERBARIUM_FIXTURE_DISPATCH_H
#define HERBARIUM_FIXTURE_DISPATCH_H

struct ops {
    int (*add)(int a, int b);
    int (*mul)(int a, int b);
    const char *name;
};

extern const struct ops g_ops;

#endif
