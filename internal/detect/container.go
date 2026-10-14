package detect

import (
	"bufio"
	"strconv"
	"strings"
)

var composeNames = []string{"compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml"}

func detectCompose(r *repoFS, res *Result) bool {
	file, ok := r.firstFile(composeNames...)
	if !ok {
		return false
	}
	res.Kind = Compose
	res.Evidence = append(res.Evidence, file)
	res.Container = &Container{Mode: ModeCompose, Path: file, From: "from " + file}
	res.require("podman compose (or docker compose)")
	if r.isFile("Dockerfile") {
		res.Evidence = append(res.Evidence, "Dockerfile")
	}
	text, _ := r.read(file)
	if port, ok := composeFirstPort(text); ok {
		res.setPort(port, "first published port of the first service in "+file)
	} else {
		res.setPort(8080, "assumed: no ports found in "+file)
	}
	return true
}

func detectDockerfile(r *repoFS, res *Result) bool {
	if !r.isFile("Dockerfile") {
		return false
	}
	res.Kind = Dockerfile
	res.Evidence = append(res.Evidence, "Dockerfile")
	res.Container = &Container{Mode: ModeBuild, Path: ".", From: "from Dockerfile"}
	res.require("podman (or docker)")
	text, _ := r.read("Dockerfile")
	if port, ok := dockerfileExpose(text); ok {
		res.setPort(port, "first EXPOSE in Dockerfile")
	} else {
		res.setPort(8080, "assumed: no EXPOSE in Dockerfile")
	}
	return true
}

// parsePort accepts "8080" or "8080/tcp"; ok is false for anything else
// (variables, ranges, out-of-range numbers).
func parsePort(s string) (int, bool) {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = s[:i]
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > 65535 {
		return 0, false
	}
	return n, true
}

// dockerfileExpose returns the first numeric port of the first EXPOSE line.
func dockerfileExpose(text string) (int, bool) {
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 || !strings.EqualFold(fields[0], "EXPOSE") {
			continue
		}
		for _, f := range fields[1:] {
			if port, ok := parsePort(f); ok {
				return port, true
			}
		}
	}
	return 0, false
}

func indentOf(line string) int { return len(line) - len(strings.TrimLeft(line, " ")) }

func stripYAMLComment(line string) string {
	if i := strings.Index(line, " #"); i >= 0 {
		line = line[:i]
	}
	if strings.HasPrefix(strings.TrimSpace(line), "#") {
		return ""
	}
	return strings.TrimRight(line, " \t\r")
}

// composeFirstPort finds the host port of the first ports entry of the first
// service. It understands the common short syntax ("8080:80",
// "127.0.0.1:8080:80", "80"), inline lists, and the long syntax's
// published/target keys. It is a line-based reader, not a YAML parser.
func composeFirstPort(text string) (int, bool) {
	var lines []string
	for _, l := range strings.Split(text, "\n") {
		if l = stripYAMLComment(l); strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	// Locate "services:" and the first service key under it.
	i := 0
	for i < len(lines) && strings.TrimSpace(lines[i]) != "services:" {
		i++
	}
	if i >= len(lines) || indentOf(lines[i]) != 0 {
		return 0, false
	}
	i++
	if i >= len(lines) {
		return 0, false
	}
	svcIndent := indentOf(lines[i])
	if svcIndent == 0 {
		return 0, false
	}
	end := i + 1
	for end < len(lines) && indentOf(lines[end]) > svcIndent {
		end++
	}
	block := lines[i+1 : end]
	for j := 0; j < len(block); j++ {
		trim := strings.TrimSpace(block[j])
		if !strings.HasPrefix(trim, "ports:") {
			continue
		}
		portsIndent := indentOf(block[j])
		rest := strings.TrimSpace(strings.TrimPrefix(trim, "ports:"))
		if strings.HasPrefix(rest, "[") {
			rest = strings.Trim(rest, "[]")
			for _, item := range strings.Split(rest, ",") {
				if p, ok := composeShortPort(item); ok {
					return p, true
				}
			}
			return 0, false
		}
		var longPublished, longTarget int
		for k := j + 1; k < len(block) && indentOf(block[k]) > portsIndent; k++ {
			t := strings.TrimSpace(block[k])
			if item, ok := strings.CutPrefix(t, "- "); ok {
				item = strings.TrimSpace(item)
				if !strings.Contains(item, ": ") && !strings.HasSuffix(item, ":") {
					if p, ok := composeShortPort(item); ok {
						return p, true
					}
					continue
				}
				t = item
			}
			if v, ok := strings.CutPrefix(t, "published:"); ok {
				longPublished, _ = parsePort(strings.Trim(strings.TrimSpace(v), `"'`))
			}
			if v, ok := strings.CutPrefix(t, "target:"); ok {
				longTarget, _ = parsePort(strings.Trim(strings.TrimSpace(v), `"'`))
			}
			if longPublished > 0 {
				return longPublished, true
			}
		}
		if longTarget > 0 {
			return longTarget, true
		}
		return 0, false
	}
	return 0, false
}

func composeShortPort(item string) (int, bool) {
	item = strings.Trim(strings.TrimSpace(item), `"'`)
	parts := strings.Split(item, ":")
	switch len(parts) {
	case 1:
		return parsePort(parts[0])
	case 2:
		return parsePort(parts[0])
	case 3:
		return parsePort(parts[1])
	}
	return 0, false
}
