#ifndef HERBARIUM_FIXTURE_HDR_INLINE_H
#define HERBARIUM_FIXTURE_HDR_INLINE_H

/* A static inline whose body lives in a header. Called twice so the
   called-once heuristic can't be what folds it. GCC inlines it at every
   site and emits no .ci node for it, so the compiler plane never sees a
   location and falls back to the including TU; DWARF's abstract instance
   root is the only witness that the body is written here. Included by
   exactly one TU on purpose — the USR scheme anchors a static at its TU,
   so a header inline pulled into N TUs yields N distinct symbols. */
static inline int hdr_clamp(int x) {
    return x < 0 ? 0 : x;
}

#endif
