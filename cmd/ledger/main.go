package main

import (
	"os"

	"github.com/CoachCoe/Ledger/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:], os.Stdout))
}
