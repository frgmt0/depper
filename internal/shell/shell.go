// Package shell builds POSIX sh scripts for the remote host. Every value
// interpolated into a script must pass through Quote (or be validated as a
// plain token first); scripts are assembled from lines with Script.
package shell

import (
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// Quote returns s as a single POSIX shell word, equivalent to Python's
// shlex.quote: safe strings are returned unchanged, everything else is wrapped
// in single quotes with embedded single quotes spelled '"'"'.
func Quote(s string) string {
	if s == "" {
		return "''"
	}
	if isSafe(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

func isSafe(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case strings.IndexByte("@%+=:,./_-", c) >= 0:
		default:
			return false
		}
	}
	return true
}

// Join quotes each argument and joins them with spaces.
func Join(args ...string) string {
	quoted := make([]string, len(args))
	for i, arg := range args {
		quoted[i] = Quote(arg)
	}
	return strings.Join(quoted, " ")
}

// Script joins lines into a script terminated by a newline.
func Script(lines ...string) string {
	return strings.Join(lines, "\n") + "\n"
}

// Prelude returns the lines every systemd --user script starts with: strict
// mode and the session variables (SessionEnv).
func Prelude() []string {
	return append([]string{"set -eu"}, SessionEnv()...)
}

// SessionEnv returns lines that export XDG_RUNTIME_DIR and
// DBUS_SESSION_BUS_ADDRESS for systemctl --user and rootless podman. A
// non-interactive SSH session may lack them, and the login may carry
// another user's XDG directories: pam_env applies /etc/environment, which
// on some images (the GitHub runner's, for one) names the image user's
// /run/user/UID and ~/.config. podman refuses a runtime directory it does
// not own and cannot read another user's configuration. Unless the
// inherited runtime directory exists and is owned by the current user, both
// variables are set to /run/user/$(id -u). XDG_CONFIG_HOME, XDG_DATA_HOME,
// XDG_CACHE_HOME and XDG_STATE_HOME are kept only when they name a
// directory the current user owns, or a missing one in a directory it owns;
// otherwise they are unset, so the defaults under $HOME apply.
func SessionEnv() []string {
	return []string{
		`if test -z "${XDG_RUNTIME_DIR:-}" || ! test -d "$XDG_RUNTIME_DIR" || ! test -O "$XDG_RUNTIME_DIR"; then XDG_RUNTIME_DIR="/run/user/$(id -u)"; DBUS_SESSION_BUS_ADDRESS="unix:path=$XDG_RUNTIME_DIR/bus"; fi`,
		`export XDG_RUNTIME_DIR`,
		`export DBUS_SESSION_BUS_ADDRESS="${DBUS_SESSION_BUS_ADDRESS:-unix:path=$XDG_RUNTIME_DIR/bus}"`,
		`for depper_xdg in XDG_CONFIG_HOME XDG_DATA_HOME XDG_CACHE_HOME XDG_STATE_HOME; do eval "depper_dir=\${$depper_xdg:-}"; if test -n "$depper_dir" && ! test -O "$depper_dir" && ! { ! test -e "$depper_dir" && test -O "$(dirname -- "$depper_dir")"; }; then unset "$depper_xdg"; fi; done`,
	}
}

// Exports returns sorted `export KEY=value` lines. Keys must already be
// validated environment names; values are quoted.
func Exports(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	lines := make([]string, len(keys))
	for i, key := range keys {
		lines[i] = "export " + key + "=" + Quote(values[key])
	}
	return lines
}

// IsPlainPath reports whether s is an absolute remote path that is safe to use
// unquoted inside systemd directives and rsync destinations: it starts with
// "/" and has no whitespace, NUL, line breaks, backslashes, quotes, or percent
// signs.
func IsPlainPath(s string) bool {
	if !strings.HasPrefix(s, "/") || strings.ContainsAny(s, "\x00\r\n\\%\"'") {
		return false
	}
	return !strings.ContainsFunc(s, unicode.IsSpace)
}

// HasLineBreak reports whether s contains NUL, CR, or LF.
func HasLineBreak(s string) bool {
	return strings.ContainsAny(s, "\x00\r\n")
}

// HasControl reports whether s contains an ASCII control character (< 0x20 or
// DEL).
func HasControl(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] == 0x7f {
			return true
		}
	}
	return false
}

// safePathRe is the alphabet of remote deployment paths: rsync (>= 3.2.4
// protects its arguments and would escape any quoting), systemd path
// directives, and probe output all take these paths unquoted.
var safePathRe = regexp.MustCompile(`^/[A-Za-z0-9._@+/-]+$`)

// IsSafePath reports whether s is an absolute remote path made only of
// letters, digits, and . _ @ + / - characters. Deployment base, release, and
// rsync destination paths must pass it; they are used without quoting.
func IsSafePath(s string) bool {
	return safePathRe.MatchString(s) && strings.Trim(s, "/") != ""
}

// SafePathChars describes IsSafePath's alphabet for error messages.
const SafePathChars = "letters, digits, and . _ @ + / -"

// Wrap makes script safe to stream to `sh -s`: tracing is switched off
// first, then the whole body is one { ... } group, which the shell parses
// completely before running any of it. A connection that drops while the
// script is being sent therefore runs nothing instead of a truncated prefix.
func Wrap(script string) string {
	if !strings.HasSuffix(script, "\n") {
		script += "\n"
	}
	return "set +xv\n{\n" + script + "}\n"
}
