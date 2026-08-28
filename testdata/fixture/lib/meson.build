libshared_inc = include_directories('.', '../include')

libshared = static_library(
  'shared',
  ['shared_utils.c', 'weak_impl.c', 'dispatch_impls.c', 'icf_pair.c'],
  include_directories: libshared_inc,
)

libshared_dep = declare_dependency(
  link_with: libshared,
  include_directories: libshared_inc,
)
