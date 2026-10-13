package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/frgmt0/depper/internal/config"
	"github.com/frgmt0/depper/internal/dotenv"
	"github.com/frgmt0/depper/internal/shell"
)

// DeployOptions are the per-invocation deploy settings.
type DeployOptions struct {
	// Env selects the dotenv source (--env / --env-stdin).
	Env EnvSource
	// Ref deploys `git archive REF` instead of the working tree.
	Ref string
	// DryRun validates locally and prints the plan without connecting.
	DryRun bool
}

// DeployResult summarizes a deploy. JSON keys are stable.
type DeployResult struct {
	Service string `json:"service"`
	Target  string `json:"target"`
	Host    string `json:"host"`
	Base    string `json:"base"`
	Mode    string `json:"mode"`
	Release string `json:"release"`
	// Metadata is the release's .depper-release.json.
	Metadata    *ReleaseMetadata `json:"metadata"`
	EndpointURL string           `json:"endpoint_url"`
	TunnelURL   string           `json:"tunnel_url"`
	Health      *HealthResult    `json:"health"`
	// Env is the names-only change summary of the environment.
	Env      KeyChanges `json:"env"`
	Pruned   []string   `json:"pruned"`
	Notes    []string   `json:"notes"`
	Warnings []string   `json:"warnings"`
	DryRun   bool       `json:"dry_run"`
}

func (e *Engine) newDeployResult() *DeployResult {
	r := &DeployResult{
		Service:  e.Config.Service.Name,
		Target:   e.Config.Target,
		Host:     e.Config.Deploy.Host,
		Mode:     e.Config.Mode(),
		Pruned:   []string{},
		Notes:    []string{},
		Warnings: []string{},
		Env:      KeyChanges{Added: []string{}, Removed: []string{}, Changed: []string{}, Unchanged: []string{}},
	}
	if e.Config.Endpoint != nil {
		r.EndpointURL = e.Config.Endpoint.URL
	}
	if e.Config.Tunnel != nil {
		r.TunnelURL = e.Config.Tunnel.URL()
	}
	return r
}

// Deploy runs the deploy transaction: sync a new release, write the
// environment (and its manifest), start script and unit as candidates, run
// setup, activate, and wait for health. Any failure after activation began
// restores the previous environment, unit, and release automatically.
func (e *Engine) Deploy(ctx context.Context, opts DeployOptions) (*DeployResult, error) {
	cfg := e.Config
	env, err := LoadEnvironment(cfg, opts.Env)
	if err != nil {
		return nil, err
	}
	if cfg.Tunnel != nil && RegisteredTunnel() == nil {
		return nil, errNoTunnelSupport
	}
	if cfg.Container != nil && cfg.Container.Command != "" {
		if _, err := SplitWords(cfg.Container.Command); err != nil {
			return nil, fmt.Errorf("[container].command: %w", err)
		}
	}
	src, err := e.prepareSource(ctx, opts.Ref, opts.DryRun)
	if err != nil {
		return nil, err
	}
	defer src.Cleanup()
	result := e.newDeployResult()
	result.Notes = append(result.Notes, src.Notes...)
	if opts.DryRun {
		result.DryRun = true
		meta := src.Meta
		result.Metadata = &meta
		e.printPlan(src, env)
		return result, nil
	}
	l, err := e.Probe(ctx, true)
	if err != nil {
		return nil, err
	}
	result.Base = l.Base
	if err := e.requireEngine(l); err != nil {
		return nil, err
	}
	if err := e.assertTunnelNotOrphaned(ctx, l); err != nil {
		return nil, err
	}
	rid, err := e.newReleaseID()
	if err != nil {
		return nil, err
	}
	meta := e.finishMetadata(src.Meta, rid)
	result.Release, result.Metadata = rid, &meta
	for _, note := range src.Notes {
		e.printf("Note: %s\n", note)
	}
	e.printf("Deploying %s to %s (%s)\n", cfg.Service.Name, cfg.Deploy.Host, rid)
	err = e.WithLock(ctx, l, func() error { return e.deployLocked(ctx, l, rid, env, src, &meta, result) })
	return result, err
}

func (e *Engine) printPlan(src *preparedSource, env *dotenv.Environment) {
	cfg := e.Config
	target := cfg.Deploy.Path
	if target == "" {
		target = "~/" + cfg.DefaultBaseRel()
	}
	health := "none"
	if h := cfg.Health; h != nil {
		if h.URL != "" {
			health = "url " + h.URL
		} else {
			health = "command"
		}
	}
	e.printf("DRY RUN: deploy %s to %s:%s\n", cfg.Service.Name, cfg.Deploy.Host, target)
	e.printf("  mode=%s; restart=%s; setup steps=%d; health=%s; keep_releases=%d\n", cfg.Mode(), cfg.Service.Restart, len(cfg.Deploy.Setup), health, cfg.Deploy.KeepReleases)
	switch {
	case src.Meta.Source == config.SourceGit:
		e.printf("  source=git; ref=%s; commit=%s\n", src.Meta.GitRef, src.Meta.GitCommit)
	case src.Meta.GitCommit != "":
		dirty := ""
		if src.Meta.GitDirty != nil && *src.Meta.GitDirty {
			dirty = " (uncommitted changes included)"
		}
		e.printf("  source=worktree; commit=%s%s\n", src.Meta.GitCommit, dirty)
	default:
		e.printf("  source=worktree (not a git repository)\n")
	}
	e.printf("  excludes=%s\n", strings.Join(append(append([]string{}, cfg.Deploy.Exclude...), env.RsyncExcludes...), ", "))
	switch {
	case env.FromStdin:
		e.printf("  dotenv=stdin; variables=%d\n", env.FileKeys)
	case env.Source != nil:
		e.printf("  dotenv=%s; variables=%d\n", env.Source.Selected, env.FileKeys)
	}
	if cfg.Tunnel != nil {
		e.printf("  tunnel=%s\n", cfg.Tunnel.URL())
	}
	for _, note := range src.Notes {
		e.printf("  note: %s\n", note)
	}
}

// candidateNames are the per-release candidate files in <base>/.depper.
var candidateNames = []string{"environment", "environment.manifest", "container.env", "unit"}

func (e *Engine) cleanupCandidates(ctx context.Context, l *Layout, id string) error {
	words := []string{"rm", "-f"}
	for _, name := range candidateNames {
		words = append(words, shell.Quote(l.Meta()+"/"+name+".next-"+id))
	}
	return e.Remote.Run(ctx, shell.Script("set +xv", "set -eu", strings.Join(words, " ")))
}

func (e *Engine) deployLocked(ctx context.Context, l *Layout, rid string, env *dotenv.Environment, src *preparedSource, meta *ReleaseMetadata, result *DeployResult) (err error) {
	cfg := e.Config
	defer func() {
		if cerr := e.cleanupCandidates(withoutCancel(ctx), l, rid); cerr != nil && err == nil {
			result.Warnings = append(result.Warnings, "removing deployment candidates failed: "+cerr.Error())
		}
	}()
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	unitFile, err := RenderUnit(cfg, l, rid)
	if err != nil {
		return err
	}
	artifacts, changes, err := e.prepareEnvironment(ctx, l, env.Values)
	if err != nil {
		return err
	}
	result.Env = changes
	release := l.Release(rid)
	if err := e.Remote.Run(ctx, shell.Script(
		"set +xv", "set -eu", "umask 077",
		"mkdir -p "+shell.Quote(release)+" "+shell.Quote(l.Transaction(rid)),
		"chmod 700 "+shell.Quote(l.Transaction(rid)),
	)); err != nil {
		return err
	}
	excludes := append(append([]string{}, cfg.Deploy.Exclude...), env.RsyncExcludes...)
	if err := e.Remote.Rsync(ctx, src.Dir, release, excludes); err != nil {
		return e.discardFailed(ctx, l, rid, false, err)
	}
	files := []UploadFile{{Contents: append(metaJSON, '\n'), Destination: release + "/" + ReleaseMetadataName, Mode: "644"}}
	files = append(files, envCandidates(l, artifacts, rid)...)
	if cfg.Container == nil {
		files = append(files, UploadFile{Contents: []byte(StartScript(cfg.Service.Command)), Destination: release + "/" + StartScriptName, Mode: "700"})
	}
	files = append(files, UploadFile{Contents: []byte(unitFile), Destination: l.Meta() + "/unit.next-" + rid, Mode: "600"})
	if err := e.UploadFiles(ctx, files...); err != nil {
		return e.discardFailed(ctx, l, rid, false, err)
	}
	activated := false
	health, err := func() (*HealthResult, error) {
		if err := e.runSetup(ctx, l, release, rid); err != nil {
			return nil, err
		}
		activated = true
		if err := e.Remote.Run(ctx, e.activateScript(l, rid)); err != nil {
			return nil, err
		}
		return e.WaitHealthy(ctx, l)
	}()
	result.Health = health
	if err != nil {
		return e.discardFailed(ctx, l, rid, activated, err)
	}
	// The commit script also lists the releases for pruning (one round trip).
	listing, err := e.Remote.Capture(ctx, e.commitScript(l, rid)+e.listScript(l))
	if err != nil {
		return fmt.Errorf("Application deployed successfully, but recording the rollback state failed: %w", err)
	}
	pruned, warnings := e.pruneList(ctx, l, parseReleaseList(listing))
	result.Pruned = append(result.Pruned, pruned...)
	result.Warnings = append(result.Warnings, warnings...)
	if cfg.Tunnel != nil {
		h := RegisteredTunnel()
		if h == nil {
			return errNoTunnelSupport
		}
		if err := h.Ensure(ctx, e, l); err != nil {
			return fmt.Errorf("Application deployed successfully, but tunnel setup failed: %w", err)
		}
	}
	return nil
}

// runSetup runs [deploy].setup in the release with only [env] exported,
// then the container setup (build or pull).
func (e *Engine) runSetup(ctx context.Context, l *Layout, release, rid string) error {
	cfg := e.Config
	base := append([]string{"set +xv"}, shell.Prelude()...)
	for _, command := range cfg.Deploy.Setup {
		lines := append(append([]string{}, base...), shell.Exports(cfg.Env)...)
		lines = append(lines, "cd "+shell.Quote(release), "/bin/sh -lc "+shell.Quote(command))
		if err := e.Remote.Run(ctx, shell.Script(lines...)); err != nil {
			return fmt.Errorf("setup command failed: %w", err)
		}
	}
	c := cfg.Container
	if c == nil {
		return nil
	}
	engine := shell.Quote(l.EnginePath)
	lines := append(append([]string{}, base...), "cd "+shell.Quote(release))
	switch c.Mode() {
	case config.ModeBuild:
		build := engine + " build -t " + shell.Quote(cfg.ImageRef(rid))
		if c.Dockerfile != "" {
			build += " -f " + shell.Quote(c.Dockerfile)
		}
		lines = append(lines, build+" "+shell.Quote(c.Build))
	case config.ModeImage:
		lines = append(lines, engine+" pull "+shell.Quote(c.Image))
	case config.ModeCompose:
		compose := engine + " compose -p " + shell.Quote(ComposeProject(cfg)) + " -f " + shell.Quote(c.Compose) + " --env-file " + shell.Quote(l.ContainerEnv()+".next-"+rid)
		lines = append(lines, compose+" config --quiet", compose+" build")
	}
	if err := e.Remote.Run(ctx, shell.Script(lines...)); err != nil {
		return fmt.Errorf("container setup failed: %w", err)
	}
	return nil
}

// activateScript backs up the live configuration into transactions/<rid>,
// installs the candidates, switches current, and restarts the unit.
func (e *Engine) activateScript(l *Layout, rid string) string {
	unitName := shell.Quote(e.Config.UnitName())
	candidate := func(name string) string { return `"$meta/"` + shell.Quote(name+".next-"+rid) }
	lines := e.txLines(l, rid)
	lines = append(lines, backupLines()...)
	lines = append(lines,
		"mv -f "+candidate("environment")+` "$meta/environment"`,
		"mv -f "+candidate("environment.manifest")+` "$meta/environment.manifest"`,
	)
	if e.Config.Container != nil {
		lines = append(lines, "mv -f "+candidate("container.env")+` "$meta/container.env"`)
	} else {
		lines = append(lines, `rm -f "$meta/container.env"`)
	}
	lines = append(lines, "mv -f "+candidate("unit")+` "$unit"`)
	lines = append(lines, setLinkLines("current", "releases/"+rid, false)...)
	lines = append(lines,
		"systemctl --user daemon-reload",
		"systemctl --user enable "+unitName+" >/dev/null",
		"systemctl --user restart "+unitName,
	)
	return shell.Script(lines...)
}

// commitScript records the previous release and the applied configuration
// after a healthy deploy.
func (e *Engine) commitScript(l *Layout, rid string) string {
	lines := e.txLines(l, rid)
	lines = append(lines, `if test -f "$tx/current"; then`)
	lines = append(lines, setLinkLines("previous", `"$(cat "$tx/current")"`, true)...)
	lines = append(lines,
		"  printf '%s\\n' "+shell.Quote("transactions/"+rid)+` > "$meta/previous-transaction.next"`,
		`  mv -f "$meta/previous-transaction.next" "$meta/previous-transaction"`,
		"fi",
	)
	lines = append(lines, snapshotLines(rid)...)
	return shell.Script(lines...)
}

// discardFailed restores the previous state (when activation began) and
// removes the failed release, its transaction backup, and its image.
func (e *Engine) discardFailed(ctx context.Context, l *Layout, rid string, activated bool, cause error) error {
	cctx := withoutCancel(ctx)
	if activated {
		if rerr := e.Remote.Run(cctx, e.restoreScript(l, rid)); rerr != nil {
			return fmt.Errorf("deployment failed: %v; automatic restore also failed: %v (backup kept in %s)", cause, rerr, l.Transaction(rid))
		}
		e.printf("Deployment failed; the previous release, environment, and unit were restored.\n")
	}
	lines := []string{"set +xv"}
	lines = append(lines, shell.Prelude()...)
	lines = append(lines, "rm -rf "+shell.Quote(l.Release(rid))+" "+shell.Quote(l.Transaction(rid)))
	if e.Config.Mode() == config.ModeBuild && l.EnginePath != "" {
		lines = append(lines, shell.Quote(l.EnginePath)+" rmi "+shell.Quote(e.Config.ImageRef(rid))+" >/dev/null 2>&1 || true")
	}
	if derr := e.Remote.Run(cctx, shell.Script(lines...)); derr != nil {
		e.printf("Warning: removing the failed release %s failed: %v\n", rid, derr)
	}
	if activated {
		return fmt.Errorf("deployment failed and was rolled back: %w", cause)
	}
	return cause
}
