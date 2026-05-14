package pkg

import (
	"go/ast"
	"go/token"
	"go/types"

	"golang.org/x/tools/go/analysis"
)

type Options struct {
	Simple            bool
	IncludeRangeLoops bool
	IncludeForLoops   bool
}

type Diagnostic struct {
	Path    string
	Line    int
	Column  int
	Message string
}

type SyntaxResult struct {
	Diagnostics []Diagnostic
	HasUnknown  bool
}

func CheckWithTypes(fset *token.FileSet, files []*ast.File, info *types.Info, opts Options) []Diagnostic {
	var diagnostics []Diagnostic
	pass := &analysis.Pass{
		Fset:      fset,
		Files:     files,
		TypesInfo: info,
		Report: func(d analysis.Diagnostic) {
			pos := fset.Position(d.Pos)
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
