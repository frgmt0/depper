// Package cli implements the depper command line.
package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/frgmt0/depper/internal/config"
	"github.com/frgmt0/depper/internal/exitcode"
	"github.com/frgmt0/depper/internal/transport"
	"github.com/frgmt0/depper/internal/version"
)

// EnvTarget selects a target when --target is not given.
const EnvTarget = "DEPPER_TARGET"

// App holds process-wide state shared by every command: I/O streams,
// environment lookup, the command executor, and the global flags.
type App struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// LookupEnv reads environment variables (os.LookupEnv by default).
	LookupEnv func(string) (string, bool)
	// Exec runs local programs (transport.Local{} by default).
	Exec transport.Executor

	target string
	// JSON is the global --json flag.
	JSON bool
}

// NewApp returns an App wired to the real process.
func NewApp() *App {
	return &App{Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr, LookupEnv: os.LookupEnv, Exec: transport.Local{}}
}

// Target returns the selected target: --target, else $DEPPER_TARGET, else "".
func (a *App) Target() string {
	if a.target != "" {
		return a.target
	}
	if value, ok := a.LookupEnv(EnvTarget); ok {
		return value
	}
	return ""
}

// Config loads repo's configuration and resolves the selected target.
func (a *App) Config(repo string) (*config.Config, error) {
	return config.LoadTarget(repo, a.Target(), a.LookupEnv)
}

// Remote returns a Remote for cfg. With dryRun, commands are printed to the
// human output stream instead of being run.
func (a *App) Remote(cfg *config.Config, dryRun bool) *transport.Remote {
	exec := a.Exec
	if dryRun {
		exec = transport.DryRun{Out: a.Human()}
	}
	r := transport.NewRemote(exec, cfg.SSHTarget())
	r.Stdout, r.Stderr = a.Human(), a.Stderr
	return r
}

// Human is where human-readable messages go: stdout normally, stderr in
// --json mode so stdout carries only JSON.
func (a *App) Human() io.Writer {
	if a.JSON {
		return a.Stderr
	}
	return a.Stdout
}

// Printf writes a human-readable message (see Human).
func (a *App) Printf(format string, args ...any) {
	_, _ = fmt.Fprintf(a.Human(), format, args...)
}

// WriteJSON writes v as indented JSON to stdout.
func (a *App) WriteJSON(v any) error {
	enc := json.NewEncoder(a.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// NewRootCommand builds the depper command tree for app.
func NewRootCommand(app *App) *cobra.Command {
	root := &cobra.Command{
		Use:           "depper",
		Short:         "Deploy a repository to a systemd user service over SSH",
		Long:          "depper deploys a repository to a systemd user service over SSH.\n\nRun without a command to deploy the current directory; `depper REPO` deploys REPO.",
		Version:       version.Version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetIn(app.Stdin)
	root.SetOut(app.Stdout)
	root.SetErr(app.Stderr)
	root.PersistentFlags().StringVar(&app.target, "target", "", "deployment target from [targets.<name>] (default $"+EnvTarget+")")
	root.PersistentFlags().BoolVar(&app.JSON, "json", false, "write machine-readable JSON to stdout")
	for _, factory := range factories {
		root.AddCommand(factory(app))
	}
	return root
}

// Run executes args against app and returns the process exit code. Errors
// are printed to stderr as "depper: <message>".
func Run(ctx context.Context, app *App, args []string) int {
	root := NewRootCommand(app)
	root.SetArgs(rewriteArgs(root, args))
	err := root.ExecuteContext(ctx)
	if err != nil {
		if msg := err.Error(); msg != "" {
			_, _ = fmt.Fprintln(app.Stderr, "depper: "+msg)
		}
	}
	return exitcode.Code(err)
}

// Main runs depper for the current process, cancelling on SIGINT/SIGTERM.
func Main() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return Run(ctx, NewApp(), os.Args[1:])
}
