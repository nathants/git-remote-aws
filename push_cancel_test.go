package main

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Stop the actual pack-objects process, not a replacement, while git bundle
// waits on it. Cancellation must terminate both processes and unblock the pipes.
func TestBundleCancellationKillsPackObjects(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	runAt(dir, "git", "init", "-q", "-b", "main")
	configureGitIdentity(dir)
	data := make([]byte, 32<<20)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("file", data, 0600); err != nil {
		t.Fatal(err)
	}
	runAt(dir, "git", "add", "file")
	runAt(dir, "git", "commit", "-qm", "data")
	tip := runAtOut(dir, "git", "rev-parse", "HEAD")
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	pidfile := filepath.Join(bin, "pid")
	wrapper := fmt.Sprintf("#!/bin/sh\necho $$ > '%s'\nexec '%s' \"$@\"\n", pidfile, git)
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(wrapper), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- createPushBundle(ctx, filepath.Join(dir, "out.bundle"), "main", tip) }()
	var parent, pack int
	defer func() {
		if pack != 0 {
			_ = syscall.Kill(pack, syscall.SIGKILL)
		}
		if parent != 0 {
			_ = syscall.Kill(parent, syscall.SIGKILL)
		}
	}()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && pack == 0 {
		if text, err := os.ReadFile(pidfile); err == nil {
			parent, _ = strconv.Atoi(strings.TrimSpace(string(text)))
		}
		if parent != 0 {
			children, _ := os.ReadFile(fmt.Sprintf("/proc/%d/task/%d/children", parent, parent))
			for _, child := range strings.Fields(string(children)) {
				pid, _ := strconv.Atoi(child)
				command, _ := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
				if strings.Contains(string(command), "pack-objects") && syscall.Kill(pid, syscall.SIGSTOP) == nil {
					pack = pid
					break
				}
			}
		}
		if pack == 0 {
			time.Sleep(time.Millisecond)
		}
	}
	if pack == 0 {
		t.Fatal("did not observe the actual Git pack-objects child")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("canceled bundle creation succeeded")
		}
	case <-time.After(3 * time.Second):
		_ = syscall.Kill(pack, syscall.SIGKILL)
		_ = syscall.Kill(parent, syscall.SIGKILL)
		<-done
		t.Fatal("cancellation left pack-objects holding the output pipes")
	}
	// A killed orphan can briefly remain a zombie until init reaps it.
	status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pack))
	if err == nil && !strings.Contains(string(status), "State:\tZ") {
		t.Fatalf("pack-objects remained alive after cancellation:\n%s", status)
	}
}
