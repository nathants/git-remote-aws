package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestKeyPolicyRejectsUncommittedChanges(t *testing.T) {
	for _, change := range []string{"unstaged", "staged", "staged-only", "deleted", "staged-deletion", "assume-unchanged", "executable", "group-executable", "unrelated", "no-index"} {
		t.Run(change, func(t *testing.T) {
			dir := t.TempDir()
			t.Chdir(dir)
			runAt(dir, "git", "init", "-q")
			configureGitIdentity(dir)
			original := strings.Repeat("1", 64) + "\n"
			write := func(value string) {
				t.Helper()
				if err := os.WriteFile(".publickeys", []byte(value), 0644); err != nil {
					t.Fatal(err)
				}
			}
			write(original)
			runAt(dir, "git", "add", ".publickeys")
			runAt(dir, "git", "commit", "-qm", "keys")
			tip := runAtOut(dir, "git", "rev-parse", "HEAD")
			switch change {
			case "executable", "group-executable":
				mode := os.FileMode(0744)
				if change == "group-executable" {
					mode = 0654 // Git tracks only the owner-executable bit.
				}
				if err := os.Chmod(".publickeys", mode); err != nil {
					t.Fatal(err)
				}
			case "no-index":
				// Commits created through Git plumbing need not have an index.
				if err := os.Remove(filepath.Join(".git", "index")); err != nil {
					t.Fatal(err)
				}
			case "staged-deletion":
				runAt(dir, "git", "rm", "--cached", ".publickeys")
			case "unrelated":
				if err := os.WriteFile("unrelated", []byte("uncommitted"), 0600); err != nil {
					t.Fatal(err)
				}
			case "deleted":
				if err := os.Remove(".publickeys"); err != nil {
					t.Fatal(err)
				}
			default:
				if change == "assume-unchanged" {
					runAt(dir, "git", "update-index", "--assume-unchanged", ".publickeys")
				}
				write("\n" + original) // Even a semantically irrelevant edit must be committed.
				if change == "staged" || change == "staged-only" {
					runAt(dir, "git", "add", ".publickeys")
				}
				if change == "staged-only" {
					write(original)
				}
			}
			_, err := pushRecipients(tip, tip)
			if change == "unrelated" || change == "no-index" || change == "group-executable" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), ".publickeys") {
				t.Fatalf("uncommitted policy was not rejected: %v", err)
			}
		})
	}
}

func TestKeyPolicyAllowsOldSingleLineFormatting(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	runAt(dir, "git", "init", "-q")
	configureGitIdentity(dir)
	key := strings.Repeat("1", 64)
	if err := os.WriteFile(".publickeys", []byte("\n"+key), 0644); err != nil {
		t.Fatal(err)
	}
	runAt(dir, "git", "add", ".publickeys")
	runAt(dir, "git", "commit", "-qm", "existing single-line policy")
	base := runAtOut(dir, "git", "rev-parse", "HEAD")
	if err := os.WriteFile(".publickeys", []byte(key+":"+strings.Repeat("2", 64)+"\n\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runAt(dir, "git", "add", ".publickeys")
	runAt(dir, "git", "commit", "-qm", "rotate")
	tip := runAtOut(dir, "git", "rev-parse", "HEAD")
	// A helper invoked below the worktree root must still check the root policy.
	if err := os.Mkdir("subdir", 0700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(filepath.Join(dir, "subdir"))
	if _, err := pushRecipients(base, tip); err != nil {
		t.Fatal(err)
	}
}

func TestBundleRejectsTipChangeDuringCreation(t *testing.T) {
	for _, objectFormat := range []string{"sha1", "sha256"} {
		for _, incremental := range []bool{false, true} {
			t.Run(objectFormat+map[bool]string{false: "/full", true: "/incremental"}[incremental], func(t *testing.T) {
				dir := t.TempDir()
				t.Chdir(dir)
				runAt(dir, "git", "init", "-q", "--initial-branch=main", "--object-format="+objectFormat)
				configureGitIdentity(dir)
				commit := func(value string) string {
					t.Helper()
					if err := os.WriteFile("file", []byte(value), 0600); err != nil {
						t.Fatal(err)
					}
					runAt(dir, "git", "add", "file")
					runAt(dir, "git", "commit", "-qm", value)
					return runAtOut(dir, "git", "rev-parse", "HEAD")
				}
				base := commit("base")
				tip := commit("selected")
				target := "refs/heads/main"
				if incremental {
					target = base + ".." + target
				}
				if err := createPushBundle(filepath.Join(t.TempDir(), "good.bundle"), target, tip); err != nil {
					t.Fatal(err)
				}
				// Simulate a local commit landing after push selected its tip and
				// recipients, but before Git resolves the branch for bundle creation.
				commit("concurrent local commit")
				if err := createPushBundle(filepath.Join(t.TempDir(), "raced.bundle"), target, tip); err == nil || !strings.Contains(err.Error(), "tip changed") {
					t.Fatalf("bundle with wrong recorded identity was accepted: %v", err)
				}
			})
		}
	}
}

func TestKeyPolicyAdoptsAbsentHistoricalFileOnly(t *testing.T) {
	for _, prior := range []string{"absent", "malformed", "blank", "symlink"} {
		t.Run(prior, func(t *testing.T) {
			dir := t.TempDir()
			t.Chdir(dir)
			runAt(dir, "git", "init", "-q")
			configureGitIdentity(dir)
			if prior == "symlink" {
				if err := os.Symlink("elsewhere", ".publickeys"); err != nil {
					t.Fatal(err)
				}
			} else if prior != "absent" {
				text := "malformed"
				if prior == "blank" {
					text = "\n"
				}
				if err := os.WriteFile(".publickeys", []byte(text), 0644); err != nil {
					t.Fatal(err)
				}
			}
			runAt(dir, "git", "add", ".")
			runAt(dir, "git", "commit", "--allow-empty", "-qm", "previous history")
			base := runAtOut(dir, "git", "rev-parse", "HEAD")
			if prior == "symlink" {
				if err := os.Remove(".publickeys"); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(".publickeys", []byte(strings.Repeat("1", 64)), 0644); err != nil {
				t.Fatal(err)
			}
			runAt(dir, "git", "add", ".")
			runAt(dir, "git", "commit", "-qm", "committed recipients")
			tip := runAtOut(dir, "git", "rev-parse", "HEAD")
			_, err := pushRecipients(base, tip)
			if (err == nil) != (prior == "absent") {
				t.Fatalf("baseline %s: %v", prior, err)
			}
		})
	}
}

func TestKeyPolicyLiteralCallerEnvironment(t *testing.T) {
	t.Setenv("GIT_LITERAL_PATHSPECS", "1")
	TestKeyPolicyRejectsUncommittedChanges(t)
}
