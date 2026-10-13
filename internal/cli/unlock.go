package cli

import "github.com/spf13/cobra"

func init() { register(newUnlockCommand) }

func newUnlockCommand(app *App) *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "unlock [REPO]",
		Short: "Remove a stale deployment lock (younger than 15 minutes needs --force)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			eng, _, err := app.engineFor(args, "unlock", false)
			if err != nil {
				return err
			}
			result, err := eng.BreakLock(cmd.Context(), force)
			if err != nil {
				return err
			}
			if app.JSON {
				return app.WriteJSON(result)
			}
			if !result.Removed {
				app.Printf("No lock is held.\n")
				return nil
			}
			if info := result.Lock.Info; info != nil {
				app.Printf("Removed the lock held by `depper %s` (%s, pid %d, since %s).\n", info.Command, info.Holder, info.PID, info.CreatedAt)
			} else {
				app.Printf("Removed the lock.\n")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "remove the lock even if it is younger than 15 minutes")
	return cmd
}
