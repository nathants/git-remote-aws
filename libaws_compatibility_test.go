package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestBundleLibawsRemovalCompatibility(t *testing.T) {
	for _, objectFormat := range []string{"sha1", "sha256"} {
		t.Run(objectFormat, func(t *testing.T) {
			saved := preLibawsFixture(t, objectFormat)
			t.Setenv("GIT_REMOTE_AWS_SECRETKEY", saved.SecretKey)
			t.Setenv("GIT_REMOTE_AWS_SECRETKEY_FILE", "")
			t.Setenv("GIT_REMOTE_AWS_SECRETKEY_CMD", "")
			t.Setenv("GIT_REMOTE_AWS_PUBLICKEY", saved.PublicKey)
			fixture := newMetadataFixture(t)
			fixture.mu.Lock()
			fixture.data, fixture.objects = saved.Data, make(map[string][]byte)
			for key, body := range saved.Objects {
				fixture.objects[key] = append([]byte(nil), body...)
			}
			fixture.mu.Unlock()
			dir := t.TempDir()
			runAt(dir, "git", "init", "-q", "--object-format="+saved.Format, "-b", "archive/home")
			configureGitIdentity(dir)
			for _, command := range []string{"list", "fetch " + saved.Tip + " refs/heads/archive/home"} {
				child := testHelperCommand(t, dir, fixture.server.URL, "20s")
				child.Env = append(child.Env, "ensure=y", "AWS_REQUEST_CHECKSUM_CALCULATION=when_required")
				child.Stdin = strings.NewReader(command + "\n\n")
				output, err := child.CombinedOutput()
				if err != nil {
					t.Fatalf("read pre-removal history: %v\n%s", err, output)
				}
				if command == "list" && !strings.Contains(string(output), saved.Tip+" refs/heads/archive/home") {
					t.Fatalf("listed a different historical tip: %s", output)
				}
			}
			if got := runAtOut(dir, "git", "show", saved.Tip+":fixture.txt"); got != "existing encrypted history" {
				t.Fatalf("historical content changed: %q", got)
			}
			fixture.mu.Lock()
			if !bytes.Equal(fixture.data, saved.Data) || fixture.commits != 0 || fixture.uploads != 0 || fixture.deletes != 0 {
				t.Error("reading existing resources with ensure=y changed data at rest")
			}
			fixture.mu.Unlock()
			runAt(dir, "git", "reset", "--hard", saved.Tip)
			runAt(dir, "git", "commit", "--allow-empty", "-qm", "compatible extension")
			tip := runAtOut(dir, "git", "rev-parse", "HEAD")
			if output, err := runMetadataHelper(t, dir, fixture.server.URL, "push refs/heads/archive/home:refs/heads/archive/home"); err != nil {
				t.Fatalf("extend pre-removal history: %v\n%s", err, output)
			}
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			if fixture.commits != 1 || fixture.uploads != 2 || fixture.deletes != 1 || !bytes.Contains(fixture.data, []byte("repo/.remote-aws-v2/repos/1/manifests/"+tip)) {
				t.Fatalf("extension changed publication layout: commits=%d uploads=%d deletes=%d data=%s", fixture.commits, fixture.uploads, fixture.deletes, fixture.data)
			}
			for key, body := range saved.Objects {
				if !strings.Contains(key, "/bundles_") && !bytes.Equal(body, fixture.objects[key]) {
					t.Errorf("extension rewrote existing ciphertext: %s", key)
				}
			}
		})
	}
}

func TestBundleSizedHistoricalExtension(t *testing.T) {
	for _, format := range []string{"sha1", "sha256"} {
		t.Run(format, func(t *testing.T) {
			saved := preLibawsFixture(t, format)
			t.Setenv("GIT_REMOTE_AWS_SECRETKEY", saved.SecretKey)
			t.Setenv("GIT_REMOTE_AWS_SECRETKEY_FILE", "")
			t.Setenv("GIT_REMOTE_AWS_SECRETKEY_CMD", "")
			fixture := newMetadataFixture(t)
			fixture.mu.Lock()
			fixture.data = saved.Data
			for key, body := range saved.Objects {
				fixture.objects[key] = append([]byte(nil), body...)
			}
			fixture.mu.Unlock()
			dir := t.TempDir()
			runAt(dir, "git", "init", "-q", "--object-format="+format, "-b", "archive/home")
			configureGitIdentity(dir)
			if output, err := runMetadataHelper(t, dir, fixture.server.URL, "fetch "+saved.Tip+" refs/heads/archive/home"); err != nil {
				t.Fatalf("fetch historical fixture: %v\n%s", err, output)
			}
			runAt(dir, "git", "reset", "--hard", saved.Tip)
			runAt(dir, "git", "config", "remote-aws.bundleSize", "16k")
			for i := range 3 {
				commitBundleData(t, dir, string(rune('a'+i)), 32<<10)
			}
			tip := runAtOut(dir, "git", "rev-parse", "HEAD")
			if output, err := runMetadataHelper(t, dir, fixture.server.URL, "push refs/heads/archive/home:refs/heads/archive/home"); err != nil {
				t.Fatalf("extend historical fixture with multiple bundles: %v\n%s", err, output)
			}
			fixture.mu.Lock()
			for key, body := range saved.Objects {
				if !strings.Contains(key, "/bundles_") && !bytes.Equal(fixture.objects[key], body) {
					t.Errorf("split extension overwrote historical ciphertext: %s", key)
				}
			}
			if fixture.commits != 1 || fixture.uploads < 3 || fixture.deletes != 1 {
				t.Errorf("split extension changed metadata protocol: commits=%d uploads=%d deletes=%d", fixture.commits, fixture.uploads, fixture.deletes)
			}
			fixture.mu.Unlock()
			clone := t.TempDir()
			runAt(clone, "git", "init", "-q", "--object-format="+format, "-b", "archive/home")
			if output, err := runMetadataHelper(t, clone, fixture.server.URL, "fetch "+tip+" refs/heads/archive/home"); err != nil {
				t.Fatalf("fetch mixed historical and split bundles: %v\n%s", err, output)
			}
			assertBundleHistory(t, dir, clone, tip)
		})
	}
}
