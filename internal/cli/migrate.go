package cli

import "github.com/spf13/cobra"

func init() { register(newMigrateCommand) }

func newMigrateCommand(app *App) *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "migrate [REPO]",
		Short: "Convert a deployment made by the Python deployer to the depper layout",
		Long: "Convert a deployment made by the Python deployer to the depper layout.\n\n" +
			"With --dry-run the host is inspected read-only and the plan is printed.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// The inspection is read-only, so it runs even with --dry-run.
			eng, _, err := app.engineFor(args, "migrate", false)
			if err != nil {
				return err
			}
			result, err := eng.Migrate(cmd.Context(), dryRun)
			if result != nil && app.JSON {
				if werr := app.WriteJSON(result); werr != nil {
					return werr
				}
			}
			if err != nil {
				return err
			}
			if app.JSON {
				return nil
			}
			switch result.Status {
			case "already-migrated":
				app.Printf("%s already uses the depper layout (%s); nothing to migrate.\n", result.Service, result.Base)
				return nil
			case "planned":
				app.Printf("DRY RUN: migrate %s from %s to %s:\n", result.Service, result.LegacyBase, result.Base)
			default:
				app.Printf("Migrated %s to %s:\n", result.Service, result.Base)
			}
			for _, step := range result.Plan {
				app.Printf("  - %s\n", step)
			}
			if result.Health != nil {
				app.Printf("  health: %s\n", result.Health.Summary())
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "inspect the host read-only and print the plan")
	return cmd
}
