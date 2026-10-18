package main

import (
	"os"

	"mirrorbird/internal/shim"
)

func main() { os.Exit(shim.Run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr)) }
