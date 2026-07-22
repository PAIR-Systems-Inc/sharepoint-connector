package config

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// TestEnvDocsInSyncWithCode guards against drift between the documented config
// surface (.env.example) and what the code actually reads. It caught nothing at
// the time of writing, but the class of bug it prevents is real: SHAREPOINT_
// SEARCH_SCOPE / _START_DATE were once documented as if they worked while being
// dead. Two directions:
//
//   - Forward: every var documented in .env.example is referenced somewhere in
//     the Go source or the deploy script (no documented-but-nonexistent vars).
//   - Reverse: every os.Getenv/os.LookupEnv("X") literal in the Go source is
//     documented in .env.example (no read-but-undocumented vars).
func TestEnvDocsInSyncWithCode(t *testing.T) {
	root := repoRoot(t)
	envExample := readFile(t, filepath.Join(root, ".env.example"))
	deployScript := readFile(t, filepath.Join(root, "deploy_fly_io.sh"))
	goSrc := readGoSources(t, filepath.Join(root, "cmd"), filepath.Join(root, "internal"))

	// Forward: documented KEY= / # KEY= lines must appear in code (Go or bash).
	documented := regexp.MustCompile(`(?m)^\s*#?\s*([A-Z][A-Z0-9_]{2,})\s*=`).FindAllStringSubmatch(envExample, -1)
	seen := map[string]bool{}
	for _, m := range documented {
		v := m[1]
		if seen[v] {
			continue
		}
		seen[v] = true
		if !strings.Contains(goSrc, `"`+v+`"`) && !strings.Contains(deployScript, v) {
			t.Errorf("env var %q is documented in .env.example but referenced nowhere in the code or deploy script (dead/typo?)", v)
		}
	}

	// Reverse: direct os.Getenv/os.LookupEnv literals must be documented.
	reads := regexp.MustCompile(`os\.(?:Getenv|LookupEnv)\("([A-Z][A-Z0-9_]*)"\)`).FindAllStringSubmatch(goSrc, -1)
	for _, m := range reads {
		v := m[1]
		if !strings.Contains(envExample, v) {
			t.Errorf("env var %q is read by the code (os.Getenv) but not documented in .env.example", v)
		}
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate test file")
	}
	// this file is internal/config/envdoc_test.go → repo root is two levels up.
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// readGoSources concatenates all non-test .go files under the given dirs.
func readGoSources(t *testing.T, dirs ...string) string {
	t.Helper()
	var sb strings.Builder
	for _, dir := range dirs {
		err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			sb.Write(b)
			sb.WriteByte('\n')
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}
	return sb.String()
}
