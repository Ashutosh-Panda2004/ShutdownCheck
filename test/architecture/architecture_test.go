// Package architecture holds structural tests that enforce the design
// invariants in shutdowncheck-spec.md. They are ordinary Go tests so that a
// violation fails CI on the pull request that introduces it, rather than being
// discovered later during a review.
package architecture

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// purePackages must be provably free of I/O and of clock reads so that
// Analyze(Timeline, Policy) -> Report stays a pure function. Every verdict the
// tool produces depends on this being true; see spec sections 7.1 and 15.
//
// internal/timeline is guarded for a structural reason rather than a stylistic
// one: analysis consumes it, so anything it could reach, analysis could reach
// transitively. That is also why the clock lives in internal/clock instead.
var purePackages = []string{
	"internal/analyze",
	"internal/timeline",
	"pkg/schema",
}

// deniedExact and deniedPrefixes describe imports that would let a pure package
// reach the outside world. net/url is deliberately absent: it only parses.
var (
	deniedExact = map[string]bool{
		"net":          true,
		"net/http":     true,
		"net/rpc":      true,
		"net/smtp":     true,
		"os":           true,
		"syscall":      true,
		"log":          true,
		"log/slog":     true,
		"io/ioutil":    true,
		"database/sql": true,
		"plugin":       true,
		"unsafe":       true,
	}
	deniedPrefixes = []string{
		"os/",
		"syscall/",
		"net/http/",
		"math/rand",
		"runtime/pprof",
		"runtime/trace",
		"golang.org/x/net/",
		"golang.org/x/sys/",
	}
)

// forbiddenTimeCalls are the time package entry points that read or wait on the
// wall/monotonic clock. Pure packages may still use time.Duration and
// time.Time values that were recorded elsewhere.
var forbiddenTimeCalls = map[string]bool{
	"Now":       true,
	"Since":     true,
	"Until":     true,
	"Sleep":     true,
	"After":     true,
	"Tick":      true,
	"NewTimer":  true,
	"NewTicker": true,
	"AfterFunc": true,
}

// skipDirs are excluded from the import graph: they either contain no Go, or
// contain intentionally-broken fixture servers that must not constrain the
// production packages. Hidden directories are skipped separately.
var skipDirs = map[string]bool{
	"bin":              true,
	"docs":             true,
	"action":           true,
	"vendor":           true,
	"testdata":         true,
	"node_modules":     true,
	"test/conformance": true,
}

type pkg struct {
	importPath string
	dir        string
	imports    []string
}

func TestAnalyzeAndSchemaArePure(t *testing.T) {
	root := repoRoot(t)
	module := modulePath(t, root)
	pkgs := loadPackages(t, root, module)

	for _, rel := range purePackages {
		importPath := module + "/" + rel
		if _, ok := pkgs[importPath]; !ok {
			t.Fatalf("guarded package %q not found; update purePackages if it moved", rel)
		}

		for _, imp := range transitiveExternalImports(pkgs, importPath) {
			if denied(imp) {
				t.Errorf("%s must stay pure but reaches %q\n"+
					"    Pure packages may not perform I/O. If you need this, put it behind an\n"+
					"    interface in a caller and pass the data in. See docs/adr/0011-pure-analysis-core.md",
					rel, imp)
			}
		}
	}
}

func TestPurePackagesDoNotReadTheClock(t *testing.T) {
	root := repoRoot(t)

	for _, rel := range purePackages {
		dir := filepath.Join(root, filepath.FromSlash(rel))
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}

		for _, entry := range entries {
			if entry.IsDir() || !nonTestGoFileName(entry.Name()) {
				continue
			}

			fset := token.NewFileSet()
			f, parseErr := parser.ParseFile(fset, filepath.Join(dir, entry.Name()), nil, 0)
			if parseErr != nil {
				t.Fatalf("parse %s/%s: %v", rel, entry.Name(), parseErr)
			}

			ast.Inspect(f, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				ident, ok := sel.X.(*ast.Ident)
				if !ok || ident.Name != "time" || !forbiddenTimeCalls[sel.Sel.Name] {
					return true
				}
				t.Errorf("%s calls time.%s at %s\n"+
					"    Pure packages must not read or wait on the clock; analysis operates only\n"+
					"    on timestamps already recorded in the timeline.",
					rel, sel.Sel.Name, fset.Position(sel.Pos()))
				return true
			})
		}
	}
}

// TestCommandPackagesContainNoLogic keeps cmd/ a thin entry point: it may wire
// up internal/cli, but business logic must not accumulate there.
func TestCommandPackagesContainNoLogic(t *testing.T) {
	root := repoRoot(t)
	module := modulePath(t, root)
	pkgs := loadPackages(t, root, module)

	allowed := module + "/internal/cli"
	for path, p := range pkgs {
		if !strings.HasPrefix(path, module+"/cmd/") {
			continue
		}
		for _, imp := range p.imports {
			if !strings.HasPrefix(imp, module+"/") {
				continue
			}
			if imp != allowed && !strings.HasPrefix(imp, allowed+"/") {
				t.Errorf("%s imports %q; cmd/ may only import %s", path, imp, allowed)
			}
		}
	}
}

// TestDeniedMatching self-checks the rule engine above, so a broken denylist
// cannot silently turn these guards into no-ops.
func TestDeniedMatching(t *testing.T) {
	cases := map[string]bool{
		"net":                   true,
		"net/http":              true,
		"net/http/httptrace":    true,
		"os":                    true,
		"os/exec":               true,
		"math/rand":             true,
		"math/rand/v2":          true,
		"golang.org/x/sys/unix": true,

		"net/url":       false,
		"time":          false,
		"fmt":           false,
		"encoding/json": false,
		"strings":       false,
		"math":          false,
		"sort":          false,
	}
	for imp, want := range cases {
		if got := denied(imp); got != want {
			t.Errorf("denied(%q) = %v, want %v", imp, got, want)
		}
	}
}

func denied(importPath string) bool {
	if deniedExact[importPath] {
		return true
	}
	for _, p := range deniedPrefixes {
		if strings.HasPrefix(importPath, p) {
			return true
		}
	}
	return false
}

// transitiveExternalImports returns every non-repo import reachable from start,
// following repo-local imports. Direct-only checking would miss a pure package
// laundering I/O through an internal helper.
func transitiveExternalImports(pkgs map[string]*pkg, start string) []string {
	external := map[string]bool{}
	seen := map[string]bool{}

	var visit func(string)
	visit = func(path string) {
		if seen[path] {
			return
		}
		seen[path] = true

		p, ok := pkgs[path]
		if !ok {
			return
		}
		for _, imp := range p.imports {
			if _, local := pkgs[imp]; local {
				visit(imp)
				continue
			}
			external[imp] = true
		}
	}
	visit(start)

	out := make([]string, 0, len(external))
	for imp := range external {
		out = append(out, imp)
	}
	sort.Strings(out)
	return out
}

func loadPackages(t *testing.T, root, module string) map[string]*pkg {
	t.Helper()

	pkgs := map[string]*pkg{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}

		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		slashRel := filepath.ToSlash(rel)

		if d.IsDir() {
			if slashRel == "." {
				return nil
			}
			if strings.HasPrefix(d.Name(), ".") || skipDirs[d.Name()] || skipDirs[slashRel] {
				return filepath.SkipDir
			}
			return nil
		}

		if !nonTestGoFileName(d.Name()) {
			return nil
		}

		fset := token.NewFileSet()
		f, parseErr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if parseErr != nil {
			return parseErr
		}

		dir := filepath.ToSlash(filepath.Dir(rel))
		importPath := module
		if dir != "." {
			importPath = module + "/" + dir
		}

		p, ok := pkgs[importPath]
		if !ok {
			p = &pkg{importPath: importPath, dir: dir}
			pkgs[importPath] = p
		}
		for _, spec := range f.Imports {
			unquoted, uErr := strconv.Unquote(spec.Path.Value)
			if uErr != nil {
				return uErr
			}
			p.imports = append(p.imports, unquoted)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk repository: %v", err)
	}
	return pkgs
}

func nonTestGoFileName(name string) bool {
	return strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go")
}

func repoRoot(t *testing.T) string {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not locate go.mod above the test directory")
		}
		dir = parent
	}
}

func modulePath(t *testing.T, root string) string {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if after, ok := strings.CutPrefix(line, "module "); ok {
			return strings.TrimSpace(after)
		}
	}
	t.Fatal("go.mod has no module directive")
	return ""
}
