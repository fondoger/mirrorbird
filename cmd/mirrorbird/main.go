package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"mirrorbird/internal/app"
	"mirrorbird/internal/desktop"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, app.Run)) }

func run(args []string, stdout, stderr io.Writer, start func(app.Options) error) int {
	flags := flag.NewFlagSet("mirrorbird", flag.ContinueOnError)
	flags.SetOutput(stderr)
	background := flags.Bool("background", false, "start in the tray without opening the settings window")
	showVersion := flags.Bool("version", false, "print MirrorBird version without starting the app")
	if err := flags.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "MirrorBird does not accept positional arguments.")
		return 2
	}
	if *showVersion {
		fmt.Fprintln(stdout, "MirrorBird "+desktop.Version())
		return 0
	}
	// app.Run / desktop.Run stay on this main goroutine. Only controller work
	// moves into workers; no settings or service is created before the UI lock.
	if err := start(app.Options{Background: *background}); err != nil {
		fmt.Fprintln(stderr, "MirrorBird could not start:", err)
		return 1
	}
	return 0
}
