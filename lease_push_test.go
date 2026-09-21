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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Script the provider responses, not DynamoDB's condition evaluator. The child
// runs the actual CLI, including error recovery, with isolated SDK configuration.
func TestLeasePush(t *testing.T) {
	if os.Getenv("GIT_REMOTE_AWS_PUSH_CHILD") == "1" {
		remote := os.Getenv("GIT_REMOTE_AWS_TEST_REMOTE")
		if remote == "" {
			remote = "aws://bucket+table/repo"
		}
		os.Args = []string{"git-remote-aws", "origin", remote}
		main()
		return
	}
	for _, scenario := range []string{"commit", "upload-failure", "bundles-missing", "commit-unknown", "no-op", "lease-loss", "ancestry-loss", "policy-loss", "recipient-loss", "planning-loss", "commit-unknown-release-failure", "branch-failure-release-failure", "no-op-release-failure"} {
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
			fixture := newMetadataFixture(t)
			if output, err := runMetadataHelper(t, dir, fixture.server.URL, "push refs/heads/master:refs/heads/master"); err != nil {
				t.Fatalf("prepare base: %v\n%s", err, output)
			}
			fixture.mu.Lock()
			baseData := bytes.Clone(fixture.data)
			fixture.mu.Unlock()
			if !strings.HasPrefix(scenario, "no-op") {
				runAt(dir, "git", "commit", "--allow-empty", "-qm", "next")
			}
			tip := runAtOut(dir, "git", "rev-parse", "HEAD")
			blockedPID := filepath.Join(t.TempDir(), "blocked-pid")
			command := map[string]string{"ancestry-loss": "merge-base", "policy-loss": "hash-object", "recipient-loss": "cat-file", "planning-loss": "rev-list"}[scenario]
			if command != "" {
				git, err := exec.LookPath("git")
				if err != nil {
					t.Fatal(err)
				}
				bin := t.TempDir()
				script := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = '%s' ]; then echo $$ > '%s'; kill -STOP $$; fi\nexec '%s' \"$@\"\n", command, blockedPID, git)
				if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0700); err != nil {
					t.Fatal(err)
				}
				t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
				defer func() {
					if text, err := os.ReadFile(blockedPID); err == nil {
						pid, err := strconv.Atoi(strings.TrimSpace(string(text)))
						if err == nil {
							_ = syscall.Kill(pid, syscall.SIGKILL)
						}
					}
				}()
			}
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
					if strings.HasSuffix(target, ".GetItem") && bytes.Contains(body, []byte(`"bucket/repo/1"`)) {
						_, _ = io.WriteString(w, `{}`)
						return
					}
					if strings.HasSuffix(target, ".DescribeTable") {
						_, _ = io.WriteString(w, `{"Table":{"TableStatus":"ACTIVE"}}`)
						return
					}
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
						data := baseData
						if strings.HasPrefix(scenario, "branch-failure") {
							data = bytes.ReplaceAll(data, []byte(`"master"`), []byte(`"other"`))
						}
						_, _ = fmt.Fprintf(w, `{"Attributes":{"id":{"S":"bucket/repo"},"owner_token":%s,"expires_at":%s,"data":%s}}`, request.ExpressionAttributeValues[":owner"], request.ExpressionAttributeValues[":expires"], data)
						return
					} else if request.UpdateExpression == "REMOVE #owner, #expires" {
						releases++
						if strings.HasSuffix(scenario, "release-failure") {
							w.WriteHeader(http.StatusBadRequest)
							_, _ = io.WriteString(w, `{"__type":"AccessDeniedException","message":"release rejected"}`)
							return
						}
					} else if data := request.ExpressionAttributeValues[":data"]; data != nil {
						commits++
						committed = data
						if strings.HasPrefix(scenario, "commit-unknown") {
							w.WriteHeader(http.StatusInternalServerError)
							_, _ = io.WriteString(w, `{"__type":"InternalServerError","message":"response lost"}`)
							return
						}
					} else if command != "" {
						if _, err := os.Stat(blockedPID); err == nil {
							w.WriteHeader(http.StatusBadRequest)
							_, _ = io.WriteString(w, `{"__type":"ConditionalCheckFailedException"}`)
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
				case http.MethodHead:
				case http.MethodGet:
					if scenario == "bundles-missing" {
						w.WriteHeader(http.StatusNotFound)
						_, _ = io.WriteString(w, `<Error><Code>NoSuchKey</Code></Error>`)
						return
					}
				case http.MethodPut:
					if strings.Contains(r.URL.Path, "/manifests/") {
						if scenario == "upload-failure" {
							w.WriteHeader(http.StatusForbidden)
							_, _ = io.WriteString(w, `<Error><Code>AccessDenied</Code><Message>upload rejected</Message></Error>`)
							return
						} else if scenario == "lease-loss" {
							blockOnce.Do(func() { close(blocked) })
							mu.Unlock()
							select {
							case <-r.Context().Done():
							case <-time.After(15 * time.Second):
								t.Error("upload did not stop after lease loss")
							}
							mu.Lock()
							return
						}
					}
				case http.MethodDelete:
					deletes++
				default:
					t.Errorf("unexpected HTTP method %s", r.Method)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				r.Body = io.NopCloser(bytes.NewReader(body))
				fixture.server.Config.Handler.ServeHTTP(w, r)
			}))
			defer server.Close()
			childTimeout := "20s"
			if command != "" {
				childTimeout = "5s"
			}
			child := testHelperCommand(t, dir, server.URL, childTimeout)
			child.Stdin = strings.NewReader("push refs/heads/master:refs/heads/master\n\n")
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
			} else if strings.HasPrefix(scenario, "commit-unknown") {
				wantCommits = 1
			}
			if commits != wantCommits || releases != wantReleases || deletes != wantDeletes {
				t.Fatalf("commits/releases/deletes = %d/%d/%d, want %d/%d/%d\n%s", commits, releases, deletes, wantCommits, wantReleases, wantDeletes, output)
			}
			if wantCommits != 0 && !bytes.Contains(committed, []byte("repo/.remote-aws-v2/repos/1/manifests/"+tip)) {
				t.Fatalf("wrong published metadata: %s", committed)
			}
			var messages []string
			if strings.HasPrefix(scenario, "commit-unknown") {
				messages = append(messages, "write outcome unknown", "response lost")
			}
			if strings.HasPrefix(scenario, "branch-failure") {
				messages = append(messages, "you cannot have multiple branches in a remote")
			}
			if strings.HasSuffix(scenario, "release-failure") {
				messages = append(messages, "release repository lease", "release rejected")
			}
			for _, message := range messages {
				if !bytes.Contains(output, []byte(message)) {
					t.Errorf("CLI lost error %q:\n%s", message, output)
				}
			}
		})
	}
}

func testHelperCommand(t *testing.T, dir, endpoint, timeout string) *exec.Cmd {
	t.Helper()
	child := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestLeasePush$", "-test.timeout="+timeout)
	child.WaitDelay = time.Second
	child.Dir = dir
	child.Env = append(os.Environ(),
		"GIT_REMOTE_AWS_PUSH_CHILD=1", "GIT_DIR=.git", "ensure=",
		"AWS_ACCESS_KEY_ID=test", "AWS_SECRET_ACCESS_KEY=test", "AWS_SESSION_TOKEN=",
		"AWS_REGION=us-east-1", "AWS_DEFAULT_REGION=us-east-1", "AWS_EC2_METADATA_DISABLED=true",
		"AWS_CONFIG_FILE=/dev/null", "AWS_SHARED_CREDENTIALS_FILE=/dev/null", "AWS_PROFILE=",
		"AWS_ENDPOINT_URL_DYNAMODB="+endpoint, "AWS_ENDPOINT_URL_S3="+endpoint,
	)
	return child
}
