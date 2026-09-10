package main

import (
	"bytes"
	"context"
	"encoding/json"
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

func TestRefSlashBranchRoundTrip(t *testing.T) {
	for _, objectFormat := range []string{"sha1", "sha256"} {
		t.Run(objectFormat, func(t *testing.T) {
			public := setupEphemeralKeys(t)
			dir := t.TempDir()
			runAt(dir, "git", "init", "-q", "--object-format="+objectFormat, "-b", "archive/home")
			configureGitIdentity(dir)
			if err := os.WriteFile(filepath.Join(dir, ".publickeys"), []byte(public+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			runAt(dir, "git", "add", ".publickeys")
			runAt(dir, "git", "commit", "-qm", "base")
			base := runAtOut(dir, "git", "rev-parse", "HEAD")

			// Script successful provider responses and retain the actual uploaded
			// ciphertext for fetch. This does not emulate lease conditions.
			var mu sync.Mutex
			data := json.RawMessage(`{"M":{}}`)
			objects := make(map[string][]byte)
			commits, uploads := 0, 0
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
					switch strings.TrimPrefix(target, "DynamoDB_20120810.") {
					case "DescribeTable":
						_, _ = io.WriteString(w, `{"Table":{"TableStatus":"ACTIVE"}}`)
					case "GetItem":
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
							_, _ = fmt.Fprintf(w, `{"Attributes":{"id":{"S":"bucket/repo"},"owner_token":%s,"expires_at":%s,"data":%s}}`, request.ExpressionAttributeValues[":owner"], request.ExpressionAttributeValues[":expires"], data)
						case "REMOVE #owner, #expires", "SET #expires = :next":
							_, _ = io.WriteString(w, `{}`)
						default:
							next := request.ExpressionAttributeValues[":data"]
							if next == nil {
								t.Errorf("unexpected metadata update: %s", body)
								w.WriteHeader(http.StatusBadRequest)
								return
							}
							data = next
							commits++
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
					objects[r.URL.Path] = body
					uploads++
				case http.MethodGet:
					body, ok := objects[r.URL.Path]
					if !ok {
						t.Errorf("read absent object: %s", r.URL.Path)
						w.WriteHeader(http.StatusNotFound)
						return
					}
					_, _ = w.Write(body)
				case http.MethodDelete:
					delete(objects, r.URL.Path)
				default:
					t.Errorf("unexpected S3 method: %s", r.Method)
					w.WriteHeader(http.StatusBadRequest)
				}
			}))
			defer server.Close()
			runHelper := func(directory, command string) (string, error) {
				t.Helper()
				child := testHelperCommand(t, directory, server.URL, "20s")
				child.Env = append(child.Env, "AWS_REQUEST_CHECKSUM_CALCULATION=when_required")
				child.Stdin = strings.NewReader(command + "\n\n")
				output, err := child.CombinedOutput()
				return string(output), err
			}
			push := "push refs/heads/archive/home:refs/heads/archive/home"
			if output, err := runHelper(dir, push); err != nil {
				t.Fatalf("publish slash branch genesis: %v\n%s", err, output)
			}
			if err := os.WriteFile(filepath.Join(dir, "payload"), []byte("slash branch payload\n"), 0600); err != nil {
				t.Fatal(err)
			}
			runAt(dir, "git", "add", "payload")
			runAt(dir, "git", "commit", "-qm", "next")
			tip := runAtOut(dir, "git", "rev-parse", "HEAD")
			if output, err := runHelper(dir, push); err != nil {
				t.Fatalf("publish slash branch delta: %v\n%s", err, output)
			}
			if output, err := runHelper(dir, push); err != nil {
				t.Fatalf("no-op slash branch push: %v\n%s", err, output)
			}
			runAt(dir, "git", "branch", "archive/other")
			for command, message := range map[string]string{
				"push refs/heads/archive/other:refs/heads/archive/other": "you cannot have multiple branches in a remote",
				"push refs/heads/archive/home:refs/heads/archive/other":  "local branch is different from remote branch",
				"push +refs/heads/archive/home:refs/heads/archive/home":  "force push is not allowed",
			} {
				if output, err := runHelper(dir, command); err == nil || !strings.Contains(output, message) {
					t.Errorf("lost single-branch/force protection: %v\n%s", err, output)
				}
			}
			if output, err := runHelper(dir, "list"); err != nil || !strings.Contains(output, tip+" refs/heads/archive/home\n") || !strings.Contains(output, "@refs/heads/archive/home HEAD\n") {
				t.Fatalf("discover slash branch: %v\n%s", err, output)
			}
			fresh := t.TempDir()
			runAt(fresh, "git", "init", "-q", "--object-format="+objectFormat, "-b", "archive/home")
			if output, err := runHelper(fresh, "fetch "+tip+" refs/heads/archive/home"); err != nil {
				t.Fatalf("fetch actual encrypted slash-branch chain: %v\n%s", err, output)
			}
			if got := runAtOut(fresh, "git", "rev-parse", tip+"^"); got != base {
				t.Fatalf("fetched wrong ancestry: %s, want %s", got, base)
			}
			if got := runAtOut(fresh, "git", "show", tip+":payload"); got != "slash branch payload" {
				t.Fatalf("fetched wrong content: %q", got)
			}
			mu.Lock()
			defer mu.Unlock()
			if commits != 2 || uploads != 4 || !bytes.Contains(data, []byte(`"branch":{"S":"archive/home"}`)) {
				t.Fatalf("wrong published metadata: commits=%d uploads=%d data=%s", commits, uploads, data)
			}
		})
	}
}

func TestRefBranchContextAndLiteralName(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	mustPanicContains(t, "context canceled", func() { refBranch(ctx, "refs/heads/main") })

	dir := t.TempDir()
	runAt(dir, "git", "init", "-q", "-b", "main")
	configureGitIdentity(dir)
	runAt(dir, "git", "commit", "--allow-empty", "-qm", "base")
	runAt(dir, "git", "checkout", "-qb", "other")
	t.Chdir(dir)
	if got := runAtOut(dir, "git", "check-ref-format", "--branch", "@{-1}"); got != "main" {
		t.Fatalf("checkout-expression control did not expand: %q", got)
	}
	mustPanicContains(t, "literal branch", func() { refBranch(t.Context(), "refs/heads/@{-1}") })
}
