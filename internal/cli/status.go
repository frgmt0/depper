package cli

import "github.com/spf13/cobra"

func init() { register(newStatusCommand) }

func newStatusCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "status [REPO]",
		Short: "Show the service state, current release, endpoint, and health",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			eng, _, err := app.engineFor(args, "status", false)
			if err != nil {
				return err
			}
			status, err := eng.Status(cmd.Context())
			if err != nil {
				return err
			}
			if app.JSON {
				return app.WriteJSON(status)
			}
			status.WriteHuman(app.Human())
			return nil
		},
	}
}
