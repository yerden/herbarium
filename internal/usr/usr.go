// Package usr synthesizes deterministic identity strings for C entities
// per the scheme in herbarium-plan.md's appendix. The rules are the
// contract; the code is not.
//
// Every project-scoped call must supply a path RELATIVE to project-root
// and normalized to forward slashes with no leading "./". Callers are
// responsible for that normalization — this package will not silently
// fix a caller who passes an absolute path.
package usr

import (
	"regexp"
	"strings"
)

// Function returns the USR for a function symbol. Path is empty for
// external (non-static) functions and the containing file's project-
// relative path for static or static-inline functions.
func Function(path, name string) string {
	if path == "" {
		return "c:@F@" + name
	}
	return "c:" + path + "@F@" + name
}

// Variable returns the USR for a variable. Same path rules as Function.
func Variable(path, name string) string {
	if path == "" {
		return "c:@V@" + name
	}
	return "c:" + path + "@V@" + name
}

// Typedef returns the USR for a typedef. Typedefs have no linkage so
// their identity is always file-scoped (path required).
func Typedef(path, name string) string { return "c:" + path + "@T@" + name }

// Struct returns the USR for a struct tag. Anonymous structs (name == "")
// get the __anon_<line>_<col> form per the appendix.
func Struct(path, name string, line, col int) string {
	return "c:" + path + "@S@" + resolveTagName(name, line, col)
}

// Union returns the USR for a union tag.
func Union(path, name string, line, col int) string {
	return "c:" + path + "@U@" + resolveTagName(name, line, col)
}

// Enum returns the USR for an enum tag.
func Enum(path, name string, line, col int) string {
	return "c:" + path + "@E@" + resolveTagName(name, line, col)
}

// EnumMember returns the USR for one enumerator inside an enum. enumName
// may be anonymous.
func EnumMember(path, enumName string, line, col int, member string) string {
	return "c:" + path + "@E@" + resolveTagName(enumName, line, col) + "@" + member
}

// Field returns the USR for a struct or union field. The container's USR
// (as returned by Struct/Union) is passed in verbatim so this function
// doesn't need to know whether the container is anonymous or the tag
// kind — it just appends the field discriminator.
func Field(containerUSR, fieldName string) string {
	return containerUSR + "@F@" + fieldName
}

func resolveTagName(name string, line, col int) string {
	if name != "" {
		return name
	}
	// Appendix: `__anon_<line>_<column>` for anonymous tags.
	return "__anon_" + itoa(line) + "_" + itoa(col)
}

// itoa avoids pulling in strconv for the two integers we format here.
// Reads better in tests and keeps the package leaf-level.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [12]byte
	i := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// cloneSuffix matches the tail GCC's IPA passes append when producing a
// specialized variant. Examples: `.constprop`, `.constprop.0`, `.isra.1`,
// `.part.2`. The base group is the parent's name.
var cloneSuffix = regexp.MustCompile(`^(.*?)(\.(constprop|isra|part|localalias|cold|hot|resolver)(\.\d+)?)$`)

// Clone reports whether name has a GCC clone suffix. If so, base is the
// parent function's name (before the suffix), suffix is the raw suffix
// starting with '.', and ok is true. Callers use this to route a clone
// entry to the parent's USR while tracking the linkage name separately
// per the appendix's GCC-generated clones rule.
func Clone(name string) (base string, suffix string, ok bool) {
	m := cloneSuffix.FindStringSubmatch(name)
	if m == nil {
		return name, "", false
	}
	return m[1], m[2], true
}

// NormalizeProjectPath makes p safe to embed in a USR: forward slashes,
// no leading "./", no trailing slash, but preserves the exact bytes
// otherwise (no case folding, no case-insensitive dedup — per appendix).
// Callers should have already made p relative to project-root; this
// function does not know project-root and will not compute a relative
// path. An absolute path in returns the same absolute path — callers
// should have caught that case upstream.
func NormalizeProjectPath(p string) string {
	p = strings.ReplaceAll(p, `\`, "/")
	p = strings.TrimPrefix(p, "./")
	p = strings.TrimSuffix(p, "/")
	return p
}
