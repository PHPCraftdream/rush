// Command typecheck prints every Go type error of a module in ONE pass.
//
// `go build`/`go test` stop at the first failing dependency layer, so a
// large static refactor reveals its compile errors one package layer per CI
// run. This tool loads the whole module (including test variants) from
// source with go/packages, which keeps type-checking dependents of broken
// packages, and prints the union of all errors, de-duplicated and sorted.
// It lives in its own nested module so it adds nothing to the product's
// go.mod. Temporary: used while the H4 composed PR is repaired.
package main

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"golang.org/x/tools/go/packages"
)

func main() {
	dir := flag.String("dir", ".", "module root to load")
	flag.Parse()
	cfg := &packages.Config{
		Mode:  packages.LoadAllSyntax,
		Dir:   *dir,
		Tests: true,
	}
	pkgs, err := packages.Load(cfg, "./...")
	if err != nil {
		fmt.Fprintln(os.Stderr, "load:", err)
		os.Exit(2)
	}
	seen := map[string]bool{}
	var out []string
	packages.Visit(pkgs, nil, func(p *packages.Package) {
		if !strings.HasPrefix(p.PkgPath, "github.com/PHPCraftdream/rush") {
			return
		}
		for _, e := range p.Errors {
			line := fmt.Sprintf("%s: %s", e.Pos, e.Msg)
			if e.Pos == "" || e.Pos == "-" {
				line = fmt.Sprintf("[%s] %s", p.PkgPath, e.Msg)
			}
			line = strings.ReplaceAll(line, *dir+string(os.PathSeparator), "")
			if !seen[line] {
				seen[line] = true
				out = append(out, line)
			}
		}
	})
	sort.Strings(out)
	for _, l := range out {
		fmt.Println(l)
	}
	fmt.Printf("TOTAL %d type errors\n", len(out))
	if len(out) > 0 {
		os.Exit(1)
	}
}
