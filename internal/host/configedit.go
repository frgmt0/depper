package host

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/frgmt0/depper/internal/config"
)

// ConfigChange is one key host init sets in depper.toml.
type ConfigChange struct {
	Table string `json:"table"`
	Key   string `json:"key"`
	// Old is the current TOML value text ("" when the key is absent).
	Old string `json:"old,omitempty"`
	New string `json:"new"`
	// Changed is false when the file already holds the value.
	Changed bool `json:"changed"`
}

func (c ConfigChange) String() string {
	if c.Old == "" {
		return fmt.Sprintf("[%s] %s = %s (new)", c.Table, c.Key, c.New)
	}
	return fmt.Sprintf("[%s] %s: %s -> %s", c.Table, c.Key, c.Old, c.New)
}

// configValue is one key to write, with its rendered TOML value.
type configValue struct {
	key   string
	value string
}

// configEdit is a planned minimal text edit of the configuration file.
type configEdit struct {
	File     string
	Mode     os.FileMode
	Original string
	Edited   string
	Table    string
	Changes  []ConfigChange
}

// Changed reports whether the edit changes the file.
func (e *configEdit) Changed() bool { return e.Original != e.Edited }

var (
	headerRe   = regexp.MustCompile(`^\s*\[\s*([^\[\]]+?)\s*\]\s*(#.*)?$`)
	arrayHdrRe = regexp.MustCompile(`^\s*\[\[`)
	keyLineRe  = regexp.MustCompile(`^(\s*)("?)([A-Za-z0-9_-]+)("?)\s*=(.*)$`)
)

// tomlString renders s as a TOML basic string. Callers pass validated
// values without control characters.
func tomlString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// normalizeTable turns a header body like ` targets . "prod" ` into
// "targets.prod".
func normalizeTable(h string) string {
	parts := strings.Split(h, ".")
	for i, p := range parts {
		p = strings.TrimSpace(p)
		if len(p) >= 2 && (p[0] == '"' || p[0] == '\'') && p[len(p)-1] == p[0] {
			p = p[1 : len(p)-1]
		}
		parts[i] = p
	}
	return strings.Join(parts, ".")
}

// splitValue separates a single-line TOML value from a trailing comment.
// ok is false for values this editor does not rewrite (multi-line strings,
// arrays, inline tables).
func splitValue(rest string) (value, comment string, ok bool) {
	s := strings.TrimLeft(rest, " \t")
	if strings.HasPrefix(s, `"""`) || strings.HasPrefix(s, `'''`) || strings.HasPrefix(s, "[") || strings.HasPrefix(s, "{") {
		return "", "", false
	}
	end := len(s)
	switch {
	case strings.HasPrefix(s, `"`):
		i := 1
		for i < len(s) && s[i] != '"' {
			if s[i] == '\\' {
				i++
			}
			i++
		}
		if i >= len(s) {
			return "", "", false
		}
		end = i + 1
	case strings.HasPrefix(s, "'"):
		i := strings.IndexByte(s[1:], '\'')
		if i < 0 {
			return "", "", false
		}
		end = i + 2
	default:
		if i := strings.IndexByte(s, '#'); i >= 0 {
			end = i
		}
	}
	value = strings.TrimSpace(s[:end])
	tail := strings.TrimSpace(s[end:])
	if tail != "" && !strings.HasPrefix(tail, "#") {
		return "", "", false
	}
	return value, tail, true
}

// sameTOMLValue compares two TOML value texts semantically.
func sameTOMLValue(a, b string) bool {
	var x, y map[string]any
	if _, err := toml.Decode("v = "+a, &x); err != nil {
		return false
	}
	if _, err := toml.Decode("v = "+b, &y); err != nil {
		return false
	}
	return reflect.DeepEqual(x["v"], y["v"])
}

// editConfigText sets values in table ("deploy" or "targets.NAME") with
// minimal line edits: a present, uncommented key has its value replaced
// (keeping indentation and a trailing comment); absent keys are inserted
// right after the table header. A missing [targets.NAME] table is appended
// at the end. Comments and everything else are left untouched.
func editConfigText(text, table string, values []configValue) (string, []ConfigChange, error) {
	lines := strings.SplitAfter(text, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	current := ""
	headerAt := -1
	found := map[string]int{}
	inMulti := ""
	for i, raw := range lines {
		line := strings.TrimRight(raw, "\r\n")
		if inMulti != "" {
			if strings.Count(line, inMulti)%2 == 1 {
				inMulti = ""
			}
			continue
		}
		if arrayHdrRe.MatchString(line) {
			current = "\x00array"
			continue
		}
		if m := headerRe.FindStringSubmatch(line); m != nil && !strings.HasPrefix(strings.TrimSpace(line), "#") {
			current = normalizeTable(m[1])
			if current == table {
				if headerAt >= 0 {
					return "", nil, fmt.Errorf("[%s] appears more than once", table)
				}
				headerAt = i
			}
			continue
		}
		for _, delim := range []string{`"""`, `'''`} {
			if strings.Count(line, delim)%2 == 1 {
				inMulti = delim
			}
		}
		if current != table {
			continue
		}
		if m := keyLineRe.FindStringSubmatch(line); m != nil && m[2] == m[4] {
			found[m[3]] = i
		}
	}
	if headerAt < 0 && table == "deploy" {
		return "", nil, fmt.Errorf("could not find a [deploy] table header to edit; add host, user and known_hosts to [deploy] by hand")
	}

	var changes []ConfigChange
	var inserts []string
	for _, v := range values {
		ch := ConfigChange{Table: table, Key: v.key, New: v.value}
		if i, ok := found[v.key]; ok {
			line := strings.TrimRight(lines[i], "\r\n")
			eol := lines[i][len(line):]
			m := keyLineRe.FindStringSubmatch(line)
			old, comment, ok := splitValue(m[5])
			if !ok {
				return "", nil, fmt.Errorf("[%s].%s has a value this editor cannot rewrite safely; set it to %s by hand", table, v.key, v.value)
			}
			ch.Old = old
			if !sameTOMLValue(old, v.value) {
				ch.Changed = true
				newLine := m[1] + m[2] + m[3] + m[4] + " = " + v.value
				if comment != "" {
					newLine += " " + comment
				}
				lines[i] = newLine + eol
			}
		} else {
			ch.Changed = true
			inserts = append(inserts, v.key+" = "+v.value+"\n")
		}
		changes = append(changes, ch)
	}
	if len(inserts) > 0 {
		if headerAt >= 0 {
			if !strings.HasSuffix(lines[headerAt], "\n") {
				lines[headerAt] += "\n"
			}
			rest := append([]string{}, lines[headerAt+1:]...)
			lines = append(append(lines[:headerAt+1], inserts...), rest...)
		} else {
			if n := len(lines); n > 0 && !strings.HasSuffix(lines[n-1], "\n") {
				lines[n-1] += "\n"
			}
			lines = append(lines, "\n", "["+table+"]\n")
			lines = append(lines, inserts...)
		}
	}
	return strings.Join(lines, ""), changes, nil
}

// connectionValues are the keys host init writes.
func connectionValues(o *Options, repo, identity string) []configValue {
	values := []configValue{{"host", tomlString(o.Host)}, {"user", tomlString(o.User)}}
	if o.Port != 22 {
		values = append(values, configValue{"port", fmt.Sprint(o.Port)})
	}
	if identity != "" {
		values = append(values, configValue{"identity_file", tomlString(displayPath(identity, ""))})
	}
	values = append(values, configValue{"known_hosts", tomlString(displayPath(o.KnownHosts, repo))})
	return values
}

// displayPath renders a local path for depper.toml: repo-relative when it is
// inside repo, "~/..." under the home directory, else absolute.
func displayPath(path, repo string) string {
	if repo != "" {
		if rel, err := filepath.Rel(repo, path); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) {
			return filepath.ToSlash(rel)
		}
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if rel, err := filepath.Rel(home, path); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel) && rel != "." {
			return "~/" + filepath.ToSlash(rel)
		}
	}
	return path
}

// planConfig reads the configuration file of repo and prepares the edit.
// It refuses when there is no configuration file.
func planConfig(repo, target string, o *Options, identity string, lookup func(string) (string, bool)) (*configEdit, *config.Config, error) {
	file, err := config.FindFile(repo)
	if err != nil {
		return nil, nil, fmt.Errorf("%v; run `depper init` first, then `depper host init` again", err)
	}
	info, err := os.Stat(file)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot read %s: %w", file, err)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot read %s: %w", file, err)
	}
	original := string(data)
	before, err := config.Parse(repo, file, original)
	if err != nil {
		return nil, nil, fmt.Errorf("fix %s before running host init: %w", filepath.Base(file), err)
	}
	table := "deploy"
	if target != "" {
		table = "targets." + target
	}
	edited, changes, err := editConfigText(original, table, connectionValues(o, repo, identity))
	if err != nil {
		return nil, nil, fmt.Errorf("cannot edit %s: %w", filepath.Base(file), err)
	}
	after, err := config.Parse(repo, file, edited)
	if err != nil {
		return nil, nil, fmt.Errorf("editing %s would make it invalid (%v); set the connection by hand", filepath.Base(file), err)
	}
	cfg, err := after.ResolveWithEnv(target, lookup)
	if err != nil {
		return nil, nil, fmt.Errorf("the edited %s does not resolve: %w", filepath.Base(file), err)
	}
	// Other targets that resolved before must still resolve.
	for _, name := range before.TargetNames() {
		if name == target {
			continue
		}
		if _, err := before.ResolveWithEnv(name, lookup); err != nil {
			continue
		}
		if _, err := after.ResolveWithEnv(name, lookup); err != nil {
			return nil, nil, fmt.Errorf("editing %s would break target %q (%v); set the connection by hand", filepath.Base(file), name, err)
		}
	}
	return &configEdit{File: file, Mode: info.Mode().Perm(), Original: original, Edited: edited, Table: table, Changes: changes}, cfg, nil
}

// apply writes the edit atomically, re-validates the file with config.Load
// and Resolve, and restores the original when validation fails.
func (e *configEdit) apply(repo, target string, lookup func(string) (string, bool)) error {
	if !e.Changed() {
		return nil
	}
	if err := atomicWrite(e.File, []byte(e.Edited), e.Mode); err != nil {
		return err
	}
	if _, err := config.LoadTarget(repo, target, lookup); err != nil {
		if rerr := atomicWrite(e.File, []byte(e.Original), e.Mode); rerr != nil {
			return fmt.Errorf("the written %s does not validate (%v) and restoring it failed: %v", e.File, err, rerr)
		}
		return fmt.Errorf("the written %s did not validate (%v); restored the original", e.File, err)
	}
	return nil
}
