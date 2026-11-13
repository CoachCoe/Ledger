package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/CoachCoe/Ledger/internal/api"
	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/client"
	adapter "github.com/CoachCoe/Ledger/internal/mcp"
)

func runMCP(args []string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	flags := flag.NewFlagSet("mcp", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	session := flags.String("session", "", "the session every tool call acts as")
	var err error
	if e := flags.Parse(args); e != nil || flags.NArg() != 0 {
		err = apierr.New("usage", "ledger mcp --session ses_...")
	}
	if err == nil {
		repo, e := locate(ctx)
		err = e
		if err == nil {
			s, e := adapter.New(*session, repo.APIVersion(), func(ctx context.Context, r api.Request) (any, error) { return client.Call(ctx, repo.Root, r) })
			err = e
			if err == nil {
				err = s.Run(ctx, &mcp.StdioTransport{})
			}
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}
