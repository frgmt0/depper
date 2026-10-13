package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/frgmt0/depper/internal/config"
	"github.com/frgmt0/depper/internal/detect"
	"github.com/frgmt0/depper/internal/exitcode"
)

func noEnv(string) (string, bool) { return "", false }

func writeFiles(t *testing.T, repo string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		path := filepath.Join(repo, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// initCases cover every detector, plus the fallback. Each result must pass
// config.Load and Resolve.
var initCases = []struct {
	name    string
	files   map[string]string
	kind    detect.Kind
	mode    string // config mode
	command string // expected [service].command ("" for containers)
	port    string // expected [env] PORT ("" when none)
	health  string
	setup   []string
	comment string // text that must appear in a trailing comment
}{
	{"compose", map[string]string{"compose.yaml": "services:\n  web:\n    ports: [\"8081:80\"]\n"}, detect.Compose, config.ModeCompose, "", "", "http://127.0.0.1:8081/", nil, `compose = "compose.yaml"  # from compose.yaml`},
	{"Dockerfile", map[string]string{"Dockerfile": "FROM alpine\nEXPOSE 3000\n"}, detect.Dockerfile, config.ModeBuild, "", "", "http://127.0.0.1:3000/", nil, `# first EXPOSE in Dockerfile`},
	{"go", map[string]string{"go.mod": "module x\n", "cmd/api/main.go": "package main\n"}, detect.Go, config.ModeProcess, "./bin/my-service", "8080", "http://127.0.0.1:8080/", []string{"go build -o bin/my-service ./cmd/api"}, `# the binary built by the setup step`},
	{"node", map[string]string{"package.json": `{"scripts":{"start":"node a.js","build":"tsc"}}`, "package-lock.json": "{}"}, detect.Node, config.ModeProcess, "npm start", "3000", "http://127.0.0.1:3000/", []string{"npm ci", "npm run build"}, `# from package.json "start" script`},
	{"python uv", map[string]string{"uv.lock": "", "pyproject.toml": "[project]\nname = \"x\"\n[project.scripts]\nrun = \"x:main\"\n"}, detect.Python, config.ModeProcess, ".venv/bin/run", "8000", "http://127.0.0.1:8000/", []string{"uv sync --frozen --no-dev"}, `# from [project.scripts] in pyproject.toml`},
	{"python requirements", map[string]string{"requirements.txt": "flask\ngunicorn\n", "app.py": "app = Flask(__name__)\n"}, detect.Python, config.ModeProcess, ".venv/bin/gunicorn --bind 127.0.0.1:8000 app:app", "8000", "http://127.0.0.1:8000/", []string{"python3 -m venv .venv", ".venv/bin/pip install -r requirements.txt"}, `# Flask app in app.py`},
	{"rust", map[string]string{"Cargo.toml": "[package]\nname = \"srv\"\n", "src/main.rs": "fn main(){}"}, detect.Rust, config.ModeProcess, "./target/release/srv", "8080", "http://127.0.0.1:8080/", []string{"cargo build --release"}, `# from the package name in Cargo.toml`},
	{"static", map[string]string{"index.html": "<h1>hi</h1>"}, detect.Static, config.ModeProcess, "python3 -m http.server 8000 --bind 127.0.0.1", "8000", "http://127.0.0.1:8000/", nil, `# index.html at the repository root`},
	{"generic fallback", map[string]string{"README.md": "x"}, detect.Generic, config.ModeProcess, "python -m your_app", "", "", nil, ""},
}

func TestInitWritesAConfigThatLoadsForEveryKind(t *testing.T) {
	for _, tc := range initCases {
		t.Run(tc.name, func(t *testing.T) {
			repo := tempRepo(t, "My Service")
			writeFiles(t, repo, tc.files)
			app, stdout, _ := testApp(nil)
			if code := Run(context.Background(), app, []string{"init", repo}); code != 0 {
				t.Fatalf("exit %d", code)
			}
			path := filepath.Join(repo, config.FileName)
			if !strings.HasPrefix(stdout.String(), "Created "+path+"\n") {
				t.Errorf("stdout = %q", stdout.String())
			}
			written, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			text := string(written)

			cfg, err := config.LoadTarget(repo, "", noEnv)
			if err != nil {
				t.Fatalf("written config does not load: %v\n%s", err, text)
			}
			if cfg.Service.Name != "my-service" || cfg.Deploy.Host != "deploy@203.0.113.10" {
				t.Errorf("service %+v host %q", cfg.Service, cfg.Deploy.Host)
			}
			if cfg.Mode() != tc.mode {
				t.Errorf("mode = %q, want %q", cfg.Mode(), tc.mode)
			}
			if cfg.Service.Command != tc.command {
				t.Errorf("command = %q, want %q", cfg.Service.Command, tc.command)
			}
			gotPort, _ := cfg.Env["PORT"]
			if gotPort != tc.port {
				t.Errorf("PORT = %q, want %q", gotPort, tc.port)
			}
			gotHealth := ""
			if cfg.Health != nil {
				gotHealth = cfg.Health.URL
			}
			if gotHealth != tc.health {
				t.Errorf("health = %q, want %q", gotHealth, tc.health)
			}
			if strings.Join(cfg.Deploy.Setup, "|") != strings.Join(tc.setup, "|") {
				t.Errorf("setup = %v, want %v", cfg.Deploy.Setup, tc.setup)
			}
			if tc.comment != "" && !strings.Contains(text, tc.comment) {
				t.Errorf("config lacks %q:\n%s", tc.comment, text)
			}
			if strings.Contains(text, "desktop") {
				t.Errorf("template contains a personal host alias:\n%s", text)
			}
			for _, want := range []string{"depper host init root@YOUR_SERVER", "# [tunnel]", "# [targets.prod]", "# [secrets]"} {
				if !strings.Contains(text, want) {
					t.Errorf("config lacks %q", want)
				}
			}
			if tc.kind != detect.Generic && !strings.Contains(text, "for a ") {
				t.Errorf("header does not name the detected kind:\n%s", text)
			}
			// Every pre-filled service/container line carries a provenance comment.
			if tc.kind != detect.Generic {
				for _, prefix := range []string{"name = ", "command = ", "compose = ", "build = ", "PORT = ", "url = "} {
					for _, line := range strings.Split(text, "\n") {
						if strings.HasPrefix(line, prefix) && !strings.Contains(line, "  # ") {
							t.Errorf("line without a provenance comment: %q", line)
						}
					}
				}
			}
			if (tc.mode == config.ModeBuild || tc.mode == config.ModeCompose) && (!strings.Contains(text, `engine = "podman"  # or "docker" (needs root-equivalent docker group)`) || cfg.Container.Engine != "podman") {
				t.Errorf("container engine is not podman:\n%s", text)
			}
			if tc.mode == config.ModeBuild && !strings.Contains(text, `127.0.0.1:3000:3000`) {
				t.Errorf("Dockerfile config does not publish the EXPOSE port:\n%s", text)
			}

			// A second init refuses; the file is untouched.
			app, _, stderr := testApp(nil)
			if code := Run(context.Background(), app, []string{"init", repo}); code != exitcode.Failure {
				t.Errorf("second init exit = %d", code)
			}
			if !strings.HasPrefix(stderr.String(), "depper: ") || !strings.Contains(stderr.String(), "already exists") {
				t.Errorf("stderr = %q", stderr.String())
			}
		})
	}
}

func TestInitCoversEveryKind(t *testing.T) {
	seen := map[detect.Kind]bool{}
	for _, tc := range initCases {
		seen[tc.kind] = true
	}
	for _, k := range []detect.Kind{detect.Compose, detect.Dockerfile, detect.Go, detect.Node, detect.Python, detect.Rust, detect.Static, detect.Generic} {
		if !seen[k] {
			t.Errorf("no init case for %s", k)
		}
	}
}

func TestInitPrecedenceAndFallback(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		want  detect.Kind
	}{
		{"compose over Dockerfile", map[string]string{"compose.yml": "services: {}\n", "Dockerfile": "FROM x\n"}, detect.Compose},
		{"Dockerfile over Go", map[string]string{"Dockerfile": "FROM x\n", "go.mod": "module x\n"}, detect.Dockerfile},
		{"Go over Node", map[string]string{"go.mod": "module x\n", "package.json": "{}"}, detect.Go},
		{"Node over Python", map[string]string{"package.json": "{}", "requirements.txt": "x\n"}, detect.Node},
		{"Python over Rust", map[string]string{"requirements.txt": "x\n", "Cargo.toml": "[package]\nname=\"x\"\n"}, detect.Python},
		{"Rust over static", map[string]string{"Cargo.toml": "[package]\nname=\"x\"\n", "index.html": "x"}, detect.Rust},
		{"static", map[string]string{"index.html": "x"}, detect.Static},
		{"fallback", map[string]string{}, detect.Generic},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := tempRepo(t, "p")
			writeFiles(t, repo, tc.files)
			res, content, err := renderStarter(repo)
			if err != nil {
				t.Fatal(err)
			}
			if res.Kind != tc.want {
				t.Fatalf("kind = %s, want %s", res.Kind, tc.want)
			}
			writeFiles(t, repo, map[string]string{config.FileName: content})
			project, err := config.Load(repo)
			if err != nil {
				t.Fatalf("load: %v\n%s", err, content)
			}
			if _, err := project.ResolveWithEnv("", noEnv); err != nil {
				t.Fatalf("resolve: %v\n%s", err, content)
			}
		})
	}
}

func TestInitSecretsFile(t *testing.T) {
	for _, name := range []string{".env.prod", ".env.production"} {
		t.Run(name, func(t *testing.T) {
			repo := tempRepo(t, "s")
			writeFiles(t, repo, map[string]string{"go.mod": "module x\n", name: "TOP_SECRET=hunter2\n"})
			app, stdout, stderr := testApp(nil)
			if code := Run(context.Background(), app, []string{"init", "--print", repo}); code != 0 {
				t.Fatalf("exit %d: %s", code, stderr)
			}
			if strings.Contains(stdout.String(), "hunter2") || strings.Contains(stdout.String(), "TOP_SECRET") {
				t.Errorf("secrets content leaked:\n%s", stdout)
			}
			if !strings.Contains(stdout.String(), "\n[secrets]\nfile = \""+name+"\"") {
				t.Errorf("[secrets] not enabled:\n%s", stdout)
			}
			writeFiles(t, repo, map[string]string{config.FileName: stdout.String()})
			cfg, err := config.LoadTarget(repo, "", noEnv)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Secrets == nil || filepath.Base(cfg.Secrets.File) != name {
				t.Errorf("secrets = %+v", cfg.Secrets)
			}
		})
	}
}

func TestInitPrintForceAndJSON(t *testing.T) {
	repo := tempRepo(t, "api")
	writeFiles(t, repo, map[string]string{"go.mod": "module x\n"})

	// --print writes to stdout and creates nothing.
	app, stdout, _ := testApp(nil)
	if code := Run(context.Background(), app, []string{"init", "--print", repo}); code != 0 {
		t.Fatalf("print exit %d", code)
	}
	if !strings.HasPrefix(stdout.String(), "# depper configuration") || strings.Contains(stdout.String(), "Created") {
		t.Errorf("print output = %q", stdout.String())
	}
	if _, err := os.Stat(filepath.Join(repo, config.FileName)); err == nil {
		t.Error("--print created depper.toml")
	}

	// --json prints the detection and the created path.
	app, stdout, _ = testApp(nil)
	if code := Run(context.Background(), app, []string{"--json", "init", repo}); code != 0 {
		t.Fatalf("json exit %d", code)
	}
	var out struct {
		Created   string        `json:"created"`
		Detection detect.Result `json:"detection"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		t.Fatalf("json = %q (%v)", stdout.String(), err)
	}
	if out.Created != filepath.Join(repo, config.FileName) || out.Detection.Kind != detect.Go || out.Detection.Name != "api" || out.Detection.Port != 8080 {
		t.Errorf("json = %+v", out)
	}

	// --json --print returns the detection and the rendered config.
	app, stdout, _ = testApp(nil)
	if code := Run(context.Background(), app, []string{"--json", "init", "--print", repo}); code != 0 {
		t.Fatalf("json print exit %d", code)
	}
	var printed struct {
		Config    string        `json:"config"`
		Detection detect.Result `json:"detection"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &printed); err != nil || printed.Detection.Kind != detect.Go || !strings.Contains(printed.Config, "[service]") {
		t.Errorf("json print = %q (%v)", stdout.String(), err)
	}

	// An existing file is kept unless --force.
	if err := os.WriteFile(filepath.Join(repo, config.FileName), []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}
	app, _, stderr := testApp(nil)
	if code := Run(context.Background(), app, []string{"init", repo}); code != exitcode.Failure || !strings.Contains(stderr.String(), "--force") {
		t.Errorf("init over existing: exit %d, stderr %q", code, stderr)
	}
	if b, _ := os.ReadFile(filepath.Join(repo, config.FileName)); string(b) != "keep me" {
		t.Errorf("existing file modified: %q", b)
	}
	// --print still works when a config exists.
	app, stdout, _ = testApp(nil)
	if code := Run(context.Background(), app, []string{"init", "--print", repo}); code != 0 || stdout.Len() == 0 {
		t.Errorf("print with existing config: exit %d", code)
	}
	app, _, _ = testApp(nil)
	if code := Run(context.Background(), app, []string{"init", "--force", repo}); code != 0 {
		t.Fatalf("force exit %d", code)
	}
	b, _ := os.ReadFile(filepath.Join(repo, config.FileName))
	if !strings.Contains(string(b), "[service]") {
		t.Errorf("--force did not overwrite: %q", b)
	}
	if _, err := config.LoadTarget(repo, "", noEnv); err != nil {
		t.Errorf("forced config does not load: %v", err)
	}
	if entries, _ := filepath.Glob(filepath.Join(repo, ".depper.toml.*")); len(entries) > 0 {
		t.Errorf("temporary files left behind: %v", entries)
	}
}

func TestInitRefusesLegacyConfigAndMissingDirectory(t *testing.T) {
	repo := tempRepo(t, "legacy")
	if err := os.WriteFile(filepath.Join(repo, config.LegacyFileName), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, force := range []bool{false, true} {
		if _, err := writeStarterConfig(repo, "x", force); err == nil || !strings.Contains(err.Error(), "deploy.toml already exists") {
			t.Errorf("legacy error (force=%v) = %v", force, err)
		}
	}
	if _, err := writeStarterConfig(filepath.Join(repo, "missing"), "x", false); err == nil {
		t.Error("missing directory accepted")
	}
	app, _, stderr := testApp(nil)
	if code := Run(context.Background(), app, []string{"init", filepath.Join(repo, "missing")}); code != exitcode.Failure || !strings.HasPrefix(stderr.String(), "depper: ") {
		t.Errorf("init missing dir: exit %d, stderr %q", code, stderr)
	}
}

// uncomment enables the commented block that starts with "# <header>" and
// ends at the next blank line.
func uncomment(content, header string) string {
	lines := strings.Split(content, "\n")
	in := false
	for i, line := range lines {
		if line == "# "+header {
			in = true
		} else if line == "" {
			in = false
		}
		if in && strings.HasPrefix(line, "# ") {
			lines[i] = strings.TrimPrefix(line, "# ")
		}
	}
	return strings.Join(lines, "\n")
}

// The commented [secrets], [endpoint], [tunnel] and [targets.*] examples must
// be valid once uncommented, for generated templates and the fallback alike.
func TestStarterCommentedExamplesAreValid(t *testing.T) {
	for _, tc := range initCases {
		t.Run(tc.name, func(t *testing.T) {
			repo := tempRepo(t, "ex")
			writeFiles(t, repo, tc.files)
			writeFiles(t, repo, map[string]string{".env.prod": "A=1\n"})
			_, content, err := renderStarter(repo)
			if err != nil {
				t.Fatal(err)
			}
			// The repo has .env.prod, so [secrets] is already active.
			for _, header := range []string{"[endpoint]", "[tunnel]", "[targets.prod]"} {
				content = uncomment(content, header)
			}
			content = strings.Replace(content, "# [targets.prod.env]\n# LOG_LEVEL", "[targets.prod.env]\nLOG_LEVEL", 1)
			writeFiles(t, repo, map[string]string{config.FileName: content})
			project, err := config.Load(repo)
			if err != nil {
				t.Fatalf("uncommented template invalid: %v\n%s", err, content)
			}
			for _, target := range []string{"", "prod"} {
				cfg, err := project.ResolveWithEnv(target, noEnv)
				if err != nil {
					t.Fatalf("resolve %q: %v\n%s", target, err, content)
				}
				if cfg.Endpoint == nil || cfg.Secrets == nil || cfg.Tunnel == nil {
					t.Errorf("examples missing: %+v", cfg)
				}
			}
		})
	}
}

// The generic template's commented health and container examples are valid too.
func TestFallbackCommentedHealthAndContainerAreValid(t *testing.T) {
	repo := tempRepo(t, "fb")
	_, content, err := renderStarter(repo)
	if err != nil {
		t.Fatal(err)
	}
	healthy := uncomment(content, "[health]")
	writeFiles(t, repo, map[string]string{config.FileName: strings.Replace(healthy, `command = "curl`, `# command = "curl`, 1)})
	cfg, err := config.LoadTarget(repo, "", noEnv)
	if err != nil || cfg.Health == nil {
		t.Fatalf("health example: %v", err)
	}

	container := uncomment(content, "[container]")
	container = strings.Replace(container, `command = "python -m your_app"`+"  # placeholder", "", 1)
	var kept []string
	for _, l := range strings.Split(container, "\n") {
		if strings.HasPrefix(l, "command = ") {
			continue
		}
		kept = append(kept, l)
	}
	writeFiles(t, repo, map[string]string{config.FileName: strings.Join(kept, "\n")})
	cfg, err = config.LoadTarget(repo, "", noEnv)
	if err != nil || cfg.Mode() != config.ModeBuild {
		t.Fatalf("container example: %v", err)
	}
}

func TestInitWritesNeutralHostAndNoSecretsByDefault(t *testing.T) {
	repo := tempRepo(t, "plain")
	_, content, err := renderStarter(repo)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(content, "\n[secrets]") {
		t.Errorf("[secrets] active without a .env.prod:\n%s", content)
	}
	if !strings.Contains(content, `host = "deploy@203.0.113.10"`) {
		t.Error("neutral placeholder host missing")
	}
}

func TestTOMLStringEscapes(t *testing.T) {
	cases := map[string]string{
		`plain`:     `"plain"`,
		`a"b`:       `"a\"b"`,
		`a\b`:       `"a\\b"`,
		"a\nb":      `"a\u000Ab"`,
		"tab\there": `"tab\u0009here"`,
		"unicode é": `"unicode é"`,
	}
	for in, want := range cases {
		if got := tomlString(in); got != want {
			t.Errorf("tomlString(%q) = %s, want %s", in, got, want)
		}
	}
}
