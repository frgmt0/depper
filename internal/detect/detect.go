// Package detect inspects a repository and suggests depper.toml values for
// the kind of project it finds. Detection is file-based only: it reads a few
// well-known files and never executes anything in the repository.
package detect

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Kind is the detected project type.
type Kind string

// Project kinds, in detection precedence order (Generic is the fallback).
const (
	Compose    Kind = "compose"
	Dockerfile Kind = "dockerfile"
	Go         Kind = "go"
	Node       Kind = "node"
	Python     Kind = "python"
	Rust       Kind = "rust"
	Static     Kind = "static"
	Generic    Kind = "generic"
)

// Container modes suggested for container projects.
const (
	ModeBuild   = "build"
	ModeCompose = "compose"
)

// Value is a suggested value, what it was inferred from, and an optional
// note (shown as a comment above the line it belongs to).
type Value struct {
	Value string `json:"value"`
	From  string `json:"from"`
	Note  string `json:"note,omitempty"`
}

// Container is a suggested [container] block.
type Container struct {
	// Mode is ModeBuild or ModeCompose.
	Mode string `json:"mode"`
	// Path is the build context (".") or the compose file name.
	Path string `json:"path"`
	From string `json:"from"`
}

// Result is what Detect found and suggests.
type Result struct {
	Kind     Kind     `json:"kind"`
	Evidence []string `json:"evidence"`
	// Name is a service name valid as a systemd unit part and an image name.
	Name string `json:"name"`
	// Command is the [service].command; empty for container projects and
	// for the generic fallback's placeholder only when Kind is not Generic.
	Command   Value      `json:"command"`
	Container *Container `json:"container,omitempty"`
	Setup     []Value    `json:"setup,omitempty"`
	// Port is 0 when nothing is known (the generic fallback).
	Port     int     `json:"port,omitempty"`
	PortFrom string  `json:"port_from,omitempty"`
	Health   string  `json:"health_url,omitempty"`
	Excludes []Value `json:"excludes,omitempty"`
	// Secrets is a dotenv file that exists in the repository (never read).
	Secrets  *Value   `json:"secrets,omitempty"`
	Requires []string `json:"requires,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}

// Detect inspects repo and returns suggestions for it.
func Detect(repo string) (Result, error) {
	abs, err := filepath.Abs(repo)
	if err != nil {
		return Result{}, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return Result{}, fmt.Errorf("repository directory %s: %w", abs, errors.Unwrap(err))
	}
	if !info.IsDir() {
		return Result{}, fmt.Errorf("%s is not a directory", abs)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		resolved = abs
	}
	r := &repoFS{root: abs}
	res := Result{Kind: Generic, Name: ServiceName(resolved)}

	switch {
	case detectCompose(r, &res):
	case detectDockerfile(r, &res):
	case detectGo(r, &res):
	case detectNode(r, &res):
	case detectPython(r, &res):
	case detectRust(r, &res):
	case detectStatic(r, &res):
	default:
		res.Evidence = nil
		res.Command = Value{Value: "python -m your_app", From: "placeholder", Note: "Placeholder: replace with the command that starts your app."}
	}
	if res.Port > 0 {
		res.Health = fmt.Sprintf("http://127.0.0.1:%d/", res.Port)
	}
	detectSecrets(r, &res)
	return res, nil
}

var nonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

// ServiceName derives a service name from a directory path: lowercase letters
// and digits separated by single dashes, at most 63 characters. Such a name
// satisfies both the systemd name rule and the image-name rule.
func ServiceName(dir string) string {
	name := nonAlnum.ReplaceAllString(strings.ToLower(filepath.Base(dir)), "-")
	name = strings.Trim(name, "-")
	if len(name) > 63 {
		name = strings.Trim(name[:63], "-")
	}
	if name == "" {
		name = "app"
	}
	return name
}

// repoFS reads files under the repository root without following symlinks
// for the final path element.
type repoFS struct{ root string }

const maxRead = 1 << 20

func (r *repoFS) path(rel string) string { return filepath.Join(r.root, filepath.FromSlash(rel)) }

func (r *repoFS) isFile(rel string) bool {
	info, err := os.Lstat(r.path(rel))
	return err == nil && info.Mode().IsRegular()
}

func (r *repoFS) isDir(rel string) bool {
	info, err := os.Lstat(r.path(rel))
	return err == nil && info.IsDir()
}

// read returns the file's content (capped at 1 MiB), or ok=false when it is
// not a readable regular file.
func (r *repoFS) read(rel string) (string, bool) {
	if !r.isFile(rel) {
		return "", false
	}
	f, err := os.Open(r.path(rel))
	if err != nil {
		return "", false
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxRead))
	if err != nil {
		return "", false
	}
	return string(b), true
}

// subdirs lists immediate subdirectory names, sorted.
func (r *repoFS) subdirs(rel string) []string {
	entries, err := os.ReadDir(r.path(rel))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	return out
}

// firstFile returns the first of names that is a regular file.
func (r *repoFS) firstFile(names ...string) (string, bool) {
	for _, n := range names {
		if r.isFile(n) {
			return n, true
		}
	}
	return "", false
}

// safeToken matches values that may be placed in a generated shell command
// (paths, file and binary names): no spaces, quotes, or shell metacharacters,
// and no leading dash.
var safeToken = regexp.MustCompile(`^[A-Za-z0-9_@+][A-Za-z0-9_.@+/-]*$`)

func safe(s string) bool {
	if !safeToken.MatchString(s) {
		return false
	}
	for _, part := range strings.Split(s, "/") {
		if part == ".." {
			return false
		}
	}
	return true
}

func (res *Result) require(items ...string) { res.Requires = append(res.Requires, items...) }

func (res *Result) warn(format string, args ...any) {
	res.Warnings = append(res.Warnings, fmt.Sprintf(format, args...))
}

func (res *Result) setPort(port int, from string) {
	res.Port, res.PortFrom = port, from
}

func detectSecrets(r *repoFS, res *Result) {
	for _, name := range []string{".env.prod", ".env.production"} {
		if r.isFile(name) {
			res.Secrets = &Value{Value: name, From: "found " + name + " in the repository"}
			res.Evidence = append(res.Evidence, name+" (contents not read)")
			return
		}
	}
}
