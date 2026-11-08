package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/CoachCoe/Ledger/internal/api"
	"github.com/CoachCoe/Ledger/internal/client"
	adapter "github.com/CoachCoe/Ledger/internal/mcp"
)

func runMCP(args []string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err := noFlags("mcp", args)
	if err == nil {
		repo, e := openRepo(ctx)
		err = e
		if err == nil {
			s, e := adapter.New(func(ctx context.Context, r api.Request) (any, error) { return client.Call(ctx, repo, r) })
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
