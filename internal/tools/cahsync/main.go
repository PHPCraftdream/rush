package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/PHPCraftdream/rush/internal/cahgen"
)

func main() {
	var o cahgen.Options
	flag.StringVar(&o.Mode, "mode", "", "refresh, check, or offline")
	flag.StringVar(&o.Version, "version", "latest", "cc-arch-hands version")
	flag.StringVar(&o.Root, "root", ".", "repository root")
	flag.BoolVar(&o.Strict, "strict", false, "fail instead of warning and keeping committed outputs")
	flag.BoolVar(&o.Check, "check", false, "check generated outputs without writing")
	flag.BoolVar(&o.Offline, "offline", false, "keep committed outputs without network or I/O")
	// Refresh never downgrades: an acquired cah older than the committed one keeps committed files.
	flag.Parse()
	code, err := cahgen.Run(context.Background(), o, cahgen.Dependencies{Log: func(s string) { fmt.Fprintln(os.Stderr, s) }})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	os.Exit(code)
}
