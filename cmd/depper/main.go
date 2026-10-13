// Command depper deploys a repository to a systemd user service over SSH.
package main

import (
	"os"

	"github.com/frgmt0/depper/internal/cli"
)

func main() { os.Exit(cli.Main()) }
