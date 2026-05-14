package main

import (
	"errors"
	"flag"
	"fmt"
	"go/build"
	"io"
	"log"
	"os"
	"sort"
	"strings"

	"github.com/alexkohler/prealloc/pkg"
	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/packages"
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

	opts := pkg.Options{}
	var fallback string
	var format string
	fs.BoolVar(&opts.Simple, "simple", true, "Report preallocation suggestions only on simple loops that have no returns/breaks/continues/gotos in them")
	fs.BoolVar(&opts.IncludeRangeLoops, "rangeloops", true, "Report preallocation suggestions on range loops")
	fs.BoolVar(&opts.IncludeForLoops, "forloops", false, "Report preallocation suggestions on for loops")
	fs.StringVar(&fallback, "fallback", "off", "How to handle syntax-only unknowns: off or typecheck")
	fs.StringVar(&format, "format", "prealloc", "Output format: prealloc or golangci-lint")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	switch fallback {
	case "off", "typecheck":
	default:
		fmt.Fprintf(stderr, "invalid -fallback %q: expected off or typecheck\n", fallback)
		return 2
	}
	switch format {
	case "prealloc", "golangci-lint":
	default:
		fmt.Fprintf(stderr, "invalid -format %q: expected prealloc or golangci-lint\n", format)
		return 2
	}

	patterns := fs.Args()
	if len(patterns) == 0 {
		patterns = []string{"."}
	}

	diagnostics, err := collectDiagnostics(patterns, opts, fallback == "typecheck")
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	sortDiagnostics(diagnostics)
	for _, d := range diagnostics {
		fmt.Fprintln(stdout, formatDiagnostic(d, format))
	}
	if len(diagnostics) > 0 {
		return 1
	}
	return 0
}

func collectDiagnostics(patterns []string, opts pkg.Options, fallbackTypecheck bool) ([]pkg.Diagnostic, error) {
	cfg := &packages.Config{
		Mode: packages.NeedName |
			packages.NeedFiles |
			packages.NeedCompiledGoFiles |
			packages.NeedSyntax,
	}
	pkgs, err := packages.Load(cfg, patterns...)
	if err != nil {
		return nil, err
	}
	if err := packageLoadError(pkgs); err != nil {
		return nil, err
	}

	var diagnostics []pkg.Diagnostic
	unknownSeen := map[string]bool{}
	unknownPatterns := make([]string, 0, len(pkgs))
	for _, p := range pkgs {
		result := pkg.CheckSyntax(p.Fset, p.Syntax, opts)
		diagnostics = append(diagnostics, result.Diagnostics...)
		if fallbackTypecheck && result.HasUnknown {
			for _, pattern := range packagePatterns(p) {
				if unknownSeen[pattern] {
					continue
				}
				unknownSeen[pattern] = true
				unknownPatterns = append(unknownPatterns, pattern)
			}
		}
	}
	if fallbackTypecheck && len(unknownPatterns) > 0 {
		typed, err := collectTypedDiagnostics(unknownPatterns, opts)
		if err != nil {
			return diagnostics, nil
		}
		diagnostics = append(diagnostics, typed...)
	}
	return dedupeDiagnostics(diagnostics), nil
}

func collectTypedDiagnostics(patterns []string, opts pkg.Options) ([]pkg.Diagnostic, error) {
	cfg := &packages.Config{
		Mode: packages.NeedName |
			packages.NeedFiles |
			packages.NeedCompiledGoFiles |
			packages.NeedSyntax |
			packages.NeedTypes |
			packages.NeedTypesInfo,
	}
	pkgs, err := packages.Load(cfg, patterns...)
	if err != nil {
		return nil, err
	}
	if err := packageLoadError(pkgs); err != nil {
		return nil, err
	}

	var diagnostics []pkg.Diagnostic
	for _, p := range pkgs {
		if p.TypesInfo == nil {
			continue
		}
		diagnostics = append(diagnostics, pkg.CheckWithTypes(p.Fset, p.Syntax, p.TypesInfo, opts)...)
	}
	return diagnostics, nil
}

func packagePatterns(p *packages.Package) []string {
	if p.PkgPath != "" && p.PkgPath != "command-line-arguments" {
		return []string{p.PkgPath}
	}
	if len(p.GoFiles) > 0 {
		return append([]string(nil), p.GoFiles...)
	}
	if len(p.CompiledGoFiles) > 0 {
		return append([]string(nil), p.CompiledGoFiles...)
	}
	return []string{p.ID}
}

func packageLoadError(pkgs []*packages.Package) error {
	var messages []string
	seen := map[string]bool{}
	packages.Visit(pkgs, nil, func(p *packages.Package) {
		for _, err := range p.Errors {
			msg := err.Error()
			if seen[msg] {
				continue
			}
			seen[msg] = true
			messages = append(messages, msg)
		}
	})
	if len(messages) == 0 {
		return nil
	}
	return errors.New(strings.Join(messages, "\n"))
}

func dedupeDiagnostics(in []pkg.Diagnostic) []pkg.Diagnostic {
	seen := make(map[string]bool, len(in))
	out := make([]pkg.Diagnostic, 0, len(in))
	for _, d := range in {
		key := fmt.Sprintf("%s:%d:%d:%s", d.Path, d.Line, d.Column, d.Message)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, d)
	}
	return out
}

func sortDiagnostics(diagnostics []pkg.Diagnostic) {
	sort.Slice(diagnostics, func(i, j int) bool {
		a, b := diagnostics[i], diagnostics[j]
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		if a.Column != b.Column {
			return a.Column < b.Column
		}
		return a.Message < b.Message
	})
}

func formatDiagnostic(d pkg.Diagnostic, format string) string {
	base := fmt.Sprintf("%s:%d:%d: %s", d.Path, d.Line, d.Column, d.Message)
	if format == "golangci-lint" && !strings.HasSuffix(base, " (prealloc)") {
		return base + " (prealloc)"
	}
	return base
}
