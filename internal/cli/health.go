package cli

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/frgmt0/depper/internal/engine"
	"github.com/frgmt0/depper/internal/exitcode"
)

func init() { register(newHealthCommand) }

func newHealthCommand(app *App) *cobra.Command {
	var (
		watch float64
		count int
	)
	cmd := &cobra.Command{
		Use:   "health [REPO]",
		Short: "Run the health check (exit 2 when unhealthy)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if watch < 0 || count < 0 {
				return fmt.Errorf("--watch and --count must not be negative")
			}
			if count == 0 && watch == 0 {
				count = 1
			}
			eng, _, err := app.engineFor(args, "health", false)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			l, err := eng.Probe(ctx, false)
			if err != nil {
				return err
			}
			if !l.Deployed() {
				return engine.ErrNotDeployed
			}
			stream := json.NewEncoder(app.Stdout)
			var last *engine.HealthResult
			for i := 0; count == 0 || i < count; i++ {
				if i > 0 {
					if err := eng.Sleep(ctx, time.Duration(watch*float64(time.Second))); err != nil {
						return err
					}
				}
				result, err := eng.CheckHealth(ctx, l)
				if err != nil {
					return err
				}
				last = result
				switch {
				case app.JSON && watch > 0:
					if err := stream.Encode(result); err != nil {
						return err
					}
				case app.JSON:
					if err := app.WriteJSON(result); err != nil {
						return err
					}
				default:
					writeHealthHuman(app, result, watch > 0)
				}
			}
			if last != nil && !last.OK {
				return exitcode.Silent(exitcode.Unhealthy)
			}
			return nil
		},
	}
	cmd.Flags().Float64Var(&watch, "watch", 0, "repeat the check every `SECONDS`")
	cmd.Flags().IntVar(&count, "count", 0, "number of checks (default 1, or unlimited with --watch)")
	return cmd
}

func writeHealthHuman(app *App, r *engine.HealthResult, timestamped bool) {
	prefix := ""
	if timestamped {
		prefix = r.CheckedAt + " "
	}
	app.Printf("%s%s %s\n", prefix, r.Target, r.Summary())
	if body := string(r.Body); body != "" && body != "null" {
		app.Printf("%s  body: %s\n", prefix, body)
	}
}
