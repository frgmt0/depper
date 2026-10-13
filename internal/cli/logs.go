package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func init() { register(newLogsCommand) }

func newLogsCommand(app *App) *cobra.Command {
	var (
		lines  int
		follow bool
	)
	cmd := &cobra.Command{
		Use:   "logs [REPO]",
		Short: "Show the service's journal",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if lines < 1 {
				return fmt.Errorf("--lines must be a positive integer")
			}
			eng, _, err := app.engineFor(args, "logs", false)
			if err != nil {
				return err
			}
			return eng.Logs(cmd.Context(), lines, follow, app.Human())
		},
	}
	cmd.Flags().IntVar(&lines, "lines", 100, "number of journal lines to show")
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "keep printing new journal entries")
	return cmd
}
