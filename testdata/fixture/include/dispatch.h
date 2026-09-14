#ifndef HERBARIUM_FIXTURE_DISPATCH_H
#define HERBARIUM_FIXTURE_DISPATCH_H

/* Exercises the type plane: a named enum with explicit values, a typedef
   over it, and a struct field typed by that typedef. All three must be
   *used* by emitted code — DWARF omits declared-but-unused types. */
enum op_status {
    OP_STATUS_OK = 0,
    OP_STATUS_OVERFLOW = 7,
    OP_STATUS_UNSUPPORTED = 9
};

typedef enum op_status op_status_t;

struct ops {
    int (*add)(int a, int b);
    int (*mul)(int a, int b);
    const char *name;
    op_status_t last_status;
};

extern const struct ops g_ops;

#endif
