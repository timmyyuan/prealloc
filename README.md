# prealloc

`prealloc` finds Go slice declarations that are followed by predictable appends and could benefit from an explicit capacity.

This version is syntax-first: it does not typecheck packages by default. It parses Go files, applies local syntax/type-shape inference, and reports only when it can prove the preallocation suggestion from that information. When the syntax-only pass cannot prove a case, it skips the diagnostic instead of guessing.

For projects that want the old precision profile, `prealloc` can fall back to typechecking only packages that contain unknown-but-promising candidates.

## Why syntax-first?

The original analyzer runs through `go/analysis`, which typechecks every target package before the checker can report anything. That is accurate, but package typechecking is often the slowest part of running this linter.

Most useful preallocation cases are visible from syntax alone:

```go
var xs []int
for i := range items {
	xs = append(xs, i)
}
```

The tool can see that `xs` is a slice, the append happens once per iteration, and `items` has a known length expression. No package-wide typecheck is needed to report:

```text
file.go:3:6: Consider preallocating xs with capacity len(items)
```

## Accuracy model

By default, `prealloc` is conservative:

- reports syntax-proven cases
- skips unknown external types, dot imports, selector-defined types, and method-return cases it cannot prove
- avoids speculative diagnostics when an expression might be a channel, iterator, function range, or non-slice value

That means the default mode prefers false negatives over false positives. In the upstream `testdata` suite with `-forloops`, syntax-only mode reports 127 of 130 typechecked diagnostics and produces 0 extra diagnostics.

Use `-fallback=typecheck` when you want the typechecked result set while still avoiding typecheck work for packages that the syntax pass fully resolves. On the same `testdata` suite, `-fallback=typecheck` matches the original typed analyzer output exactly.

## Performance

The main performance win comes from avoiding full package typechecking on the common path.

Example local measurements on Go 1.24, using the Go standard library `net/http` package with `-forloops`:

```text
old prealloc typechecked analyzer:        avg 1.338s
new prealloc syntax-only:                 avg 0.336s
new prealloc -fallback=typecheck:         avg 0.678s
```

On tiny packages, process startup and `go list` overhead can dominate. On larger packages and `./...` runs, skipping typecheck is where the syntax-first design pays off.

## Installation

```sh
go install github.com/alexkohler/prealloc@latest
```

If you are trying an unmerged fork or feature branch, clone it and run `go install .` from the repository root.

## Usage

```sh
prealloc [flags] files/directories/packages
```

Examples:

```sh
prealloc ./...
prealloc -forloops ./...
prealloc -fallback=typecheck ./...
prealloc -format=golangci-lint ./...
```

`prealloc` accepts files, directories, import paths, and `...` package patterns.

## Library Usage

The simplest package-level API handles package loading, syntax-first analysis, optional fallback, and output formatting:

```go
package main

import (
	"fmt"

	prealloc "github.com/alexkohler/prealloc/pkg"
)

func main() {
	opts := prealloc.DefaultOptions()
	opts.Dir = "/path/to/module"
	opts.IncludeForLoops = true
	opts.Fallback = prealloc.FallbackTypecheck
	opts.Format = prealloc.FormatGolangCILint
	opts.ExcludePathSubstrings = []string{"vendor/generated"}

	lines, err := prealloc.CheckPackageLines([]string{"./..."}, opts)
	if err != nil {
		panic(err)
	}
	for _, line := range lines {
		fmt.Println(line)
	}
}
```

If you want structured diagnostics instead of formatted strings:

```go
diagnostics, err := prealloc.CheckPackages([]string{"./..."}, opts)
```

If you want a Go struct that marshals to the same top-level JSON shape as `golangci-lint --output.json.path`:

```go
report, err := prealloc.CheckPackageGolangCILintJSON([]string{"./..."}, opts)
```

The returned `prealloc.GolangCILintJSON` struct can be round-tripped with `encoding/json`:

```go
data, err := json.Marshal(report)

var decoded prealloc.GolangCILintJSON
err = json.Unmarshal(data, &decoded)
diagnostics := decoded.Diagnostics()
```

Set `Dir` to analyze patterns relative to a specific module directory without changing the process working directory. Set `ExcludePathSubstrings` to skip diagnostics for any file path that contains one of the configured substrings.

For integrations that already loaded and typechecked packages, use the lower-level API:

```go
diagnostics := prealloc.CheckWithTypes(fset, files, typesInfo, opts)
```

## Flags

- `-simple` (default `true`): report only on simple loops with no returns, breaks, continues, or gotos. Turning this off may increase false positives.
- `-rangeloops` (default `true`): report suggestions in range loops.
- `-forloops` (default `false`): report suggestions in counted for loops.
- `-fallback` (default `off`): unknown handling mode. Use `off` to skip unknown syntax-only candidates, or `typecheck` to typecheck only packages with unknown candidates.
- `-format` (default `prealloc`): output format. Use `prealloc` for the standard output or `golangci-lint` to append the linter name.

## Output Formats

Default format:

```text
path/to/file.go:12:6: Consider preallocating xs with capacity len(items)
```

GolangCI-Lint-style format:

```text
path/to/file.go:12:6: Consider preallocating xs with capacity len(items) (prealloc)
```

## Fixing a Diagnostic

Change a zero-capacity or under-capacity slice declaration into a `make` call with the expected capacity:

```go
var xs []int
for i := range items {
	xs = append(xs, i)
}
```

becomes:

```go
xs := make([]int, 0, len(items))
for i := range items {
	xs = append(xs, i)
}
```

For very large copies, `copy` can be faster than repeated `append`:

```go
xs := make([]int, len(items))
copy(xs, items)
```

## Exit Codes

- `0`: no diagnostics
- `1`: diagnostics were found or package loading failed
- `2`: invalid command-line flags

## Development

Run the full local check:

```sh
go test -v ./...
golangci-lint run --verbose
```

The test suite checks that `-fallback=typecheck` matches the typed analyzer on the upstream fixtures and that syntax-only mode does not produce extra diagnostics compared with the typed analyzer.
