package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/frgmt0/depper/internal/engine"
)

func init() { register(newDeployCommand) }

func newDeployCommand(app *App) *cobra.Command {
	var (
		dryRun, envStdin bool
		envFile, ref     string
	)
	cmd := &cobra.Command{
		Use:   "deploy [REPO]",
		Short: "Deploy the repository as a new release (the default command)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			eng, _, err := app.engineFor(args, "deploy", dryRun)
			if err != nil {
				return err
			}
			opts := engine.DeployOptions{Ref: ref, DryRun: dryRun, Env: engine.EnvSource{File: envFile}}
			if envStdin {
				opts.Env.Stdin = app.Stdin
			}
			result, err := eng.Deploy(cmd.Context(), opts)
			if err != nil {
				return err
			}
			if app.JSON {
				return app.WriteJSON(result)
			}
			if !result.DryRun {
				writeDeploySummary(app.Human(), result)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "validate locally and print the plan without connecting")
	cmd.Flags().StringVar(&envFile, "env", "", "dotenv `FILE` to use for this deployment instead of [secrets].file")
	cmd.Flags().BoolVar(&envStdin, "env-stdin", false, "read dotenv content from stdin")
	cmd.Flags().StringVar(&ref, "ref", "", "deploy the committed content of git `REF` (git archive) instead of the working tree")
	return cmd
}

func writeDeploySummary(w io.Writer, r *engine.DeployResult) {
	p := func(format string, args ...any) { _, _ = fmt.Fprintf(w, format, args...) }
	p("Deployed %s to %s (%s)\n", r.Service, r.Host, r.Release)
	if m := r.Metadata; m != nil && m.GitCommit != "" {
		line := "commit " + m.ShortCommit()
		if m.GitRef != "" {
			line += " (" + m.GitRef + ")"
		}
		if m.GitDirty != nil && *m.GitDirty {
			line += " with uncommitted changes"
		}
		p("  source:   %s, %s\n", m.Source, line)
	}
	if r.EndpointURL != "" {
		p("  endpoint: %s\n", r.EndpointURL)
	}
	if r.TunnelURL != "" {
		p("  tunnel:   %s\n", r.TunnelURL)
	}
	p("  health:   %s\n", r.Health.Summary())
	p("  env:      %s\n", r.Env.Summary())
	if len(r.Pruned) > 0 {
		p("  pruned:   %s\n", strings.Join(r.Pruned, ", "))
	}
	for _, warning := range r.Warnings {
		p("  warning:  %s\n", warning)
	}
}
