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
//	depper REPO GROUP SUB ... -> depper GROUP SUB REPO ...
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
		// Keep a command group's subcommand path together, so REPO lands
		// after it: "REPO secrets list" -> "secrets list REPO".
		path := []string{args[i+1]}
		cur := findCommand(root, args[i+1])
		j := i + 2
		for cur != nil && cur.HasSubCommands() && j < len(args) {
			sub := findCommand(cur, args[j])
			if sub == nil {
				break
			}
			path = append(path, args[j])
			cur = sub
			j++
		}
		out := append([]string{}, path...)
		out = append(out, args[:i+1]...)
		return append(out, args[j:]...)
	}
	return append([]string{defaultCommand}, args...)
}

// findCommand returns the direct subcommand of parent with the given name or
// alias, or nil.
func findCommand(parent *cobra.Command, name string) *cobra.Command {
	for _, c := range parent.Commands() {
		if c.Name() == name {
			return c
		}
		for _, alias := range c.Aliases {
			if alias == name {
				return c
			}
		}
	}
	return nil
}
