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
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestLeaseHelperSignals(t *testing.T) {
	for _, operation := range []string{"push", "list for-push", "fetch", "idle"} {
		for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
			t.Run(operation+"/"+sig.String(), func(t *testing.T) {
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
				runAt(dir, "git", "commit", "--allow-empty", "-qm", "next")
				var releases atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body, err := io.ReadAll(r.Body)
					if err != nil {
						t.Error(err)
						return
					}
					data := `{"branch":{"S":"master"},"bundles":{"S":"repo/bundles_old"}}`
					if target := r.Header.Get("X-Amz-Target"); target != "" {
						w.Header().Set("Content-Type", "application/x-amz-json-1.0")
						switch strings.TrimPrefix(target, "DynamoDB_20120810.") {
						case "DescribeTable":
							_, _ = io.WriteString(w, `{"Table":{"TableStatus":"ACTIVE"}}`)
						case "GetItem":
							_, _ = fmt.Fprintf(w, `{"Item":{"id":{"S":"bucket/repo"},"data":{"M":%s}}}`, data)
						case "UpdateItem":
							var request struct {
								UpdateExpression          string
								ExpressionAttributeValues map[string]json.RawMessage
							}
							if err := json.Unmarshal(body, &request); err != nil {
								t.Error(err)
								return
							}
							switch request.UpdateExpression {
							case "SET #owner = :owner, #expires = :expires":
								_, _ = fmt.Fprintf(w, `{"Attributes":{"id":{"S":"bucket/repo"},"owner_token":%s,"expires_at":%s,"data":{"M":%s}}}`, request.ExpressionAttributeValues[":owner"], request.ExpressionAttributeValues[":expires"], data)
							case "REMOVE #owner, #expires":
								releases.Add(1)
								_, _ = io.WriteString(w, `{}`)
							case "SET #expires = :next":
								_, _ = io.WriteString(w, `{}`)
							default:
								t.Errorf("unexpected payload write: %s", body)
								w.WriteHeader(http.StatusBadRequest)
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
					case http.MethodGet:
						_, _ = fmt.Fprintf(w, "%s..%s", zeroHash, base)
					default:
						t.Errorf("unexpected S3 operation: %s", r.Method)
						w.WriteHeader(http.StatusBadRequest)
					}
				}))
				defer server.Close()

				git, err := exec.LookPath("git")
				if err != nil {
					t.Fatal(err)
				}
				bin := t.TempDir()
				pidfile := filepath.Join(bin, "pids")
				blocked := map[string]string{"push": "bundle", "list for-push": "hash-object", "fetch": "merge-base"}[operation]
				// Delay the external command without SIGSTOP: a stopped orphaned
				// process group receives kernel SIGHUP, masking missing cancellation.
				script := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = '%s' ]; then\n printf '%%s\\n' \"$3\" > '%s.bundle'\n sleep 30 &\n child=$!\n printf '%%s %%s\\n' $$ $child > '%s'\n wait $child\nfi\nexec '%s' \"$@\"\n", blocked, pidfile, pidfile, git)
				defer func() {
					if text, err := os.ReadFile(pidfile); err == nil {
						for _, text := range strings.Fields(string(text)) {
							if pid, err := strconv.Atoi(text); err == nil && pid > 0 {
								_ = syscall.Kill(pid, syscall.SIGKILL)
							}
						}
					}
					if operation == "push" {
						if text, err := os.ReadFile(pidfile + ".bundle"); err == nil {
							directory := filepath.Dir(strings.TrimSpace(string(text)))
							if filepath.Dir(directory) == "/tmp" && strings.HasPrefix(filepath.Base(directory), tempdirPrefix) {
								_ = os.RemoveAll(directory)
							}
						}
					}
				}()
				if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0700); err != nil {
					t.Fatal(err)
				}
				t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
				child := testHelperCommand(t, dir, server.URL, "15s")
				child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
				input, err := child.StdinPipe()
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = input.Close() }()
				stdout, err := child.StdoutPipe()
				if err != nil {
					t.Fatal(err)
				}
				var stderr bytes.Buffer
				child.Stderr = &stderr
				if err := child.Start(); err != nil {
					t.Fatal(err)
				}
				done := make(chan error, 1)
				go func() { done <- child.Wait() }()
				var pids []int
				waited := false
				defer func() {
					_ = child.Process.Kill()
					for _, pid := range pids {
						_ = syscall.Kill(pid, syscall.SIGKILL)
					}
					if !waited {
						<-done
					}
				}()
				command := map[string]string{
					"push":          "push refs/heads/master:refs/heads/master\n",
					"list for-push": "list for-push\n",
					"fetch":         "fetch " + base + " refs/heads/master\n",
					"idle":          "capabilities\n",
				}[operation]
				if _, err := io.WriteString(input, command); err != nil {
					t.Fatal(err)
				}
				ready := make(chan error, 1)
				go func() {
					if operation == "idle" {
						capabilities := make([]byte, len("push\nfetch\n\n"))
						_, err := io.ReadFull(stdout, capabilities)
						ready <- err
						return
					}
					deadline := time.Now().Add(5 * time.Second)
					for time.Now().Before(deadline) {
						text, err := os.ReadFile(pidfile)
						if err == nil && len(strings.Fields(string(text))) == 2 {
							ready <- nil
							return
						}
						time.Sleep(time.Millisecond)
					}
					ready <- fmt.Errorf("Git did not reach the blocked command")
				}()
				select {
				case err := <-ready:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(6 * time.Second):
					t.Fatal("helper did not become ready for cancellation")
				}
				if operation != "idle" {
					text, err := os.ReadFile(pidfile)
					if err != nil {
						t.Fatal(err)
					}
					for _, text := range strings.Fields(string(text)) {
						pid, err := strconv.Atoi(text)
						if err != nil || pid <= 0 {
							t.Fatalf("invalid child pid: %q", text)
						}
						pids = append(pids, pid)
					}
				}
				// Ctrl-C targets the foreground group; service SIGTERM can target
				// only the helper. Both must cancel its separately grouped Git work.
				target := child.Process.Pid
				if sig == syscall.SIGINT {
					target = -target
				}
				if err := syscall.Kill(target, sig); err != nil {
					t.Fatal(err)
				}
				select {
				case err := <-done:
					waited = true
					if err == nil {
						t.Fatal("interrupted helper succeeded")
					}
				case <-time.After(5 * time.Second):
					_ = child.Process.Kill()
					<-done
					waited = true
					t.Fatalf("helper did not exit on %s:\n%s", sig, stderr.String())
				}
				for _, pid := range pids {
					status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
					if err == nil && !bytes.Contains(status, []byte("State:\tZ")) {
						t.Errorf("Git process %d survived %s", pid, sig)
					}
				}
				wantReleases := int32(0)
				if operation == "push" {
					wantReleases = 1
					text, err := os.ReadFile(pidfile + ".bundle")
					if err != nil {
						t.Fatal(err)
					}
					if _, err := os.Stat(filepath.Dir(strings.TrimSpace(string(text)))); !os.IsNotExist(err) {
						t.Errorf("push temporary files survived cancellation: %v", err)
					}
					if refs := runAtOut(dir, "git", "for-each-ref", "refs/git-remote-aws/"); refs != "" {
						t.Errorf("temporary bundle refs survived cancellation: %s", refs)
					}
				}
				if got := releases.Load(); got != wantReleases {
					t.Errorf("lease releases = %d, want %d:\n%s", got, wantReleases, stderr.String())
				}
			})
		}
	}
}
