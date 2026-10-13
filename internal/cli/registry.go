package cli

import "github.com/spf13/cobra"

// commandFactory builds one top-level command bound to the App.
type commandFactory func(app *App) *cobra.Command

var factories []commandFactory

// register adds a top-level command. Each command lives in its own file and
// registers itself from an init function:
//
//	func init() { register(newStatusCommand) }
//
// so adding a command never requires editing a shared file.
func register(factory commandFactory) { factories = append(factories, factory) }
