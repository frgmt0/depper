package detect

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

type pyproject struct {
	Project struct {
		Name         string            `toml:"name"`
		Dependencies []string          `toml:"dependencies"`
		Scripts      map[string]string `toml:"scripts"`
	} `toml:"project"`
}

// pyDeps maps a normalized dependency name to its extras (comma separated).
type pyDeps map[string]string

var (
	reqName   = regexp.MustCompile(`^\s*([A-Za-z0-9][A-Za-z0-9._-]*)\s*(?:\[([^\]]*)\])?`)
	pyIdent   = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	pyModule  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)*$`)
	pyVarSpec = `(?m)^\s*([A-Za-z_][A-Za-z0-9_]*)\s*(?::[^=\n]+)?=\s*`
)

func normalizeDep(name string) string {
	name = strings.ToLower(name)
	return strings.NewReplacer("_", "-", ".", "-").Replace(name)
}

func (d pyDeps) addSpec(spec string) {
	spec = strings.TrimSpace(spec)
	if spec == "" || strings.HasPrefix(spec, "#") || strings.HasPrefix(spec, "-") {
		return
	}
	m := reqName.FindStringSubmatch(spec)
	if m == nil {
		return
	}
	d[normalizeDep(m[1])] = strings.ToLower(m[2])
}

func (d pyDeps) has(name string) bool { _, ok := d[name]; return ok }

func detectPython(r *repoFS, res *Result) bool {
	hasLock := r.isFile("uv.lock")
	hasPyproject := r.isFile("pyproject.toml")
	hasReq := r.isFile("requirements.txt")
	if !hasLock && !hasPyproject && !hasReq {
		return false
	}
	res.Kind = Python

	var proj pyproject
	usesUV := hasLock
	if hasPyproject {
		res.Evidence = append(res.Evidence, "pyproject.toml")
		text, _ := r.read("pyproject.toml")
		md, err := toml.Decode(text, &proj)
		if err != nil {
			res.warn("pyproject.toml could not be parsed (%v); dependencies and entry points are guesses", err)
			proj = pyproject{}
		} else if md.IsDefined("tool", "uv") {
			usesUV = true
		}
	}
	if hasLock {
		res.Evidence = append(res.Evidence, "uv.lock")
	}

	deps := pyDeps{}
	for _, spec := range proj.Project.Dependencies {
		deps.addSpec(spec)
	}
	if hasReq {
		res.Evidence = append(res.Evidence, "requirements.txt")
		text, _ := r.read("requirements.txt")
		for _, line := range strings.Split(text, "\n") {
			if i := strings.Index(line, " #"); i >= 0 {
				line = line[:i]
			}
			deps.addSpec(line)
		}
	}

	switch {
	case usesUV:
		install, from := "uv sync --frozen --no-dev", "from uv.lock"
		if !hasLock {
			install, from = "uv sync --no-dev", "from [tool.uv] in pyproject.toml (no uv.lock, so versions are not pinned)"
		}
		res.Setup = []Value{{Value: install, From: from}}
		res.require("uv")
	case hasReq:
		res.Setup = []Value{
			{Value: "python3 -m venv .venv", From: "from requirements.txt"},
			{Value: ".venv/bin/pip install -r requirements.txt", From: "from requirements.txt"},
		}
		res.require("python3 with venv and pip")
	default:
		res.Setup = []Value{
			{Value: "python3 -m venv .venv", From: "from pyproject.toml"},
			{Value: ".venv/bin/pip install .", From: "from pyproject.toml"},
		}
		res.require("python3 with venv and pip")
	}
	res.Excludes = []Value{
		{Value: ".venv/", From: "created on the server by the setup step"},
		{Value: "__pycache__/", From: "Python bytecode caches"},
	}

	res.setPort(8000, "default for Python apps; make sure your app listens on it")
	res.Command = pythonCommand(r, res, &proj, deps)
	return true
}

func pythonCommand(r *repoFS, res *Result, proj *pyproject, deps pyDeps) Value {
	port := res.Port
	host := "127.0.0.1"

	if r.isFile("manage.py") {
		res.Evidence = append(res.Evidence, "manage.py")
		if deps.has("gunicorn") {
			for _, d := range r.subdirs(".") {
				if !pyIdent.MatchString(d) || !r.isFile(d+"/wsgi.py") {
					continue
				}
				res.Evidence = append(res.Evidence, d+"/wsgi.py")
				return Value{
					Value: fmt.Sprintf(".venv/bin/gunicorn %s.wsgi:application --bind %s:%d", d, host, port),
					From:  "Django project (manage.py) with gunicorn in the dependencies",
				}
			}
		}
		note := "runserver is Django's development server and is not meant for production."
		from := "Django project (manage.py)"
		if deps.has("gunicorn") {
			note += " gunicorn is listed but no <project>/wsgi.py was found, so switch to it once you know the WSGI module."
		} else {
			note += " Add gunicorn to your dependencies and run it against your WSGI module instead."
		}
		res.warn("Django: manage.py runserver is the development server; it is not meant for production")
		return Value{Value: fmt.Sprintf(".venv/bin/python manage.py runserver %s:%d", host, port), From: from, Note: note}
	}

	if deps.has("fastapi") || deps.has("uvicorn") {
		target, found, file := findAppObject(r, "FastAPI")
		from := "FastAPI app in " + file
		note := ""
		if !found {
			target, from = "main:app", "assumed: FastAPI in the dependencies, app object not found"
			note = "Could not find where the FastAPI app is created: adjust module:variable."
			res.warn("FastAPI app object not found; the uvicorn target %s is a guess", target)
		} else {
			res.Evidence = append(res.Evidence, file)
		}
		if !deps.has("uvicorn") && !strings.Contains(deps["fastapi"], "standard") {
			note = strings.TrimSpace(note + " uvicorn is not listed in the dependencies: add it, or the service will not start.")
			res.warn("uvicorn is not in the dependencies; add it so the service can start")
		}
		return Value{
			Value: fmt.Sprintf(".venv/bin/python -m uvicorn %s --host %s --port %d", target, host, port),
			From:  from,
			Note:  note,
		}
	}

	if deps.has("flask") {
		target, found, file := findAppObject(r, "Flask")
		if found {
			res.Evidence = append(res.Evidence, file)
		}
		if deps.has("gunicorn") {
			from, note := "Flask app in "+file+", gunicorn in the dependencies", ""
			if !found {
				target, from = "app:app", "assumed: Flask and gunicorn in the dependencies, app object not found"
				note = "Could not find where the Flask app is created: adjust module:variable."
				res.warn("Flask app object not found; the gunicorn target %s is a guess", target)
			}
			return Value{Value: fmt.Sprintf(".venv/bin/gunicorn --bind %s:%d %s", host, port, target), From: from, Note: note}
		}
		note := "flask run is Flask's development server and is not meant for production. Add gunicorn to your dependencies and run it instead."
		res.warn("Flask: flask run is the development server; it is not meant for production")
		if found {
			return Value{
				Value: fmt.Sprintf(".venv/bin/flask --app %s run --host %s --port %d", target, host, port),
				From:  "Flask app in " + file,
				Note:  note,
			}
		}
		return Value{
			Value: fmt.Sprintf(".venv/bin/flask run --host %s --port %d", host, port),
			From:  "Flask in the dependencies; flask finds app.py or wsgi.py itself",
			Note:  note,
		}
	}

	if len(proj.Project.Scripts) > 0 {
		names := make([]string, 0, len(proj.Project.Scripts))
		for n := range proj.Project.Scripts {
			if safe(n) {
				names = append(names, n)
			}
		}
		sort.Strings(names)
		if len(names) > 0 {
			note := ""
			if len(names) > 1 {
				note = "pyproject.toml defines several scripts (" + strings.Join(names, ", ") + "): pick the one that starts your app."
			}
			return Value{Value: ".venv/bin/" + names[0], From: "from [project.scripts] in pyproject.toml", Note: note}
		}
	}

	if module, from, ok := pythonModule(r, proj); ok {
		return Value{Value: ".venv/bin/python -m " + module, From: from}
	}
	for _, f := range []string{"main.py", "app.py", "server.py"} {
		if r.isFile(f) {
			res.Evidence = append(res.Evidence, f)
			return Value{Value: ".venv/bin/python " + f, From: "found " + f + " at the repository root"}
		}
	}
	module := strings.ReplaceAll(res.Name, "-", "_")
	if !pyIdent.MatchString(module) {
		module = "app"
	}
	res.warn("no entry point found; the command runs python -m %s as a guess", module)
	return Value{
		Value: ".venv/bin/python -m " + module,
		From:  "assumed from the directory name",
		Note:  "No entry point found (no framework, [project.scripts], __main__.py or main.py): change this command.",
	}
}

// pythonModule finds a package with a __main__.py named after the project.
func pythonModule(r *repoFS, proj *pyproject) (module, from string, ok bool) {
	var names []string
	if proj.Project.Name != "" {
		names = append(names, strings.ReplaceAll(normalizeDep(proj.Project.Name), "-", "_"))
	}
	for _, name := range names {
		if pyModule.MatchString(name) && r.isFile(name+"/__main__.py") {
			return name, "package " + name + " has a __main__.py", true
		}
	}
	return "", "", false
}

var appFileCandidates = []string{
	"main.py", "app.py", "server.py", "api.py", "wsgi.py",
	"app/main.py", "app/__init__.py", "app/app.py",
}

// findAppObject looks for `<var> = <ctor>(` in the usual entry files and
// returns "module:var", whether it was found, and the file name.
func findAppObject(r *repoFS, ctor string) (target string, found bool, file string) {
	re := regexp.MustCompile(pyVarSpec + regexp.QuoteMeta(ctor) + `\(`)
	for _, f := range appFileCandidates {
		text, ok := r.read(f)
		if !ok {
			continue
		}
		m := re.FindStringSubmatch(text)
		if m == nil {
			continue
		}
		module := strings.TrimSuffix(f, ".py")
		module = strings.TrimSuffix(module, "/__init__")
		module = strings.ReplaceAll(module, "/", ".")
		return module + ":" + m[1], true, f
	}
	return "", false, "the repository"
}
