// Command farrier is the CLI frontend: flag parsing only, every real
// decision lives in internal/core (CLAUDE.md "House patterns").
package main

import (
	"fmt"
	"os"
)

// commands maps a subcommand name to its runner. Each runner owns its own
// flag parsing and returns the process exit code.
var commands = map[string]func(args []string) int{
	"init": runInit,
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: farrier <command> [flags]")
		printCommands()
		return 2
	}

	cmd, ok := commands[args[0]]
	if !ok {
		fmt.Fprintf(os.Stderr, "farrier: unknown command %q\n", args[0])
		printCommands()
		return 2
	}
	return cmd(args[1:])
}

func printCommands() {
	fmt.Fprintln(os.Stderr, "commands:")
	for name := range commands {
		fmt.Fprintf(os.Stderr, "  %s\n", name)
	}
}
