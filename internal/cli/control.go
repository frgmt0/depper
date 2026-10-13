package cli

import "github.com/spf13/cobra"

func init() {
	register(func(app *App) *cobra.Command {
		return newControlCommand(app, "restart", "Restart the service", "Restarted")
	})
	register(func(app *App) *cobra.Command { return newControlCommand(app, "stop", "Stop the service", "Stopped") })
}

func newControlCommand(app *App, action, short, done string) *cobra.Command {
	return &cobra.Command{
		Use:   action + " [REPO]",
		Short: short,
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			eng, cfg, err := app.engineFor(args, action, false)
			if err != nil {
				return err
			}
			if err := eng.Control(cmd.Context(), action); err != nil {
				return err
			}
			if app.JSON {
				return app.WriteJSON(map[string]string{"service": cfg.Service.Name, "unit": cfg.UnitName(), "action": action})
			}
			app.Printf("%s %s\n", done, cfg.UnitName())
			return nil
		},
	}
}
