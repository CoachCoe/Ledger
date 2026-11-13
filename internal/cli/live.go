package cli

import (
	"context"
	"flag"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/CoachCoe/Ledger/internal/api"
	"github.com/CoachCoe/Ledger/internal/apierr"
	"github.com/CoachCoe/Ledger/internal/client"
	"github.com/CoachCoe/Ledger/internal/daemon"
	"github.com/CoachCoe/Ledger/internal/identity"
	"github.com/CoachCoe/Ledger/internal/schema"
)

func cmdDaemon(args []string) (any, error) {
	if len(args) > 0 && args[0] == "stop" {
		return liveCommand("daemon")(args)
	}
	flags := flag.NewFlagSet("daemon", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var opts daemon.Options
	flags.DurationVar(&opts.Idle, "idle", 10*time.Minute, "idle shutdown")
	flags.DurationVar(&opts.Poll, "poll", 10*time.Second, "observation fallback")
	flags.BoolVar(&opts.DisableWatchers, "no-watch", false, "disable filesystem triggers")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return nil, apierr.New("usage", "ledger daemon [--idle duration] [--poll duration] [--no-watch]")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	repo, err := locateNative(ctx)
	if err != nil {
		return nil, err
	}
	dir, err := identity.DefaultKeyDir()
	if err != nil {
		return nil, err
	}
	err = daemon.Run(ctx, repo, identity.LocalKeys{Dir: dir}, opts)
	return map[string]bool{"stopped": true}, err
}

func liveCommand(group string) func([]string) (any, error) {
	return func(args []string) (any, error) {
		if len(args) == 0 {
			return nil, apierr.New("usage", "ledger %s <operation> [--session id] [--operation-id id] [JSON]", group)
		}
		operation := group + "." + args[0]
		flags := flag.NewFlagSet(operation, flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		sid := flags.String("session", "", "acting session (empty = root)")
		opid := flags.String("operation-id", "", "retry identity")
		if err := flags.Parse(args[1:]); err != nil || flags.NArg() > 1 {
			return nil, apierr.New("usage", "invalid arguments for %s", operation)
		}
		input := []byte(`{}`)
		if flags.NArg() == 1 {
			input = []byte(flags.Arg(0))
		}
		if *opid == "" && !api.ReadOnly(operation) {
			*opid = schema.NewID(schema.PrefixOperation)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		repo, err := locateNative(ctx)
		if err != nil {
			cancel()
			return nil, err
		}
		request := api.Request{Version: api.Version, Operation: operation, OperationID: *opid, SessionID: *sid, Input: input}
		// A check may legitimately run for hours (its own timeout bounds it), so
		// evidence.run has no client deadline; Ctrl-C disconnects, which
		// cancels the run on the daemon and records a failed attestation.
		// speculation.evaluate runs checks and the scorer and has no deadline
		// either, but a disconnect leaves it running; speculation cancel stops it.
		if operation == "evidence.run" || operation == "speculation.evaluate" {
			cancel()
			ctx, cancel = signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		}
		defer cancel()
		return client.Call(ctx, repo.Root, request)
	}
}
