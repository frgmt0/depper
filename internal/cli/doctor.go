package cli

import (
	"github.com/spf13/cobra"

	"github.com/frgmt0/depper/internal/doctor"
	"github.com/frgmt0/depper/internal/exitcode"
)

func init() { register(newDoctorCommand) }

func newDoctorCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor [REPO]",
		Short: "Check that the deploy host is ready (exit 1 if any check fails)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := app.Config(repoArg(args))
			if err != nil {
				return err
			}
			d := &doctor.Doctor{Config: cfg, Remote: app.Remote(cfg, false)}
			report := d.Run(cmd.Context())
			if app.JSON {
				if err := app.WriteJSON(report); err != nil {
					return err
				}
			} else {
				report.WriteHuman(app.Human())
			}
			if report.Failed() {
				return exitcode.Silent(exitcode.Failure)
			}
			return nil
		},
	}
}
