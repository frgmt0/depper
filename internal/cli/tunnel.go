package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/frgmt0/depper/internal/tunnel"
)

// Importing internal/tunnel also registers it with the engine (its init
// function), which enables [tunnel] for deploys.

func init() {
	register(newTunnelCommand)
	register(newTunnelStatusCommand)
	register(newTunnelLogsCommand)
	register(newTunnelStopCommand)
}

// tunnelJSON is the `depper tunnel --json` output.
type tunnelJSON struct {
	Name     string `json:"name"`
	ID       string `json:"id"`
	Hostname string `json:"hostname"`
	Service  string `json:"service"`
	URL      string `json:"url"`
	Unit     string `json:"unit"`
}

func newTunnelCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "tunnel [REPO]",
		Short: "Create or verify the owned Cloudflare Tunnel and start its connector",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			eng, cfg, err := app.engineFor(args, "tunnel", false)
			if err != nil {
				return err
			}
			state, err := tunnel.Default.Run(cmd.Context(), eng)
			if err != nil {
				return err
			}
			if app.JSON {
				return app.WriteJSON(tunnelJSON{Name: state.Name, ID: state.ID, Hostname: state.Hostname, Service: state.Service, URL: state.URL(), Unit: cfg.TunnelUnitName()})
			}
			return nil
		},
	}
}

func newTunnelStatusCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "tunnel-status [REPO]",
		Short: "Show the tunnel connector unit's systemd status",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			eng, _, err := app.engineFor(args, "tunnel-status", false)
			if err != nil {
				return err
			}
			return tunnel.Status(cmd.Context(), eng)
		},
	}
}

func newTunnelLogsCommand(app *App) *cobra.Command {
	var lines int
	cmd := &cobra.Command{
		Use:   "tunnel-logs [REPO]",
		Short: "Show the tunnel connector's journal",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if lines < 1 {
				return fmt.Errorf("--lines must be a positive integer")
			}
			eng, _, err := app.engineFor(args, "tunnel-logs", false)
			if err != nil {
				return err
			}
			return tunnel.Logs(cmd.Context(), eng, lines, app.Human())
		},
	}
	cmd.Flags().IntVar(&lines, "lines", 100, "number of journal lines to show")
	return cmd
}

func newTunnelStopCommand(app *App) *cobra.Command {
	return &cobra.Command{
		Use:   "tunnel-stop [REPO]",
		Short: "Stop and disable the tunnel connector (keeps the tunnel and its DNS route)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			eng, cfg, err := app.engineFor(args, "tunnel-stop", false)
			if err != nil {
				return err
			}
			if err := tunnel.Stop(cmd.Context(), eng); err != nil {
				return err
			}
			app.Printf("Stopped and disabled %s; the tunnel, its state, and its DNS route are kept.\n", cfg.TunnelUnitName())
			return nil
		},
	}
}
