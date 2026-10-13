// Package dotenv is a deliberately small, non-shell dotenv reader for
// deployment secrets. It never expands variables or runs commands, and its
// errors name a line number but never echo source content.
package dotenv

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// Limit is the maximum dotenv source size in bytes (1 MiB).
const Limit = 1024 * 1024

// Error is a content-safe dotenv error.
type Error struct{ msg string }

func (e *Error) Error() string { return e.msg }

func errorf(format string, args ...any) error { return &Error{msg: fmt.Sprintf(format, args...)} }

// Source is a validated dotenv file selection.
type Source struct {
	// Selected is the lexical absolute spelling of the selected path.
	Selected string
	// Resolved is the symlink-resolved file that is actually read.
	Resolved string
	// RsyncExcludes are anchored, escaped rsync patterns for every in-repo
	// spelling of the file, so a sync never uploads it.
	RsyncExcludes []string
}

// ExpandUser expands a leading "~" or "~user" like Python's
// os.path.expanduser. Paths without a leading tilde, or whose user cannot be
// found, are returned unchanged.
func ExpandUser(path string) string {
	if !strings.HasPrefix(path, "~") {
		return path
	}
	rest := path[1:]
	name, tail := rest, ""
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		name, tail = rest[:i], rest[i:]
	}
	var home string
	if name == "" {
		if h, err := os.UserHomeDir(); err == nil {
			home = h
		} else if u, err := user.Current(); err == nil {
			home = u.HomeDir
		}
	} else if u, err := user.Lookup(name); err == nil {
		home = u.HomeDir
	}
	if home == "" {
		return path
	}
	if home != "/" {
		home = strings.TrimRight(home, "/")
	}
	if tail == "" {
		return home
	}
	if home == "/" {
		return tail
	}
	return home + tail
}

func traversalPath(selected string) (expanded, traversal string, err error) {
	expanded = ExpandUser(selected)
	traversal = expanded
	if !filepath.IsAbs(expanded) {
		cwd, err := os.Getwd()
		if err != nil {
			return "", "", errorf("Selected dotenv file is not readable")
		}
		// Joined without cleaning: "alias/../x" must be resolved by following
		// alias first, exactly as the kernel would.
		traversal = cwd + string(filepath.Separator) + expanded
	}
	return expanded, traversal, nil
}

// resolveStrict resolves every symlink in path, applying ".." to the
// already-resolved prefix (physical semantics, like Path.resolve(strict=True)).
func resolveStrict(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	return filepath.Abs(resolved)
}

func resolveRepo(repo string) string {
	abs, err := filepath.Abs(ExpandUser(repo))
	if err != nil {
		return repo
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return abs
}

var rsyncEscaper = strings.NewReplacer(`\`, `\\`, "[", "[[]", "]", "[]]", "*", "[*]", "?", "[?]")

// RsyncExcludes returns anchored rsync exclude patterns for the lexical and
// resolved spellings of selected that lie inside repo. The file does not need
// to exist. An empty selected returns nil.
func RsyncExcludes(repo, selected string) []string {
	if selected == "" {
		return nil
	}
	expanded, traversal, err := traversalPath(selected)
	if err != nil {
		return nil
	}
	lexical, err := filepath.Abs(expanded)
	if err != nil {
		return nil
	}
	candidates := []string{lexical}
	if resolved, err := resolveStrict(traversal); err == nil {
		candidates = append(candidates, resolved)
	}
	root := resolveRepo(repo)
	var excludes []string
	seen := map[string]bool{}
	for _, candidate := range candidates {
		rel, err := filepath.Rel(root, candidate)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		// Anchor and escape rsync's pattern metacharacters so a secrets
		// filename never broadens a sync.
		pattern := "/" + rsyncEscaper.Replace(filepath.ToSlash(rel))
		if !seen[pattern] {
			seen[pattern] = true
			excludes = append(excludes, pattern)
		}
	}
	return excludes
}

// ResolveSource validates a selected dotenv file without reading its
// contents. Relative paths are relative to the current directory. An empty
// selected returns (nil, nil).
func ResolveSource(repo, selected string) (*Source, error) {
	if selected == "" {
		return nil, nil
	}
	expanded, traversal, err := traversalPath(selected)
	if err != nil {
		return nil, err
	}
	lexical, err := filepath.Abs(expanded)
	if err != nil {
		return nil, errorf("Selected dotenv file is not readable")
	}
	resolved, err := resolveStrict(traversal)
	if err != nil {
		return nil, errorf("Selected dotenv file is not readable")
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return nil, errorf("Selected dotenv file is not readable")
	}
	if !info.Mode().IsRegular() {
		return nil, errorf("Selected dotenv source must be a regular file")
	}
	if info.Size() > Limit {
		return nil, errorf("Selected dotenv source is larger than the 1 MiB limit")
	}
	return &Source{Selected: lexical, Resolved: resolved, RsyncExcludes: RsyncExcludes(repo, selected)}, nil
}

// openFile is replaced in tests to simulate unreadable files.
var openFile = func(name string) (io.ReadCloser, error) { return os.Open(name) }

// Read reads and parses the resolved source file.
func Read(source *Source) (map[string]string, error) {
	f, err := openFile(source.Resolved)
	if err != nil {
		return nil, errorf("Selected dotenv file is not readable")
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, Limit+1))
	if err != nil {
		return nil, errorf("Selected dotenv file is not readable")
	}
	return decode(raw, "Selected dotenv source", "Selected dotenv file")
}

// ParseReader reads dotenv content from r (for example stdin for
// --env-stdin) with the same size, encoding, and syntax rules as a file.
func ParseReader(r io.Reader) (map[string]string, error) {
	raw, err := io.ReadAll(io.LimitReader(r, Limit+1))
	if err != nil {
		return nil, errorf("dotenv input could not be read")
	}
	return decode(raw, "dotenv input", "dotenv input")
}

func decode(raw []byte, sizeLabel, encodingLabel string) (map[string]string, error) {
	if len(raw) > Limit {
		return nil, errorf("%s is larger than the 1 MiB limit", sizeLabel)
	}
	raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))
	if !utf8.Valid(raw) {
		return nil, errorf("%s must be UTF-8", encodingLabel)
	}
	text := string(raw)
	if nul := strings.IndexByte(text, 0); nul >= 0 {
		return nil, errorf("Invalid dotenv syntax at line %d", strings.Count(text[:nul], "\n")+1)
	}
	return Parse(text)
}

func isKeyStart(c byte) bool {
	return c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
}

func isKeyChar(c byte) bool { return isKeyStart(c) || (c >= '0' && c <= '9') }

// Parse parses assignments, comments, `export`, and quoted values without
// any expansion. Later assignments of the same key win.
func Parse(text string) (map[string]string, error) {
	values := map[string]string{}
	pos, line, length := 0, 1, len(text)
	in := func(p int, set string) bool { return p < length && strings.IndexByte(set, text[p]) >= 0 }
	syntax := func(at int) error { return errorf("Invalid dotenv syntax at line %d", at) }
	for pos < length {
		for in(pos, " \t\r") {
			pos++
		}
		if pos >= length {
			break
		}
		if text[pos] == '\n' {
			pos++
			line++
			continue
		}
		if text[pos] == '#' {
			end := strings.IndexByte(text[pos:], '\n')
			if end < 0 {
				break
			}
			pos += end + 1
			line++
			continue
		}
		startLine := line
		exportEnd := pos + 6
		if strings.HasPrefix(text[pos:], "export") && exportEnd < length && in(exportEnd, " \t") {
			pos = exportEnd
			for in(pos, " \t") {
				pos++
			}
		}
		if pos >= length || !isKeyStart(text[pos]) {
			return nil, syntax(startLine)
		}
		keyStart := pos
		for pos < length && isKeyChar(text[pos]) {
			pos++
		}
		key := text[keyStart:pos]
		for in(pos, " \t") {
			pos++
		}
		if pos >= length || text[pos] != '=' {
			return nil, syntax(startLine)
		}
		pos++
		for in(pos, " \t") {
			pos++
		}
		value := ""
		if in(pos, `'"`) {
			quote := text[pos]
			pos++
			var b strings.Builder
			for pos < length && text[pos] != quote {
				c := text[pos]
				if quote == '"' && c == '\\' {
					if pos+1 >= length {
						return nil, syntax(startLine)
					}
					switch next := text[pos+1]; next {
					case 'n':
						b.WriteByte('\n')
					case 'r':
						b.WriteByte('\r')
					case 't':
						b.WriteByte('\t')
					case '\\':
						b.WriteByte('\\')
					case '"':
						b.WriteByte('"')
					default:
						// Unknown escapes are kept verbatim; the following
						// character is consumed as-is (it may be a multi-byte
						// rune, whose continuation bytes are copied next).
						b.WriteByte('\\')
						b.WriteByte(next)
						if next == '\n' {
							line++
						}
					}
					pos += 2
					continue
				}
				b.WriteByte(c)
				if c == '\n' {
					line++
				}
				pos++
			}
			if pos >= length {
				return nil, errorf("Unterminated quoted dotenv value at line %d", startLine)
			}
			pos++
			value = b.String()
			for in(pos, " \t\r") {
				pos++
			}
			switch {
			case pos < length && text[pos] == '#':
				end := strings.IndexByte(text[pos:], '\n')
				if end < 0 {
					pos = length
				} else {
					pos += end + 1
					line++
				}
			case pos < length && text[pos] == '\n':
				pos++
				line++
			case pos < length:
				return nil, syntax(startLine)
			}
		} else if pos < length && text[pos] == '#' {
			end := strings.IndexByte(text[pos:], '\n')
			if end < 0 {
				pos = length
			} else {
				pos += end + 1
				line++
			}
		} else {
			end := strings.IndexByte(text[pos:], '\n')
			if end < 0 {
				end = length
			} else {
				end += pos
			}
			raw := strings.TrimRight(text[pos:end], " \t\r")
			value = raw
			if i := strings.Index(raw, " #"); i >= 0 {
				value = raw[:i]
			}
			if i := strings.Index(raw, "\t#"); i >= 0 && i < len(value) {
				value = raw[:i]
			}
			value = strings.TrimRight(value, " \t")
			pos = end
			if pos < length {
				pos++
				line++
			}
		}
		values[key] = value
	}
	return values, nil
}

// Environment is the merged deployment environment: [env] overlaid by the
// dotenv source. Values are secret and must never be printed.
type Environment struct {
	Values map[string]string
	// Source is the dotenv file that was read; nil when none was selected or
	// the content came from stdin.
	Source *Source
	// FromStdin reports that dotenv content was read from stdin.
	FromStdin bool
	// FileKeys is the number of keys read from the dotenv source.
	FileKeys int
	// RsyncExcludes covers the configured secrets file and the selected
	// source, whichever lie inside the repository.
	RsyncExcludes []string
}

// ErrOverrideAndStdin is returned when both a file override and stdin are
// selected.
var ErrOverrideAndStdin = errors.New("--env and --env-stdin cannot be combined")

// LoadEnvironment resolves and reads the dotenv source for a deployment.
// configured is the absolute [secrets].file path ("" if unset); override is
// a --env path relative to the current directory ("" if unset); stdin, when
// non-nil, supplies --env-stdin content and takes the place of any file.
// The configured file is always excluded from rsync, even when overridden.
func LoadEnvironment(repo string, base map[string]string, configured, override string, stdin io.Reader) (*Environment, error) {
	if stdin != nil && override != "" {
		return nil, ErrOverrideAndStdin
	}
	env := &Environment{Values: map[string]string{}}
	var fileValues map[string]string
	switch {
	case stdin != nil:
		values, err := ParseReader(stdin)
		if err != nil {
			return nil, err
		}
		fileValues, env.FromStdin = values, true
	default:
		selected := configured
		if override != "" {
			selected = override
		}
		source, err := ResolveSource(repo, selected)
		if err != nil {
			return nil, err
		}
		if source != nil {
			if fileValues, err = Read(source); err != nil {
				return nil, err
			}
			env.Source = source
		}
	}
	seen := map[string]bool{}
	add := func(patterns []string) {
		for _, p := range patterns {
			if !seen[p] {
				seen[p] = true
				env.RsyncExcludes = append(env.RsyncExcludes, p)
			}
		}
	}
	add(RsyncExcludes(repo, configured))
	if env.Source != nil {
		add(env.Source.RsyncExcludes)
	}
	for k, v := range base {
		env.Values[k] = v
	}
	for k, v := range fileValues {
		env.Values[k] = v
	}
	env.FileKeys = len(fileValues)
	return env, nil
}
