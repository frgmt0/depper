package cli

import (
	"github.com/spf13/cobra"
)

func init() { register(newRollbackCommand) }

func newRollbackCommand(app *App) *cobra.Command {
	var to string
	cmd := &cobra.Command{
		Use:   "rollback [REPO]",
		Short: "Switch back to the previous release (or --to a kept release)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			eng, _, err := app.engineFor(args, "rollback", false)
			if err != nil {
				return err
			}
			result, err := eng.Rollback(cmd.Context(), to)
			if err != nil {
				return err
			}
			if app.JSON {
				return app.WriteJSON(result)
			}
			app.Printf("Rolled back %s to %s\n", result.Service, result.To)
			app.Printf("  health:   %s\n", result.Health.Summary())
			return nil
		},
	}
	cmd.Flags().StringVar(&to, "to", "", "release `ID` to switch to (default: the previous release)")
	return cmd
}
