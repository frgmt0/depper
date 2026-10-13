package transport

import (
	"context"
	"io"
	"strings"

	"github.com/frgmt0/depper/internal/shell"
)

// RemoteShell is the command ssh runs on the host; scripts arrive on its
// stdin. The remote requirement is POSIX sh, so scripts must avoid bashisms.
const RemoteShell = "sh -s"

// Remote runs scripts and syncs files on one deploy host.
type Remote struct {
	Exec   Executor
	Target Target
	// SSHProgram and RsyncProgram default to "ssh" and "rsync".
	SSHProgram   string
	RsyncProgram string
	// Stdout and Stderr receive streamed (non-captured) output; nil means
	// os.Stdout / os.Stderr. In --json mode point Stdout at stderr.
	Stdout io.Writer
	Stderr io.Writer
}

// NewRemote returns a Remote that runs commands through exec.
func NewRemote(exec Executor, target Target) *Remote {
	return &Remote{Exec: exec, Target: target}
}

func (r *Remote) ssh() string {
	if r.SSHProgram == "" {
		return "ssh"
	}
	return r.SSHProgram
}

func (r *Remote) rsync() string {
	if r.RsyncProgram == "" {
		return "rsync"
	}
	return r.RsyncProgram
}

// ScriptArgs returns the argv used to run a script (the script is sent on
// stdin): ssh <options> -T -- <host> 'sh -s'.
func (r *Remote) ScriptArgs() []string { return r.Target.SSHArgs(r.ssh(), RemoteShell) }

// Every script is sent through shell.Wrap, so the remote shell parses it
// completely before running any of it.

// Run executes script with `sh -s` on the host, streaming its output to
// Remote.Stdout / Remote.Stderr.
func (r *Remote) Run(ctx context.Context, script string) error {
	return r.Stream(ctx, script, nil)
}

// Stream executes script on the host, streaming stdout to w (nil means
// Remote.Stdout) and stderr to Remote.Stderr. Used for logs.
func (r *Remote) Stream(ctx context.Context, script string, w io.Writer) error {
	if w == nil {
		w = r.Stdout
	}
	_, err := r.Exec.Run(ctx, Cmd{Args: r.ScriptArgs(), Stdin: strings.NewReader(shell.Wrap(script)), Stdout: w, Stderr: r.Stderr})
	return err
}

// Capture executes script on the host and returns its stdout. On failure
// the *CommandError carries the remote stderr.
func (r *Remote) Capture(ctx context.Context, script string) (string, error) {
	res, err := r.Exec.Run(ctx, Cmd{Args: r.ScriptArgs(), Stdin: strings.NewReader(shell.Wrap(script)), Capture: true})
	return res.Stdout, err
}

// RsyncArgs returns the argv Rsync would run.
func (r *Remote) RsyncArgs(src, remoteDir string, excludes []string) ([]string, error) {
	return r.Target.RsyncArgs(r.rsync(), r.ssh(), src, remoteDir, excludes)
}

// Rsync copies the contents of local directory src into remoteDir.
func (r *Remote) Rsync(ctx context.Context, src, remoteDir string, excludes []string) error {
	args, err := r.RsyncArgs(src, remoteDir, excludes)
	if err != nil {
		return err
	}
	_, err = r.Exec.Run(ctx, Cmd{Args: args, Stdout: r.Stdout, Stderr: r.Stderr})
	return err
}
