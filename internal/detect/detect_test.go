package detect

import (
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// repoWith creates a repo directory named dir containing files.
func repoWith(t *testing.T, dir string, files map[string]string) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(root, dir)
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		path := filepath.Join(repo, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return repo
}

func values(vs []Value) []string {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = v.Value
	}
	return out
}

type detectCase struct {
	name      string
	files     map[string]string
	kind      Kind
	command   string
	setup     []string
	port      int
	container string // "mode:path" or ""
	warn      string // substring expected in some warning ("" = none required)
	noWarn    bool   // no warnings expected at all
	evidence  []string
}

var detectCases = []detectCase{
	// Compose.
	{name: "compose.yaml", files: map[string]string{"compose.yaml": "services:\n  web:\n    ports:\n      - \"8080:80\"\n"}, kind: Compose, container: "compose:compose.yaml", port: 8080, evidence: []string{"compose.yaml"}},
	{name: "compose.yml", files: map[string]string{"compose.yml": "services:\n  web:\n    image: nginx\n"}, kind: Compose, container: "compose:compose.yml", port: 8080},
	{name: "docker-compose.yaml", files: map[string]string{"docker-compose.yaml": "services:\n  web:\n    ports: [\"127.0.0.1:9000:80\"]\n"}, kind: Compose, container: "compose:docker-compose.yaml", port: 9000},
	{name: "docker-compose.yml", files: map[string]string{"docker-compose.yml": "services:\n  web:\n    ports:\n      - 7000:80/tcp\n"}, kind: Compose, container: "compose:docker-compose.yml", port: 7000},
	{name: "compose beats Dockerfile and go.mod", files: map[string]string{"compose.yaml": "services:\n  a:\n    image: x\n", "Dockerfile": "FROM scratch\nEXPOSE 1\n", "go.mod": "module x\n"}, kind: Compose, container: "compose:compose.yaml", port: 8080, evidence: []string{"compose.yaml", "Dockerfile"}},
	{name: "compose file order prefers compose.yaml", files: map[string]string{"compose.yaml": "services:\n  a:\n    image: x\n", "docker-compose.yml": "services:\n  a:\n    image: x\n"}, kind: Compose, container: "compose:compose.yaml", port: 8080},

	// Dockerfile.
	{name: "Dockerfile with EXPOSE", files: map[string]string{"Dockerfile": "FROM alpine\nEXPOSE 3000/tcp 4000\n"}, kind: Dockerfile, container: "build:.", port: 3000, noWarn: true},
	{name: "Dockerfile lowercase expose first wins", files: map[string]string{"Dockerfile": "FROM a\nexpose 9090\nFROM b\nEXPOSE 1234\n"}, kind: Dockerfile, container: "build:.", port: 9090},
	{name: "Dockerfile without EXPOSE", files: map[string]string{"Dockerfile": "FROM alpine\n"}, kind: Dockerfile, container: "build:.", port: 8080},
	{name: "Dockerfile EXPOSE variable ignored", files: map[string]string{"Dockerfile": "FROM alpine\nEXPOSE $PORT\n"}, kind: Dockerfile, container: "build:.", port: 8080},
	{name: "Dockerfile beats go.mod and package.json", files: map[string]string{"Dockerfile": "FROM x\n", "go.mod": "module x\n", "package.json": "{}"}, kind: Dockerfile, container: "build:.", port: 8080},

	// Go.
	{name: "go root main", files: map[string]string{"go.mod": "module x\n", "main.go": "package main\nfunc main() {}\n"}, kind: Go, command: "./bin/my-app", setup: []string{"go build -o bin/my-app ./..."}, port: 8080, noWarn: true},
	{name: "go single cmd", files: map[string]string{"go.mod": "module x\n", "cmd/server/main.go": "package main\n", "cmd/empty/README": "x"}, kind: Go, command: "./bin/my-app", setup: []string{"go build -o bin/my-app ./cmd/server"}, port: 8080, evidence: []string{"go.mod", "cmd/server"}},
	{name: "go several cmds", files: map[string]string{"go.mod": "module x\n", "cmd/a/main.go": "package main\n", "cmd/b/main.go": "package main\n"}, kind: Go, command: "./bin/my-app", setup: []string{"go build -o bin/my-app ./..."}, port: 8080, warn: "several programs"},
	{name: "go no main", files: map[string]string{"go.mod": "module x\n", "lib.go": "package lib\n"}, kind: Go, command: "./bin/my-app", setup: []string{"go build -o bin/my-app ./..."}, port: 8080, warn: "no main package"},
	{name: "go beats node", files: map[string]string{"go.mod": "module x\n", "main.go": "package main\n", "package.json": "{}"}, kind: Go, command: "./bin/my-app", setup: []string{"go build -o bin/my-app ./..."}, port: 8080},

	// Node.
	{name: "node npm start and build", files: map[string]string{"package.json": `{"scripts":{"start":"node a.js","build":"tsc"}}`, "package-lock.json": "{}"}, kind: Node, command: "npm start", setup: []string{"npm ci", "npm run build"}, port: 3000, evidence: []string{"package.json", "package-lock.json"}},
	{name: "node npm start only", files: map[string]string{"package.json": `{"scripts":{"start":"node a.js"}}`, "package-lock.json": "{}"}, kind: Node, command: "npm start", setup: []string{"npm ci --omit=dev"}, port: 3000, noWarn: true},
	{name: "node pnpm", files: map[string]string{"package.json": `{"scripts":{"start":"x"}}`, "pnpm-lock.yaml": ""}, kind: Node, command: "pnpm start", setup: []string{"pnpm install --frozen-lockfile --prod"}, port: 3000},
	{name: "node pnpm with build", files: map[string]string{"package.json": `{"scripts":{"start":"x","build":"y"}}`, "pnpm-lock.yaml": ""}, kind: Node, command: "pnpm start", setup: []string{"pnpm install --frozen-lockfile", "pnpm run build"}, port: 3000},
	{name: "node yarn", files: map[string]string{"package.json": `{"scripts":{"start":"x"}}`, "yarn.lock": ""}, kind: Node, command: "yarn start", setup: []string{"yarn install --frozen-lockfile --production"}, port: 3000},
	{name: "node main only", files: map[string]string{"package.json": `{"main":"server/index.js"}`, "package-lock.json": "{}"}, kind: Node, command: "node server/index.js", setup: []string{"npm ci --omit=dev"}, port: 3000},
	{name: "node unsafe main", files: map[string]string{"package.json": `{"main":"x.js; rm -rf ~"}`, "package-lock.json": "{}"}, kind: Node, command: "npm start", setup: []string{"npm ci --omit=dev"}, port: 3000, warn: "no \"start\" script"},
	{name: "node no lockfile", files: map[string]string{"package.json": `{"scripts":{"start":"x"}}`}, kind: Node, command: "npm start", setup: []string{"npm install --omit=dev"}, port: 3000},
	{name: "node malformed package.json", files: map[string]string{"package.json": `{nope`}, kind: Node, command: "npm start", setup: []string{"npm install --omit=dev"}, port: 3000, warn: "could not be parsed"},
	{name: "node beats python", files: map[string]string{"package.json": `{"scripts":{"start":"x"}}`, "requirements.txt": "flask\n"}, kind: Node, command: "npm start", setup: []string{"npm install --omit=dev"}, port: 3000},

	// Python.
	{name: "python uv with scripts", files: map[string]string{"uv.lock": "", "pyproject.toml": "[project]\nname = \"demo\"\n[project.scripts]\nserve = \"demo:main\"\n"}, kind: Python, command: ".venv/bin/serve", setup: []string{"uv sync --frozen --no-dev"}, port: 8000, evidence: []string{"pyproject.toml", "uv.lock"}},
	{name: "python uv module", files: map[string]string{"uv.lock": "", "pyproject.toml": "[project]\nname = \"my-svc\"\n", "my_svc/__main__.py": ""}, kind: Python, command: ".venv/bin/python -m my_svc", setup: []string{"uv sync --frozen --no-dev"}, port: 8000, noWarn: true},
	{name: "python tool.uv without lock", files: map[string]string{"pyproject.toml": "[project]\nname = \"demo\"\n[tool.uv]\ndev-dependencies = []\n", "main.py": ""}, kind: Python, command: ".venv/bin/python main.py", setup: []string{"uv sync --no-dev"}, port: 8000},
	{name: "python requirements plain main.py", files: map[string]string{"requirements.txt": "requests==2.0\n", "main.py": "print(1)\n"}, kind: Python, command: ".venv/bin/python main.py", setup: []string{"python3 -m venv .venv", ".venv/bin/pip install -r requirements.txt"}, port: 8000},
	{name: "python pyproject without uv uses pip", files: map[string]string{"pyproject.toml": "[project]\nname = \"demo\"\n[project.scripts]\ndemo = \"demo:main\"\n"}, kind: Python, command: ".venv/bin/demo", setup: []string{"python3 -m venv .venv", ".venv/bin/pip install ."}, port: 8000},
	{name: "python requirements beats pyproject for install", files: map[string]string{"pyproject.toml": "[project]\nname = \"demo\"\n", "requirements.txt": "x\n"}, kind: Python, command: ".venv/bin/python -m my_app", setup: []string{"python3 -m venv .venv", ".venv/bin/pip install -r requirements.txt"}, port: 8000, warn: "no entry point"},
	{name: "python fastapi", files: map[string]string{"requirements.txt": "fastapi>=0.1\nuvicorn[standard]\n", "api.py": "from fastapi import FastAPI\nservice = FastAPI()\n"}, kind: Python, command: ".venv/bin/python -m uvicorn api:service --host 127.0.0.1 --port 8000", setup: []string{"python3 -m venv .venv", ".venv/bin/pip install -r requirements.txt"}, port: 8000, noWarn: true},
	{name: "python fastapi in app/main.py", files: map[string]string{"requirements.txt": "FastAPI\nuvicorn\n", "app/main.py": "app: FastAPI = FastAPI(title='x')\n"}, kind: Python, command: ".venv/bin/python -m uvicorn app.main:app --host 127.0.0.1 --port 8000", setup: []string{"python3 -m venv .venv", ".venv/bin/pip install -r requirements.txt"}, port: 8000},
	{name: "python fastapi standard extra", files: map[string]string{"requirements.txt": "fastapi[standard]\n", "main.py": "app = FastAPI()\n"}, kind: Python, command: ".venv/bin/python -m uvicorn main:app --host 127.0.0.1 --port 8000", setup: []string{"python3 -m venv .venv", ".venv/bin/pip install -r requirements.txt"}, port: 8000, noWarn: true},
	{name: "python fastapi without uvicorn", files: map[string]string{"requirements.txt": "fastapi\n", "main.py": "app = FastAPI()\n"}, kind: Python, command: ".venv/bin/python -m uvicorn main:app --host 127.0.0.1 --port 8000", setup: []string{"python3 -m venv .venv", ".venv/bin/pip install -r requirements.txt"}, port: 8000, warn: "uvicorn is not in the dependencies"},
	{name: "python uvicorn app not found", files: map[string]string{"requirements.txt": "uvicorn\nfastapi\n"}, kind: Python, command: ".venv/bin/python -m uvicorn main:app --host 127.0.0.1 --port 8000", setup: []string{"python3 -m venv .venv", ".venv/bin/pip install -r requirements.txt"}, port: 8000, warn: "app object not found"},
	{name: "python django runserver", files: map[string]string{"requirements.txt": "Django==5.0\n", "manage.py": "#!/usr/bin/env python\n"}, kind: Python, command: ".venv/bin/python manage.py runserver 127.0.0.1:8000", setup: []string{"python3 -m venv .venv", ".venv/bin/pip install -r requirements.txt"}, port: 8000, warn: "runserver"},
	{name: "python django gunicorn", files: map[string]string{"requirements.txt": "django\ngunicorn\n", "manage.py": "", "mysite/wsgi.py": "application = None\n"}, kind: Python, command: ".venv/bin/gunicorn mysite.wsgi:application --bind 127.0.0.1:8000", setup: []string{"python3 -m venv .venv", ".venv/bin/pip install -r requirements.txt"}, port: 8000, noWarn: true},
	{name: "python django gunicorn without wsgi falls back", files: map[string]string{"requirements.txt": "django\ngunicorn\n", "manage.py": ""}, kind: Python, command: ".venv/bin/python manage.py runserver 127.0.0.1:8000", setup: []string{"python3 -m venv .venv", ".venv/bin/pip install -r requirements.txt"}, port: 8000, warn: "runserver"},
	{name: "python flask dev server", files: map[string]string{"requirements.txt": "flask\n", "app.py": "app = Flask(__name__)\n"}, kind: Python, command: ".venv/bin/flask --app app:app run --host 127.0.0.1 --port 8000", setup: []string{"python3 -m venv .venv", ".venv/bin/pip install -r requirements.txt"}, port: 8000, warn: "development server"},
	{name: "python flask gunicorn", files: map[string]string{"requirements.txt": "flask\ngunicorn\n", "wsgi.py": "application = Flask(__name__)\n"}, kind: Python, command: ".venv/bin/gunicorn --bind 127.0.0.1:8000 wsgi:application", setup: []string{"python3 -m venv .venv", ".venv/bin/pip install -r requirements.txt"}, port: 8000, noWarn: true},
	{name: "python flask unknown app", files: map[string]string{"requirements.txt": "flask\n"}, kind: Python, command: ".venv/bin/flask run --host 127.0.0.1 --port 8000", setup: []string{"python3 -m venv .venv", ".venv/bin/pip install -r requirements.txt"}, port: 8000, warn: "development server"},
	{name: "python requirements comments and options ignored", files: map[string]string{"requirements.txt": "# flask\n-r other.txt\nrequests  # flask\n", "main.py": ""}, kind: Python, command: ".venv/bin/python main.py", setup: []string{"python3 -m venv .venv", ".venv/bin/pip install -r requirements.txt"}, port: 8000},
	{name: "python beats rust", files: map[string]string{"requirements.txt": "x\n", "main.py": "", "Cargo.toml": "[package]\nname = \"x\"\n"}, kind: Python, command: ".venv/bin/python main.py", setup: []string{"python3 -m venv .venv", ".venv/bin/pip install -r requirements.txt"}, port: 8000},

	// Rust.
	{name: "rust single bin", files: map[string]string{"Cargo.toml": "[package]\nname = \"crate-name\"\n[[bin]]\nname = \"server\"\npath = \"src/x.rs\"\n"}, kind: Rust, command: "./target/release/server", setup: []string{"cargo build --release"}, port: 8080, noWarn: true},
	{name: "rust package name", files: map[string]string{"Cargo.toml": "[package]\nname = \"crate-name\"\n", "src/main.rs": "fn main(){}"}, kind: Rust, command: "./target/release/crate-name", setup: []string{"cargo build --release"}, port: 8080},
	{name: "rust several bins", files: map[string]string{"Cargo.toml": "[package]\nname = \"c\"\n[[bin]]\nname = \"b\"\n[[bin]]\nname = \"a\"\n"}, kind: Rust, command: "./target/release/a", setup: []string{"cargo build --release"}, port: 8080, warn: "several binaries"},
	{name: "rust workspace", files: map[string]string{"Cargo.toml": "[workspace]\nmembers = [\"a\"]\n"}, kind: Rust, command: "./target/release/my-app", setup: []string{"cargo build --release"}, port: 8080, warn: "workspace"},
	{name: "rust malformed", files: map[string]string{"Cargo.toml": "[package"}, kind: Rust, command: "./target/release/my-app", setup: []string{"cargo build --release"}, port: 8080, warn: "could not be parsed"},

	// Static.
	{name: "static index.html", files: map[string]string{"index.html": "<html></html>"}, kind: Static, command: "python3 -m http.server 8000 --bind 127.0.0.1", port: 8000, evidence: []string{"index.html"}},
	{name: "index.html below the root is not static", files: map[string]string{"public/index.html": "<html></html>"}, kind: Generic, command: "python -m your_app"},
	{name: "static loses to package.json", files: map[string]string{"index.html": "x", "package.json": `{"scripts":{"start":"x"}}`}, kind: Node, command: "npm start", setup: []string{"npm install --omit=dev"}, port: 3000},

	// Fallback.
	{name: "empty repo", files: map[string]string{}, kind: Generic, command: "python -m your_app"},
	{name: "unrecognized files", files: map[string]string{"README.md": "hi", "notes.txt": "x"}, kind: Generic, command: "python -m your_app"},
}

func TestDetect(t *testing.T) {
	for _, tc := range detectCases {
		t.Run(tc.name, func(t *testing.T) {
			repo := repoWith(t, "My App", tc.files)
			res, err := Detect(repo)
			if err != nil {
				t.Fatal(err)
			}
			if res.Kind != tc.kind {
				t.Fatalf("kind = %s, want %s (evidence %v)", res.Kind, tc.kind, res.Evidence)
			}
			if res.Name != "my-app" {
				t.Errorf("name = %q", res.Name)
			}
			if res.Command.Value != tc.command {
				t.Errorf("command = %q, want %q", res.Command.Value, tc.command)
			}
			if !reflect.DeepEqual(values(res.Setup), tc.setup) && !(len(res.Setup) == 0 && len(tc.setup) == 0) {
				t.Errorf("setup = %v, want %v", values(res.Setup), tc.setup)
			}
			if res.Port != tc.port {
				t.Errorf("port = %d, want %d", res.Port, tc.port)
			}
			wantHealth := ""
			if tc.port > 0 {
				wantHealth = "http://127.0.0.1:" + itoa(tc.port) + "/"
			}
			if res.Health != wantHealth {
				t.Errorf("health = %q, want %q", res.Health, wantHealth)
			}
			got := ""
			if res.Container != nil {
				got = res.Container.Mode + ":" + res.Container.Path
			}
			if got != tc.container {
				t.Errorf("container = %q, want %q", got, tc.container)
			}
			if (tc.container != "") == (res.Command.Value != "" && res.Kind != Generic) && tc.container != "" {
				t.Errorf("container projects must not set a command: %q", res.Command.Value)
			}
			for _, e := range tc.evidence {
				if !contains(res.Evidence, e) {
					t.Errorf("evidence %v lacks %q", res.Evidence, e)
				}
			}
			if tc.warn != "" && !anyContains(res.Warnings, tc.warn) {
				t.Errorf("warnings %v lack %q", res.Warnings, tc.warn)
			}
			if tc.noWarn && len(res.Warnings) > 0 {
				t.Errorf("unexpected warnings: %v", res.Warnings)
			}
			if res.Kind != Generic && res.Command.From == "" && res.Container == nil {
				t.Errorf("command has no provenance: %+v", res.Command)
			}
			for _, s := range res.Setup {
				if s.From == "" {
					t.Errorf("setup %q has no provenance", s.Value)
				}
			}
		})
	}
}

func TestDetectCoversEveryKind(t *testing.T) {
	seen := map[Kind]bool{}
	for _, tc := range detectCases {
		seen[tc.kind] = true
	}
	for _, k := range []Kind{Compose, Dockerfile, Go, Node, Python, Rust, Static, Generic} {
		if !seen[k] {
			t.Errorf("no detect case for %s", k)
		}
	}
}

func TestDetectSecretsFile(t *testing.T) {
	cases := []struct {
		name  string
		files []string
		want  string
	}{
		{"none", nil, ""},
		{"prod", []string{".env.prod"}, ".env.prod"},
		{"production", []string{".env.production"}, ".env.production"},
		{"prod wins", []string{".env.production", ".env.prod"}, ".env.prod"},
		{"plain .env ignored", []string{".env"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files := map[string]string{"go.mod": "module x\n"}
			for _, f := range tc.files {
				files[f] = "TOP_SECRET=hunter2\n"
			}
			res, err := Detect(repoWith(t, "app", files))
			if err != nil {
				t.Fatal(err)
			}
			got := ""
			if res.Secrets != nil {
				got = res.Secrets.Value
			}
			if got != tc.want {
				t.Errorf("secrets = %q, want %q", got, tc.want)
			}
		})
	}
}

// The secrets file is detected by name only; an unreadable file still counts
// and its contents never reach the result.
func TestDetectNeverReadsSecretsContents(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read anything")
	}
	repo := repoWith(t, "app", map[string]string{"go.mod": "module x\n", ".env.prod": "TOP_SECRET=hunter2\n"})
	if err := os.Chmod(filepath.Join(repo, ".env.prod"), 0); err != nil {
		t.Fatal(err)
	}
	res, err := Detect(repo)
	if err != nil {
		t.Fatal(err)
	}
	if res.Secrets == nil {
		t.Fatal("unreadable .env.prod not detected")
	}
}

func TestDetectIgnoresSymlinkedManifests(t *testing.T) {
	repo := repoWith(t, "app", nil)
	outside := filepath.Join(filepath.Dir(repo), "outside.json")
	if err := os.WriteFile(outside, []byte(`{"scripts":{"start":"x"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(repo, "package.json")); err != nil {
		t.Fatal(err)
	}
	res, err := Detect(repo)
	if err != nil {
		t.Fatal(err)
	}
	if res.Kind != Generic {
		t.Errorf("symlinked package.json detected as %s", res.Kind)
	}
}

func TestDetectErrors(t *testing.T) {
	repo := repoWith(t, "app", map[string]string{"file": "x"})
	if _, err := Detect(filepath.Join(repo, "missing")); err == nil {
		t.Error("missing directory accepted")
	}
	if _, err := Detect(filepath.Join(repo, "file")); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Errorf("file accepted: %v", err)
	}
}

func TestServiceName(t *testing.T) {
	cases := map[string]string{
		"/x/api":                               "api",
		"/x/My App!":                           "my-app",
		"/x/my app":                            "my-app",
		"/x/.hidden":                           "hidden",
		"/x/---":                               "app",
		"/x/Foo_Bar.baz":                       "foo-bar-baz",
		"/x/a__b":                              "a-b",
		"/x/UPPER":                             "upper",
		"/x/ünï":                               "n",
		"/x/" + strings.Repeat("a", 80):        strings.Repeat("a", 63),
		"/x/" + strings.Repeat("a", 62) + "-b": strings.Repeat("a", 62),
	}
	for in, want := range cases {
		if got := ServiceName(in); got != want {
			t.Errorf("ServiceName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestComposeFirstPort(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want int
		ok   bool
	}{
		{"short", "services:\n  a:\n    ports:\n      - \"8080:80\"\n", 8080, true},
		{"with ip", "services:\n  a:\n    ports:\n      - 127.0.0.1:9000:80\n", 9000, true},
		{"container only", "services:\n  a:\n    ports:\n      - \"80\"\n", 80, true},
		{"inline list", "services:\n  a:\n    ports: [\"3000:3000\"]\n", 3000, true},
		{"first service only", "services:\n  a:\n    image: x\n  b:\n    ports:\n      - \"1:1\"\n", 0, false},
		{"long syntax", "services:\n  a:\n    ports:\n      - target: 80\n        published: 8181\n", 8181, true},
		{"comments", "# top\nservices: # x\n  a:\n    ports:\n      # no\n      - \"4000:80\" # yes\n", 4000, true},
		{"range ignored", "services:\n  a:\n    ports:\n      - \"8000-8010:8000\"\n", 0, false},
		{"no services", "version: '3'\n", 0, false},
		{"empty", "", 0, false},
	}
	for _, tc := range cases {
		got, ok := composeFirstPort(tc.yaml)
		if got != tc.want || ok != tc.ok {
			t.Errorf("%s: got (%d, %v), want (%d, %v)", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}

func contains(items []string, want string) bool {
	for _, i := range items {
		if i == want {
			return true
		}
	}
	return false
}

func anyContains(items []string, sub string) bool {
	for _, i := range items {
		if strings.Contains(i, sub) {
			return true
		}
	}
	return false
}

func itoa(n int) string { return strconv.Itoa(n) }
