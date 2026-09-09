package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nathants/go-libsodium"
)

// Script the provider responses, not DynamoDB's condition evaluator. The child
// runs the actual push path with isolated SDK configuration and real Git data.
func TestLeasePush(t *testing.T) {
	if os.Getenv("GIT_REMOTE_AWS_PUSH_CHILD") == "1" {
		libsodium.Init()
		push("table", "bucket", "repo", "push refs/heads/master:refs/heads/master")
		return
	}
	for _, scenario := range []string{"commit", "upload-failure", "commit-unknown", "no-op", "lease-loss"} {
		t.Run(scenario, func(t *testing.T) {
			public := setupEphemeralKeys(t)
			dir := t.TempDir()
			runAt(dir, "git", "init", "-q", "-b", "master")
			configureGitIdentity(dir)
			if err := os.WriteFile(filepath.Join(dir, ".publickeys"), []byte(public+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			runAt(dir, "git", "add", ".publickeys")
			runAt(dir, "git", "commit", "-qm", "base")
			base := runAtOut(dir, "git", "rev-parse", "HEAD")
			if scenario != "no-op" {
				runAt(dir, "git", "commit", "--allow-empty", "-qm", "next")
			}
			tip := runAtOut(dir, "git", "rev-parse", "HEAD")
			var mu sync.Mutex
			var commits, releases, deletes int
			var committed json.RawMessage
			var acquired bool
			blocked := make(chan struct{})
			var blockOnce sync.Once
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				mu.Lock()
				defer mu.Unlock()
				if target := r.Header.Get("X-Amz-Target"); target != "" {
					w.Header().Set("Content-Type", "application/x-amz-json-1.0")
					var request struct {
						UpdateExpression          string
						ExpressionAttributeValues map[string]json.RawMessage
					}
					if err := json.Unmarshal(body, &request); err != nil {
						t.Error(err)
					}
					if !strings.HasSuffix(target, ".UpdateItem") {
						t.Errorf("unexpected DynamoDB operation: %s", target)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					if !acquired {
						acquired = true
						data := `{"branch":{"S":"master"},"bundles":{"S":"repo/bundles_old"}}`
						_, _ = fmt.Fprintf(w, `{"Attributes":{"id":{"S":"bucket/repo"},"owner_token":%s,"expires_at":%s,"data":{"M":%s}}}`, request.ExpressionAttributeValues[":owner"], request.ExpressionAttributeValues[":expires"], data)
						return
					} else if request.UpdateExpression == "REMOVE #owner, #expires" {
						releases++
					} else if data := request.ExpressionAttributeValues[":data"]; data != nil {
						commits++
						committed = data
						if scenario == "commit-unknown" {
							w.WriteHeader(http.StatusInternalServerError)
							_, _ = io.WriteString(w, `{"__type":"InternalServerError","message":"response lost"}`)
							return
						}
					} else if scenario == "lease-loss" {
						select {
						case <-blocked:
							w.WriteHeader(http.StatusBadRequest)
							_, _ = io.WriteString(w, `{"__type":"ConditionalCheckFailedException"}`)
							return
						default:
						}
					}
					_, _ = io.WriteString(w, `{}`)
					return
				}
				switch r.Method {
				case http.MethodGet:
					_, _ = fmt.Fprintf(w, "%s..%s", zeroHash, base)
				case http.MethodPut:
					if strings.Contains(r.URL.Path, "/bundles_") {
						if scenario == "upload-failure" {
							w.WriteHeader(http.StatusForbidden)
							_, _ = io.WriteString(w, `<Error><Code>AccessDenied</Code><Message>upload rejected</Message></Error>`)
						} else if scenario == "lease-loss" {
							blockOnce.Do(func() { close(blocked) })
							mu.Unlock()
							select {
							case <-r.Context().Done():
							case <-time.After(15 * time.Second):
								t.Error("upload did not stop after lease loss")
							}
							mu.Lock()
						}
					}
				case http.MethodDelete:
					deletes++
				default:
					t.Errorf("unexpected HTTP method %s", r.Method)
					w.WriteHeader(http.StatusBadRequest)
				}
			}))
			defer server.Close()
			child := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestLeasePush$", "-test.timeout=20s")
			child.Dir = dir
			child.Env = append(os.Environ(),
				"GIT_REMOTE_AWS_PUSH_CHILD=1", "AWS_ACCESS_KEY_ID=test", "AWS_SECRET_ACCESS_KEY=test", "AWS_SESSION_TOKEN=",
				"AWS_REGION=us-east-1", "AWS_DEFAULT_REGION=us-east-1", "AWS_EC2_METADATA_DISABLED=true",
				"AWS_CONFIG_FILE=/dev/null", "AWS_SHARED_CREDENTIALS_FILE=/dev/null", "AWS_PROFILE=",
				"AWS_ENDPOINT_URL_DYNAMODB="+server.URL, "AWS_ENDPOINT_URL_S3="+server.URL,
			)
			output, err := child.CombinedOutput()
			wantSuccess := scenario == "commit" || scenario == "no-op"
			if (err == nil) != wantSuccess {
				t.Fatalf("push result: %v\n%s", err, output)
			}
			mu.Lock()
			defer mu.Unlock()
			wantCommits, wantReleases, wantDeletes := 0, 1, 0
			if scenario == "commit" {
				wantCommits, wantReleases, wantDeletes = 1, 0, 1
			} else if scenario == "commit-unknown" {
				wantCommits = 1
			}
			if commits != wantCommits || releases != wantReleases || deletes != wantDeletes {
				t.Fatalf("commits/releases/deletes = %d/%d/%d, want %d/%d/%d\n%s", commits, releases, deletes, wantCommits, wantReleases, wantDeletes, output)
			}
			if wantCommits != 0 && !bytes.Contains(committed, []byte("repo/bundles_"+tip)) {
				t.Fatalf("wrong published metadata: %s", committed)
			}
		})
	}
}
