package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRefSlashBranchRoundTrip(t *testing.T) {
	for _, objectFormat := range []string{"sha1", "sha256"} {
		t.Run(objectFormat, func(t *testing.T) {
			public := setupEphemeralKeys(t)
			dir := t.TempDir()
			runAt(dir, "git", "init", "-q", "--object-format="+objectFormat, "-b", "archive/home")
			configureGitIdentity(dir)
			if err := os.WriteFile(filepath.Join(dir, ".publickeys"), []byte(public+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			runAt(dir, "git", "add", ".publickeys")
			runAt(dir, "git", "commit", "-qm", "base")
			base := runAtOut(dir, "git", "rev-parse", "HEAD")

			fixture := newMetadataFixture(t)
			runHelper := func(directory, command string) (string, error) {
				t.Helper()
				return runMetadataHelper(t, directory, fixture.server.URL, command)
			}
			push := "push refs/heads/archive/home:refs/heads/archive/home"
			if output, err := runHelper(dir, push); err != nil {
				t.Fatalf("publish slash branch genesis: %v\n%s", err, output)
			}
			if err := os.WriteFile(filepath.Join(dir, "payload"), []byte("slash branch payload\n"), 0600); err != nil {
				t.Fatal(err)
			}
			runAt(dir, "git", "add", "payload")
			runAt(dir, "git", "commit", "-qm", "next")
			tip := runAtOut(dir, "git", "rev-parse", "HEAD")
			if output, err := runHelper(dir, push); err != nil {
				t.Fatalf("publish slash branch delta: %v\n%s", err, output)
			}
			if output, err := runHelper(dir, push); err != nil {
				t.Fatalf("no-op slash branch push: %v\n%s", err, output)
			}
			runAt(dir, "git", "branch", "archive/other")
			for command, message := range map[string]string{
				"push refs/heads/archive/other:refs/heads/archive/other": "you cannot have multiple branches in a remote",
				"push refs/heads/archive/home:refs/heads/archive/other":  "local branch is different from remote branch",
				"push +refs/heads/archive/home:refs/heads/archive/home":  "force push is not allowed",
			} {
				if output, err := runHelper(dir, command); err == nil || !strings.Contains(output, message) {
					t.Errorf("lost single-branch/force protection: %v\n%s", err, output)
				}
			}
			if output, err := runHelper(dir, "list"); err != nil || !strings.Contains(output, tip+" refs/heads/archive/home\n") || !strings.Contains(output, "@refs/heads/archive/home HEAD\n") {
				t.Fatalf("discover slash branch: %v\n%s", err, output)
			}
			fresh := t.TempDir()
			runAt(fresh, "git", "init", "-q", "--object-format="+objectFormat, "-b", "archive/home")
			if output, err := runHelper(fresh, "fetch "+tip+" refs/heads/archive/home"); err != nil {
				t.Fatalf("fetch actual encrypted slash-branch chain: %v\n%s", err, output)
			}
			if got := runAtOut(fresh, "git", "rev-parse", tip+"^"); got != base {
				t.Fatalf("fetched wrong ancestry: %s, want %s", got, base)
			}
			if got := runAtOut(fresh, "git", "show", tip+":payload"); got != "slash branch payload" {
				t.Fatalf("fetched wrong content: %q", got)
			}
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			if fixture.commits != 2 || fixture.uploads != 4 || fixture.missing != 0 || !bytes.Contains(fixture.data, []byte(`"branch":{"S":"archive/home"}`)) {
				t.Fatalf("wrong published metadata: commits=%d uploads=%d missing=%d data=%s", fixture.commits, fixture.uploads, fixture.missing, fixture.data)
			}
		})
	}
}

func TestRefBranchContextAndLiteralName(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	mustPanicContains(t, "context canceled", func() { refBranch(ctx, "refs/heads/main") })

	dir := t.TempDir()
	runAt(dir, "git", "init", "-q", "-b", "main")
	configureGitIdentity(dir)
	runAt(dir, "git", "commit", "--allow-empty", "-qm", "base")
	runAt(dir, "git", "checkout", "-qb", "other")
	t.Chdir(dir)
	if got := runAtOut(dir, "git", "check-ref-format", "--branch", "@{-1}"); got != "main" {
		t.Fatalf("checkout-expression control did not expand: %q", got)
	}
	mustPanicContains(t, "literal branch", func() { refBranch(t.Context(), "refs/heads/@{-1}") })
}
