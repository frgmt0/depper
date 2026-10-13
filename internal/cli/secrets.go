package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/frgmt0/depper/internal/engine"
	"github.com/frgmt0/depper/internal/exitcode"
)

func init() { register(newSecretsCommand) }

func newSecretsCommand(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "secrets",
		Short: "Inspect and update the service environment (secrets) on the host",
		Long:  "Inspect and update the service environment on the host. The local dotenv file is the source of truth; values are never printed or read back from the host.",
	}
	cmd.AddCommand(newSecretsListCommand(app), newSecretsDiffCommand(app), newSecretsPushCommand(app))
	return cmd
}

// secretsListResult is the JSON output of `secrets list`. It carries names
// and timestamps only; never values or fingerprints.
type secretsListResult struct {
	Service string `json:"service"`
	Target  string `json:"target"`
	// Known is false when the host has no environment manifest yet.
	Known        bool     `json:"known"`
	Keys         []string `json:"keys"`
	UpdatedAt    string   `json:"updated_at"`
	FileModified bool     `json:"file_modified"`
}

func newSecretsListCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "list [REPO]",
		Short: "List the variable names deployed to the host (never values)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			eng, cfg, err := app.engineFor(args, "secrets list", false)
			if err != nil {
				return err
			}
			l, err := eng.Probe(cmd.Context(), false)
			if err != nil {
				return err
			}
			result := &secretsListResult{Service: cfg.Service.Name, Target: cfg.Target, Keys: []string{}}
			if l.Deployed() {
				state, err := eng.ReadSecretsState(cmd.Context(), l)
				if err != nil {
					return err
				}
				if state.Manifest != nil {
					result.Known = true
					result.Keys = state.Manifest.KeyNames()
					result.UpdatedAt = state.Manifest.UpdatedAt
					result.FileModified = state.FileModified()
				}
			}
			if app.JSON {
				return app.WriteJSON(result)
			}
			writeSecretsList(app.Human(), result)
			return nil
		},
	}
}

func writeSecretsList(w io.Writer, r *secretsListResult) {
	if !r.Known {
		_, _ = fmt.Fprintf(w, "%s: no environment manifest on the host yet (written by the next deploy or secrets push)\n", r.Service)
		return
	}
	_, _ = fmt.Fprintf(w, "%s: %d variables, updated %s\n", r.Service, len(r.Keys), r.UpdatedAt)
	for _, k := range r.Keys {
		_, _ = fmt.Fprintf(w, "  %s\n", k)
	}
	if r.FileModified {
		_, _ = fmt.Fprintln(w, "warning: the environment file on the host was modified outside depper; run `depper secrets push` to restore it")
	}
}

// secretsDiffResult is the JSON output of `secrets diff`.
type secretsDiffResult struct {
	Service string `json:"service"`
	Target  string `json:"target"`
	*engine.EnvDiff
}

func newSecretsDiffCommand(app *App) *cobra.Command {
	var (
		envFile  string
		envStdin bool
	)
	cmd := &cobra.Command{
		Use:   "diff [REPO]",
		Short: "Compare the local environment with the host by key name (exit 3 on drift)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			eng, cfg, err := app.engineFor(args, "secrets diff", false)
			if err != nil {
				return err
			}
			env, err := engine.LoadEnvironment(cfg, envSource(app, envFile, envStdin))
			if err != nil {
				return err
			}
			l, err := eng.Probe(cmd.Context(), false)
			if err != nil {
				return err
			}
			diff, err := eng.DiffEnvironment(cmd.Context(), l, env.Values)
			if err != nil {
				return err
			}
			if app.JSON {
				if err := app.WriteJSON(&secretsDiffResult{Service: cfg.Service.Name, Target: cfg.Target, EnvDiff: diff}); err != nil {
					return err
				}
			} else {
				writeSecretsDiff(app.Human(), cfg.Service.Name, diff)
			}
			if diff.Drift() {
				return exitcode.Silent(exitcode.Drift)
			}
			return nil
		},
	}
	addEnvFlags(cmd, &envFile, &envStdin, "dotenv `FILE` to compare instead of [secrets].file", "read the dotenv content to compare from stdin")
	return cmd
}

func writeSecretsDiff(w io.Writer, service string, d *engine.EnvDiff) {
	var b strings.Builder
	b.WriteString(service + ": secrets " + d.State)
	if d.State != engine.SecretsUnknown {
		b.WriteString(" (" + d.Keys.Summary() + ")")
	}
	if d.Reason != "" {
		b.WriteString("; " + d.Reason)
	}
	b.WriteString("\n")
	if d.State != engine.SecretsUnknown {
		for _, g := range []struct {
			label string
			names []string
		}{{"added", d.Keys.Added}, {"removed", d.Keys.Removed}, {"changed", d.Keys.Changed}} {
			for _, name := range g.names {
				fmt.Fprintf(&b, "  %-8s %s\n", g.label, name)
			}
		}
	}
	_, _ = io.WriteString(w, b.String())
}

func newSecretsPushCommand(app *App) *cobra.Command {
	var (
		envFile  string
		envStdin bool
	)
	cmd := &cobra.Command{
		Use:   "push [REPO]",
		Short: "Replace only the environment on the host and restart the service",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			eng, cfg, err := app.engineFor(args, "secrets push", false)
			if err != nil {
				return err
			}
			env, err := engine.LoadEnvironment(cfg, envSource(app, envFile, envStdin))
			if err != nil {
				return err
			}
			result, err := eng.PushEnvironment(cmd.Context(), env)
			if err != nil {
				return err
			}
			if app.JSON {
				return app.WriteJSON(result)
			}
			app.Printf("Pushed the environment of %s (release %s)\n", result.Service, result.Release)
			app.Printf("  env:      %s\n", result.Env.Summary())
			if result.Health != nil {
				app.Printf("  health:   %s\n", result.Health.Summary())
			}
			return nil
		},
	}
	addEnvFlags(cmd, &envFile, &envStdin, "dotenv `FILE` to push instead of [secrets].file", "read dotenv content from stdin")
	return cmd
}

func envSource(app *App, file string, stdin bool) engine.EnvSource {
	src := engine.EnvSource{File: file}
	if stdin {
		src.Stdin = app.Stdin
	}
	return src
}

func addEnvFlags(cmd *cobra.Command, file *string, stdin *bool, fileUsage, stdinUsage string) {
	cmd.Flags().StringVar(file, "env", "", fileUsage)
	cmd.Flags().BoolVar(stdin, "env-stdin", false, stdinUsage)
}
