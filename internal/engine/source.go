package engine

import (
	"archive/tar"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/frgmt0/depper/internal/config"
	"github.com/frgmt0/depper/internal/shell"
	"github.com/frgmt0/depper/internal/transport"
	"github.com/frgmt0/depper/internal/version"
)

// ReleaseMetadata is <release>/.depper-release.json. It holds no secrets.
type ReleaseMetadata struct {
	ID        string `json:"id"`
	CreatedAt string `json:"created_at"`
	// Source is "worktree" or "git".
	Source string `json:"source"`
	// GitCommit is the full commit hash ("" when not a git repository).
	GitCommit string `json:"git_commit"`
	// GitRef is the deployed ref (git source) or the checked-out branch
	// (worktree source); "" when unknown.
	GitRef string `json:"git_ref"`
	// GitDirty reports uncommitted changes that were shipped (worktree
	// source in a git repository only; null otherwise).
	GitDirty      *bool  `json:"git_dirty"`
	DepperVersion string `json:"depper_version"`
	DeployedBy    string `json:"deployed_by"`
}

// ShortCommit returns the first 12 characters of the commit.
func (m *ReleaseMetadata) ShortCommit() string {
	if m == nil {
		return ""
	}
	if len(m.GitCommit) > 12 {
		return m.GitCommit[:12]
	}
	return m.GitCommit
}

func parseReleaseMetadata(raw string) *ReleaseMetadata {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var m ReleaseMetadata
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil
	}
	for _, s := range []*string{&m.ID, &m.CreatedAt, &m.Source, &m.GitCommit, &m.GitRef, &m.DepperVersion, &m.DeployedBy} {
		*s = sanitizeLabel(*s)
	}
	return &m
}

// preparedSource is the local directory a release is synced from.
type preparedSource struct {
	Dir   string
	Meta  ReleaseMetadata
	Notes []string
	// cleanup removes temporary files (git source).
	cleanup func()
}

func (p *preparedSource) Cleanup() {
	if p != nil && p.cleanup != nil {
		p.cleanup()
	}
}

// git runs git in the repository and returns trimmed stdout.
func (e *Engine) git(ctx context.Context, args ...string) (string, error) {
	res, err := e.Local.Run(ctx, transport.Cmd{Args: append([]string{"git", "-C", e.Config.Repo}, args...), Capture: true})
	return strings.TrimSpace(res.Stdout), err
}

// validRef rejects refs that git could read as options or that contain
// control characters or whitespace.
func validRef(ref string) bool {
	return ref != "" && !strings.HasPrefix(ref, "-") && !shell.HasControl(ref) && !strings.ContainsAny(ref, " \t")
}

// ResolveCommit returns the full commit hash of ref in the repository.
func (e *Engine) ResolveCommit(ctx context.Context, ref string) (string, error) {
	if !validRef(ref) {
		return "", fmt.Errorf("invalid git ref %q", ref)
	}
	commit, err := e.git(ctx, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	if err != nil || commit == "" {
		var nf *transport.NotFoundError
		if errors.As(err, &nf) {
			return "", fmt.Errorf("git is required to resolve %q: %w", ref, err)
		}
		return "", fmt.Errorf("git ref %q does not name a commit in %s", ref, e.Config.Repo)
	}
	return commit, nil
}

func (e *Engine) gitDirty(ctx context.Context) (bool, error) {
	out, err := e.git(ctx, "status", "--porcelain")
	return out != "", err
}

// prepareSource selects what to ship: the working tree, or `git archive` of
// a ref (source = "git" or --ref) extracted into a temporary directory.
// With planOnly, nothing is extracted.
func (e *Engine) prepareSource(ctx context.Context, ref string, planOnly bool) (*preparedSource, error) {
	useGit := ref != "" || e.Config.Deploy.Source == config.SourceGit
	if !useGit {
		p := &preparedSource{Dir: e.Config.Repo, Meta: ReleaseMetadata{Source: config.SourceWorktree}}
		commit, err := e.git(ctx, "rev-parse", "--verify", "--quiet", "HEAD")
		if err != nil || commit == "" {
			return p, nil // not a git repository (or no commits): ship the directory as is
		}
		p.Meta.GitCommit = commit
		if branch, err := e.git(ctx, "rev-parse", "--abbrev-ref", "HEAD"); err == nil {
			p.Meta.GitRef = sanitizeLabel(branch)
		}
		if dirty, err := e.gitDirty(ctx); err == nil {
			p.Meta.GitDirty = &dirty
		}
		return p, nil
	}
	if ref == "" {
		ref = "HEAD"
	}
	commit, err := e.ResolveCommit(ctx, ref)
	if err != nil {
		return nil, err
	}
	p := &preparedSource{Meta: ReleaseMetadata{Source: config.SourceGit, GitCommit: commit, GitRef: ref}}
	if dirty, err := e.gitDirty(ctx); err == nil && dirty {
		p.Notes = append(p.Notes, fmt.Sprintf("the working tree has uncommitted changes; only committed content of %s (%s) is deployed", ref, p.Meta.ShortCommit()))
	}
	if planOnly {
		return p, nil
	}
	prefix, err := e.git(ctx, "rev-parse", "--show-prefix")
	if err != nil {
		return nil, fmt.Errorf("git rev-parse --show-prefix failed: %w", err)
	}
	treeish := commit
	if prefix = strings.TrimSuffix(prefix, "/"); prefix != "" {
		treeish += ":" + prefix
	}
	tmp, err := os.MkdirTemp("", "depper-source-")
	if err != nil {
		return nil, fmt.Errorf("creating a temporary directory: %w", err)
	}
	p.cleanup = func() { _ = os.RemoveAll(tmp) }
	archive := filepath.Join(tmp, "source.tar")
	if _, err := e.Local.Run(ctx, transport.Cmd{Args: []string{"git", "-C", e.Config.Repo, "archive", "--format=tar", "--output=" + archive, treeish}, Capture: true}); err != nil {
		p.Cleanup()
		return nil, fmt.Errorf("git archive %s failed: %w", ref, err)
	}
	tree := filepath.Join(tmp, "tree")
	if err := extractTar(archive, tree); err != nil {
		p.Cleanup()
		return nil, fmt.Errorf("extracting git archive: %w", err)
	}
	p.Dir = tree
	return p, nil
}

// extractTar extracts a git archive into dir, refusing entries that would
// escape it.
func extractTar(archive, dir string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tr := tar.NewReader(f)
	links := map[string]bool{}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := path.Clean(hdr.Name)
		if name == "." {
			continue
		}
		if path.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") {
			return fmt.Errorf("unsafe path %q in archive", hdr.Name)
		}
		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			if links[parent] {
				return fmt.Errorf("unsafe path %q beneath a symlink in archive", hdr.Name)
			}
		}
		target := filepath.Join(dir, filepath.FromSlash(name))
		mode := os.FileMode(hdr.Mode & 0o777)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode|0o200)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
			links[name] = true
		case tar.TypeXGlobalHeader, tar.TypeXHeader:
		default:
			return fmt.Errorf("unsupported entry type for %q in archive", hdr.Name)
		}
	}
}

// finishMetadata completes the release metadata.
func (e *Engine) finishMetadata(meta ReleaseMetadata, id string) ReleaseMetadata {
	meta.ID = id
	meta.CreatedAt = e.now().Format(time.RFC3339)
	meta.DepperVersion = version.Version
	meta.DeployedBy = e.Holder
	return meta
}
