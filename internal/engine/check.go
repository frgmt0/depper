package engine

import (
	"context"
	"io"
	"strings"
)

// Code states of `depper check` (CheckResult.State).
const (
	CodeUpToDate    = "up-to-date"
	CodeOutdated    = "outdated"
	CodeNotDeployed = "not-deployed"
	CodeUnknown     = "unknown"
)

// CheckResult is `depper check`. JSON keys are stable:
//
//	state         "up-to-date", "outdated", "not-deployed", or "unknown"
//	local_ref     the ref compared (--ref, else HEAD)
//	local_commit  its commit ("" when not a git repository)
//	deployed      current release metadata (null when none)
//	secrets       secrets drift (see EnvDiff)
//	unit          systemd unit state
//	drift         true when the exit status is 3 (outdated, not deployed,
//	              or secrets drift)
type CheckResult struct {
	Service     string           `json:"service"`
	Target      string           `json:"target"`
	State       string           `json:"state"`
	Reason      string           `json:"reason"`
	LocalRef    string           `json:"local_ref"`
	LocalCommit string           `json:"local_commit"`
	Deployed    *ReleaseMetadata `json:"deployed"`
	Secrets     *EnvDiff         `json:"secrets"`
	Unit        *UnitState       `json:"unit"`
	Drift       bool             `json:"drift"`
}

// Check compares the deployed release with the local repository and the
// local environment with the host's manifest.
func (e *Engine) Check(ctx context.Context, ref string, env EnvSource) (*CheckResult, error) {
	local, err := LoadEnvironment(e.Config, env)
	if err != nil {
		return nil, err
	}
	if ref == "" {
		ref = "HEAD"
	}
	result := &CheckResult{Service: e.Config.Service.Name, Target: e.Config.Target, LocalRef: ref}
	commit, gitErr := e.ResolveCommit(ctx, ref)
	if gitErr == nil {
		result.LocalCommit = commit
	} else if ref != "HEAD" {
		return nil, gitErr
	}
	l, err := e.Probe(ctx, false)
	if err != nil {
		return nil, err
	}
	out, err := e.Remote.Capture(ctx, e.statusScript(l))
	if err != nil {
		return nil, err
	}
	m := sectionMap(out)
	result.Unit = parseUnitState(m["unit"])
	current := releaseFromLink(m["current"])
	if current != "" {
		result.Deployed = parseReleaseMetadata(m["release"])
		if result.Deployed == nil {
			result.Deployed = &ReleaseMetadata{ID: current}
		}
	}
	switch {
	case !l.Deployed() || current == "":
		result.State, result.Reason = CodeNotDeployed, "no release is deployed"
	case result.LocalCommit == "":
		result.State, result.Reason = CodeUnknown, "the repository is not a git repository (or has no commits)"
	case result.Deployed.GitCommit == "":
		result.State, result.Reason = CodeUnknown, "the deployed release has no recorded commit"
	case result.Deployed.GitCommit == result.LocalCommit:
		result.State = CodeUpToDate
		if result.Deployed.GitDirty != nil && *result.Deployed.GitDirty {
			result.Reason = "the deployed release also included uncommitted changes"
		}
	default:
		result.State, result.Reason = CodeOutdated, "deployed "+result.Deployed.ShortCommit()+", local "+ref+" is "+shortHash(result.LocalCommit)
	}
	diff, err := e.DiffEnvironment(ctx, l, local.Values)
	if err != nil {
		return nil, err
	}
	result.Secrets = diff
	result.Drift = result.State == CodeOutdated || result.State == CodeNotDeployed || diff.Drift()
	return result, nil
}

func shortHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

// WriteHuman prints the check result.
func (r *CheckResult) WriteHuman(w io.Writer) {
	var b strings.Builder
	b.WriteString(r.Service + ": " + r.State)
	if r.Reason != "" {
		b.WriteString(" (" + r.Reason + ")")
	}
	b.WriteString("\n")
	if r.Secrets != nil {
		b.WriteString("  secrets: " + r.Secrets.State)
		if r.Secrets.State != SecretsUnknown {
			b.WriteString(" (" + r.Secrets.Keys.Summary() + ")")
		}
		if r.Secrets.Reason != "" {
			b.WriteString("; " + r.Secrets.Reason)
		}
		b.WriteString("\n")
	}
	if r.Unit != nil {
		b.WriteString("  unit:    " + r.Unit.describe() + "\n")
	}
	_, _ = io.WriteString(w, b.String())
}
