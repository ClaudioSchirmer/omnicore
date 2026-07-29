// Command omnicore-admin is the operator-side CLI for one-off framework
// administration tasks. Today it ships a single subcommand,
// replay-all-as-events, used to bootstrap a brand-new consumer service
// (B) against an existing producer (A) whose Kafka retention does not
// cover the full history.
//
// Usage:
//
//	omnicore-admin replay-all-as-events --aggregate <name> [flags]
//
// The subcommand pattern uses stdlib flag.FlagSet so the framework adds
// no extra module dependencies. New subcommands plug into the
// dispatcher in run() — keep the entry point self-contained and the
// individual subcommands behind their own packages once the CLI grows.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/ClaudioSchirmer/omnicore/cmd/omnicore-admin/listfailures"
	"github.com/ClaudioSchirmer/omnicore/cmd/omnicore-admin/replay"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 2 {
		usage(os.Stderr)
		return fmt.Errorf("omnicore-admin: subcommand required")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	sub := os.Args[1]
	args := os.Args[2:]

	switch sub {
	case "replay-all-as-events":
		return replay.Run(ctx, args)
	case "list-failures":
		return listfailures.Run(ctx, args)
	case "-h", "--help", "help":
		usage(os.Stdout)
		return nil
	default:
		usage(os.Stderr)
		return fmt.Errorf("omnicore-admin: unknown subcommand %q", sub)
	}
}

func usage(out *os.File) {
	fmt.Fprintln(out, "omnicore-admin — framework administration CLI")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Usage:")
	fmt.Fprintln(out, "  omnicore-admin <subcommand> [flags]")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Subcommands:")
	fmt.Fprintln(out, "  replay-all-as-events       Replay every active aggregate as a synthetic INSERTED event")
	fmt.Fprintln(out, "                             (use to bootstrap a brand-new consumer against an existing producer)")
	fmt.Fprintln(out, "  list-failures              List pending rows of the unified projection failure ledger")
	fmt.Fprintln(out, "                             (read-only — replay is automatic via the mongo.parkedRetry loop)")
	fmt.Fprintln(out, "  help                       Show this message")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Run 'omnicore-admin <subcommand> -h' for subcommand-specific help.")
	_ = flag.CommandLine.Output() // keep stdlib import live even when help is short
}
