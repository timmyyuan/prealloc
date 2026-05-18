package pkg

import (
	"errors"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"sort"
	"strings"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/packages"
)

type FallbackMode string

const (
	FallbackOff       FallbackMode = "off"
	FallbackTypecheck FallbackMode = "typecheck"
)

type OutputFormat string

const (
	FormatPrealloc     OutputFormat = "prealloc"
	FormatGolangCILint OutputFormat = "golangci-lint"
)

type Options struct {
	Dir                   string
	Simple                bool
	IncludeRangeLoops     bool
	IncludeForLoops       bool
	Fallback              FallbackMode
	Format                OutputFormat
	ExcludePathSubstrings []string
}

type Diagnostic struct {
	Path    string
	Line    int
	Column  int
	Message string
}

type GolangCILintJSON struct {
	Issues []GolangCILintIssue    `json:"Issues"`
	Report GolangCILintJSONReport `json:"Report"`
}

type GolangCILintJSONReport struct {
	Linters []GolangCILintLinter `json:"Linters"`
}

type GolangCILintLinter struct {
	Name    string `json:"Name"`
	Enabled bool   `json:"Enabled,omitempty"`
}

type GolangCILintIssue struct {
	FromLinter           string               `json:"FromLinter"`
	Text                 string               `json:"Text"`
	Severity             string               `json:"Severity"`
	SourceLines          []string             `json:"SourceLines"`
	Pos                  GolangCILintPosition `json:"Pos"`
	ExpectNoLint         bool                 `json:"ExpectNoLint"`
	ExpectedNoLintLinter string               `json:"ExpectedNoLintLinter"`
}

type GolangCILintPosition struct {
	Filename string `json:"Filename"`
	Offset   int    `json:"Offset"`
	Line     int    `json:"Line"`
	Column   int    `json:"Column"`
}

type SyntaxResult struct {
	Diagnostics []Diagnostic
	HasUnknown  bool
}

func DefaultOptions() Options {
	return Options{
		Simple:            true,
		IncludeRangeLoops: true,
		IncludeForLoops:   false,
		Fallback:          FallbackOff,
		Format:            FormatPrealloc,
	}
}

func CheckPackages(patterns []string, opts Options) ([]Diagnostic, error) {
	opts = normalizeOptions(opts)
	if err := opts.validate(); err != nil {
		return nil, err
	}
	if len(patterns) == 0 {
		patterns = []string{"."}
	}

	cfg := &packages.Config{
		Dir: opts.Dir,
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

	var diagnostics []Diagnostic
	unknownSeen := map[string]bool{}
	unknownPatterns := make([]string, 0, len(pkgs))
	for _, p := range pkgs {
		files := filterASTFiles(p.Fset, p.Syntax, opts.ExcludePathSubstrings)
		if len(files) == 0 {
			continue
		}
		result := CheckSyntax(p.Fset, files, opts)
		diagnostics = append(diagnostics, result.Diagnostics...)
		if opts.Fallback == FallbackTypecheck && result.HasUnknown {
			for _, pattern := range packagePatterns(p) {
				if unknownSeen[pattern] {
					continue
				}
				unknownSeen[pattern] = true
				unknownPatterns = append(unknownPatterns, pattern)
			}
		}
	}
	if opts.Fallback == FallbackTypecheck && len(unknownPatterns) > 0 {
		typed, err := CheckPackagesWithTypes(unknownPatterns, opts)
		if err != nil {
			diagnostics = filterDiagnostics(DeduplicateDiagnostics(diagnostics), opts.ExcludePathSubstrings)
			SortDiagnostics(diagnostics)
			return diagnostics, nil
		}
		diagnostics = append(diagnostics, typed...)
	}
	diagnostics = filterDiagnostics(DeduplicateDiagnostics(diagnostics), opts.ExcludePathSubstrings)
	SortDiagnostics(diagnostics)
	return diagnostics, nil
}

func CheckPackagesWithTypes(patterns []string, opts Options) ([]Diagnostic, error) {
	opts = normalizeOptions(opts)
	if err := opts.validate(); err != nil {
		return nil, err
	}
	if len(patterns) == 0 {
		patterns = []string{"."}
	}

	cfg := &packages.Config{
		Dir: opts.Dir,
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

	var diagnostics []Diagnostic
	for _, p := range pkgs {
		if p.TypesInfo == nil {
			continue
		}
		files := filterASTFiles(p.Fset, p.Syntax, opts.ExcludePathSubstrings)
		if len(files) == 0 {
			continue
		}
		diagnostics = append(diagnostics, CheckWithTypes(p.Fset, files, p.TypesInfo, opts)...)
	}
	diagnostics = filterDiagnostics(DeduplicateDiagnostics(diagnostics), opts.ExcludePathSubstrings)
	SortDiagnostics(diagnostics)
	return diagnostics, nil
}

func CheckPackageLines(patterns []string, opts Options) ([]string, error) {
	opts = normalizeOptions(opts)
	diagnostics, err := CheckPackages(patterns, opts)
	if err != nil {
		return nil, err
	}
	return FormatDiagnostics(diagnostics, opts.Format), nil
}

func CheckPackageGolangCILintJSON(patterns []string, opts Options) (GolangCILintJSON, error) {
	diagnostics, err := CheckPackages(patterns, opts)
	if err != nil {
		return GolangCILintJSON{}, err
	}
	return DiagnosticsToGolangCILintJSON(diagnostics), nil
}

func DiagnosticsToGolangCILintJSON(diagnostics []Diagnostic) GolangCILintJSON {
	SortDiagnostics(diagnostics)
	issues := make([]GolangCILintIssue, 0, len(diagnostics))
	for _, d := range diagnostics {
		issues = append(issues, d.GolangCILintIssue())
	}
	return GolangCILintJSON{
		Issues: issues,
		Report: GolangCILintJSONReport{
			Linters: []GolangCILintLinter{
				{Name: "prealloc", Enabled: true},
			},
		},
	}
}

func (d Diagnostic) GolangCILintIssue() GolangCILintIssue {
	return GolangCILintIssue{
		FromLinter:   "prealloc",
		Text:         d.Message,
		Severity:     "",
		SourceLines:  []string{},
		Pos:          GolangCILintPosition{Filename: d.Path, Line: d.Line, Column: d.Column},
		ExpectNoLint: false,
	}
}

func (output GolangCILintJSON) Diagnostics() []Diagnostic {
	diagnostics := make([]Diagnostic, 0, len(output.Issues))
	for _, issue := range output.Issues {
		diagnostics = append(diagnostics, issue.Diagnostic())
	}
	return diagnostics
}

func (issue GolangCILintIssue) Diagnostic() Diagnostic {
	return Diagnostic{
		Path:    issue.Pos.Filename,
		Line:    issue.Pos.Line,
		Column:  issue.Pos.Column,
		Message: issue.Text,
	}
}

func FormatDiagnostic(d Diagnostic, format OutputFormat) string {
	base := fmt.Sprintf("%s:%d:%d: %s", d.Path, d.Line, d.Column, d.Message)
	if format == FormatGolangCILint && !strings.HasSuffix(base, " (prealloc)") {
		return base + " (prealloc)"
	}
	return base
}

func FormatDiagnostics(diagnostics []Diagnostic, format OutputFormat) []string {
	lines := make([]string, 0, len(diagnostics))
	for _, d := range diagnostics {
		lines = append(lines, FormatDiagnostic(d, format))
	}
	return lines
}

func SortDiagnostics(diagnostics []Diagnostic) {
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

func DeduplicateDiagnostics(in []Diagnostic) []Diagnostic {
	seen := make(map[string]bool, len(in))
	out := make([]Diagnostic, 0, len(in))
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

func CheckWithTypes(fset *token.FileSet, files []*ast.File, info *types.Info, opts Options) []Diagnostic {
	var diagnostics []Diagnostic
	pass := &analysis.Pass{
		Fset:      fset,
		Files:     files,
		TypesInfo: info,
		Report: func(d analysis.Diagnostic) {
			pos := fset.Position(d.Pos)
			if pathExcluded(pos.Filename, opts.ExcludePathSubstrings) {
				return
			}
			diagnostics = append(diagnostics, Diagnostic{
				Path:    pos.Filename,
				Line:    pos.Line,
				Column:  pos.Column,
				Message: d.Message,
			})
		},
	}
	Check(pass, opts.Simple, opts.IncludeRangeLoops, opts.IncludeForLoops)
	return diagnostics
}

func filterASTFiles(fset *token.FileSet, files []*ast.File, substrings []string) []*ast.File {
	if len(substrings) == 0 {
		return files
	}
	filtered := make([]*ast.File, 0, len(files))
	for _, file := range files {
		filename := fset.PositionFor(file.Pos(), false).Filename
		if pathExcluded(filename, substrings) {
			continue
		}
		filtered = append(filtered, file)
	}
	return filtered
}

func filterDiagnostics(diagnostics []Diagnostic, substrings []string) []Diagnostic {
	if len(substrings) == 0 {
		return diagnostics
	}
	filtered := make([]Diagnostic, 0, len(diagnostics))
	for _, diagnostic := range diagnostics {
		if pathExcluded(diagnostic.Path, substrings) {
			continue
		}
		filtered = append(filtered, diagnostic)
	}
	return filtered
}

func pathExcluded(path string, substrings []string) bool {
	for _, substring := range substrings {
		if substring != "" && strings.Contains(path, substring) {
			return true
		}
	}
	return false
}

func normalizeOptions(opts Options) Options {
	if opts.Fallback == "" {
		opts.Fallback = FallbackOff
	}
	if opts.Format == "" {
		opts.Format = FormatPrealloc
	}
	return opts
}

func (mode FallbackMode) MarshalText() ([]byte, error) {
	return []byte(mode), nil
}

func (mode *FallbackMode) UnmarshalText(text []byte) error {
	next := FallbackMode(text)
	switch next {
	case FallbackOff, FallbackTypecheck:
		*mode = next
		return nil
	default:
		return fmt.Errorf("invalid fallback %q: expected %q or %q", next, FallbackOff, FallbackTypecheck)
	}
}

func (format OutputFormat) MarshalText() ([]byte, error) {
	return []byte(format), nil
}

func (format *OutputFormat) UnmarshalText(text []byte) error {
	next := OutputFormat(text)
	switch next {
	case FormatPrealloc, FormatGolangCILint:
		*format = next
		return nil
	default:
		return fmt.Errorf("invalid format %q: expected %q or %q", next, FormatPrealloc, FormatGolangCILint)
	}
}

func (opts Options) validate() error {
	switch opts.Fallback {
	case FallbackOff, FallbackTypecheck:
	default:
		return fmt.Errorf("invalid fallback %q: expected %q or %q", opts.Fallback, FallbackOff, FallbackTypecheck)
	}
	switch opts.Format {
	case FormatPrealloc, FormatGolangCILint:
	default:
		return fmt.Errorf("invalid format %q: expected %q or %q", opts.Format, FormatPrealloc, FormatGolangCILint)
	}
	return nil
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
