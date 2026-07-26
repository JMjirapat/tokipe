package agentkit_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The test inventory went stale twice — QA rounds 1 and 5, both filed as
// Blockers. Both times the fix was to type a fresh number, and both times it
// went stale again the moment the next regression test landed. A
// hand-maintained number in a document cannot stay true, so this test makes the
// claim self-checking: the doc declares counts in machine-readable markers and
// the build fails when they drift.
//
// It guards README.md, not docs/DELIVERY-1.md. That moved after Phase 5, when
// this very test failed the build: DELIVERY-1 is a frozen record of what shipped
// at tag v1.0.0, and forcing it to match HEAD would have destroyed its value as
// evidence. A guard belongs on a document that is supposed to describe the
// present.
//
// Deliberately NOT enforced here: the "run events" figure, which would require
// running the whole suite from inside the suite. The doc therefore states the
// command for it rather than a number.

var inventoryMarker = regexp.MustCompile(`<!-- inventory:([a-z-]+)=(\d+) -->`)

func TestDeliveryInventoryIsCurrent(t *testing.T) {
	const doc = "README.md"

	raw, err := os.ReadFile(doc)
	if err != nil {
		t.Fatalf("read %s: %v", doc, err)
	}

	declared := map[string]int{}
	for _, m := range inventoryMarker.FindAllStringSubmatch(string(raw), -1) {
		n, err := strconv.Atoi(m[2])
		if err != nil {
			t.Fatalf("marker %q has a non-numeric value: %v", m[0], err)
		}
		declared[m[1]] = n
	}
	if len(declared) == 0 {
		t.Fatalf("%s declares no inventory markers; this test cannot protect it", doc)
	}

	actual := map[string]int{
		"test-funcs": countTestFuncs(t),
		"packages":   countPackages(t),
	}

	for key, want := range declared {
		got, ok := actual[key]
		if !ok {
			t.Errorf("%s declares unknown marker %q", doc, key)
			continue
		}
		if got != want {
			t.Errorf("%s claims %s=%d, actual is %d — update the marker before sign-off",
				doc, key, want, got)
		}
	}
	for key := range actual {
		if _, ok := declared[key]; !ok {
			t.Errorf("%s has no marker for %q", doc, key)
		}
	}
}

// countTestFuncs mirrors the command the delivery doc publishes:
//
//	grep -rhoE '^func (Test|Example)[A-Za-z0-9_]*' --include='*_test.go' . | wc -l
//
// It walks from the repo root, so nested modules are included exactly as the
// shell command includes them.
func countTestFuncs(t *testing.T) int {
	t.Helper()
	funcRe := regexp.MustCompile(`(?m)^func (Test|Example)[A-Za-z0-9_]*`)

	count := 0
	err := filepath.WalkDir(repoRoot(t), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == ".git" || name == "docs" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		count += len(funcRe.FindAll(body, -1))
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	return count
}

// countPackages mirrors `go list ./... | wc -l`: every directory under the
// root module holding at least one non-test .go file. Nested modules are
// excluded, as `go list ./...` excludes them.
func countPackages(t *testing.T) int {
	t.Helper()
	root := repoRoot(t)

	pkgs := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "docs", ".github":
				return filepath.SkipDir
			}
			// A directory with its own go.mod is a separate module.
			if path != root {
				if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		pkgs[filepath.Dir(path)] = true
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	return len(pkgs)
}

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return wd // this test lives in the root package
}
