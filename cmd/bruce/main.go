package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"bruce-go/internal/cli"
	"bruce-go/internal/integrated"
	"bruce-go/internal/tui"
)

const version = "0.1.0"

func main() {
	var (
		settings string
		noMCP    bool
		showVer  bool
	)
	flag.StringVar(&settings, "settings", "", "setting.json 路径，默认 ~/.bruce/setting.json")
	flag.BoolVar(&noMCP, "no-mcp", false, "启动时不自动连接 MCP server")
	flag.BoolVar(&showVer, "version", false, "显示版本")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "Bruce Go Coding Agent %s\n\n", version)
		fmt.Fprintln(flag.CommandLine.Output(), "用法:")
		fmt.Fprintln(flag.CommandLine.Output(), "  bruce [--settings path] [--no-mcp]")
		fmt.Fprintln(flag.CommandLine.Output())
		fmt.Fprintln(flag.CommandLine.Output(), cli.Help())
	}
	flag.Parse()
	if showVer {
		fmt.Println(version)
		return
	}
	if flag.NArg() > 0 {
		fmt.Fprintln(os.Stderr, "未知参数:", flag.Arg(0))
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
	if err := tui.Run(ctx, rt); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
