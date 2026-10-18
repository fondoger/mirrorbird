package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"mirrorbird/internal/attachment"
	"mirrorbird/internal/desktop"
)

func main() {
	method := flag.String("method", "status", "local diagnostic operation")
	params := flag.String("params", "{}", "JSON parameters; avoid putting sensitive text in shell history")
	watch := flag.Bool("watch", false, "print live protocol events (may contain conversation content)")
	installation := flag.Bool("installation", false, "resolve Desktop and its backend without launching either")
	desktopPath := flag.String("desktop", "", "optional Desktop executable for installation diagnostics")
	codexPath := flag.String("codex", "", "optional explicitly configured backend for installation diagnostics")
	flag.Parse()
	if *installation {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		value, err := desktop.ResolveInstallation(ctx, desktop.Installation{Desktop: *desktopPath, Codex: *codexPath})
		if err != nil {
			fail(err.Error())
		}
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(value); err != nil {
			fail("cannot write installation diagnostics")
		}
		return
	}
	if *desktopPath != "" || *codexPath != "" {
		fail("--desktop and --codex require --installation")
	}
	if !json.Valid([]byte(*params)) {
		fail("invalid JSON parameters")
	}
	home, err := attachment.Home()
	if err != nil {
		fail(err.Error())
	}
	probe, probeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	client, err := attachment.Discover(probe, home)
	probeCancel()
	if err != nil {
		fail("No live MirrorBird attachment. Start ChatGPT through MirrorBird first.")
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Second)
	defer cancel()
	if *watch {
		if err := client.Subscribe(ctx); err != nil {
			fail(err.Error())
		}
		for event := range client.Events() {
			_ = json.NewEncoder(os.Stdout).Encode(event)
		}
		return
	}
	result, err := client.Call(ctx, *method, json.RawMessage(*params))
	if err != nil {
		fail(err.Error())
	}
	var value any
	if json.Unmarshal(result, &value) != nil {
		fail("invalid attachment response")
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(value)
}

func fail(message string) { fmt.Fprintln(os.Stderr, message); os.Exit(1) }
