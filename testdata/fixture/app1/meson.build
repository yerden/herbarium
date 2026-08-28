executable(
  'app1',
  ['main.c', 'strong_override.c'],
  dependencies: libshared_dep,
  link_args: ['-Wl,-Map=app1.map', '-Wl,--gc-sections'],
)
