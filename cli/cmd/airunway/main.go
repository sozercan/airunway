package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/ai-runway/airunway/cli/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	code := cli.Run(ctx, os.Args[1:], cli.RunOptions{})
	stop()
	os.Exit(code)
}
