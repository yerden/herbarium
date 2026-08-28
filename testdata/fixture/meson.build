project(
  'herbarium-fixture',
  'c',
  default_options: ['c_std=c11', 'warning_level=2'],
)

# Dump flags come from the meson setup command line (-Dc_args=...).
# See scripts/build.sh. meson.build stays flag-agnostic on purpose:
# herbarium's contract is that users don't have to edit meson.build.

subdir('lib')
subdir('app1')
subdir('app2')
