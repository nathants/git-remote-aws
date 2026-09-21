package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

func commitBundleData(t *testing.T, dir, message string, size int) string {
	t.Helper()
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "data"), data, 0600); err != nil {
		t.Fatal(err)
	}
	runAt(dir, "git", "add", ".")
	runAt(dir, "git", "commit", "-qm", message)
	return runAtOut(dir, "git", "rev-parse", "HEAD")
}

func assertBundleHistory(t *testing.T, source, destination, tip string) {
	t.Helper()
	runAt(destination, "git", "update-ref", "refs/heads/archive/home", tip)
	runAt(destination, "git", "fsck", "--strict")
	for _, args := range [][]string{
		{"git", "rev-list", "--topo-order", tip},
		{"git", "ls-tree", "-r", tip},
	} {
		if got, want := runAtOut(destination, args...), runAtOut(source, args...); got != want {
			t.Fatalf("fetched history differs for %v:\ngot %s\nwant %s", args, got, want)
		}
	}
}

func TestBundleSizedPush(t *testing.T) {
	for _, format := range []string{"sha1", "sha256"} {
		t.Run(format, func(t *testing.T) {
			public := setupEphemeralKeys(t)
			dir := t.TempDir()
			runAt(dir, "git", "init", "-q", "--object-format="+format, "-b", "archive/home")
			configureGitIdentity(dir)
			runAt(dir, "git", "config", "remote-aws.bundleSize", "64k")
			for i := range 6 {
				commitBundleData(t, dir, fmt.Sprintf("historical data %d", i), 32<<10)
			}
			// Every chunk must use the final pushed policy, even when its boundary
			// predates the introduction of the recipients file.
			if err := os.WriteFile(filepath.Join(dir, ".publickeys"), []byte(public+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			runAt(dir, "git", "add", ".publickeys")
			runAt(dir, "git", "commit", "-qm", "recipients")
			fixture := newMetadataFixture(t)
			// Reproduce S3's single-request rejection at a small scale. The helper
			// must split actual Git history, not merely suppress EntityTooLarge.
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPut && r.ContentLength > 100<<10 {
					w.WriteHeader(http.StatusBadRequest)
					_, _ = io.WriteString(w, `<Error><Code>EntityTooLarge</Code><Message>bundle exceeds single upload limit</Message></Error>`)
					return
				}
				fixture.server.Config.Handler.ServeHTTP(w, r)
			}))
			defer server.Close()
			push := "push refs/heads/archive/home:refs/heads/archive/home"
			clone := t.TempDir()
			runAt(clone, "git", "init", "-q", "--object-format="+format, "-b", "archive/home")
			var prior []bundleRef
			for round := range 2 {
				if round != 0 {
					for i := range 6 {
						commitBundleData(t, dir, fmt.Sprintf("incremental data %d", i), 32<<10)
					}
				}
				tip := runAtOut(dir, "git", "rev-parse", "HEAD")
				if output, err := runMetadataHelper(t, dir, server.URL, push); err != nil {
					t.Fatalf("sized push %d: %v\n%s", round, err, output)
				}
				fixture.mu.Lock()
				m := fixtureManifest(t, fixture)
				bundles := m.Bundles
				commits := fixture.commits
				fixture.mu.Unlock()
				if len(bundles)-len(prior) < 2 || commits != round+1 || m.Tip != tip {
					t.Fatalf("push did not atomically publish multiple bundles: %+v commits=%d", m, commits)
				}
				if len(prior) != 0 && !reflect.DeepEqual(prior, bundles[:len(prior)]) {
					t.Fatal("push replaced historical bundle references")
				}
				previous := strings.Repeat("0", len(tip))
				for _, name := range bundles {
					parts := bundleNameParts(name.Range)
					if parts[0] != previous {
						t.Fatalf("discontinuous bundle chain: %+v", bundles)
					}
					previous = parts[1]
				}
				if output, err := runMetadataHelper(t, clone, server.URL, "fetch "+tip+" refs/heads/archive/home"); err != nil {
					t.Fatalf("fetch split history: %v\n%s", err, output)
				}
				assertBundleHistory(t, dir, clone, tip)
				prior = bundles
			}
		})
	}
}

func TestBundleSizedMerges(t *testing.T) {
	for _, remote := range []string{"empty", "first-parent", "second-parent", "second-parent-ancestor"} {
		t.Run(remote, func(t *testing.T) {
			public := setupEphemeralKeys(t)
			dir := t.TempDir()
			runAt(dir, "git", "init", "-q", "--object-format=sha256", "-b", "archive/home")
			configureGitIdentity(dir)
			if err := os.WriteFile(filepath.Join(dir, ".publickeys"), []byte(public+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			root := commitBundleData(t, dir, "root", 32<<10)
			fixture := newMetadataFixture(t)
			push := "push refs/heads/archive/home:refs/heads/archive/home"
			publish := func() {
				t.Helper()
				if output, err := runMetadataHelper(t, dir, fixture.server.URL, push); err != nil {
					t.Fatalf("publish merge history: %v\n%s", err, output)
				}
			}
			if remote == "first-parent" {
				publish()
			}
			for i := range 3 {
				commitBundleData(t, dir, fmt.Sprintf("side %d", i), 32<<10)
				if remote == "second-parent-ancestor" && i == 0 {
					publish()
				}
			}
			if remote == "second-parent" {
				publish()
			}
			runAt(dir, "git", "branch", "side")
			runAt(dir, "git", "reset", "--hard", root)
			mainline := 1
			if remote == "second-parent-ancestor" {
				mainline = 5
			}
			for i := range mainline {
				commitBundleData(t, dir, fmt.Sprintf("mainline %d", i), 32<<10)
			}
			runAt(dir, "git", "merge", "-q", "--no-ff", "--strategy=ours", "side", "-m", "import side history")
			commitBundleData(t, dir, "after merge", 32<<10)
			tip := runAtOut(dir, "git", "rev-parse", "HEAD")
			runAt(dir, "git", "config", "remote-aws.bundleSize", "16k")
			publish()
			clone := t.TempDir()
			runAt(clone, "git", "init", "-q", "--object-format=sha256", "-b", "archive/home")
			if output, err := runMetadataHelper(t, clone, fixture.server.URL, "fetch "+tip+" refs/heads/archive/home"); err != nil {
				t.Fatalf("fetch merge history: %v\n%s", err, output)
			}
			assertBundleHistory(t, dir, clone, tip)
		})
	}
}

func TestBundleSizedPushFailureAndRetry(t *testing.T) {
	public := setupEphemeralKeys(t)
	dir := t.TempDir()
	runAt(dir, "git", "init", "-q", "-b", "archive/home")
	configureGitIdentity(dir)
	if err := os.WriteFile(filepath.Join(dir, ".publickeys"), []byte(public+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runAt(dir, "git", "config", "remote-aws.bundleSize", "16k")
	for i := range 3 {
		commitBundleData(t, dir, fmt.Sprintf("data %d", i), 32<<10)
	}
	fixture := newMetadataFixture(t)
	var puts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && puts.Add(1) == 2 {
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `<Error><Code>AccessDenied</Code><Message>second bundle rejected</Message></Error>`)
			return
		}
		fixture.server.Config.Handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	push := "push refs/heads/archive/home:refs/heads/archive/home"
	if output, err := runMetadataHelper(t, dir, server.URL, push); err == nil || !strings.Contains(output, "second bundle rejected") {
		t.Fatalf("partial push did not fail at the second bundle: %v\n%s", err, output)
	}
	fixture.mu.Lock()
	if fixture.commits != 0 || fixture.uploads != 1 || fixture.deletes != 0 || string(fixture.data) != `{"M":{}}` {
		t.Errorf("failed batch changed published metadata: commits=%d uploads=%d deletes=%d data=%s", fixture.commits, fixture.uploads, fixture.deletes, fixture.data)
	}
	var savedKey string
	var savedCiphertext []byte
	for key, body := range fixture.objects {
		savedKey, savedCiphertext = key, append([]byte(nil), body...)
	}
	fixture.mu.Unlock()
	if output, err := runMetadataHelper(t, dir, server.URL, push); err != nil || !strings.Contains(output, "reuse completed bundle") {
		t.Fatalf("same-tip retry did not reuse completed ciphertext: %v\n%s", err, output)
	}
	fixture.mu.Lock()
	if fixture.commits != 1 || !bytes.Equal(fixture.objects[savedKey], savedCiphertext) {
		t.Error("retry republished metadata or replaced completed ciphertext")
	}
	fixture.mu.Unlock()
	tip := runAtOut(dir, "git", "rev-parse", "HEAD")
	clone := t.TempDir()
	runAt(clone, "git", "init", "-q", "-b", "archive/home")
	if output, err := runMetadataHelper(t, clone, server.URL, "fetch "+tip+" refs/heads/archive/home"); err != nil {
		t.Fatalf("fetch retried batch: %v\n%s", err, output)
	}
	assertBundleHistory(t, dir, clone, tip)
	if refs := runAtOut(dir, "git", "for-each-ref", "refs/git-remote-aws/"); refs != "" {
		t.Fatalf("temporary refs survived the failed or retried push: %s", refs)
	}
}

func TestBundleSizeConfiguration(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	runAt(dir, "git", "init", "-q")
	if size, err := pushBundleSize(t.Context()); err != nil || size != 256<<20 {
		t.Fatalf("wrong default: %d, %v", size, err)
	}
	for _, value := range []string{"1g", "64k", "0", "-1", "", "no", "99999999999999999999g"} {
		runAt(dir, "git", "config", "remote-aws.bundleSize", value)
		size, err := pushBundleSize(t.Context())
		want := map[string]int64{"1g": 1 << 30, "64k": 64 << 10}[value]
		if want == 0 && err == nil || want != 0 && (err != nil || size != want) {
			t.Errorf("configuration %q: %d, %v", value, size, err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := pushBundleSize(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled configuration read: %v", err)
	}
}

func TestBundleActualSizeRefinement(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	runAt(dir, "git", "init", "-q", "-b", "archive/home")
	configureGitIdentity(dir)
	for i := range 2 {
		runAt(dir, "git", "commit", "--allow-empty", "-qm", fmt.Sprintf("empty %d", i))
	}
	tip := runAtOut(dir, "git", "rev-parse", "HEAD")
	estimate, err := strconv.ParseInt(runAtOut(dir, "git", "rev-list", "--objects", "--disk-usage", tip), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "unsplit.bundle")
	if err := createPinnedPushBundle(t.Context(), file, "", tip); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(file)
	if err != nil || info.Size() <= estimate {
		t.Fatalf("fixture must underestimate the packed bundle including its header: estimate=%d, stat=%v, err=%v", estimate, info, err)
	}
	count := 0
	builder := pushBundleBuilder{ctx: t.Context(), directory: t.TempDir(), target: estimate}
	builder.consume = func(filename string) error {
		count++
		files, err := os.ReadDir(builder.directory)
		if err != nil || len(files) != 1 || files[0].Name() != filepath.Base(filename) {
			t.Errorf("more than one bundle retained: %v, %v", files, err)
		}
		return nil
	}
	if err := builder.create("", tip); err != nil || count != 2 {
		t.Fatalf("actual-size refinement did not split: count=%d, err=%v", count, err)
	}
	files, err := os.ReadDir(builder.directory)
	if err != nil || len(files) != 0 {
		t.Errorf("consumed temporary bundles survived: %v, %v", files, err)
	}
}
