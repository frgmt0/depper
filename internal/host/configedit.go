package host

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"unicode"

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
	// ifPresent writes the value only when the key is already in the
	// table (port 22 replaces an existing port, but is not added).
	ifPresent bool
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

// describe lists the changed keys.
func (e *configEdit) describe() string {
	var parts []string
	for _, c := range e.Changes {
		if c.Changed {
			parts = append(parts, c.String())
		}
	}
	return strings.Join(parts, "; ")
}

var keyLineRe = regexp.MustCompile(`^(\s*)("?)([A-Za-z0-9_-]+)("?)\s*=(.*)$`)

// tomlString renders s as a TOML basic string. Callers pass validated
// values without control characters.
func tomlString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

// parseTableHeader recognises a TOML table header line: "[" or "[[", a dotted
// key of bare keys (A-Za-z0-9_-), basic strings ("..." with escapes) and
// literal strings ('...'), optional whitespace around the dots, "]" or "]]",
// then optional whitespace and an optional # comment. key is the normalized
// path (quotes removed, escapes resolved, parts joined with "."), e.g.
// `[ targets . "prod" ]` gives "targets.prod". array reports "[[". ok is
// false for any other line, including a malformed header. The caller must
// only offer lines that start outside any multi-line value (tomlScanner).
func parseTableHeader(line string) (key string, array, ok bool) {
	s := strings.TrimLeft(line, " \t")
	if !strings.HasPrefix(s, "[") {
		return "", false, false
	}
	s = s[1:]
	if strings.HasPrefix(s, "[") {
		array, s = true, s[1:]
	}
	var parts []string
	for {
		s = strings.TrimLeft(s, " \t")
		var part string
		part, s, ok = parseHeaderKeyPart(s)
		if !ok {
			return "", false, false
		}
		parts = append(parts, part)
		s = strings.TrimLeft(s, " \t")
		if strings.HasPrefix(s, ".") {
			s = s[1:]
			continue
		}
		break
	}
	closer := "]"
	if array {
		closer = "]]"
	}
	if !strings.HasPrefix(s, closer) {
		return "", false, false
	}
	s = strings.TrimLeft(s[len(closer):], " \t")
	if s != "" && s[0] != '#' {
		return "", false, false
	}
	return strings.Join(parts, "."), array, true
}

// parseHeaderKeyPart reads one bare, basic-string or literal-string key from
// the start of s and returns it with the remaining text.
func parseHeaderKeyPart(s string) (part, rest string, ok bool) {
	if s == "" {
		return "", "", false
	}
	switch s[0] {
	case '"':
		var b strings.Builder
		for i := 1; i < len(s); i++ {
			switch c := s[i]; c {
			case '"':
				return b.String(), s[i+1:], true
			case '\\':
				r, n, valid := unescapeTOML(s[i+1:])
				if !valid {
					return "", "", false
				}
				b.WriteRune(r)
				i += n
			default:
				if c < 0x20 && c != '\t' || c == 0x7f {
					return "", "", false
				}
				b.WriteByte(c)
			}
		}
		return "", "", false
	case '\'':
		end := strings.IndexByte(s[1:], '\'')
		if end < 0 {
			return "", "", false
		}
		return s[1 : end+1], s[end+2:], true
	}
	end := 0
	for end < len(s) && (s[end] >= 'a' && s[end] <= 'z' || s[end] >= 'A' && s[end] <= 'Z' || s[end] >= '0' && s[end] <= '9' || s[end] == '_' || s[end] == '-') {
		end++
	}
	if end == 0 {
		return "", "", false
	}
	return s[:end], s[end:], true
}

// unescapeTOML decodes the escape sequence after a backslash in a basic
// string. n is the number of bytes of s it consumed.
func unescapeTOML(s string) (r rune, n int, ok bool) {
	if s == "" {
		return 0, 0, false
	}
	switch s[0] {
	case 'b':
		return '\b', 1, true
	case 't':
		return '\t', 1, true
	case 'n':
		return '\n', 1, true
	case 'f':
		return '\f', 1, true
	case 'r':
		return '\r', 1, true
	case 'e':
		return 0x1b, 1, true
	case '"', '\\':
		return rune(s[0]), 1, true
	case 'u', 'U':
		width := 4
		if s[0] == 'U' {
			width = 8
		}
		if len(s) < 1+width {
			return 0, 0, false
		}
		v, err := strconv.ParseUint(s[1:1+width], 16, 32)
		if err != nil || v > unicode.MaxRune || v >= 0xd800 && v <= 0xdfff {
			return 0, 0, false
		}
		return rune(v), 1 + width, true
	}
	return 0, 0, false
}

// tomlScanner follows the lexical state of a TOML document line by line:
// whether a multi-line string (""" or ”') or a multi-line array or inline
// table is open. Delimiters inside basic and literal strings and comments
// do not count.
type tomlScanner struct {
	// ml is the open multi-line string delimiter ("" when none).
	ml string
	// depth is the number of open [ and { in values.
	depth int
}

// topLevel reports whether the next line starts outside any value, where
// headers and keys are recognised.
func (s *tomlScanner) topLevel() bool { return s.ml == "" && s.depth == 0 }

// scan advances the state over one line (without its newline). header is
// true for a table header line, whose brackets are not value brackets.
func (s *tomlScanner) scan(line string, header bool) {
	i := 0
	for i < len(line) {
		c := line[i]
		switch {
		case s.ml == `"""`:
			switch {
			case c == '\\':
				i += 2
			case strings.HasPrefix(line[i:], `"""`):
				i += 3
				// Up to two more quotes belong to the content.
				for n := 0; n < 2 && i < len(line) && line[i] == '"'; n++ {
					i++
				}
				s.ml = ""
			default:
				i++
			}
		case s.ml == `'''`:
			if strings.HasPrefix(line[i:], `'''`) {
				i += 3
				for n := 0; n < 2 && i < len(line) && line[i] == '\''; n++ {
					i++
				}
				s.ml = ""
			} else {
				i++
			}
		case c == '#':
			return
		case strings.HasPrefix(line[i:], `"""`):
			s.ml, i = `"""`, i+3
		case strings.HasPrefix(line[i:], `'''`):
			s.ml, i = `'''`, i+3
		case c == '"':
			i++
			for i < len(line) && line[i] != '"' {
				if line[i] == '\\' {
					i++
				}
				i++
			}
			i++
		case c == '\'':
			if j := strings.IndexByte(line[i+1:], '\''); j >= 0 {
				i += j + 2
			} else {
				i = len(line)
			}
		case (c == '[' || c == '{') && !header:
			s.depth++
			i++
		case (c == ']' || c == '}') && !header:
			if s.depth > 0 {
				s.depth--
			}
			i++
		default:
			i++
		}
	}
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
	var sc tomlScanner
	for i, raw := range lines {
		line := strings.TrimRight(raw, "\r\n")
		if !sc.topLevel() {
			sc.scan(line, false)
			continue
		}
		if name, array, ok := parseTableHeader(line); ok {
			current = name
			if array {
				// [[x]] tables are never edited.
				current = "\x00array"
			}
			if current == table {
				if headerAt >= 0 {
					return "", nil, fmt.Errorf("[%s] appears more than once", table)
				}
				headerAt = i
			}
			sc.scan(line, true)
			continue
		}
		if current == table {
			if m := keyLineRe.FindStringSubmatch(line); m != nil && m[2] == m[4] {
				if _, dup := found[m[3]]; dup {
					return "", nil, fmt.Errorf("[%s].%s appears more than once", table, m[3])
				}
				found[m[3]] = i
			}
		}
		sc.scan(line, false)
	}
	if sc.ml != "" {
		return "", nil, fmt.Errorf("a multi-line string is not closed")
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
			if v.ifPresent {
				continue
			}
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

// connectionValues are the keys host init writes. identity is the private
// key file to record ("" to leave identity_file alone); forcePort writes the
// port even when it is 22 and absent.
func connectionValues(o *Options, repo, identity string, forcePort bool) []configValue {
	values := []configValue{{key: "host", value: tomlString(o.Host)}, {key: "user", value: tomlString(o.User)}}
	values = append(values, configValue{key: "port", value: fmt.Sprint(o.Port), ifPresent: o.Port == 22 && !forcePort})
	if identity != "" {
		values = append(values, configValue{key: "identity_file", value: tomlString(displayPath(identity, ""))})
	}
	values = append(values, configValue{key: "known_hosts", value: tomlString(displayPath(o.KnownHosts, repo))})
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

// connection is the resolved SSH connection of a configuration.
type connection struct {
	Host, User, IdentityFile, KnownHosts string
	Port                                 int
}

func connectionOf(cfg *config.Config) connection {
	port := cfg.Deploy.Port
	if port == 0 {
		port = 22
	}
	return connection{Host: cfg.Deploy.Host, User: cfg.Deploy.User, Port: port, IdentityFile: cleanPath(cfg.Deploy.IdentityFile), KnownHosts: cleanPath(cfg.Deploy.KnownHosts)}
}

func cleanPath(p string) string {
	if p == "" {
		return ""
	}
	return filepath.Clean(p)
}

// diff lists the fields of c that differ from want.
func (c connection) diff(want connection) []string {
	var out []string
	add := func(field string, got, w any) {
		if !reflect.DeepEqual(got, w) {
			out = append(out, fmt.Sprintf("%s = %q (want %q)", field, fmt.Sprint(got), fmt.Sprint(w)))
		}
	}
	add("host", c.Host, want.Host)
	add("user", c.User, want.User)
	add("port", c.Port, want.Port)
	add("identity_file", c.IdentityFile, want.IdentityFile)
	add("known_hosts", c.KnownHosts, want.KnownHosts)
	return out
}

// tableHasKey reports whether the raw TOML document sets key in table
// ("deploy" or "targets.NAME").
func tableHasKey(doc map[string]any, table, key string) bool {
	t := rawTable(doc, table)
	_, ok := t[key]
	return ok
}

func rawTable(doc map[string]any, table string) map[string]any {
	cur := doc
	for _, part := range strings.Split(table, ".") {
		next, ok := cur[part].(map[string]any)
		if !ok {
			return nil
		}
		cur = next
	}
	return cur
}

// withoutKeys returns a deep copy of doc without keys in table; the table
// (and an emptied "targets") is dropped when nothing else is left in it.
func withoutKeys(doc map[string]any, table string, keys []string) map[string]any {
	out := deepCopy(doc).(map[string]any)
	parts := strings.Split(table, ".")
	parents := []map[string]any{out}
	cur := out
	for _, part := range parts {
		next, ok := cur[part].(map[string]any)
		if !ok {
			return out
		}
		parents = append(parents, next)
		cur = next
	}
	for _, k := range keys {
		delete(cur, k)
	}
	for i := len(parts) - 1; i >= 0; i-- {
		child := parents[i+1]
		if len(child) != 0 {
			break
		}
		delete(parents[i], parts[i])
	}
	return out
}

func deepCopy(v any) any {
	switch x := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(x))
		for k, e := range x {
			m[k] = deepCopy(e)
		}
		return m
	case []any:
		s := make([]any, len(x))
		for i, e := range x {
			s[i] = deepCopy(e)
		}
		return s
	case []map[string]any:
		s := make([]map[string]any, len(x))
		for i, e := range x {
			s[i] = deepCopy(e).(map[string]any)
		}
		return s
	}
	return v
}

// planConfig reads the configuration file of repo and prepares the edit of
// table [deploy] (target "") or [targets.NAME]. It refuses when there is no
// configuration file, and whenever the edited file would not resolve to
// exactly the intended connection or would change anything else: the other
// tables, and the connection of every other target.
//
// deployIdentity is the private key of the authorized public key ("" when
// it is not on disk). It is written as identity_file with --key, or when
// the table (or an inherited [deploy]) already names a different key.
func planConfig(repo, target string, o *Options, deployIdentity string, lookup func(string) (string, bool)) (*configEdit, *config.Config, error) {
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
	base := filepath.Base(file)
	before, err := config.Parse(repo, file, original)
	if err != nil {
		return nil, nil, fmt.Errorf("fix %s before running host init: %w", base, err)
	}
	var rawBefore map[string]any
	if _, err := toml.Decode(original, &rawBefore); err != nil {
		return nil, nil, fmt.Errorf("fix %s before running host init: %w", base, err)
	}
	table := "deploy"
	if target != "" {
		table = "targets." + target
	}

	// The intended connection.
	want := connection{Host: o.Host, User: o.User, Port: o.Port, KnownHosts: cleanPath(o.KnownHosts)}
	identity := ""
	if o.KeyExplicit {
		identity = deployIdentity
	}
	if identity == "" && tableHasKey(rawBefore, table, "identity_file") {
		if deployIdentity == "" {
			return nil, nil, fmt.Errorf("[%s] sets identity_file, but the private key of %s (the key host init authorizes) is not on this machine, so the configured key would not match the authorized one; pass --key with the public key of the private key you deploy with", table, displayPath(o.KeyFile, ""))
		}
		identity = deployIdentity
	}
	want.IdentityFile = cleanPath(identity)

	forcePort := false
	var edited string
	var changes []ConfigChange
	var cfg *config.Config
	for attempt := 0; ; attempt++ {
		edited, changes, err = editConfigText(original, table, connectionValues(o, repo, identity, forcePort))
		if err != nil {
			return nil, nil, fmt.Errorf("cannot edit %s: %w", base, err)
		}
		after, err := config.Parse(repo, file, edited)
		if err != nil {
			return nil, nil, fmt.Errorf("editing %s would make it invalid (%v); set the connection by hand", base, err)
		}
		cfg, err = after.ResolveWithEnv(target, lookup)
		if err != nil {
			return nil, nil, fmt.Errorf("the edited %s does not resolve: %w", base, err)
		}
		got := connectionOf(cfg)
		if attempt == 0 {
			// A value inherited from [deploy] that differs is overridden in
			// the target table.
			retry := false
			if got.Port != want.Port && !forcePort {
				forcePort, retry = true, true
			}
			if got.IdentityFile != want.IdentityFile && identity == "" && deployIdentity != "" {
				identity, want.IdentityFile, retry = deployIdentity, cleanPath(deployIdentity), true
			}
			if retry {
				continue
			}
		}
		if d := got.diff(want); len(d) > 0 {
			hint := "an environment override (DEPPER_*) or a value inherited from [deploy]?"
			if got.IdentityFile != want.IdentityFile && deployIdentity == "" {
				hint = "pass --key with the public key of the private key you deploy with"
			}
			return nil, nil, fmt.Errorf("after the edit, %s would resolve [%s] to %s (%s); %s was not changed: set the connection by hand", base, table, strings.Join(d, ", "), hint, base)
		}
		// Nothing outside the edited keys may change.
		var rawAfter map[string]any
		if _, err := toml.Decode(edited, &rawAfter); err != nil {
			return nil, nil, fmt.Errorf("editing %s would make it invalid (%v); set the connection by hand", base, err)
		}
		var keys []string
		for _, c := range changes {
			keys = append(keys, c.Key)
		}
		if !reflect.DeepEqual(withoutKeys(rawBefore, table, keys), withoutKeys(rawAfter, table, keys)) {
			return nil, nil, fmt.Errorf("editing %s would change more than [%s] %s (an internal editor error); %s was not changed: set the connection by hand", base, table, strings.Join(keys, ", "), base)
		}
		// Every other target (and [deploy] when a target is edited)
		// resolves to the same connection as before.
		others := before.TargetNames()
		if target != "" {
			others = append([]string{""}, others...)
		}
		for _, name := range others {
			if name == target || (target == "" && name != "") {
				continue
			}
			b, err := before.ResolveWithEnv(name, lookup)
			if err != nil {
				continue
			}
			a, err := after.ResolveWithEnv(name, lookup)
			if err != nil {
				return nil, nil, fmt.Errorf("editing %s would break %s (%v); set the connection by hand", base, targetLabel(name), err)
			}
			if d := connectionOf(a).diff(connectionOf(b)); len(d) > 0 {
				return nil, nil, fmt.Errorf("editing %s would change the connection of %s (%s); set the connection by hand", base, targetLabel(name), strings.Join(d, ", "))
			}
		}
		// Targets that inherit from an edited [deploy] change with it; they
		// must still resolve.
		if target == "" {
			for _, name := range before.TargetNames() {
				if _, err := before.ResolveWithEnv(name, lookup); err != nil {
					continue
				}
				if _, err := after.ResolveWithEnv(name, lookup); err != nil {
					return nil, nil, fmt.Errorf("editing %s would break %s (%v); set the connection by hand", base, targetLabel(name), err)
				}
			}
		}
		break
	}
	return &configEdit{File: file, Mode: info.Mode().Perm(), Original: original, Edited: edited, Table: table, Changes: changes}, cfg, nil
}

func targetLabel(name string) string {
	if name == "" {
		return "[deploy]"
	}
	return "target " + fmt.Sprintf("%q", name)
}

// ErrConfigChanged reports that depper.toml changed while host init ran.
var ErrConfigChanged = fmt.Errorf("depper.toml changed while host init was running")

// apply writes the edit atomically, after checking that the file still holds
// what was read, re-validates it with config.LoadTarget, and restores the
// original when validation fails.
func (e *configEdit) apply(repo, target string, lookup func(string) (string, bool)) error {
	if !e.Changed() {
		return nil
	}
	current, err := os.ReadFile(e.File)
	if err != nil {
		return fmt.Errorf("cannot re-read %s before writing it: %w", e.File, err)
	}
	if !bytes.Equal(current, []byte(e.Original)) {
		return fmt.Errorf("%s: %w; it was not written. Run host init again (the host is already prepared, so only the configuration is written)", e.File, ErrConfigChanged)
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
