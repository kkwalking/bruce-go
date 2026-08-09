package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"bruce-go/internal/cli"
	"bruce-go/internal/integrated"
	"bruce-go/internal/tui"
	"bruce-go/internal/version"
)

func main() {
	var (
		settings string
		noMCP    bool
		showVer  bool
	)
	flag.StringVar(&settings, "settings", "", "path to setting.json (default: ~/.bruce/setting.json)")
	flag.BoolVar(&noMCP, "no-mcp", false, "do not connect to MCP servers at startup")
	flag.BoolVar(&showVer, "version", false, "show version")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "Bruce Go Coding Agent %s\n\n", version.Current)
		fmt.Fprintln(flag.CommandLine.Output(), "Usage:")
		fmt.Fprintln(flag.CommandLine.Output(), "  bruce [--settings path] [--no-mcp]")
		fmt.Fprintln(flag.CommandLine.Output())
		fmt.Fprintln(flag.CommandLine.Output(), cli.Help())
	}
	flag.Parse()
	if showVer {
		fmt.Println(version.Current)
		return
	}
	if flag.NArg() > 0 {
		fmt.Fprintln(os.Stderr, "Unknown argument:", flag.Arg(0))
		os.Exit(2)
	}

	workspace, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	ctx := context.Background()
	rt, err := integrated.New(ctx, integrated.Options{Workspace: workspace, SettingsPath: settings, StartMCP: !noMCP})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer rt.Close()
	if err := tui.Run(ctx, rt); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
