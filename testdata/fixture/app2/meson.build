executable(
  'app2',
  ['main.c'],
  dependencies: libshared_dep,
  link_args: ['-Wl,-Map=app2.map', '-Wl,--gc-sections'],
)
