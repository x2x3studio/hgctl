package main

import (
	"context"
	"os"

	"github.com/x2x3studio/hgctl/internal/hgctl"
)

func main() {
	app, err := hgctl.New(os.Stdin, os.Stdout, os.Stderr)
	if err != nil {
		_, _ = os.Stderr.WriteString("hgctl: " + err.Error() + "\n")
		os.Exit(1)
	}
	os.Exit(app.Run(context.Background(), os.Args[1:]))
}
