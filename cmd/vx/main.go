// Command vx is the vx command line client.
package main

import (
	"context"
	"os"
	"os/signal"

	"github.com/TerracodeBA/vx/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	dir, err := os.Getwd()
	if err != nil {
		os.Stderr.WriteString("vx: " + err.Error() + "\n")
		os.Exit(1)
	}
	app := &cli.App{Out: os.Stdout, Err: os.Stderr, Getenv: os.Getenv, Dir: dir}
	os.Exit(app.Run(ctx, os.Args[1:]))
}
