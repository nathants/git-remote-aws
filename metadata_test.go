package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Script successful provider responses and retain actual uploaded ciphertext.
// This fixture does not emulate DynamoDB's lease condition evaluator.
type metadataFixture struct {
	server                    *httptest.Server
	mu                        sync.Mutex
	data                      json.RawMessage
	objects                   map[string][]byte
	commits, uploads, deletes int
	reads, missing            int
	afterRead                 func()
}

func newMetadataFixture(t *testing.T) *metadataFixture {
	t.Helper()
	fixture := &metadataFixture{data: json.RawMessage(`{"M":{}}`), objects: make(map[string][]byte)}
	fixture.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		fixture.mu.Lock()
		defer fixture.mu.Unlock()
		if target := r.Header.Get("X-Amz-Target"); target != "" {
			w.Header().Set("Content-Type", "application/x-amz-json-1.0")
			switch strings.TrimPrefix(target, "DynamoDB_20120810.") {
			case "DescribeTable":
				_, _ = io.WriteString(w, `{"Table":{"TableStatus":"ACTIVE"}}`)
			case "GetItem":
				fixture.reads++
				data, afterRead := fixture.data, fixture.afterRead
				fixture.afterRead = nil
				if afterRead != nil {
					fixture.mu.Unlock()
					afterRead()
					fixture.mu.Lock()
				}
				_, _ = fmt.Fprintf(w, `{"Item":{"id":{"S":"bucket/repo"},"data":%s}}`, data)
			case "UpdateItem":
				var request struct {
					UpdateExpression          string
					ExpressionAttributeValues map[string]json.RawMessage
				}
				if err := json.Unmarshal(body, &request); err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				switch request.UpdateExpression {
				case "SET #owner = :owner, #expires = :expires":
					_, _ = fmt.Fprintf(w, `{"Attributes":{"id":{"S":"bucket/repo"},"owner_token":%s,"expires_at":%s,"data":%s}}`, request.ExpressionAttributeValues[":owner"], request.ExpressionAttributeValues[":expires"], fixture.data)
				case "REMOVE #owner, #expires", "SET #expires = :next":
					_, _ = io.WriteString(w, `{}`)
				default:
					next := request.ExpressionAttributeValues[":data"]
					if next == nil {
						t.Errorf("unexpected metadata update: %s", body)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					fixture.data = next
					fixture.commits++
					_, _ = io.WriteString(w, `{}`)
				}
			default:
				t.Errorf("unexpected DynamoDB operation: %s", target)
				w.WriteHeader(http.StatusBadRequest)
			}
			return
		}
		switch r.Method {
		case http.MethodHead:
			w.Header().Set("X-Amz-Bucket-Region", "us-east-1")
		case http.MethodPut:
			fixture.objects[r.URL.Path] = body
			fixture.uploads++
		case http.MethodGet:
			body, ok := fixture.objects[r.URL.Path]
			if !ok {
				fixture.missing++
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, `<Error><Code>NoSuchKey</Code><Message>list was deleted</Message></Error>`)
				return
			}
			_, _ = w.Write(body)
		case http.MethodDelete:
			delete(fixture.objects, r.URL.Path)
			fixture.deletes++
		default:
			t.Errorf("unexpected S3 method: %s", r.Method)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(fixture.server.Close)
	return fixture
}

func runMetadataHelper(t *testing.T, directory, endpoint, command string) (string, error) {
	t.Helper()
	child := testHelperCommand(t, directory, endpoint, "20s")
	child.Env = append(child.Env, "AWS_REQUEST_CHECKSUM_CALCULATION=when_required")
	child.Stdin = strings.NewReader(command + "\n\n")
	output, err := child.CombinedOutput()
	return string(output), err
}

func TestBundleReadSurvivesConcurrentPublication(t *testing.T) {
	for _, operation := range []string{"list", "list for-push", "fetch"} {
		t.Run(operation, func(t *testing.T) {
			public := setupEphemeralKeys(t)
			dir := t.TempDir()
			runAt(dir, "git", "init", "-q", "--object-format=sha256", "-b", "archive/home")
			configureGitIdentity(dir)
			if err := os.WriteFile(filepath.Join(dir, ".publickeys"), []byte(public+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			runAt(dir, "git", "add", ".publickeys")
			runAt(dir, "git", "commit", "-qm", "base")
			base := runAtOut(dir, "git", "rev-parse", "HEAD")
			fixture := newMetadataFixture(t)
			push := "push refs/heads/archive/home:refs/heads/archive/home"
			if output, err := runMetadataHelper(t, dir, fixture.server.URL, push); err != nil {
				t.Fatalf("initial publication: %v\n%s", err, output)
			}
			runAt(dir, "git", "commit", "--allow-empty", "-qm", "next")
			tip := runAtOut(dir, "git", "rev-parse", "HEAD")
			readerDirectory := dir
			if operation == "fetch" {
				readerDirectory = t.TempDir()
				runAt(readerDirectory, "git", "init", "-q", "--object-format=sha256", "-b", "archive/home")
				operation = "fetch " + base + " refs/heads/archive/home"
			}
			// Hold the already-selected DynamoDB response until the actual writer
			// publishes the next pointer and deletes the old bundle list.
			fixture.mu.Lock()
			fixture.afterRead = func() {
				if output, err := runMetadataHelper(t, dir, fixture.server.URL, push); err != nil {
					t.Errorf("overlapping publication: %v\n%s", err, output)
				}
			}
			fixture.mu.Unlock()
			output, err := runMetadataHelper(t, readerDirectory, fixture.server.URL, operation)
			if err != nil {
				t.Fatalf("reader failed across valid publication: %v\n%s", err, output)
			}
			if strings.HasPrefix(operation, "list") {
				if !strings.Contains(output, tip+" refs/heads/archive/home\n") {
					t.Fatalf("reader did not rediscover the current pointer:\n%s", output)
				}
			} else {
				if got := runAtOut(readerDirectory, "git", "show", base+":.publickeys"); got != public {
					t.Fatalf("requested historical commit was not fetched intact: %q", got)
				}
				if got := runAtOut(readerDirectory, "git", "rev-parse", tip+"^"); got != base {
					t.Fatalf("rediscovered chain lost its original base: %q", got)
				}
			}
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			if fixture.reads != 2 || fixture.missing != 1 || fixture.deletes != 1 || fixture.commits != 2 || !bytes.Contains(fixture.data, []byte("repo/bundles_"+tip)) {
				t.Fatalf("race not exercised: reads=%d missing=%d deletes=%d commits=%d", fixture.reads, fixture.missing, fixture.deletes, fixture.commits)
			}
		})
	}
}

func TestBundleRediscoveryScopeAndLimit(t *testing.T) {
	for _, scenario := range []struct {
		name        string
		reads, gets int32
		success     bool
	}{
		{name: "advances-twice", reads: 3, gets: 3, success: true},
		{name: "unchanged-missing", reads: 3, gets: 3},
		{name: "keeps-advancing", reads: 3, gets: 3},
		{name: "access-denied", reads: 1, gets: 1},
		{name: "no-such-bucket", reads: 1, gets: 1},
		{name: "other-404", reads: 1, gets: 1},
		{name: "malformed-list", reads: 1, gets: 1},
		{name: "truncated-list", reads: 1, gets: 1},
		{name: "pointer-vanishes", reads: 2, gets: 1},
		{name: "pointer-cleared", reads: 2, gets: 1},
		{name: "branch-changes", reads: 2, gets: 1},
		{name: "metadata-denied", reads: 2, gets: 1},
		{name: "initially-empty", reads: 1, gets: 0, success: true},
		{name: "bundle-body-missing", reads: 1, gets: 2},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			var mu sync.Mutex
			var reads, gets int32
			tip := strings.Repeat("a", 40)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				if target := r.Header.Get("X-Amz-Target"); target != "" {
					w.Header().Set("Content-Type", "application/x-amz-json-1.0")
					switch strings.TrimPrefix(target, "DynamoDB_20120810.") {
					case "DescribeTable":
						_, _ = io.WriteString(w, `{"Table":{"TableStatus":"ACTIVE"}}`)
					case "GetItem":
						reads++
						if scenario.name == "initially-empty" || scenario.name == "pointer-vanishes" && reads > 1 {
							_, _ = io.WriteString(w, `{}`)
							return
						}
						if scenario.name == "metadata-denied" && reads > 1 {
							w.WriteHeader(http.StatusBadRequest)
							_, _ = io.WriteString(w, `{"__type":"AccessDeniedException","message":"denied"}`)
							return
						}
						branch, key := "master", "repo/bundles_old"
						if scenario.name == "advances-twice" || scenario.name == "keeps-advancing" {
							key = fmt.Sprintf("repo/bundles_%d", reads)
						}
						if scenario.name == "pointer-cleared" && reads > 1 {
							key = ""
						}
						if scenario.name == "branch-changes" && reads > 1 {
							branch = "other"
						}
						_, _ = fmt.Fprintf(w, `{"Item":{"id":{"S":"bucket/repo"},"data":{"M":{"branch":{"S":%q},"bundles":{"S":%q}}}}}`, branch, key)
					default:
						t.Errorf("reader attempted unexpected DynamoDB operation: %s", target)
						w.WriteHeader(http.StatusBadRequest)
					}
					return
				}
				switch r.Method {
				case http.MethodHead:
					w.Header().Set("X-Amz-Bucket-Region", "us-east-1")
				case http.MethodGet:
					gets++
					if scenario.name == "advances-twice" && gets == 3 || scenario.name == "bundle-body-missing" && gets == 1 {
						_, _ = fmt.Fprintf(w, "%s..%s", zeroHash, tip)
						return
					}
					if scenario.name == "malformed-list" {
						_, _ = io.WriteString(w, "not a bundle list")
						return
					}
					if scenario.name == "truncated-list" {
						w.Header().Set("Content-Length", "1000")
						_, _ = io.WriteString(w, "short")
						return
					}
					status, code := http.StatusNotFound, "NoSuchKey"
					switch scenario.name {
					case "access-denied":
						status, code = http.StatusForbidden, "AccessDenied"
					case "no-such-bucket":
						code = "NoSuchBucket"
					case "other-404":
						code = "NotFound"
					}
					w.WriteHeader(status)
					// The message deliberately contains NoSuchKey in nonretryable cases.
					_, _ = fmt.Fprintf(w, `<Error><Code>%s</Code><Message>NoSuchKey is not an error classifier</Message></Error>`, code)
				default:
					t.Errorf("reader attempted S3 mutation: %s", r.Method)
					w.WriteHeader(http.StatusBadRequest)
				}
			}))
			defer server.Close()
			dir := t.TempDir()
			runAt(dir, "git", "init", "-q", "-b", "master")
			command := "list"
			if scenario.name == "bundle-body-missing" {
				command = "fetch " + tip + " refs/heads/master"
			}
			output, err := runMetadataHelper(t, dir, server.URL, command)
			if (err == nil) != scenario.success {
				t.Errorf("unexpected discovery result: %v\n%s", err, output)
			}
			mu.Lock()
			defer mu.Unlock()
			if reads != scenario.reads || gets != scenario.gets {
				t.Errorf("wrong retry scope: reads=%d gets=%d, want %d/%d\n%s", reads, gets, scenario.reads, scenario.gets, output)
			}
		})
	}
}

func TestBundleRediscoveryCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	meta, bundles, err := readPublishedMetadata(ctx, "unused-table", "unused-bucket", "unused-repo")
	if !errors.Is(err, context.Canceled) || meta != nil || bundles != nil {
		t.Fatalf("canceled discovery returned metadata: %+v %v %v", meta, bundles, err)
	}
}
