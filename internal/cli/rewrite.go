package cli

import (
	"strings"

	"github.com/spf13/cobra"
)

// defaultCommand is run when no command is named.
const defaultCommand = "deploy"

// globalValueFlags are root flags that take a separate value argument.
var globalValueFlags = map[string]bool{"--target": true}

// rewriteArgs implements the CLI conveniences inherited from the Python
// deployer, once a "deploy" command is registered:
//
//	depper [flags]            -> depper deploy [flags]
//	depper [flags] REPO ...   -> depper deploy [flags] REPO ...
//	depper REPO COMMAND ...   -> depper COMMAND REPO ...   (not init/help)
//
// The command name is placed first so cobra never mistakes it for a flag
// value. Help, version, and "--" are never rewritten.
func rewriteArgs(root *cobra.Command, args []string) []string {
	known := map[string]bool{"help": true, "completion": true, cobra.ShellCompRequestCmd: true, cobra.ShellCompNoDescRequestCmd: true}
	for _, c := range root.Commands() {
		known[c.Name()] = true
		for _, alias := range c.Aliases {
			known[alias] = true
		}
	}
	if !known[defaultCommand] {
		return args
	}
	i := 0
	for i < len(args) {
		arg := args[i]
		if arg == "-h" || arg == "--help" || arg == "--version" || arg == "-v" || arg == "--" {
			return args
		}
		if globalValueFlags[arg] {
			i += 2
			continue
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			break
		}
		i++
	}
	if i > len(args) {
		return args // dangling --target: let cobra report it
	}
	if i < len(args) && known[args[i]] {
		return args
	}
	if i+1 < len(args) && known[args[i+1]] && args[i+1] != "init" && args[i+1] != "help" {
		out := []string{args[i+1]}
		out = append(out, args[:i+1]...)
		return append(out, args[i+2:]...)
	}
	return append([]string{defaultCommand}, args...)
}
