// Command ownsand is the OwnSand command-line interface. It is built on the
// OwnSand Go SDK and uses only the standard library flag package.
package main

import (
	"context"
	"os"
	"os/signal"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := run(ctx, os.Args[1:], &stdio{in: os.Stdin, out: os.Stdout, err: os.Stderr}, os.Getenv)
	stop()
	os.Exit(code)
}
