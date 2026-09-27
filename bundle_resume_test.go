package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestBundleResumeKeepsCompletedBoundaries(t *testing.T) {
	for _, format := range []string{"sha1", "sha256"} {
		for _, target := range []string{"1k", "1m"} {
			t.Run(format+"/"+target, func(t *testing.T) {
				public := setupEphemeralKeys(t)
				dir := t.TempDir()
				runAt(dir, "git", "init", "-q", "--object-format="+format, "-b", "archive/home")
				configureGitIdentity(dir)
				if err := os.WriteFile(filepath.Join(dir, ".publickeys"), []byte(public+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
				runAt(dir, "git", "config", "remote-aws.bundleSize", "16k")
				var commits []string
				for i := range 5 {
					commits = append(commits, commitBundleData(t, dir, fmt.Sprintf("data %d", i), 32<<10))
				}
				fixture := newMetadataFixture(t)
				fixture.pageSize = 1 // Discovery must follow every page.
				var puts atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodPut && puts.Add(1) == 4 {
						w.WriteHeader(http.StatusForbidden)
						_, _ = io.WriteString(w, `<Error><Code>AccessDenied</Code><Message>interrupt push</Message></Error>`)
						return
					}
					fixture.server.Config.Handler.ServeHTTP(w, r)
				}))
				defer server.Close()
				push := "push refs/heads/archive/home:refs/heads/archive/home"
				if output, err := runMetadataHelper(t, dir, server.URL, push); err == nil || !strings.Contains(output, "interrupt push") {
					t.Fatalf("initial push: %v\n%s", err, output)
				}
				fixture.mu.Lock()
				saved := make(map[string][]byte)
				for key, value := range fixture.objects {
					saved[key] = bytes.Clone(value)
				}
				fixture.objectHeads, fixture.headerReads = nil, nil
				fixture.mu.Unlock()
				if len(saved) != 3 {
					t.Fatalf("expected three completed bundles, got %d", len(saved))
				}
				runAt(dir, "git", "config", "remote-aws.bundleSize", target)
				git, err := exec.LookPath("git")
				if err != nil {
					t.Fatal(err)
				}
				shim := t.TempDir()
				work := filepath.Join(shim, "ranges")
				script := fmt.Sprintf(`#!/bin/sh
case "$1 $2 $3" in
  'rev-list --objects --disk-usage') echo "$4" >> '%s';;
esac
if [ "$1 $2" = 'bundle create' ]; then echo "${3##*/}" >> '%s'; fi
exec '%s' "$@"
`, work, work, git)
				if err := os.WriteFile(filepath.Join(shim, "git"), []byte(script), 0700); err != nil {
					t.Fatal(err)
				}
				t.Setenv("PATH", shim+":"+os.Getenv("PATH"))
				output, err := runMetadataHelper(t, dir, server.URL, push)
				if err != nil {
					t.Fatalf("resume: %v\n%s", err, output)
				}
				ranges, err := os.ReadFile(work)
				if err != nil || len(ranges) == 0 {
					t.Fatalf("did not observe remaining Git work: %v", err)
				}
				for name := range strings.FieldsSeq(string(ranges)) {
					start, _, err := validBundleRange(name)
					if err != nil || start != commits[2] && start != commits[3] {
						t.Errorf("estimated or packed completed history: %s", name)
					}
				}
				fixture.mu.Lock()
				m := fixtureManifest(t, fixture)
				var reused []string
				for _, ref := range m.Bundles {
					key := "/bucket/" + ref.Key
					if original, ok := saved[key]; ok {
						reused = append(reused, key)
						if !bytes.Equal(original, fixture.objects[key]) {
							t.Error("resume rewrote completed ciphertext")
						}
					}
				}
				for key := range saved {
					if fixture.objectHeads[key] != 1 || fixture.headerReads[key] != 0 {
						t.Errorf("warm resume %s: HEAD=%d GET=%d, want 1 and 0", key, fixture.objectHeads[key], fixture.headerReads[key])
					}
				}
				for key, count := range fixture.objectHeads {
					if strings.Contains(key, "/bundles/") && (fixture.objects[key] == nil || count != 1) {
						t.Errorf("probed missing or duplicate candidate: %s, HEAD=%d", key, count)
					}
				}
				fixture.mu.Unlock()
				if len(reused) != len(saved) {
					t.Fatalf("bundleSize change discarded completed boundaries: reused %d of %d", len(reused), len(saved))
				}
				clone := t.TempDir()
				runAt(clone, "git", "init", "-q", "--object-format="+format, "-b", "archive/home")
				if output, err := runMetadataHelper(t, clone, server.URL, "fetch "+m.Tip+" refs/heads/archive/home"); err != nil {
					t.Fatalf("fetch resumed history: %v\n%s", err, output)
				}
				assertBundleHistory(t, dir, clone, m.Tip)
			})
		}
	}
}

// Used by selection tests below to make the selected ranges easy to inspect.
func assertResumeRanges(t *testing.T, got []bundleRef, want ...string) {
	t.Helper()
	var names []string
	for _, ref := range got {
		names = append(names, ref.Range)
	}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("resume ranges %v, want %v", names, want)
	}
}

func TestBundleResumeSelectsDeterministicContiguousPrefix(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	runAt(dir, "git", "init", "-q", "-b", "main")
	configureGitIdentity(dir)
	var commits []string
	for range 6 {
		runAt(dir, "git", "commit", "--allow-empty", "-qm", "checkpoint")
		commits = append(commits, runAtOut(dir, "git", "rev-parse", "HEAD"))
	}
	zero, tip := strings.Repeat("0", 40), last(commits)
	refs := map[string]bundleRef{}
	add := func(from, to string) string {
		name := from + ".." + to
		refs[name] = bundleRef{Range: name, Key: "repo/" + name}
		return name
	}
	add(zero, commits[0])
	add(commits[0], commits[1])
	first := add(zero, commits[1]) // Same endpoint, fewer objects.
	add(commits[1], commits[2])
	add(commits[2], commits[3])
	second := add(commits[1], commits[3])
	lastRange := add(commits[4], tip) // A gap: cannot extend the prefix yet.
	for range 5 {
		got, err := completedPushPrefix(t.Context(), "", tip, refs)
		if err != nil {
			t.Fatal(err)
		}
		assertResumeRanges(t, got, first, second)
	}
	// Equal-length routes to the same boundary use stable key order, not map
	// iteration or request completion order.
	fork, finish := add(zero, commits[2]), commits[2]+".."+commits[3]
	want := []string{first, second}
	if finish < second {
		want = []string{fork, finish}
	}
	for range 5 {
		got, err := completedPushPrefix(t.Context(), "", tip, refs)
		if err != nil {
			t.Fatal(err)
		}
		assertResumeRanges(t, got, want...)
	}
	bridge := add(commits[3], commits[4])
	got, err := completedPushPrefix(t.Context(), commits[1], tip, refs)
	if err != nil {
		t.Fatal(err)
	}
	assertResumeRanges(t, got, second, bridge, lastRange)

	// A remote base can be on a merged side branch rather than the first-parent
	// path. Only boundaries that already include it can resume that push.
	runAt(dir, "git", "checkout", "-qb", "side", commits[1])
	runAt(dir, "git", "commit", "--allow-empty", "-qm", "side")
	side := runAtOut(dir, "git", "rev-parse", "HEAD")
	runAt(dir, "git", "checkout", "-q", "main")
	runAt(dir, "git", "merge", "-q", "--no-ff", "side", "-m", "merge")
	merge := runAtOut(dir, "git", "rev-parse", "HEAD")
	runAt(dir, "git", "commit", "--allow-empty", "-qm", "after merge")
	tip = runAtOut(dir, "git", "rev-parse", "HEAD")
	refs = map[string]bundleRef{}
	add(side, commits[5]) // This first-parent ancestor does not contain side.
	first, second = add(side, merge), add(merge, tip)
	got, err = completedPushPrefix(t.Context(), side, tip, refs)
	if err != nil {
		t.Fatal(err)
	}
	assertResumeRanges(t, got, first, second)
}

func TestBundleResumeValidationConcurrencyAndCancellation(t *testing.T) {
	for _, scenario := range []string{"success", "lease loss", "head failure"} {
		t.Run(scenario, func(t *testing.T) {
			dir, fixture, tip := namespaceSource(t, "sha1")
			t.Chdir(dir)
			fixture.mu.Lock()
			template := fixtureManifest(t, fixture).Bundles[0]
			body := bytes.Clone(fixture.objects["/bucket/"+template.Key])
			var refs []bundleRef
			// Inspect the real ciphertext at distinct keys; this test exercises
			// metadata workers, while selection tests exercise Git ancestry.
			for i := range 19 {
				ref := template
				ref.Range = fmt.Sprintf("%040x..%040x", i, i+1)
				ref.Key = (repository{Namespace: "repo", Name: "1"}).bundleKey(tip, ref.Range)
				fixture.objects["/bucket/"+ref.Key], fixture.pushTips["/bucket/"+ref.Key] = body, tip
				refs = append(refs, ref)
			}
			fixture.objectHeads, fixture.headerReads = nil, nil
			fixture.mu.Unlock()
			started := make(chan struct{}, 100)
			release := make(chan struct{})
			var active, peak atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodHead {
					n := active.Add(1)
					defer active.Add(-1)
					for before := peak.Load(); n > before && !peak.CompareAndSwap(before, n); before = peak.Load() {
					}
					started <- struct{}{}
					select {
					case <-release:
					case <-r.Context().Done():
						return
					}
					if scenario == "head failure" {
						if r.URL.Path == "/bucket/"+refs[0].Key {
							w.WriteHeader(http.StatusForbidden)
							return
						}
						<-r.Context().Done()
						return
					}
					if r.URL.Path == "/bucket/"+refs[0].Key {
						time.Sleep(20 * time.Millisecond) // Complete out of chain order.
					}
				}
				fixture.server.Config.Handler.ServeHTTP(w, r)
			}))
			defer server.Close()
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			validation := bundleUploadClients(server.URL).newBundleValidation(ctx, "bucket")
			type result struct {
				refs []bundleRef
				err  error
			}
			done := make(chan result, 1)
			go func() {
				checked, err := validation.inspectPushPrefix(ctx, tip, refs)
				done <- result{checked, err}
			}()
			for range 8 {
				select {
				case <-started:
				case <-time.After(5 * time.Second):
					t.Fatal("metadata validation did not start eight concurrent requests")
				}
			}
			loss := errors.New("lease lost during resume")
			if scenario == "lease loss" {
				cancel(loss)
			}
			close(release)
			var got result
			select {
			case got = <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("resume validation did not join its workers")
			}
			if peak.Load() != 8 {
				t.Fatalf("metadata concurrency = %d, want 8", peak.Load())
			}
			if scenario != "success" {
				if got.err == nil || len(got.refs) != 0 || scenario == "lease loss" && !errors.Is(got.err, loss) {
					t.Fatalf("failed validation returned %d bundles, %v", len(got.refs), got.err)
				}
				return
			}
			if got.err != nil || !reflect.DeepEqual(got.refs, refs) {
				t.Fatalf("validation changed chain order or identities: %v", got.err)
			}
			fixture.mu.Lock()
			for _, ref := range refs {
				if fixture.objectHeads["/bucket/"+ref.Key] != 1 || fixture.headerReads["/bucket/"+ref.Key] != 1 {
					t.Errorf("cold validation must use one HEAD and one envelope GET: %s", ref.Key)
				}
			}
			fixture.objectHeads, fixture.headerReads = nil, nil
			fixture.mu.Unlock()
			if _, err := validation.inspectPushPrefix(ctx, tip, refs); err != nil {
				t.Fatal(err)
			}
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			for _, ref := range refs {
				if fixture.objectHeads["/bucket/"+ref.Key] != 1 || fixture.headerReads["/bucket/"+ref.Key] != 0 {
					t.Errorf("warm validation must use one HEAD and no envelope GET: %s", ref.Key)
				}
			}
		})
	}
}
