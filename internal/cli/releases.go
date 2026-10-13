package cli

import (
	"strings"

	"github.com/spf13/cobra"
)

func init() { register(newReleasesCommand) }

func newReleasesCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "releases [REPO]",
		Short: "List releases on the host, newest first",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			eng, _, err := app.engineFor(args, "releases", false)
			if err != nil {
				return err
			}
			l, err := eng.Probe(cmd.Context(), false)
			if err != nil {
				return err
			}
			list, err := eng.ListReleases(cmd.Context(), l)
			if err != nil {
				return err
			}
			if app.JSON {
				return app.WriteJSON(list)
			}
			if len(list.Releases) == 0 {
				app.Printf("No releases.\n")
				return nil
			}
			for _, r := range list.Releases {
				var cols []string
				cols = append(cols, r.ID)
				if m := r.Metadata; m != nil {
					if c := m.ShortCommit(); c != "" {
						commit := c
						if m.GitRef != "" {
							commit += " " + m.GitRef
						}
						if m.GitDirty != nil && *m.GitDirty {
							commit += "+dirty"
						}
						cols = append(cols, commit)
					}
					if m.DeployedBy != "" {
						cols = append(cols, "by "+m.DeployedBy)
					}
				}
				switch {
				case r.Current:
					cols = append(cols, "(current)")
				case r.Previous:
					cols = append(cols, "(previous)")
				}
				if !r.RollbackAvailable && !r.Current {
					cols = append(cols, "[no saved configuration]")
				}
				app.Printf("%s\n", strings.Join(cols, "  "))
			}
			return nil
		},
	}
}
