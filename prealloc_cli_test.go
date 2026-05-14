package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexkohler/prealloc/pkg"
)

func TestRunSyntaxOnlyFormats(t *testing.T) {
	dir := writeTempModule(t, map[string]string{
		"p.go": `package p

func f(items []int) {
	var xs []int
	for i := range items {
		xs = append(xs, i)
	}
}
`,
	})
	t.Chdir(dir)

	var stdout, stderr bytes.Buffer
	code := run([]string{"-format=golangci-lint", "."}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run exit code = %d, want 1; stderr=%q", code, stderr.String())
	}
	got := stdout.String()
	if !strings.Contains(got, "p.go:4:6: Consider preallocating xs with capacity len(items) (prealloc)") {
		t.Fatalf("golangci-lint output missing expected diagnostic:\n%s", got)
	}

	stdout.Reset()
	stderr.Reset()
	code = run([]string{"-format=prealloc", "."}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("run exit code = %d, want 1; stderr=%q", code, stderr.String())
	}
	got = stdout.String()
	if !strings.Contains(got, "p.go:4:6: Consider preallocating xs with capacity len(items)") {
		t.Fatalf("prealloc output missing expected diagnostic:\n%s", got)
	}
	if strings.Contains(got, "(prealloc)") {
		t.Fatalf("prealloc output should not include linter suffix:\n%s", got)
	}
}

func TestRunFallbackTypecheck(t *testing.T) {
	dir := writeTempModule(t, map[string]string{
		"p.go": `package p

import "sort"

func f() {
	var xs sort.IntSlice
	for i := range 5 {
		xs = append(xs, i)
	}
}
`,
	})
	t.Chdir(dir)

	var stdout, stderr bytes.Buffer
	code := run([]string{"-fallback=off", "."}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("fallback=off exit code = %d, want 0; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("fallback=off should not report unknown external typedefs:\n%s", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	code = run([]string{"-fallback=typecheck", "."}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("fallback=typecheck exit code = %d, want 1; stderr=%q", code, stderr.String())
	}
	got := stdout.String()
	if !strings.Contains(got, "p.go:6:6: Consider preallocating xs with capacity 5") {
		t.Fatalf("fallback=typecheck output missing expected diagnostic:\n%s", got)
	}
}

func TestFallbackTypecheckMatchesTypedCoreOnTestdata(t *testing.T) {
	opts := pkg.Options{
		Simple:            true,
		IncludeRangeLoops: true,
		IncludeForLoops:   true,
	}

	typed, err := collectTypedDiagnostics([]string{"./testdata"}, opts)
	if err != nil {
		t.Fatal(err)
	}
	fallback, err := collectDiagnostics([]string{"./testdata"}, opts, true)
	if err != nil {
		t.Fatal(err)
	}
	typedLines := diagnosticLines(typed)
	fallbackLines := diagnosticLines(fallback)
	if strings.Join(fallbackLines, "\n") != strings.Join(typedLines, "\n") {
		t.Fatalf("fallback=typecheck diagnostics differ from typed core\nmissing from fallback:\n%s\nextra in fallback:\n%s",
			strings.Join(missingLines(typedLines, fallbackLines), "\n"),
			strings.Join(missingLines(fallbackLines, typedLines), "\n"),
		)
	}

	syntaxOnly, err := collectDiagnostics([]string{"./testdata"}, opts, false)
	if err != nil {
		t.Fatal(err)
	}
	extraSyntax := missingLines(diagnosticLines(syntaxOnly), typedLines)
	if len(extraSyntax) > 0 {
		t.Fatalf("syntax-only produced diagnostics absent from typed core:\n%s", strings.Join(extraSyntax, "\n"))
	}
}

func writeTempModule(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	files["go.mod"] = "module example.com/preallocfixture\n\ngo 1.24.0\n"
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func diagnosticLines(diagnostics []pkg.Diagnostic) []string {
	sortDiagnostics(diagnostics)
	lines := make([]string, 0, len(diagnostics))
	for _, d := range diagnostics {
		lines = append(lines, formatDiagnostic(d, "prealloc"))
	}
	return lines
}

func missingLines(want, got []string) []string {
	gotSet := make(map[string]bool, len(got))
	for _, line := range got {
		gotSet[line] = true
	}
	var missing []string
	for _, line := range want {
		if !gotSet[line] {
			missing = append(missing, line)
		}
	}
	return missing
}
