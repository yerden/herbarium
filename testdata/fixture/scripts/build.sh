#!/usr/bin/env bash
# Phase 0 reproducer: configure + build the fixture with the dump flags
# herbarium expects. Run from anywhere; paths resolve relative to this script.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
builddir="${1:-$here/builddir}"

c_args=(
  -g -gcolumn-info
  -fcallgraph-info=su,da
  -fdump-ipa-cgraph
  -fdump-ipa-inline
  -fdump-ipa-devirt
  -fdump-ipa-icf
  -fno-inline-functions-called-once
  -ffunction-sections
  -fdata-sections
)

rm -rf "$builddir"
meson setup "$builddir" \
  --buildtype=debugoptimized \
  "-Dc_args=${c_args[*]}" \
  "$here"

meson compile -C "$builddir"
