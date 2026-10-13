package cli

import (
	"github.com/frgmt0/depper/internal/config"
	"github.com/frgmt0/depper/internal/engine"
)

// repoArg returns the optional REPO positional argument ("." by default).
func repoArg(args []string) string {
	if len(args) == 1 {
		return args[0]
	}
	return "."
}

// engineFor loads REPO's configuration and returns an engine for command.
// dryRun makes every remote command print instead of run.
func (a *App) engineFor(args []string, command string, dryRun bool) (*engine.Engine, *config.Config, error) {
	cfg, err := a.Config(repoArg(args))
	if err != nil {
		return nil, nil, err
	}
	eng := engine.New(cfg, a.Remote(cfg, dryRun), a.Exec, a.Human())
	eng.Command = command
	return eng, cfg, nil
}
