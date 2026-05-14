package main

import (
	"flag"
	"fmt"
	"go/build"
	"io"
	"log"
	"os"

	"github.com/alexkohler/prealloc/pkg"
	"golang.org/x/tools/go/analysis"
)

// Support: (in order of priority)
//  * Full make suggestion with type?
//	* Test flag
//  * Use an import rather than the duplicated import.go

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

type prealloc struct {
	simple            bool
	includeRangeLoops bool
	includeForLoops   bool
}

func NewAnalyzer() *analysis.Analyzer {
	// Ignore build flags
	build.Default.UseAllFiles = true

	// Remove log timestamp
	log.SetFlags(0)

	p := &prealloc{}

	a := &analysis.Analyzer{
		Name: "prealloc",
		Doc:  "Find slice declarations that could potentially be preallocated",
		Run:  p.run,
	}
	a.Flags.Init("prealloc", flag.ExitOnError)
	a.Flags.BoolVar(&p.simple, "simple", true, "Report preallocation suggestions only on simple loops that have no returns/breaks/continues/gotos in them")
	a.Flags.BoolVar(&p.includeRangeLoops, "rangeloops", true, "Report preallocation suggestions on range loops")
	a.Flags.BoolVar(&p.includeForLoops, "forloops", false, "Report preallocation suggestions on for loops")
	return a
}

func (p *prealloc) run(pass *analysis.Pass) (any, error) {
	pkg.Check(pass, p.simple, p.includeRangeLoops, p.includeForLoops)
	return nil, nil
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("prealloc", flag.ContinueOnError)
	fs.SetOutput(stderr)

	opts := pkg.DefaultOptions()
	fs.BoolVar(&opts.Simple, "simple", true, "Report preallocation suggestions only on simple loops that have no returns/breaks/continues/gotos in them")
	fs.BoolVar(&opts.IncludeRangeLoops, "rangeloops", true, "Report preallocation suggestions on range loops")
	fs.BoolVar(&opts.IncludeForLoops, "forloops", false, "Report preallocation suggestions on for loops")
	fs.TextVar(&opts.Fallback, "fallback", pkg.FallbackOff, "How to handle syntax-only unknowns: off or typecheck")
	fs.TextVar(&opts.Format, "format", pkg.FormatPrealloc, "Output format: prealloc or golangci-lint")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	patterns := fs.Args()
	if len(patterns) == 0 {
		patterns = []string{"."}
	}

	diagnostics, err := pkg.CheckPackages(patterns, opts)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	pkg.SortDiagnostics(diagnostics)
	for _, d := range diagnostics {
		_, _ = fmt.Fprintln(stdout, pkg.FormatDiagnostic(d, opts.Format))
	}
	if len(diagnostics) > 0 {
		return 1
	}
	return 0
}
