package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nathants/go-libsodium"
)

func TestBundleResumeFailsClosed(t *testing.T) {
	for _, scenario := range []struct{ name, message string }{
		{"list-failure", "discover completed push bundles"},
		{"missing-push-tip", "different or unknown push tip"},
		{"wrong-push-tip", "different or unknown push tip"},
		{"malformed-range", "invalid completed push object"},
		{"missing-after-list", "inspect bundle"},
		{"size-after-list", "bundle identity changed"},
		{"etag-after-list", "bundle identity changed"},
		{"changed-after-head", "PreconditionFailed"},
		{"zero-recipient-count", "recipient policy disagrees"},
		{"oversized-recipient-count", "recipient policy disagrees"},
		{"invalid-record-length", "invalid encrypted recipient record"},
		{"duplicate-recipients", "duplicate encrypted recipients"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			public := setupEphemeralKeys(t)
			other, _, err := libsodium.BoxKeypair()
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			runAt(dir, "git", "init", "-q", "-b", "archive/home")
			configureGitIdentity(dir)
			if err := os.WriteFile(filepath.Join(dir, ".publickeys"), fmt.Appendf(nil, "%s\n%x\n", public, other), 0600); err != nil {
				t.Fatal(err)
			}
			tip := commitBundleData(t, dir, "data", 32<<10)
			key := "/bucket/" + (repository{Namespace: "repo", Name: "1"}).bundleKey(tip, strings.Repeat("0", 40)+".."+tip)
			fixture := newMetadataFixture(t)
			var retry atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Leave a fully uploaded, cached bundle, but no published manifest.
				if r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/manifests/") {
					w.WriteHeader(http.StatusForbidden)
					_, _ = io.WriteString(w, `<Error><Code>AccessDenied</Code><Message>interrupt publication</Message></Error>`)
					return
				}
				if retry.Load() && scenario.name == "list-failure" && r.Method == http.MethodGet && r.URL.Query().Get("prefix") == strings.TrimPrefix(filepath.Dir(key)+"/", "/bucket/") {
					w.WriteHeader(http.StatusForbidden)
					_, _ = io.WriteString(w, `<Error><Code>AccessDenied</Code></Error>`)
					return
				}
				if retry.Load() && r.URL.Path == key {
					fixture.mu.Lock()
					if r.Method == http.MethodHead {
						switch scenario.name {
						case "missing-after-list":
							delete(fixture.objects, key)
						case "size-after-list":
							fixture.objects[key] = append(fixture.objects[key], 0)
						case "etag-after-list":
							fixture.objects[key][len(fixture.objects[key])-1] ^= 1
						}
					}
					if r.Method == http.MethodGet && scenario.name == "changed-after-head" {
						fixture.objects[key][0] ^= 1
					}
					fixture.mu.Unlock()
				}
				fixture.server.Config.Handler.ServeHTTP(w, r)
			}))
			defer server.Close()
			push := "push refs/heads/archive/home:refs/heads/archive/home"
			if output, err := runMetadataHelper(t, dir, server.URL, push); err == nil || !strings.Contains(output, "interrupt publication") {
				t.Fatalf("initial push: %v\n%s", err, output)
			}
			fixture.mu.Lock()
			if len(fixture.objects[key]) == 0 || fixture.uploads != 1 || fixture.commits != 0 {
				fixture.mu.Unlock()
				t.Fatal("did not leave exactly one unpublished bundle")
			}
			body := fixture.objects[key]
			switch scenario.name {
			case "missing-push-tip":
				delete(fixture.pushTips, key)
			case "wrong-push-tip":
				fixture.pushTips[key] = strings.Repeat("a", 40)
			case "malformed-range":
				fixture.objects[filepath.Dir(key)+"/not-a-range"] = body
			case "zero-recipient-count":
				binary.LittleEndian.PutUint32(body, 0)
			case "oversized-recipient-count":
				binary.LittleEndian.PutUint32(body, ^uint32(0))
			case "invalid-record-length":
				binary.LittleEndian.PutUint32(body[4:], 0)
			case "duplicate-recipients":
				// Two real sealed-key records; only duplicate their fingerprints.
				const recordBytes = 4 + 64 + 48 + 32
				copy(body[8+recordBytes:8+recordBytes+64], body[8:8+64])
			}
			fixture.mu.Unlock()
			if scenario.name == "changed-after-head" {
				cache := runAtOut(dir, "git", "rev-parse", "--path-format=absolute", "--git-path", "git-remote-aws/validation-v2")
				if err := os.RemoveAll(cache); err != nil {
					t.Fatal(err)
				}
			}
			retry.Store(true)
			output, err := runMetadataHelper(t, dir, server.URL, push)
			if err == nil || !strings.Contains(output, scenario.message) {
				t.Fatalf("unsafe resume: %v\n%s", err, output)
			}
			if strings.Contains(output, "reuse completed bundle") || strings.Contains(output, "git bundle:") {
				t.Errorf("reported invalid object as reusable or tried to replace it:\n%s", output)
			}
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			if fixture.commits != 0 || fixture.uploads != 1 || fixture.deletes != 0 || string(fixture.data) != `{"M":{}}` {
				t.Errorf("failed validation changed remote state: commits=%d uploads=%d deletes=%d data=%s", fixture.commits, fixture.uploads, fixture.deletes, fixture.data)
			}
		})
	}
}
