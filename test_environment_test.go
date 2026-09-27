package main

import (
	"context"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Ask Git for its repository selectors rather than maintaining a partial list.
// Author dates, pathspec settings, and the helper's own GIT_REMOTE_AWS_* settings
// are not repository selectors and must remain available to tests.
var testGitVariables = sync.OnceValue(func() map[string]bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	output, err := gitCommand(ctx, "rev-parse", "--local-env-vars").Output()
	if err != nil {
		panic(fmt.Errorf("inspect Git test environment: %w", err))
	}
	names := map[string]bool{}
	for _, name := range strings.Fields(string(output)) {
		names[name] = true
	}
	return names
})

// Test commands target their explicit directory. Inherited GIT_DIR, worktree,
// index, object, and config overrides must not redirect them into another repo.
// A test of such overrides can deliberately append them to the returned env.
func testEnvironment() []string {
	names := testGitVariables()
	var env []string
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if !names[name] {
			env = append(env, entry)
		}
	}
	return env
}

func TestBundleHistoricalIgnoresCallerGitRepository(t *testing.T) {
	// Build before injecting the caller's environment, so a redirected archive
	// cannot hide unsafe fixture preparation behind a missing-revision error.
	historicalHelper(t, preLibawsRevision, preLibawsLibsodium)
	for _, allOverrides := range []bool{false, true} {
		name := "GIT_DIR"
		if allOverrides {
			name = "all repository overrides"
		}
		t.Run(name, func(t *testing.T) {
			caller := t.TempDir()
			runAt(caller, "git", "init", "-q", "-b", "caller")
			configureGitIdentity(caller)
			runAt(caller, "git", "config", "user.name", "original owner")
			if err := os.WriteFile(filepath.Join(caller, "precious"), []byte("caller data\n"), 0600); err != nil {
				t.Fatal(err)
			}
			runAt(caller, "git", "add", "precious")
			runAt(caller, "git", "commit", "-qm", "original")
			if err := os.WriteFile(filepath.Join(caller, "precious"), []byte("staged caller data\n"), 0600); err != nil {
				t.Fatal(err)
			}
			runAt(caller, "git", "add", "precious")
			if err := os.WriteFile(filepath.Join(caller, "precious"), []byte("unstaged caller data\n"), 0600); err != nil {
				t.Fatal(err)
			}
			snapshot := func() map[string]string {
				files := map[string]string{}
				if err := filepath.WalkDir(caller, func(path string, entry fs.DirEntry, err error) error {
					if err != nil || entry.IsDir() {
						return err
					}
					body, err := os.ReadFile(path)
					files[path] = string(body)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				return files
			}
			before := snapshot()
			t.Cleanup(func() {
				if !maps.Equal(before, snapshot()) {
					t.Error("fixture commands changed the caller's worktree, config, index, refs, or objects")
				}
			})
			t.Setenv("GIT_DIR", filepath.Join(caller, ".git"))
			if allOverrides {
				for key, value := range map[string]string{
					"GIT_WORK_TREE": caller, "GIT_COMMON_DIR": filepath.Join(caller, ".git"),
					"GIT_INDEX_FILE":                   filepath.Join(caller, ".git", "index"),
					"GIT_OBJECT_DIRECTORY":             filepath.Join(caller, ".git", "objects"),
					"GIT_ALTERNATE_OBJECT_DIRECTORIES": filepath.Join(caller, ".git", "objects"),
					"GIT_CONFIG":                       filepath.Join(caller, ".git", "config"),
					"GIT_CONFIG_COUNT":                 "1", "GIT_CONFIG_KEY_0": "core.worktree", "GIT_CONFIG_VALUE_0": caller,
				} {
					t.Setenv(key, value)
				}
				// These existing tests call production Git operations in-process.
				// Pass the hostile env deliberately to exercise TestMain's entry
				// isolation, not just the subprocess command helpers.
				child := groupCommand(t.Context(), os.Args[0], "-test.run=^TestBundlePinnedRefOwnership$", "-test.timeout=20s")
				child.Env = os.Environ()
				if output, err := child.CombinedOutput(); err != nil {
					t.Fatalf("tests under caller Git environment: %v\n%s", err, output)
				}
			}
			if _, err := publishPreLibawsHistory(t, "sha1"); err != nil {
				t.Fatal(err)
			}
			// The result-returning command helper must also target its explicit
			// directory, rather than the caller's repository.
			dir := t.TempDir()
			if _, stderr, err := runAtResult(dir, "git", "init", "-q", "-b", "fixture"); err != nil {
				t.Fatalf("initialize fixture: %v\n%s", err, stderr)
			}
			if out, stderr, err := runAtResult(dir, "git", "symbolic-ref", "--short", "HEAD"); err != nil || strings.TrimSpace(out) != "fixture" {
				t.Fatalf("read fixture branch: %q, %v\n%s", out, err, stderr)
			}
			// Archival must read this source clone even when a test changes cwd
			// and the inherited environment points at an unrelated repository.
			t.Chdir(caller)
			if _, err := buildHistoricalHelper(preLibawsRevision, preLibawsLibsodium); err != nil {
				t.Fatal(err)
			}
		})
	}
}
