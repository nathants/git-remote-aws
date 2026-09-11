package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type libawsCompatibilityFixture struct {
	Producer  string            `json:"producer"`
	Format    string            `json:"git_object_format"`
	Tip       string            `json:"tip"`
	PublicKey string            `json:"public_key"`
	SecretKey string            `json:"test_only_secret_key"`
	Data      json.RawMessage   `json:"dynamodb_data"`
	Objects   map[string][]byte `json:"s3_objects"`
}

func TestBundleLibawsRemovalCompatibility(t *testing.T) {
	for _, objectFormat := range []string{"sha1", "sha256"} {
		t.Run(objectFormat, func(t *testing.T) {
			fixturePath := filepath.Join("testdata", "libaws-"+objectFormat+".json")
			if os.Getenv("GIT_REMOTE_AWS_GENERATE_LIBAWS_FIXTURES") == "1" {
				generateLibawsCompatibilityFixture(t, objectFormat, fixturePath)
			}
			raw, err := os.ReadFile(fixturePath)
			if err != nil {
				t.Fatal(err)
			}
			var saved libawsCompatibilityFixture
			if err := json.Unmarshal(raw, &saved); err != nil {
				t.Fatal(err)
			}
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
			if fixture.commits != 1 || fixture.uploads != 2 || fixture.deletes != 1 || !bytes.Contains(fixture.data, []byte("repo/bundles_"+tip)) {
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

// Generate only with the retained pre-removal revision documented in testdata.
// The checked-in fixtures hold synthetic secrets, actual encrypted bundles and
// the old DynamoDB payload; ordinary tests do not regenerate them.
func generateLibawsCompatibilityFixture(t *testing.T, objectFormat, destination string) {
	t.Helper()
	producer := runAtOut(".", "git", "rev-parse", "HEAD")
	if producer != "672701aae88116fe04e9e391cf942db1c6066024" {
		t.Fatal("fixture generation requires the pre-libaws-removal helper revision documented in testdata/README.md")
	}
	changed := runAtOut(".", "git", "diff", "--name-only", producer, "--", "*.go", "go.mod", "go.sum")
	untracked := runAtOut(".", "git", "ls-files", "--others", "--exclude-standard", "--", "*.go", "go.mod", "go.sum")
	for _, name := range strings.Split(changed+"\n"+untracked, "\n") {
		if name != "" && !strings.HasSuffix(name, "_test.go") {
			t.Fatalf("fixture producer has changed production sources or dependencies: %s", name)
		}
	}
	public := setupEphemeralKeys(t)
	dir := t.TempDir()
	runAt(dir, "git", "init", "-q", "--object-format="+objectFormat, "-b", "archive/home")
	configureGitIdentity(dir)
	if err := os.WriteFile(filepath.Join(dir, ".publickeys"), []byte(public+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fixture.txt"), []byte("existing encrypted history\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runAt(dir, "git", "add", ".publickeys", "fixture.txt")
	runAt(dir, "git", "commit", "-qm", "compatibility fixture")
	fixture := newMetadataFixture(t)
	if output, err := runMetadataHelper(t, dir, fixture.server.URL, "push refs/heads/archive/home:refs/heads/archive/home"); err != nil {
		t.Fatalf("generate pre-removal fixture: %v\n%s", err, output)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	saved := libawsCompatibilityFixture{
		Producer: producer, Format: objectFormat, Tip: runAtOut(dir, "git", "rev-parse", "HEAD"),
		PublicKey: public, SecretKey: os.Getenv("GIT_REMOTE_AWS_SECRETKEY"), Data: fixture.data, Objects: fixture.objects,
	}
	data, err := json.MarshalIndent(saved, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, append(data, '\n'), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestBundleSizedHistoricalExtension(t *testing.T) {
	for _, format := range []string{"sha1", "sha256"} {
		t.Run(format, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("testdata", "libaws-"+format+".json"))
			if err != nil {
				t.Fatal(err)
			}
			var saved libawsCompatibilityFixture
			if err := json.Unmarshal(raw, &saved); err != nil {
				t.Fatal(err)
			}
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
