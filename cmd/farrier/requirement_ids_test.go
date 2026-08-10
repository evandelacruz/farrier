package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// requirementID matches the shape every requirement ID in
// docs/functional-requirements.md takes: two to six capitals, a hyphen, three
// digits (UP-006, KEY-003, ACME-002).
var requirementID = regexp.MustCompile(`[A-Z]{2,6}-[0-9]{3}`)

// guardedTrees are the source roots that produce the product binary, and so
// everything an operator can be shown. tools/ is deliberately absent: it is
// agent orchestration, its readers are agents, and IDs are its vocabulary.
var guardedTrees = []string{"cmd", "internal", "web"}

// allowedRequirementIDLiterals is the exception list: a repo-relative file
// path mapped to the exact string literals in it that may carry an ID. It is
// empty, and staying empty is the point — an entry here is a deliberate,
// reviewable decision that some operator-facing string genuinely needs
// build-time vocabulary in it, rather than an ID that slipped through.
var allowedRequirementIDLiterals = map[string][]string{}

// TestNoRequirementIDsInStringLiterals holds the line CLAUDE.md draws:
// requirement IDs are build-time vocabulary. They belong in comments, doc
// comments, docs/, commit messages, and PR bodies, where agents and reviewers
// need them to find the requirement a change answers. They do not belong in
// anything an operator reads at runtime — an event detail, an error, a flag
// description — where the ID names a document the reader does not have and
// displaces the sentence that would have told them what to do.
//
// The check walks the AST rather than grepping, because the distinction it
// enforces is exactly the one a grep cannot make: a comment and a string
// literal look identical to a regexp, and comments are where the IDs are
// supposed to live. go/parser drops comments unless asked for them, so an
// ast.Inspect over BasicLit nodes sees only the strings that can reach a
// terminal.
//
// Test files are excluded. They never ship in the binary, and a failure
// message citing the requirement it is defending is read by the same audience
// the ID is for.
func TestNoRequirementIDsInStringLiterals(t *testing.T) {
	root := repoRoot(t)

	for _, tree := range guardedTrees {
		err := filepath.WalkDir(filepath.Join(root, tree), func(path string, d fs.DirEntry, err error) error {
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
			for _, found := range requirementIDLiterals(t, path) {
				if allowed(rel, found.value) {
					continue
				}
				t.Errorf("%s:%d: string literal names requirement %s: %q\n"+
					"requirement IDs are build-time vocabulary — say what the ID stands for, "+
					"or point at docs/security.md or docs/operating.md if the reader needs more",
					rel, found.line, strings.Join(found.ids, ", "), found.value)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", tree, err)
		}
	}
}

type idLiteral struct {
	line  int
	value string
	ids   []string
}

// requirementIDLiterals parses one file and returns its string literals that
// carry a requirement ID. A parse failure fails the test rather than being
// skipped: a guard that silently passes on the files it could not read is
// worse than none.
func requirementIDLiterals(t *testing.T, path string) []idLiteral {
	t.Helper()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	var found []idLiteral
	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		// Unquote so a raw literal and an interpreted one are compared as
		// the same text, and so an escaped ID is caught too.
		value, err := strconv.Unquote(lit.Value)
		if err != nil {
			value = lit.Value
		}
		if ids := requirementID.FindAllString(value, -1); len(ids) > 0 {
			found = append(found, idLiteral{
				line:  fset.Position(lit.Pos()).Line,
				value: value,
				ids:   ids,
			})
		}
		return true
	})
	return found
}

func allowed(relPath, value string) bool {
	for _, exception := range allowedRequirementIDLiterals[filepath.ToSlash(relPath)] {
		if exception == value {
			return true
		}
	}
	return false
}

// repoRoot walks up from the test's working directory to the module root, so
// the guard covers the whole tree no matter which package it is invoked from.
func repoRoot(t *testing.T) string {
	t.Helper()

	dir, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("working directory: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod found above the test's working directory")
		}
		dir = parent
	}
}
