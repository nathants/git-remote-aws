package main

import (
	"bytes"
	"context"
	"debug/buildinfo"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/nathants/go-libsodium"
)

// Old data must come from old code. Compatibility tests build historical helpers
// from these revisions of this repository when a test first needs one. Each tree
// is read with git archive, which leaves the worktree, index, and refs untouched,
// and is built unchanged against its own go.mod and go.sum with the installed
// toolchain. Built helpers last for one test run. A clone lacking these commits,
// such as a shallow clone, fails these tests; nothing newer is substituted.
// Never patch these trees or update their dependencies.
const (
	// The last helper before recipient key chains.
	preKeychainRevision  = "d31fda32401e60c41338019f04d3ab667cf1e124"
	preKeychainLibsodium = "v0.0.0-20260502104057-4e1a79aae4f3"
	// Tree-identical to 3ebf0a3, the last helper before libaws removal. Its
	// go-libsodium matches current code, so publishPreLibawsHistory's legacy
	// layout check is what rejects a newer producer.
	preLibawsRevision  = "672701aae88116fe04e9e391cf942db1c6066024"
	preLibawsLibsodium = "v0.0.0-20260907150908-165cd76c0d68"
)

// A history published by the pre-libaws-removal helper: its stored S3 objects,
// DynamoDB payload, and the synthetic test keys that decrypt it.
type libawsCompatibilityFixture struct {
	Format, Tip, PublicKey, SecretKey string
	Data                              json.RawMessage
	Objects                           map[string][]byte
}

// lazy runs one producer per test run. A producer that fails, panics, or stops
// its test leaves an error for later callers rather than an empty value.
type lazy[T any] struct {
	once  sync.Once
	value T
	err   error
}

func (l *lazy[T]) get(produce func() (T, error)) (T, error) {
	l.once.Do(func() {
		l.err = errors.New("historical artifact generation did not complete")
		l.value, l.err = produce()
	})
	return l.value, l.err
}

var (
	historicalRoot     lazy[string]
	historicalHelpers  sync.Map // revision -> *lazy[string]
	historicalFixtures sync.Map // object format -> *lazy[libawsCompatibilityFixture]
)

func TestMain(m *testing.M) {
	// Tests also call production Git operations in-process after t.Chdir.
	// Clear the caller's selectors at entry, except in the explicit helper
	// subprocess that must exercise main with its deliberately supplied env.
	if os.Getenv("GIT_REMOTE_AWS_PUSH_CHILD") != "1" {
		for name := range testGitVariables() {
			if err := os.Unsetenv(name); err != nil {
				fmt.Fprintln(os.Stderr, "clear Git test environment:", err)
				os.Exit(1)
			}
		}
	}
	code := m.Run()
	if root := historicalRoot.value; root != "" {
		if err := os.RemoveAll(root); err != nil {
			fmt.Fprintln(os.Stderr, "remove historical artifacts:", err)
			code = 1
		}
	}
	os.Exit(code)
}

// historicalHelper returns a directory whose git-remote-aws executable was
// built from revision's unchanged tree and pinned module graph.
func historicalHelper(t *testing.T, revision, libsodiumVersion string) string {
	t.Helper()
	value, _ := historicalHelpers.LoadOrStore(revision, &lazy[string]{})
	dir, err := value.(*lazy[string]).get(func() (string, error) {
		return buildHistoricalHelper(revision, libsodiumVersion)
	})
	if err != nil {
		t.Fatalf("historical helper %s: %v", revision, err)
	}
	return dir
}

func buildHistoricalHelper(revision, libsodiumVersion string) (string, error) {
	root, err := historicalRoot.get(func() (string, error) { return os.MkdirTemp("", "git-remote-aws-historical-") })
	if err != nil {
		return "", err
	}
	dir := filepath.Join(root, revision)
	source, archive := filepath.Join(dir, "source"), filepath.Join(dir, "source.tar")
	if err := os.MkdirAll(source, 0o700); err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(source); _ = os.Remove(archive) }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	build := groupCommand(ctx, "go", "build", "-o", filepath.Join(dir, "git-remote-aws"), ".")
	// Replace GOFLAGS rather than extend it: caller flags such as -mod=mod,
	// -modfile, -overlay, or -tags could change the pinned graph or source. This
	// also omits -race; frozen producers cannot be fixed, so instrumentation
	// applies to the test binary and current helpers.
	build.Dir, build.Env = source, append(testEnvironment(), "GOWORK=off", "GOTOOLCHAIN=local", "GOFLAGS=-mod=readonly")
	// A missing revision fails here; nothing newer substitutes. Always read
	// this clone, even if the caller changes its cwd or Git environment.
	pack := gitCommand(ctx, "archive", "--format=tar", "-o", archive, revision+"^{commit}")
	pack.Dir, pack.Env = repoRoot(), testEnvironment()
	for _, step := range []*exec.Cmd{
		pack,
		groupCommand(ctx, "tar", "-xf", archive, "-C", source),
		build,
	} {
		if err := runHistorical(step); err != nil {
			return "", err
		}
	}
	info, err := buildinfo.ReadFile(filepath.Join(dir, "git-remote-aws"))
	if err != nil {
		return "", err
	}
	for _, dep := range info.Deps {
		if dep.Path == "github.com/nathants/go-libsodium" && dep.Version == libsodiumVersion && dep.Replace == nil {
			return dir, nil
		}
	}
	return "", fmt.Errorf("built without its pinned go-libsodium %s", libsodiumVersion)
}

// runHistorical runs one generation step. Canceling a step must also stop its
// descendants before temporary files are removed: build steps use groupCommand,
// and preLibawsPush signals the pinned helper to stop its own Git groups.
func runHistorical(step *exec.Cmd) error {
	if output, err := step.CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %w\n%s", strings.Join(step.Args, " "), err, output)
	}
	return nil
}

// preLibawsFixture returns a history that the pre-libaws-removal helper
// published to the scripted provider, once per object format and test run.
func preLibawsFixture(t *testing.T, objectFormat string) libawsCompatibilityFixture {
	t.Helper()
	value, _ := historicalFixtures.LoadOrStore(objectFormat, &lazy[libawsCompatibilityFixture]{})
	saved, err := value.(*lazy[libawsCompatibilityFixture]).get(func() (libawsCompatibilityFixture, error) {
		return publishPreLibawsHistory(t, objectFormat)
	})
	if err != nil {
		t.Fatalf("pre-libaws %s history: %v", objectFormat, err)
	}
	saved.Data, saved.Objects = bytes.Clone(saved.Data), maps.Clone(saved.Objects)
	for key, body := range saved.Objects {
		saved.Objects[key] = bytes.Clone(body)
	}
	return saved
}

func publishPreLibawsHistory(t *testing.T, objectFormat string) (libawsCompatibilityFixture, error) {
	helper := filepath.Join(historicalHelper(t, preLibawsRevision, preLibawsLibsodium), "git-remote-aws")
	fixture := newMetadataFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	push, saved, err := preLibawsPush(ctx, t, helper, objectFormat, fixture.server.URL)
	if err != nil {
		return saved, err
	}
	if err := runHistorical(push); err != nil {
		return saved, fmt.Errorf("historical push: %w", err)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	saved.Data, saved.Objects = bytes.Clone(fixture.data), maps.Clone(fixture.objects)
	// The pinned producer must write the legacy layout that promotion reads.
	if _, ok := saved.Objects["/bucket/repo/bundles_"+saved.Tip]; !ok {
		return saved, fmt.Errorf("no legacy bundle list among %v", slices.Sorted(maps.Keys(saved.Objects)))
	}
	return saved, nil
}

// preLibawsPush commits a recipient file in a new repository and prepares the
// pinned producer's push of it to the scripted provider at endpoint.
func preLibawsPush(ctx context.Context, t *testing.T, helper, objectFormat, endpoint string) (*exec.Cmd, libawsCompatibilityFixture, error) {
	libsodium.Init()
	public, secret, err := libsodium.BoxKeypair()
	if err != nil {
		return nil, libawsCompatibilityFixture{}, err
	}
	saved := libawsCompatibilityFixture{Format: objectFormat, PublicKey: hex.EncodeToString(public), SecretKey: hex.EncodeToString(secret)}
	dir := t.TempDir()
	runAt(dir, "git", "init", "-q", "--object-format="+objectFormat, "-b", "archive/home")
	configureGitIdentity(dir)
	for name, content := range map[string]string{".publickeys": saved.PublicKey + "\n", "fixture.txt": "existing encrypted history\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
			return nil, saved, err
		}
	}
	runAt(dir, "git", "add", ".publickeys", "fixture.txt")
	runAt(dir, "git", "commit", "-qm", "compatibility fixture")
	saved.Tip = runAtOut(dir, "git", "rev-parse", "HEAD")
	push := exec.CommandContext(ctx, helper, "origin", "aws://bucket+table/repo")
	// The pinned helper runs Git in separate process groups, which a group kill
	// would orphan. On SIGTERM it stops them and removes its temporary files;
	// WaitDelay bounds an unresponsive helper before it is killed.
	push.Cancel = func() error { return push.Process.Signal(syscall.SIGTERM) }
	push.WaitDelay = 5 * time.Second
	push.Dir = dir
	push.Stdin = strings.NewReader("push refs/heads/archive/home:refs/heads/archive/home\n\n")
	push.Env = append(offlineEnvironment(endpoint), "GIT_DIR=.git", "ensure=",
		"GIT_REMOTE_AWS_PUBLICKEY="+saved.PublicKey, "GIT_REMOTE_AWS_SECRETKEY="+saved.SecretKey,
		"GIT_REMOTE_AWS_SECRETKEY_FILE=", "GIT_REMOTE_AWS_SECRETKEY_CMD=")
	return push, saved, nil
}

// Offline historical helpers may reach only the scripted provider: drop every
// inherited AWS setting, then supply fake credentials and local endpoints.
func offlineEnvironment(endpoint string) []string {
	var env []string
	for _, entry := range testEnvironment() {
		if !strings.HasPrefix(entry, "AWS_") {
			env = append(env, entry)
		}
	}
	return append(env, "AWS_ACCESS_KEY_ID=test", "AWS_SECRET_ACCESS_KEY=test",
		"AWS_REGION=us-east-1", "AWS_DEFAULT_REGION=us-east-1", "AWS_EC2_METADATA_DISABLED=true",
		"AWS_CONFIG_FILE=/dev/null", "AWS_SHARED_CREDENTIALS_FILE=/dev/null",
		"AWS_ENDPOINT_URL_DYNAMODB="+endpoint, "AWS_ENDPOINT_URL_S3="+endpoint,
		"AWS_REQUEST_CHECKSUM_CALCULATION=when_required")
}

func TestBundleHistoricalStepCancellationStopsDescendants(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	// A bounded background descendant appends until it is killed.
	step := groupCommand(ctx, "sh", "-c", "(for i in $(seq 100); do echo beat >> beats; sleep 0.05; done) & wait")
	step.Dir = dir
	if err := runHistorical(step); err == nil {
		t.Fatal("canceled step succeeded")
	}
	beats := func() int {
		body, _ := os.ReadFile(filepath.Join(dir, "beats"))
		return len(body)
	}
	time.Sleep(100 * time.Millisecond)
	before := beats()
	time.Sleep(300 * time.Millisecond)
	if after := beats(); before == 0 || after != before {
		t.Fatalf("descendant did not start or outlived its canceled step: %d then %d bytes", before, after)
	}
}

func TestBundleHistoricalMissingRevisionFails(t *testing.T) {
	missing := strings.Repeat("0", 40)
	if _, err := buildHistoricalHelper(missing, preLibawsLibsodium); err == nil || !strings.Contains(err.Error(), "git archive") {
		t.Fatalf("missing revision was not an archive error: %v", err)
	}
	if entries, err := os.ReadDir(filepath.Join(historicalRoot.value, missing)); err != nil || len(entries) != 0 {
		t.Fatalf("temporary checkout remains: %v %v", entries, err)
	}
}

func TestBundleHistoricalGenerationIsOffline(t *testing.T) {
	var escaped atomic.Int32
	trap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		escaped.Add(1)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer trap.Close()
	// Inherited settings that would redirect or break the helper if they leaked.
	for key, value := range map[string]string{
		"AWS_PROFILE": "git-remote-aws-missing-profile", "AWS_REGION": "eu-central-1",
		"AWS_ACCESS_KEY_ID": "inherited", "AWS_SECRET_ACCESS_KEY": "inherited", "AWS_SESSION_TOKEN": "inherited",
		"AWS_ENDPOINT_URL": trap.URL, "AWS_ENDPOINT_URL_S3": trap.URL,
		"AWS_ENDPOINT_URL_DYNAMODB": trap.URL, "AWS_ENDPOINT_URL_STS": trap.URL,
	} {
		t.Setenv(key, value)
	}
	if _, err := publishPreLibawsHistory(t, "sha1"); err != nil {
		t.Fatal(err)
	}
	if escaped.Load() != 0 {
		t.Fatalf("historical generation sent %d requests to inherited endpoints", escaped.Load())
	}
}

func TestBundleHistoricalPushCancellationStopsGit(t *testing.T) {
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	shims := t.TempDir()
	bundle, beats := filepath.Join(shims, "bundle"), filepath.Join(shims, "beats")
	// Bundle creation becomes a bounded heartbeat that records its output path.
	script := fmt.Sprintf("#!/bin/sh\nif [ \"$1 $2\" = \"bundle create\" ]; then\n\techo \"$3\" > %q\n\tfor i in $(seq 200); do echo beat >> %q; sleep 0.05; done\n\texit 1\nfi\nexec %q \"$@\"\n", bundle, beats, realGit)
	if err := os.WriteFile(filepath.Join(shims, "git"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(historicalHelper(t, preLibawsRevision, preLibawsLibsodium), "git-remote-aws")
	fixture := newMetadataFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	push, _, err := preLibawsPush(ctx, t, helper, "sha1", fixture.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	push.Env = append(push.Env, "PATH="+shims+string(os.PathListSeparator)+os.Getenv("PATH"))
	size := func() int64 {
		info, err := os.Stat(beats)
		if err != nil {
			return 0
		}
		return info.Size()
	}
	go func() {
		for deadline := time.Now().Add(time.Minute); size() == 0 && ctx.Err() == nil && time.Now().Before(deadline); {
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
	}()
	if err := runHistorical(push); err == nil {
		t.Fatal("canceled push succeeded")
	}
	// The helper's temporary directory holds the bundle path; it stays empty here.
	output, err := os.ReadFile(bundle)
	tempdir := filepath.Dir(strings.TrimSpace(string(output)))
	if err != nil || !filepath.IsAbs(tempdir) {
		t.Fatalf("bundle path %q: %v", output, err)
	}
	t.Cleanup(func() { _ = os.Remove(tempdir) })
	time.Sleep(100 * time.Millisecond)
	before := size()
	time.Sleep(300 * time.Millisecond)
	if after := size(); before == 0 || after != before {
		t.Fatalf("Git did not start or outlived its canceled helper: %d then %d bytes", before, after)
	}
	if _, err := os.Stat(tempdir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled helper left %s: %v", tempdir, err)
	}
}
