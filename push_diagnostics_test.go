package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Exercise Git's own rejection after list for-push. With a missing remote
// object Git also enters the helper's push handler, which must refuse with the
// same identities. The provider is local; real Git ancestry is not mocked.
func TestBundlePushPreflightDiagnostics(t *testing.T) {
	buildGitRemoteAws(t)
	for _, scenario := range []string{"diverged", "behind", "missing-remote-object"} {
		t.Run(scenario, func(t *testing.T) {
			public := setupEphemeralKeys(t)
			dir := t.TempDir()
			runAt(dir, "git", "init", "-q", "-b", "master")
			configureGitIdentity(dir)
			if err := os.WriteFile(filepath.Join(dir, ".publickeys"), []byte(public+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			runAt(dir, "git", "add", ".publickeys")
			runAt(dir, "git", "commit", "-qm", "base")
			base := runAtOut(dir, "git", "rev-parse", "HEAD")
			fixture := newMetadataFixture(t)
			runAt(dir, "git", "remote", "add", "origin", "aws://bucket+table/repo")
			writer := dir
			if scenario == "missing-remote-object" {
				writer = t.TempDir()
				runAt("", "git", "clone", "-q", "--no-hardlinks", dir, writer)
				configureGitIdentity(writer)
			}
			runAt(writer, "git", "commit", "--allow-empty", "-qm", "remote")
			remote := runAtOut(writer, "git", "rev-parse", "HEAD")
			if output, err := runMetadataHelper(t, writer, fixture.server.URL, "push refs/heads/master:refs/heads/master"); err != nil {
				t.Fatalf("publish fixture: %v\n%s", err, output)
			}
			if writer == dir {
				runAt(dir, "git", "reset", "--hard", base)
			}
			if scenario == "diverged" {
				runAt(dir, "git", "commit", "--allow-empty", "-qm", "local")
			}
			local := runAtOut(dir, "git", "rev-parse", "HEAD")
			fixture.mu.Lock()
			commits, uploads, checks := fixture.commits, fixture.uploads, fixture.versioningChecks
			fixture.mu.Unlock()
			command := gitCommand(t.Context(), "push", "origin", "master")
			command.Dir, command.Env = dir, append(offlineEnvironment(fixture.server.URL), "ensure=")
			var stdout, stderr bytes.Buffer
			command.Stdout, command.Stderr = &stdout, &stderr
			if err := command.Run(); err == nil {
				t.Fatal("divergent push succeeded")
			}
			output := stderr.String()
			for _, expected := range []string{"[rejected]", "local=" + local, "remote=" + remote, "ref=refs/heads/master"} {
				if !strings.Contains(output, expected) {
					t.Fatalf("missing diagnostic %q:\n%s", expected, output)
				}
			}
			expected := "local-only=0 remote-only=1"
			if scenario == "diverged" {
				expected = "local-only=1 remote-only=1"
			}
			if scenario == "missing-remote-object" {
				expected = "remote tip unavailable locally; fetch to compare ancestry"
			}
			if !strings.Contains(output, expected) {
				t.Fatalf("missing ancestry %q:\n%s", expected, output)
			}
			if scenario != "missing-remote-object" && !strings.Contains(output, "merge-bases="+base) {
				t.Fatalf("missing merge base:\n%s", output)
			}
			if scenario == "missing-remote-object" {
				checks++
				if strings.Count(output, "push comparison: ref=") != 2 || !strings.Contains(output, "remote has new commits, pull before pushing") {
					t.Fatalf("missing locked push-handler refusal diagnostics:\n%s", output)
				}
			}
			if listed, err := runMetadataHelper(t, dir, fixture.server.URL, "list"); err != nil || strings.Contains(listed, "push comparison") || !strings.Contains(listed, remote+" refs/heads/master") {
				t.Fatalf("ordinary ref discovery changed: %v\n%s", err, listed)
			}
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			if fixture.commits != commits || fixture.uploads != uploads || fixture.versioningChecks != checks {
				t.Fatalf("provider counts commits=%d/%d uploads=%d/%d versioning=%d/%d:\n%s", fixture.commits, commits, fixture.uploads, uploads, fixture.versioningChecks, checks, output)
			}
			if strings.Contains(stdout.String(), "push comparison") {
				t.Fatal("diagnostics polluted Git's stdout protocol")
			}
		})
	}
}
