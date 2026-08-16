package orchestrate

import (
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// ORCH-003's structural half. TestLocalhostRunsTheIdenticalPath proves the
// paths this tree runs today are the same for a loopback host as for a
// remote one; it cannot prove that a path added tomorrow will be. This
// guard reads the source instead of running it, and fails when product code
// starts asking whether a host is local.
//
// It flags the three ways that question gets asked in Go: comparing
// something to a loopback name, switching on one, and calling net.IP's
// IsLoopback. Mentioning a loopback address is fine and stays fine —
// orchestrate.LoopbackAddress publishes a container port on the host's
// loopback interface, api.DefaultAddr binds the dashboard to it, and
// forge's smoke job reaches Forgejo through it. None of those decide
// anything about where a host is; they name an address to use. The guard
// looks only for the deciding.
//
// It reads product code alone. Tests compare against 127.0.0.1 constantly —
// this file's own sibling starts servers there — and the invariant is about
// what Farrier does to a host, not about how it is tested.

// loopbackNames are the spellings that identify a host as the local
// machine. Comparing against any of them in product code is the locality
// branch ORCH-003 forbids.
var loopbackNames = map[string]bool{
	"localhost": true,
	"127.0.0.1": true,
	"::1":       true,
	"[::1]":     true,
}

// localityExemption records a comparison that reads like a locality branch
// but is not one, with the reason it is allowed. Each is matched on the
// rendered source of the expression rather than a line number, so it
// survives the file moving and stops matching the moment the code changes.
type localityExemption struct {
	file string
	expr string
	why  string
}

var localityExemptions = []localityExemption{{
	file: "internal/core/registry/registry.go",
	expr: `first == "localhost"`,
	why: "container image reference parsing: decides whether the first path " +
		"component of an image name is a registry host, which is unrelated to " +
		"any host Farrier deploys to",
}, {
	file: "internal/core/deploy/ciaddress.go",
	expr: `ip.IsLoopback()`,
	why: "the address the forge's web UI is served at, not the host it is " +
		"deployed to: a loopback one is refused when the deployment carries " +
		"CI (UP-006), because it is what job containers are told to clone " +
		"from and inside a container it names the container. The SSH target " +
		"is untouched — ssh://user@localhost still runs the identical path, " +
		"and is the ordinary way to reach the host this refusal fires on",
}, {
	file: "internal/core/deploy/ciaddress.go",
	expr: `name == "localhost"`,
	why: "the same served-address check, spelled as a hostname: RFC 6761 " +
		"reserves localhost for the loopback interface, so it is the same " +
		"unreachable-from-a-container address by another name",
}}

// scannedRoots are the directories holding product code. tools/ is agent
// orchestration and never ships in the binary, and web/ is not Go.
var scannedRoots = []string{"internal", "cmd"}

func TestNoLocalityBranchInProductCode(t *testing.T) {
	root := repoRoot(t)
	used := make(map[int]bool)

	for _, dir := range scannedRoots {
		walkRoot := filepath.Join(root, dir)
		err := filepath.WalkDir(walkRoot, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}

			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)

			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				return err
			}

			for _, found := range localityBranches(fset, file) {
				if i, ok := exemptionFor(rel, found.expr); ok {
					used[i] = true
					continue
				}
				t.Errorf("%s:%d: branches on whether a host is local: %s\n"+
					"ORCH-003: ssh://user@localhost runs the identical path as any remote host. "+
					"If this comparison is about something other than a host's locality, add it to "+
					"localityExemptions in %s with the reason.",
					rel, fset.Position(found.pos).Line, found.expr, "locality_guard_test.go")
			}
			return nil
		})
		if err != nil {
			t.Fatalf("scan %s: %v", dir, err)
		}
	}

	// An exemption that no longer matches anything is stale, and a stale
	// exemption is a hole nobody is watching.
	for i, ex := range localityExemptions {
		if !used[i] {
			t.Errorf("localityExemptions entry %d (%s: %s) matched nothing — delete it", i, ex.file, ex.expr)
		}
	}
}

// localityFinding is one place the source decides something from a host's
// locality.
type localityFinding struct {
	pos  token.Pos
	expr string
}

// localityBranches reports every locality test in file: equality against a
// loopback name, a switch case on one, or a call to net.IP.IsLoopback.
func localityBranches(fset *token.FileSet, file *ast.File) []localityFinding {
	var found []localityFinding
	render := func(n ast.Node) string {
		var b strings.Builder
		if err := format.Node(&b, fset, n); err != nil {
			return "<unprintable expression>"
		}
		return b.String()
	}

	ast.Inspect(file, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.BinaryExpr:
			if node.Op != token.EQL && node.Op != token.NEQ {
				return true
			}
			if isLoopbackLiteral(node.X) || isLoopbackLiteral(node.Y) {
				found = append(found, localityFinding{pos: node.Pos(), expr: render(node)})
			}
		case *ast.CaseClause:
			for _, expr := range node.List {
				if isLoopbackLiteral(expr) {
					found = append(found, localityFinding{pos: expr.Pos(), expr: "case " + render(expr)})
				}
			}
		case *ast.CallExpr:
			if sel, ok := node.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "IsLoopback" {
				found = append(found, localityFinding{pos: node.Pos(), expr: render(node)})
			}
		}
		return true
	})
	return found
}

// isLoopbackLiteral reports whether expr is a string literal naming the
// local machine.
func isLoopbackLiteral(expr ast.Expr) bool {
	lit, ok := expr.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return false
	}
	value, err := strconv.Unquote(lit.Value)
	if err != nil {
		return false
	}
	return loopbackNames[value]
}

func exemptionFor(file, expr string) (int, bool) {
	for i, ex := range localityExemptions {
		if ex.file == file && ex.expr == expr {
			return i, true
		}
	}
	return 0, false
}

// repoRoot walks up from the test's working directory to the module root,
// so the guard can read the whole tree regardless of where `go test` was
// invoked from.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("working directory: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod above %s", dir)
		}
		dir = parent
	}
}
