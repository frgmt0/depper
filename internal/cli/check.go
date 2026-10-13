package cli

import (
	"github.com/spf13/cobra"

	"github.com/frgmt0/depper/internal/engine"
	"github.com/frgmt0/depper/internal/exitcode"
)

func init() { register(newCheckCommand) }

func newCheckCommand(app *App) *cobra.Command {
	var (
		ref, envFile string
		envStdin     bool
	)
	cmd := &cobra.Command{
		Use:   "check [REPO]",
		Short: "Compare the deployed release and secrets with the local repository (exit 3 on drift)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			eng, _, err := app.engineFor(args, "check", false)
			if err != nil {
				return err
			}
			src := engine.EnvSource{File: envFile}
			if envStdin {
				src.Stdin = app.Stdin
			}
			result, err := eng.Check(cmd.Context(), ref, src)
			if err != nil {
				return err
			}
			if app.JSON {
				if err := app.WriteJSON(result); err != nil {
					return err
				}
			} else {
				result.WriteHuman(app.Human())
			}
			if result.Drift {
				return exitcode.Silent(exitcode.Drift)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&ref, "ref", "", "local git `REF` to compare with (default HEAD)")
	cmd.Flags().StringVar(&envFile, "env", "", "dotenv `FILE` to compare instead of [secrets].file")
	cmd.Flags().BoolVar(&envStdin, "env-stdin", false, "read the dotenv content to compare from stdin")
	return cmd
}
