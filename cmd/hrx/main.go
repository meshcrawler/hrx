package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/meshcrawler/hrx/internal/app"
)

func main() {
	if len(os.Args) == 1 || (os.Args[1] != "--version" && !strings.HasPrefix(os.Args[1], "_")) {
		fmt.Fprintln(os.Stderr, app.Version())
	}
	if err := app.Run(context.Background(), os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "hrx:", err)
		os.Exit(app.ExitCode(err))
	}
}
