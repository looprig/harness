// Command swe is the SWE-Swarm TUI entry point. It is wiring only: it hands the
// shared CLI runtime the SWE swarm's constructor and a startup banner, then exits
// with the runtime's process exit code. All behavior lives in internal/cli (the
// runtime) and swarms/swe (the agent).
package main

import (
	"context"
	"os"

	"github.com/inventivepotter/urvi/internal/cli"
	"github.com/inventivepotter/urvi/swarms/swe"
)

func main() {
	os.Exit(cli.Run(context.Background(), swe.New, cli.Banner{Name: "SWE"}))
}
