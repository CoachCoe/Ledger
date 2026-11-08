// A deterministic, provider-free agent. Build from the Ledger source:
// go build -o /tmp/ledger-mock ./examples/mock-agent
// Then run /tmp/ledger-mock /absolute/path/to/ledger in an initialized fixture.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/CoachCoe/Ledger/internal/mockagent"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: mock-agent /absolute/path/to/ledger (run inside an initialized fixture repository)")
		os.Exit(1)
	}
	ctx := context.Background()
	command := exec.Command(os.Args[1], "mcp") //nolint:gosec // the user explicitly supplies the Ledger executable, without a shell
	s, err := mcp.NewClient(&mcp.Implementation{Name: "ledger-external-mock", Version: "1"}, nil).Connect(ctx, &mcp.CommandTransport{Command: command}, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	out, err := (mockagent.Client{Session: s}).Lifecycle(ctx)
	_ = s.Close()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := json.NewEncoder(os.Stdout).Encode(out); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
