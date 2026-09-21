package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
)

func TestBundleValidationCacheReuse(t *testing.T) {
	dir, fixture, _ := namespaceSource(t, "sha1")
	fixture.mu.Lock()
	source := fixtureManifest(t, fixture)
	fixture.objectHeads, fixture.headerReads = nil, nil
	fixture.mu.Unlock()
	for i, repo := range []string{"repo/2", "repo/3"} {
		if output, err := runRepositoryHelper(t, dir, fixture.server.URL, repo, "push refs/heads/archive/home:refs/heads/archive/home"); err != nil {
			t.Fatalf("adoption: %v\n%s", err, output)
		}
		fixture.mu.Lock()
		for _, ref := range source.Bundles {
			path := "/bucket/" + ref.Key
			wantRanges := 2
			if i > 0 {
				wantRanges = 0
			}
			if fixture.objectHeads[path] != 1 || fixture.headerReads[path] != wantRanges {
				t.Errorf("%s: HEAD=%d range GET=%d; want HEAD=1 range GET=%d", repo, fixture.objectHeads[path], fixture.headerReads[path], wantRanges)
			}
		}
		fixture.objectHeads, fixture.headerReads = nil, nil
		fixture.mu.Unlock()
	}
}

func TestBundleValidationCacheSafety(t *testing.T) {
	for _, scenario := range []string{"missing-cache", "truncated-cache", "bad-checksum", "cache-directory-blocked", "cache-fifo", "missing-object", "changed-object", "wrong-policy", "endpoint-isolation", "legacy-hit"} {
		t.Run(scenario, func(t *testing.T) {
			dir, fixture, _ := namespaceSource(t, "sha1")
			t.Chdir(dir)
			clients := bundleUploadClients(fixture.server.URL)
			fixture.mu.Lock()
			ref := fixtureManifest(t, fixture).Bundles[0]
			fixture.mu.Unlock()
			first := clients.newBundleValidation(t.Context(), "bucket")
			if _, err := first.inspect(t.Context(), ref); err != nil {
				t.Fatal(err)
			}
			files, err := filepath.Glob(filepath.Join(first.directory, "*.json"))
			if err != nil || len(files) != 1 {
				t.Fatalf("cache not persisted: %v %v", files, err)
			}
			wantError := false
			wantRanges := 2
			switch scenario {
			case "missing-cache":
				err = os.Remove(files[0])
			case "truncated-cache":
				err = os.WriteFile(files[0], []byte("{"), 0600)
			case "bad-checksum":
				var entry cachedBundleValidation
				data, readErr := os.ReadFile(files[0])
				if readErr != nil {
					t.Fatal(readErr)
				}
				if err := json.Unmarshal(data, &entry); err != nil {
					t.Fatal(err)
				}
				entry.Bundle.Recipients[0] = strings.Repeat("a", 128)
				data, err = json.Marshal(entry)
				if err == nil {
					err = os.WriteFile(files[0], data, 0600)
				}
			case "cache-directory-blocked":
				err = os.RemoveAll(first.directory)
				if err == nil {
					err = os.WriteFile(first.directory, []byte("not a directory"), 0600)
				}
			case "cache-fifo":
				err = os.Remove(files[0])
				if err == nil {
					err = syscall.Mkfifo(files[0], 0600)
				}
			case "missing-object":
				fixture.mu.Lock()
				delete(fixture.objects, "/bucket/"+ref.Key)
				fixture.mu.Unlock()
				wantError, wantRanges = true, 0
			case "changed-object":
				fixture.mu.Lock()
				fixture.objects["/bucket/"+ref.Key][0] ^= 1
				fixture.mu.Unlock()
				wantError, wantRanges = true, 0
			case "wrong-policy":
				ref.Recipients = []string{strings.Repeat("a", 128)}
				wantError, wantRanges = true, 0
			case "endpoint-isolation":
				proxy := httptest.NewServer(fixture.server.Config.Handler)
				defer proxy.Close()
				clients = bundleUploadClients(proxy.URL)
			case "legacy-hit":
				ref.Size, ref.ETag, ref.Recipients = 0, "", nil
				wantRanges = 0
			}
			if err != nil {
				t.Fatal(err)
			}
			fixture.mu.Lock()
			fixture.objectHeads, fixture.headerReads = nil, nil
			fixture.mu.Unlock()
			checked, err := clients.newBundleValidation(t.Context(), "bucket").inspect(t.Context(), ref)
			if (err != nil) != wantError {
				t.Fatalf("validation: %v", err)
			}
			if err == nil && (checked.ETag == "" || checked.Size == 0 || len(checked.Recipients) == 0) {
				t.Fatal("validation not enriched")
			}
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			if fixture.objectHeads["/bucket/"+ref.Key] != 1 || fixture.headerReads["/bucket/"+ref.Key] != wantRanges {
				t.Fatalf("HEAD=%v range GET=%v want ranges=%d", fixture.objectHeads, fixture.headerReads, wantRanges)
			}
		})
	}
}

func TestBundleValidationConflictingDescriptors(t *testing.T) {
	dir, fixture, _ := namespaceSource(t, "sha1")
	t.Chdir(dir)
	fixture.mu.Lock()
	ref := fixtureManifest(t, fixture).Bundles[0]
	fixture.mu.Unlock()
	clients := bundleUploadClients(fixture.server.URL)
	validation := clients.newBundleValidation(t.Context(), "bucket")
	first, err := validation.inspect(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	// Returned recipient slices must not mutate the operation's retained evidence.
	first.Recipients[0] = strings.Repeat("f", 128)
	if again, err := validation.inspect(t.Context(), ref); err != nil || !reflect.DeepEqual(again, ref) {
		t.Fatalf("mutable cache evidence: %+v %v", again, err)
	}
	for _, field := range []string{"range", "size", "etag", "recipients"} {
		bad := ref
		switch field {
		case "range":
			bad.Range = strings.Replace(ref.Range, "0", "1", 1)
		case "size":
			bad.Size++
		case "etag":
			bad.ETag += "different"
		case "recipients":
			bad.Recipients = []string{strings.Repeat("f", 128)}
		}
		if _, err := validation.inspect(t.Context(), bad); err == nil {
			t.Errorf("accepted conflicting %s", field)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := validation.inspect(ctx, ref); !errors.Is(err, context.Canceled) {
		t.Fatalf("cached validation ignored cancellation: %v", err)
	}
}

func TestBundleValidationCacheWorktreeAndConcurrentWrites(t *testing.T) {
	dir, fixture, _ := namespaceSource(t, "sha1")
	worktree := filepath.Join(t.TempDir(), "linked")
	runAt(dir, "git", "worktree", "add", "--detach", worktree, "HEAD")
	t.Chdir(worktree)
	clients := bundleUploadClients(fixture.server.URL)
	fixture.mu.Lock()
	ref := fixtureManifest(t, fixture).Bundles[0]
	fixture.mu.Unlock()
	validation := clients.newBundleValidation(t.Context(), "bucket")
	expected := runAtOut(worktree, "git", "rev-parse", "--path-format=absolute", "--git-path", "git-remote-aws/validation-v2")
	if validation.directory != expected || strings.HasPrefix(expected, filepath.Join(worktree, ".git")+"/") {
		t.Fatalf("not Git-resolved metadata: %q != %q", validation.directory, expected)
	}
	var group sync.WaitGroup
	for range 8 {
		group.Go(func() {
			if _, err := clients.newBundleValidation(t.Context(), "bucket").inspect(t.Context(), ref); err != nil {
				t.Error(err)
			}
		})
	}
	group.Wait()
	fixture.mu.Lock()
	fixture.headerReads = nil
	fixture.mu.Unlock()
	if _, err := clients.newBundleValidation(t.Context(), "bucket").inspect(t.Context(), ref); err != nil {
		t.Fatal(err)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if len(fixture.headerReads) != 0 {
		t.Fatal("concurrent writes did not leave a usable cache")
	}
	files, err := os.ReadDir(expected)
	if err != nil || len(files) != 1 {
		t.Fatalf("cache temp files leaked: %v %v", files, err)
	}
}
