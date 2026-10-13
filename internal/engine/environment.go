package engine

import (
	"context"
	"io"
	"time"

	"github.com/frgmt0/depper/internal/config"
	"github.com/frgmt0/depper/internal/dotenv"
	"github.com/frgmt0/depper/internal/unit"
)

// EnvSource selects the dotenv content for an operation: File (--env, a
// path relative to the current directory) or Stdin (--env-stdin); neither
// means the configured [secrets].file.
type EnvSource struct {
	File  string
	Stdin io.Reader
}

// LoadEnvironment reads the merged environment ([env] overlaid by the
// dotenv source) without contacting the host, and validates it for the
// runtime mode (see CheckContainerEnv).
func LoadEnvironment(cfg *config.Config, src EnvSource) (*dotenv.Environment, error) {
	configured := ""
	if cfg.Secrets != nil {
		configured = cfg.Secrets.File
	}
	env, err := dotenv.LoadEnvironment(cfg.Repo, cfg.Env, configured, src.File, src.Stdin)
	if err != nil {
		return nil, err
	}
	if cfg.Container != nil {
		if err := CheckContainerEnv(env.Values); err != nil {
			return nil, err
		}
	}
	return env, nil
}

// EnvArtifacts are the files written together whenever the environment
// changes: the systemd EnvironmentFile and its manifest.
type EnvArtifacts struct {
	Environment []byte
	Manifest    *Manifest
}

// BuildEnvArtifacts renders the environment files for values. In build and
// image mode the environment file also carries ContainerEnvArgsVar (not a
// user variable: it is not fingerprinted or listed).
func BuildEnvArtifacts(cfg *config.Config, salt []byte, values map[string]string, now time.Time) (*EnvArtifacts, error) {
	fileValues := values
	if mode := cfg.Mode(); mode == config.ModeBuild || mode == config.ModeImage {
		if err := CheckContainerEnv(values); err != nil {
			return nil, err
		}
		fileValues = make(map[string]string, len(values)+1)
		for k, v := range values {
			fileValues[k] = v
		}
		fileValues[ContainerEnvArgsVar] = ContainerEnvArgs(values)
	}
	body := []byte(unit.EnvironmentFile(fileValues))
	return &EnvArtifacts{Environment: body, Manifest: NewManifest(salt, body, values, now)}, nil
}

// envCandidates are the artifacts as <file>.next-<id> candidates next to the
// live files (all mode 600).
func envCandidates(l *Layout, a *EnvArtifacts, id string) []UploadFile {
	return []UploadFile{
		{Contents: a.Environment, Destination: l.Environment() + ".next-" + id, Mode: "600"},
		{Contents: a.Manifest.Encode(), Destination: l.Manifest() + ".next-" + id, Mode: "600"},
	}
}

// prepareEnvironment reads the secrets state, creates the salt if needed,
// and returns the artifacts plus the names-only change summary. Call it
// while holding the lock.
func (e *Engine) prepareEnvironment(ctx context.Context, l *Layout, values map[string]string) (*EnvArtifacts, KeyChanges, error) {
	state, err := e.ReadSecretsState(ctx, l)
	if err != nil {
		return nil, KeyChanges{}, err
	}
	salt, err := e.EnsureSalt(ctx, l, state)
	if err != nil {
		return nil, KeyChanges{}, err
	}
	a, err := BuildEnvArtifacts(e.Config, salt, values, e.now())
	if err != nil {
		return nil, KeyChanges{}, err
	}
	var previous map[string]string
	if state.Manifest != nil {
		previous = state.Manifest.Keys
	}
	return a, CompareFingerprints(previous, a.Manifest.Keys), nil
}
