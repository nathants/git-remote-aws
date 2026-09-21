package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func runRepositoryHelper(t *testing.T, dir, endpoint, repo, command string) (string, error) {
	t.Helper()
	child := testHelperCommand(t, dir, endpoint, "20s")
	child.Env = append(child.Env, "AWS_REQUEST_CHECKSUM_CALCULATION=when_required", "GIT_REMOTE_AWS_TEST_REMOTE=aws://bucket+table/"+repo)
	child.Stdin = strings.NewReader(command + "\n\n")
	output, err := child.CombinedOutput()
	return string(output), err
}

// Caller holds the fixture mutex.
func repositoryManifest(t *testing.T, fixture *metadataFixture, repo string) *manifest {
	t.Helper()
	if repo == "repo" {
		return fixtureManifest(t, fixture)
	}
	original := fixture.data
	fixture.data = fixture.records["bucket/"+repo]
	defer func() { fixture.data = original }()
	return fixtureManifest(t, fixture)
}

func namespaceSource(t *testing.T, format string) (string, *metadataFixture, string) {
	t.Helper()
	public := setupEphemeralKeys(t)
	dir := t.TempDir()
	runAt(dir, "git", "init", "-q", "--object-format="+format, "-b", "archive/home")
	configureGitIdentity(dir)
	if err := os.WriteFile(filepath.Join(dir, ".publickeys"), []byte(public+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runAt(dir, "git", "config", "remote-aws.bundleSize", "16k")
	var tip string
	for i := range 5 {
		tip = commitBundleData(t, dir, fmt.Sprintf("data %d", i), 32<<10)
	}
	fixture := newMetadataFixture(t)
	fixture.pageSize = 2
	if output, err := runRepositoryHelper(t, dir, fixture.server.URL, "repo", "push refs/heads/archive/home:refs/heads/archive/home"); err != nil {
		t.Fatalf("source push: %v\n%s", err, output)
	}
	return dir, fixture, tip
}

func TestBundleNamespaceAdoption(t *testing.T) {
	for _, format := range []string{"sha1", "sha256"} {
		t.Run(format, func(t *testing.T) {
			dir, fixture, originalTip := namespaceSource(t, format)
			fixture.mu.Lock()
			original := fixtureManifest(t, fixture)
			before := fixture.uploads
			ciphertext := make(map[string][]byte)
			for _, ref := range original.Bundles {
				ciphertext[ref.Key] = bytes.Clone(fixture.objects["/bucket/"+ref.Key])
			}
			fixture.mu.Unlock()
			runAt(dir, "git", "commit", "--amend", "-qm", "rewritten last commit")
			tip := runAtOut(dir, "git", "rev-parse", "HEAD")
			push := "push refs/heads/archive/home:refs/heads/archive/home"
			if output, err := runRepositoryHelper(t, dir, fixture.server.URL, "repo/2", push); err != nil || !strings.Contains(output, "reuse 4 existing bundles") {
				t.Fatalf("shallow rewrite did not reuse prefix: %v\n%s", err, output)
			}
			fixture.mu.Lock()
			second := repositoryManifest(t, fixture, "repo/2")
			if fixture.uploads-before != 2 || len(second.Bundles) != 5 || !reflect.DeepEqual(original.Bundles[:4], second.Bundles[:4]) {
				t.Errorf("reuse repacked/uploaded history: uploads=%d manifest=%+v", fixture.uploads-before, second)
			}
			for key, body := range ciphertext {
				if !bytes.Equal(body, fixture.objects["/bucket/"+key]) {
					t.Errorf("mutated existing ciphertext %s", key)
				}
			}
			before = fixture.uploads
			fixture.mu.Unlock()
			if output, err := runRepositoryHelper(t, dir, fixture.server.URL, "repo/3", push); err != nil {
				t.Fatalf("identical history adoption: %v\n%s", err, output)
			}
			fixture.mu.Lock()
			third := repositoryManifest(t, fixture, "repo/3")
			if fixture.uploads-before != 1 || !reflect.DeepEqual(second.Bundles, third.Bundles) {
				t.Error("identical history uploaded bundles")
			}
			fixture.mu.Unlock()
			for _, alias := range []string{"repo", "repo/1"} {
				output, err := runRepositoryHelper(t, dir, fixture.server.URL, alias, "list")
				if err != nil || !strings.Contains(output, originalTip+" refs/heads/archive/home") {
					t.Fatalf("default alias changed: %v\n%s", err, output)
				}
				if output, err := runRepositoryHelper(t, dir, fixture.server.URL, alias, push); err == nil || !strings.Contains(output, "pull before pushing") {
					t.Fatalf("reuse bypassed immutable destination: %v\n%s", err, output)
				}
			}
			// Superseding the source manifest must not invalidate a borrower.
			runAt(dir, "git", "reset", "--hard", originalTip)
			commitBundleData(t, dir, "source extension", 1024)
			if output, err := runRepositoryHelper(t, dir, fixture.server.URL, "repo", push); err != nil {
				t.Fatalf("source advance: %v\n%s", err, output)
			}
			clone := t.TempDir()
			runAt(clone, "git", "init", "-q", "--object-format="+format, "-b", "archive/home")
			if output, err := runRepositoryHelper(t, clone, fixture.server.URL, "repo/2", "fetch "+tip+" refs/heads/archive/home"); err != nil {
				t.Fatalf("borrower fetch after source advance: %v\n%s", err, output)
			}
			assertBundleHistory(t, dir, clone, tip)
		})
	}
}

func TestBundleNamespaceRejectsBrokenAdoption(t *testing.T) {
	for _, scenario := range []string{"gap", "wrong-tip", "cross-namespace", "missing-object", "changed-object", "changed-policy"} {
		t.Run(scenario, func(t *testing.T) {
			dir, fixture, _ := namespaceSource(t, "sha1")
			fixture.mu.Lock()
			m := fixtureManifest(t, fixture)
			before := fixture.commits
			switch scenario {
			case "gap":
				m.Bundles = append(m.Bundles[:1], m.Bundles[2:]...)
			case "wrong-tip":
				m.Tip = strings.Repeat("a", 40)
			case "cross-namespace":
				m.Bundles[0].Key = "other/" + m.Bundles[0].Range
			case "missing-object":
				delete(fixture.objects, "/bucket/"+m.Bundles[1].Key)
			case "changed-object":
				fixture.objects["/bucket/"+m.Bundles[1].Key][0] ^= 1
			case "changed-policy":
				m.Bundles[0].Recipients = []string{strings.Repeat("a", 128)}
			}
			data, err := json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			for key := range fixture.objects {
				if strings.Contains(key, "/manifests/") {
					fixture.objects[key] = data
				}
			}
			fixture.mu.Unlock()
			if output, err := runRepositoryHelper(t, dir, fixture.server.URL, "repo/2", "push refs/heads/archive/home:refs/heads/archive/home"); err == nil {
				t.Fatalf("accepted broken adoption: %s", output)
			}
			fixture.mu.Lock()
			if fixture.commits != before {
				t.Error("published broken adoption")
			}
			fixture.mu.Unlock()
		})
	}
}

func TestBundleLegacyPromotionWithoutUpload(t *testing.T) {
	for _, format := range []string{"sha1", "sha256"} {
		t.Run(format, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("testdata", "libaws-"+format+".json"))
			if err != nil {
				t.Fatal(err)
			}
			var saved libawsCompatibilityFixture
			if err := json.Unmarshal(data, &saved); err != nil {
				t.Fatal(err)
			}
			t.Setenv("GIT_REMOTE_AWS_SECRETKEY", saved.SecretKey)
			t.Setenv("GIT_REMOTE_AWS_SECRETKEY_FILE", "")
			t.Setenv("GIT_REMOTE_AWS_SECRETKEY_CMD", "")
			fixture := newMetadataFixture(t)
			fixture.data = bytes.Clone(saved.Data)
			for key, data := range saved.Objects {
				fixture.objects[key] = bytes.Clone(data)
			}
			dir := t.TempDir()
			runAt(dir, "git", "init", "-q", "--object-format="+format, "-b", "archive/home")
			if output, err := runRepositoryHelper(t, dir, fixture.server.URL, "repo/1", "fetch "+saved.Tip+" refs/heads/archive/home"); err != nil {
				t.Fatalf("legacy fetch: %v\n%s", err, output)
			}
			runAt(dir, "git", "reset", "--hard", saved.Tip)
			// A sibling can adopt a legacy list before its owner is promoted.
			if output, err := runRepositoryHelper(t, dir, fixture.server.URL, "repo/seed", "push refs/heads/archive/home:refs/heads/archive/home"); err != nil {
				t.Fatalf("adopt unpromoted source: %v\n%s", err, output)
			}
			fixture.mu.Lock()
			if fixture.uploads != 1 || fixture.commits != 1 || fixture.deletes != 0 || !bytes.Equal(saved.Data, fixture.data) {
				t.Fatal("adoption mutated the legacy source")
			}
			fixture.uploads, fixture.commits, fixture.deletes = 0, 0, 0
			fixture.mu.Unlock()
			if output, err := runRepositoryHelper(t, dir, fixture.server.URL, "repo/1", "push refs/heads/archive/home:refs/heads/archive/home"); err != nil {
				t.Fatalf("metadata-only promotion: %v\n%s", err, output)
			}
			fixture.mu.Lock()
			m := fixtureManifest(t, fixture)
			if fixture.uploads != 1 || fixture.commits != 1 || fixture.deletes != 1 {
				t.Errorf("promotion was not metadata-only: uploads=%d commits=%d deletes=%d", fixture.uploads, fixture.commits, fixture.deletes)
			}
			for _, ref := range m.Bundles {
				if !strings.HasPrefix(ref.Key, "repo/") || strings.Contains(ref.Key, ".remote-aws-v2") || !bytes.Equal(fixture.objects["/bucket/"+ref.Key], saved.Objects["/bucket/"+ref.Key]) {
					t.Errorf("promotion moved/changed %s", ref.Key)
				}
			}
			fixture.mu.Unlock()
			if output, err := runRepositoryHelper(t, dir, fixture.server.URL, "repo/2", "push refs/heads/archive/home:refs/heads/archive/home"); err != nil {
				t.Fatalf("adopt promoted legacy chunks: %v\n%s", err, output)
			}
			fixture.mu.Lock()
			if fixture.uploads != 2 || !reflect.DeepEqual(m.Bundles, repositoryManifest(t, fixture, "repo/2").Bundles) {
				t.Error("legacy reuse uploaded or moved bundles")
			}
			fixture.mu.Unlock()
			clone := t.TempDir()
			runAt(clone, "git", "init", "-q", "--object-format="+format, "-b", "archive/home")
			if output, err := runRepositoryHelper(t, clone, fixture.server.URL, "repo/2", "fetch "+saved.Tip+" refs/heads/archive/home"); err != nil {
				t.Fatalf("fetch promoted legacy chunks: %v\n%s", err, output)
			}
			assertBundleHistory(t, dir, clone, saved.Tip)
		})
	}
}

func TestBundleS3OnlyRecovery(t *testing.T) {
	dir, fixture, tip := namespaceSource(t, "sha256")
	fixture.mu.Lock()
	var key string
	for object := range fixture.objects {
		if strings.Contains(object, "/manifests/") {
			key = strings.TrimPrefix(object, "/bucket/")
		}
	}
	fixture.data = json.RawMessage(`{"M":{}}`)
	beforeReads := fixture.reads
	beforeUploads := fixture.uploads
	fixture.mu.Unlock()
	binary := buildGitRemoteAws(t)
	destination := filepath.Join(t.TempDir(), "recovered")
	for _, args := range [][]string{
		{"--recover", "--remote", "aws://bucket+table/repo"},
		{"--recover", "--remote", "aws://bucket+table/repo/1", "--manifest", key, "--directory", destination},
	} {
		child := testHelperCommand(t, dir, fixture.server.URL, "20s")
		child.Path = binary
		child.Args = append([]string{binary}, args...)
		out, err := child.CombinedOutput()
		if err != nil {
			t.Fatalf("S3-only recovery: %v\n%s", err, out)
		}
		if len(args) == 3 && !bytes.Contains(out, []byte(key)) {
			t.Fatalf("recovery did not discover manifest: %s", out)
		}
	}
	assertBundleHistory(t, dir, destination, tip)
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if fixture.reads != beforeReads || fixture.uploads != beforeUploads {
		t.Error("S3-only recovery accessed DynamoDB or mutated S3")
	}
}

func TestBundleNamespaceNames(t *testing.T) {
	for _, input := range []string{"WickedEngine3", "WickedEngine3/1", "WickedEngine3/1/"} {
		repo, err := parseRepository(input)
		if err != nil || repo.canonical() != "WickedEngine3/1" || repo.id() != "WickedEngine3" {
			t.Errorf("alias %q: %+v %v", input, repo, err)
		}
	}
	for _, input := range []string{"", "/repo", "repo/../2", "repo//2", "repo/.", "repo/..", "repo/2/3", "repo/%2f", "repo\\other"} {
		if _, err := parseRepository(input); err == nil {
			t.Errorf("accepted unsafe identity %q", input)
		}
	}
}

func TestBundleFetchDuringClonePlaceholder(t *testing.T) {
	dir, fixture, tip := namespaceSource(t, "sha256")
	clone := t.TempDir()
	runAt(clone, "git", "init", "-q", "--object-format=sha256", "-b", "archive/home")
	// Git clone leaves this temporary HEAD while the remote helper imports.
	if err := os.WriteFile(filepath.Join(clone, ".git", "HEAD"), []byte("ref: refs/heads/.invalid\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if output, err := runRepositoryHelper(t, clone, fixture.server.URL, "repo", "fetch "+tip+" refs/heads/archive/home"); err != nil {
		t.Fatalf("fetch rejected clone's temporary HEAD: %v\n%s", err, output)
	}
	runAt(clone, "git", "symbolic-ref", "HEAD", "refs/heads/archive/home")
	assertBundleHistory(t, dir, clone, tip)
}

func TestBundleFetchDetectsMissingReachableObjects(t *testing.T) {
	for _, object := range []string{"HEAD:data", "HEAD^{tree}", "HEAD^"} {
		t.Run(object, func(t *testing.T) {
			dir, fixture, tip := namespaceSource(t, "sha1")
			oid := runAtOut(dir, "git", "rev-parse", object)
			if err := os.Remove(filepath.Join(dir, ".git", "objects", oid[:2], oid[2:])); err != nil {
				t.Fatal(err)
			}
			output, err := runRepositoryHelper(t, dir, fixture.server.URL, "repo", "fetch "+tip+" refs/heads/archive/home")
			if object == "HEAD^" && err == nil {
				// Missing commit ancestry causes the normal fetch planner to
				// import the complete chain, restoring the missing object.
				runAt(dir, "git", "cat-file", "-e", oid)
				runAt(dir, "git", "fsck", "--strict")
			} else if err == nil || !strings.Contains(output, "incomplete fetched history") {
				t.Fatalf("fetch accepted incomplete object closure: %v\n%s", err, output)
			}
		})
	}
}

func TestBundleNamespaceLostPointerCannotRewrite(t *testing.T) {
	dir, fixture, _ := namespaceSource(t, "sha1")
	fixture.mu.Lock()
	fixture.data = json.RawMessage(`{"M":{}}`)
	before := fixture.commits
	fixture.mu.Unlock()
	runAt(dir, "git", "reset", "--hard", "HEAD^")
	if output, err := runRepositoryHelper(t, dir, fixture.server.URL, "repo", "push refs/heads/archive/home:refs/heads/archive/home"); err == nil || !strings.Contains(output, "S3 history") {
		t.Fatalf("lost pointer allowed a rewind: %v\n%s", err, output)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if fixture.commits != before {
		t.Fatal("lost-pointer rewind published metadata")
	}
}

func TestBundleNamespaceRecipientPolicies(t *testing.T) {
	for _, rotation := range []bool{false, true} {
		t.Run(fmt.Sprint(rotation), func(t *testing.T) {
			dir, fixture, _ := namespaceSource(t, "sha1")
			previousPublic := os.Getenv("GIT_REMOTE_AWS_PUBLICKEY")
			previousSecret := os.Getenv("GIT_REMOTE_AWS_SECRETKEY")
			fixture.mu.Lock()
			old := fixtureManifest(t, fixture)
			fixture.mu.Unlock()
			nextPublic := setupEphemeralKeys(t)
			nextSecret := os.Getenv("GIT_REMOTE_AWS_SECRETKEY")
			policy := nextPublic
			if rotation {
				policy = previousPublic + ":" + nextPublic
				t.Setenv("GIT_REMOTE_AWS_SECRETKEY", previousSecret+":"+nextSecret)
			}
			if err := os.WriteFile(filepath.Join(dir, ".publickeys"), []byte(policy+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			runAt(dir, "git", "add", ".publickeys")
			runAt(dir, "git", "commit", "-qm", "new encryption policy")
			tip := runAtOut(dir, "git", "rev-parse", "HEAD")
			if output, err := runRepositoryHelper(t, dir, fixture.server.URL, "repo/2", "push refs/heads/archive/home:refs/heads/archive/home"); err != nil {
				t.Fatalf("policy push: %v\n%s", err, output)
			}
			fixture.mu.Lock()
			next := repositoryManifest(t, fixture, "repo/2")
			fixture.mu.Unlock()
			if rotation {
				if !reflect.DeepEqual(old.Bundles, next.Bundles[:len(old.Bundles)]) {
					t.Fatal("rotation did not inherit decryptable historical generations")
				}
			} else {
				for _, ref := range next.Bundles {
					for _, prior := range old.Bundles {
						if ref.Key == prior.Key {
							t.Fatal("adopted ciphertext for a different recipient")
						}
					}
				}
			}
			clone := t.TempDir()
			runAt(clone, "git", "init", "-q", "-b", "archive/home")
			if output, err := runRepositoryHelper(t, clone, fixture.server.URL, "repo/2", "fetch "+tip+" refs/heads/archive/home"); err != nil {
				t.Fatalf("policy clone: %v\n%s", err, output)
			}
			assertBundleHistory(t, dir, clone, tip)
		})
	}
}

func TestBundleFetchAuthenticatesCiphertext(t *testing.T) {
	_, fixture, tip := namespaceSource(t, "sha1")
	fixture.mu.Lock()
	m := fixtureManifest(t, fixture)
	ref := &m.Bundles[0]
	ciphertext := fixture.objects["/bucket/"+ref.Key]
	ciphertext[len(ciphertext)-1] ^= 1
	// Even if storage metadata was replaced along with the payload, the
	// stream's authentication must fail before importing this root bundle.
	ref.ETag = fixtureETag(ciphertext)
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	for key := range fixture.objects {
		if strings.Contains(key, "/manifests/") {
			fixture.objects[key] = data
		}
	}
	fixture.mu.Unlock()
	clone := t.TempDir()
	runAt(clone, "git", "init", "-q", "-b", "archive/home")
	output, err := runRepositoryHelper(t, clone, fixture.server.URL, "repo", "fetch "+tip+" refs/heads/archive/home")
	if err == nil || strings.Contains(output, "git unbundle:") {
		t.Fatalf("corrupt ciphertext reached Git: %v\n%s", err, output)
	}
}

func TestBundleNamespaceAliasCollision(t *testing.T) {
	dir, fixture, _ := namespaceSource(t, "sha1")
	fixture.mu.Lock()
	fixture.records["bucket/repo/1"] = bytes.Clone(fixture.data)
	before := fixture.commits
	fixture.mu.Unlock()
	for _, alias := range []string{"repo", "repo/1"} {
		if output, err := runRepositoryHelper(t, dir, fixture.server.URL, alias, "push refs/heads/archive/home:refs/heads/archive/home"); err == nil || !strings.Contains(output, "conflicts with the implicit /1 alias") {
			t.Fatalf("ambiguous alias was accepted: %v\n%s", err, output)
		}
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if fixture.commits != before {
		t.Fatal("alias conflict changed history")
	}
}

func TestBundleIncrementalDoesNotReinspectHistory(t *testing.T) {
	dir, fixture, _ := namespaceSource(t, "sha1")
	fixture.mu.Lock()
	old := fixtureManifest(t, fixture)
	fixture.mu.Unlock()
	historical := make(map[string]bool)
	for _, ref := range old.Bundles {
		historical["/bucket/"+ref.Key] = true
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if historical[r.URL.Path] {
			t.Error("ordinary incremental push reread immutable historical ciphertext")
			w.WriteHeader(http.StatusForbidden)
			return
		}
		fixture.server.Config.Handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	commitBundleData(t, dir, "incremental change", 1024)
	if output, err := runRepositoryHelper(t, dir, server.URL, "repo", "push refs/heads/archive/home:refs/heads/archive/home"); err != nil {
		t.Fatalf("incremental push: %v\n%s", err, output)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if next := fixtureManifest(t, fixture); !reflect.DeepEqual(old.Bundles, next.Bundles[:len(old.Bundles)]) {
		t.Fatal("incremental push changed immutable references")
	}
}

func TestBundleNamespaceMergeAndIsolation(t *testing.T) {
	dir, fixture, _ := namespaceSource(t, "sha256")
	fixture.mu.Lock()
	old := fixtureManifest(t, fixture)
	fixture.mu.Unlock()
	runAt(dir, "git", "branch", "side")
	runAt(dir, "git", "reset", "--hard", "HEAD~3")
	commitBundleData(t, dir, "divergent mainline", 1024)
	runAt(dir, "git", "merge", "-q", "--no-ff", "--strategy=ours", "side", "-m", "adopt side history")
	tip := runAtOut(dir, "git", "rev-parse", "HEAD")
	if output, err := runRepositoryHelper(t, dir, fixture.server.URL, "repo/merge", "push refs/heads/archive/home:refs/heads/archive/home"); err != nil {
		t.Fatalf("second-parent adoption: %v\n%s", err, output)
	}
	fixture.mu.Lock()
	merged := repositoryManifest(t, fixture, "repo/merge")
	fixture.mu.Unlock()
	if !reflect.DeepEqual(old.Bundles, merged.Bundles[:len(old.Bundles)]) {
		t.Fatal("merge did not reuse second-parent ancestry")
	}
	clone := t.TempDir()
	runAt(clone, "git", "init", "-q", "--object-format=sha256", "-b", "archive/home")
	if output, err := runRepositoryHelper(t, clone, fixture.server.URL, "repo/merge", "fetch "+tip+" refs/heads/archive/home"); err != nil {
		t.Fatalf("second-parent clone: %v\n%s", err, output)
	}
	assertBundleHistory(t, dir, clone, tip)
	if output, err := runRepositoryHelper(t, dir, fixture.server.URL, "separate", "push refs/heads/archive/home:refs/heads/archive/home"); err != nil {
		t.Fatalf("separate namespace push: %v\n%s", err, output)
	}
	fixture.mu.Lock()
	separate := repositoryManifest(t, fixture, "separate")
	fixture.mu.Unlock()
	for _, ref := range separate.Bundles {
		if !strings.HasPrefix(ref.Key, "separate/") {
			t.Fatalf("cross-namespace adoption: %+v", ref)
		}
	}
}

func TestBundleRecoveryIgnoresCallerGitDirectories(t *testing.T) {
	dir, fixture, tip := namespaceSource(t, "sha1")
	fixture.mu.Lock()
	var key string
	for object := range fixture.objects {
		if strings.Contains(object, "/manifests/") {
			key = strings.TrimPrefix(object, "/bucket/")
		}
	}
	fixture.mu.Unlock()
	runAt(dir, "git", "commit", "--allow-empty", "-qm", "local work not on remote")
	before := runAtOut(dir, "git", "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(dir, "precious"), []byte("uncommitted local file"), 0600); err != nil {
		t.Fatal(err)
	}
	binary := buildGitRemoteAws(t)
	destination := filepath.Join(t.TempDir(), "recovered")
	child := testHelperCommand(t, dir, fixture.server.URL, "20s")
	child.Path = binary
	child.Args = []string{binary, "--recover", "--remote", "aws://bucket+table/repo", "--manifest", key, "--directory", destination}
	child.Env = append(child.Env, "GIT_DIR="+filepath.Join(dir, ".git"), "GIT_WORK_TREE="+dir, "GIT_INDEX_FILE="+filepath.Join(dir, ".git", "index"))
	output, err := child.CombinedOutput()
	if err != nil {
		t.Fatalf("isolated recovery failed: %v\n%s", err, output)
	}
	if got := runAtOut(dir, "git", "rev-parse", "HEAD"); got != before {
		t.Fatal("recovery moved the caller's branch")
	}
	if text, err := os.ReadFile(filepath.Join(dir, "precious")); err != nil || string(text) != "uncommitted local file" {
		t.Fatal("recovery changed caller files")
	}
	assertBundleHistory(t, dir, destination, tip)
}
