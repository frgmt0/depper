// Package unit escapes values for systemd unit files and environment files.
package unit

import (
	"sort"
	"strings"
)

// Arg returns value as one double-quoted systemd command-line argument
// (ExecStart= and friends): backslashes and double quotes are escaped, and "$"
// and "%" are doubled so systemd performs neither variable nor specifier
// expansion.
func Arg(value string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "$", "$$", "%", "%%")
	return `"` + r.Replace(value) + `"`
}

// Path escapes value for systemd path directives (WorkingDirectory=,
// EnvironmentFile=, ...), which are not shell words: quotes would become part
// of the path, so backslash and space are hex-escaped and "%" is doubled.
func Path(value string) string {
	r := strings.NewReplacer(`\`, `\x5c`, " ", `\x20`, "%", "%%")
	return r.Replace(value)
}

// ExecPath is Path for a path that appears inside an ExecStart= line, where
// "$" must additionally be doubled.
func ExecPath(value string) string {
	return strings.ReplaceAll(Path(value), "$", "$$")
}

// EnvironmentFile renders values as a systemd EnvironmentFile= body: one
// KEY="value" line per key, sorted. systemd keeps literal newlines inside
// double quotes and does not C-unescape \n, so only backslash and double quote
// are escaped. Keys must already be validated environment names.
func EnvironmentFile(values map[string]string) string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	escape := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	var b strings.Builder
	for _, key := range keys {
		b.WriteString(key)
		b.WriteString(`="`)
		b.WriteString(escape.Replace(values[key]))
		b.WriteString("\"\n")
	}
	return b.String()
}
