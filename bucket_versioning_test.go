package main

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestLeaseVersioningPreflight(t *testing.T) {
	dir, fixture, tip := namespaceSource(t, "sha1")
	fixture.mu.Lock()
	fixture.versioningDenied = true
	commits, uploads, deletes := fixture.commits, fixture.uploads, fixture.deletes
	checks := fixture.versioningChecks
	fixture.mu.Unlock()
	// Both an already-current v2 destination and a new destination must check.
	for _, repo := range []string{"repo", "repo/2"} {
		output, err := runRepositoryHelper(t, dir, fixture.server.URL, repo, "push refs/heads/archive/home:refs/heads/archive/home")
		if err == nil || !strings.Contains(output, "GetBucketVersioning") {
			t.Errorf("push bypassed versioning failure: %v\n%s", err, output)
		}
	}
	// Read-only operations must work without versioning permissions.
	if output, err := runRepositoryHelper(t, dir, fixture.server.URL, "repo", "list"); err != nil {
		t.Fatalf("list required versioning permission: %v\n%s", err, output)
	}
	clone := t.TempDir()
	runAt(clone, "git", "init", "-q", "-b", "archive/home")
	if output, err := runRepositoryHelper(t, clone, fixture.server.URL, "repo", "fetch "+tip+" refs/heads/archive/home"); err != nil {
		t.Fatalf("fetch required versioning permission: %v\n%s", err, output)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if fixture.commits != commits || fixture.uploads != uploads || fixture.deletes != deletes || fixture.versioningChecks-checks != 2 {
		t.Errorf("failed preflight mutated history or skipped checks: commits=%d uploads=%d deletes=%d checks=%d", fixture.commits-commits, fixture.uploads-uploads, fixture.deletes-deletes, fixture.versioningChecks-checks)
	}
}

func TestLeaseVersioningStates(t *testing.T) {
	for _, scenario := range []struct {
		name, status, failure string
		want                  []string
	}{
		{"enabled", "Enabled", "", []string{"get"}},
		{"never-enabled", "", "", []string{"get", "put"}},
		{"suspended", "Suspended", "", []string{"get", "put"}},
		{"unknown", "Unknown", "unknown versioning status", []string{"get"}},
		{"get-denied", "", "get", []string{"get"}},
		{"put-denied", "", "put", []string{"get", "put"}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			var calls []string
			status := scenario.status
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !r.URL.Query().Has("versioning") || r.URL.Path != "/bucket" {
					t.Errorf("unexpected operation: %s", r.URL)
				}
				switch r.Method {
				case http.MethodGet:
					calls = append(calls, "get")
					if scenario.failure == "get" {
						w.WriteHeader(http.StatusForbidden)
						_, _ = io.WriteString(w, `<Error><Code>AccessDenied</Code></Error>`)
						return
					}
					_, _ = fmt.Fprintf(w, `<VersioningConfiguration><Status>%s</Status><MfaDelete>Enabled</MfaDelete></VersioningConfiguration>`, status)
				case http.MethodPut:
					calls = append(calls, "put")
					var config struct {
						Status    string
						MFADelete string `xml:"MfaDelete"`
					}
					if err := xml.NewDecoder(r.Body).Decode(&config); err != nil || config.Status != "Enabled" || config.MFADelete != "" {
						t.Errorf("bad versioning request: %+v %v", config, err)
					}
					if scenario.failure == "put" {
						w.WriteHeader(http.StatusForbidden)
						_, _ = io.WriteString(w, `<Error><Code>AccessDenied</Code></Error>`)
						return
					}
					status = "Enabled"
				default:
					t.Errorf("unexpected method: %s", r.Method)
				}
			}))
			defer server.Close()
			clients := bundleUploadClients(server.URL)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			err := clients.ensureBucketVersioning(ctx, "bucket")
			if (err != nil) != (scenario.failure != "") || !reflect.DeepEqual(calls, scenario.want) {
				t.Fatalf("err=%v calls=%v want=%v", err, calls, scenario.want)
			}
			if scenario.failure == "put" && !strings.Contains(err.Error(), "s3:PutBucketVersioning") {
				t.Fatalf("missing permission guidance: %v", err)
			}
		})
	}
}

func TestLeaseVersioningEnablementProceeds(t *testing.T) {
	dir, fixture, _ := namespaceSource(t, "sha1")
	var enabled atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Has("versioning") {
			switch r.Method {
			case http.MethodGet:
				if enabled.Load() {
					t.Error("unexpected post-enablement polling")
				}
				_, _ = io.WriteString(w, `<VersioningConfiguration><Status>Suspended</Status></VersioningConfiguration>`)
			case http.MethodPut:
				enabled.Store(true)
			default:
				t.Errorf("unexpected versioning operation: %s", r.Method)
			}
			return
		}
		if r.Method == http.MethodPut || strings.HasSuffix(r.Header.Get("X-Amz-Target"), ".UpdateItem") {
			if !enabled.Load() {
				t.Error("push wrote before enabling versioning")
			}
		}
		fixture.server.Config.Handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	if output, err := runRepositoryHelper(t, dir, server.URL, "repo/2", "push refs/heads/archive/home:refs/heads/archive/home"); err != nil {
		t.Fatalf("push did not proceed after enablement: %v\n%s", err, output)
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	if !enabled.Load() || fixture.records["bucket/repo/2"] == nil {
		t.Fatal("enablement did not lead to publication")
	}
}
