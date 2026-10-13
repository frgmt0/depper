package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/frgmt0/depper/internal/ci"
	"github.com/frgmt0/depper/internal/version"
)

func init() { register(newCICommand) }

func newCICommand(app *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ci",
		Short: "Continuous delivery helpers",
	}
	cmd.AddCommand(newCIInitCommand(app))
	return cmd
}

func newCIInitCommand(app *App) *cobra.Command {
	var (
		branch string
		force  bool
	)
	cmd := &cobra.Command{
		Use:   "init [REPO]",
		Short: "Write a GitHub Actions workflow that deploys on push (" + ci.WorkflowPath + ")",
		Long: "Write " + ci.WorkflowPath + ", a GitHub Actions workflow that installs this depper version " +
			"and runs `depper deploy` (then `depper health` when [health] is configured) on every push to the branch.\n\n" +
			"Use the global --target flag to deploy a [targets.<name>] overlay.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := app.Config(repoArg(args))
			if err != nil {
				return err
			}
			content, err := ci.Generate(ci.Options{
				Target:  cfg.Target,
				Branch:  branch,
				Version: version.Version,
				Secrets: cfg.Secrets != nil,
				Health:  cfg.Health != nil,
			})
			if err != nil {
				return err
			}
			path := filepath.Join(cfg.Repo, filepath.FromSlash(ci.WorkflowPath))
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return fmt.Errorf("create workflow directory: %w", err)
			}
			flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL
			if force {
				flags = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
			}
			f, err := os.OpenFile(path, flags, 0o644)
			if errors.Is(err, os.ErrExist) {
				return fmt.Errorf("%s already exists; pass --force to overwrite it", path)
			}
			if err != nil {
				return fmt.Errorf("write workflow: %w", err)
			}
			if _, err := f.WriteString(content); err != nil {
				_ = f.Close()
				return fmt.Errorf("write workflow: %w", err)
			}
			if err := f.Close(); err != nil {
				return fmt.Errorf("write workflow: %w", err)
			}
			app.Printf("Created %s\n\n", path)
			for _, line := range ci.SecretsHelp(cfg.Deploy.Host, cfg.Deploy.Port, cfg.Secrets != nil) {
				app.Printf("%s\n", line)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&branch, "branch", "main", "branch whose pushes trigger a deployment")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite an existing workflow")
	return cmd
}
