package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Interleave real Git ref mutations with the helper's real bundle commands.
// A changed object ID is not the only way another writer can replace a pin.
func TestBundlePinnedRefOwnership(t *testing.T) {
	for _, format := range []string{"sha1", "sha256"} {
		for _, scenario := range []string{"unchanged", "pin-collision", "pin-response-lost", "different-oid", "symref-same-oid", "symref-different-oid", "symref-dangling", "bundle-and-cleanup-failure"} {
			t.Run(format+"/"+scenario, func(t *testing.T) {
				dir := t.TempDir()
				t.Chdir(dir)
				runAt(dir, "git", "init", "-q", "--object-format="+format, "-b", "main")
				configureGitIdentity(dir)
				runAt(dir, "git", "commit", "--allow-empty", "-qm", "base")
				base := runAtOut(dir, "git", "rev-parse", "HEAD")
				runAt(dir, "git", "commit", "--allow-empty", "-qm", "tip")
				tip := runAtOut(dir, "git", "rev-parse", "HEAD")
				runAt(dir, "git", "update-ref", "refs/heads/other", base)
				git, err := exec.LookPath("git")
				if err != nil {
					t.Fatal(err)
				}
				bin := t.TempDir()
				marker := filepath.Join(bin, "pin")
				script := fmt.Sprintf(`#!/bin/sh
set -eu
real=%q
scenario=%q
marker=%q
if [ "$1" = update-ref ] && [ "$2" = --no-deref ] && [ "$3" != -d ] && [ "$3" != --stdin ]; then
    printf '%%s\n' "$3" > "$marker"
    if [ "$scenario" = pin-collision ]; then
        "$real" "$@"
    elif [ "$scenario" = pin-response-lost ]; then
        "$real" "$@"
        echo 'pin response lost' >&2
        exit 1
    fi
fi
if [ "$1" = bundle ] && [ "$2" = create ] && [ "$scenario" = bundle-and-cleanup-failure ]; then
    "$real" update-ref --no-deref "$(cat "$marker")" %q
    echo 'bundle creation failed' >&2
    exit 1
fi
if [ "$1" = bundle ] && [ "$2" = list-heads ]; then
    "$real" "$@"
    ref=$(cat "$marker")
    case "$scenario" in
        different-oid) "$real" update-ref --no-deref "$ref" %q ;;
        symref-same-oid) "$real" symbolic-ref "$ref" refs/heads/main ;;
        symref-different-oid) "$real" symbolic-ref "$ref" refs/heads/other ;;
        symref-dangling) "$real" symbolic-ref "$ref" refs/heads/missing ;;
    esac
    exit 0
fi
exec "$real" "$@"
`, git, scenario, marker, base, base)
				if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0700); err != nil {
					t.Fatal(err)
				}
				t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
				err = createPinnedPushBundle(t.Context(), filepath.Join(t.TempDir(), "bundle"), "", tip)
				if (err == nil) != (scenario == "unchanged") {
					t.Errorf("unexpected bundle outcome: %v", err)
				}
				message := map[string]string{
					"pin-collision": "reference already exists", "pin-response-lost": "pin response lost",
					"bundle-and-cleanup-failure": "bundle creation failed",
				}[scenario]
				if message != "" && (err == nil || !strings.Contains(err.Error(), message)) {
					t.Errorf("primary failure was lost: %v", err)
				}
				recorded, readErr := os.ReadFile(marker)
				if readErr != nil {
					t.Fatal(readErr)
				}
				ref := strings.TrimSpace(string(recorded))
				switch {
				case scenario == "unchanged":
					if refs := runAtOut(dir, git, "for-each-ref", "refs/git-remote-aws/"); refs != "" {
						t.Fatalf("owned ref survived cleanup: %s", refs)
					}
				case strings.HasPrefix(scenario, "symref-"):
					want := map[string]string{"symref-same-oid": "main", "symref-different-oid": "other", "symref-dangling": "missing"}[scenario]
					out, inspectErr := exec.Command(git, "symbolic-ref", ref).CombinedOutput()
					if inspectErr != nil || strings.TrimSpace(string(out)) != "refs/heads/"+want {
						t.Errorf("concurrent symbolic ref was lost: %v, %s", inspectErr, out)
					}
				default:
					want := base
					if strings.HasPrefix(scenario, "pin-") {
						want = tip
					}
					out, inspectErr := exec.Command(git, "rev-parse", "--verify", ref).CombinedOutput()
					if inspectErr != nil || strings.TrimSpace(string(out)) != want {
						t.Errorf("unowned direct ref was lost: %v, %s", inspectErr, out)
					}
				}
				if got := runAtOut(dir, git, "rev-parse", "refs/heads/main", "refs/heads/other"); got != tip+"\n"+base {
					t.Errorf("cleanup changed a user branch: %s", got)
				}
				// Successful and aborted cleanup transactions must release their
				// locks so the preserved ref remains writable by its owner.
				runAt(dir, git, "update-ref", "--no-deref", ref, tip)
				runAt(dir, git, "update-ref", "--no-deref", "-d", ref, tip)
			})
		}
	}
}

func TestBundlePinnedRefCleanupTransaction(t *testing.T) {
	for _, scenario := range []string{"type-check-locked", "type-check-failure"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			t.Chdir(dir)
			runAt(dir, "git", "init", "-q", "-b", "main")
			configureGitIdentity(dir)
			runAt(dir, "git", "commit", "--allow-empty", "-qm", "tip")
			tip := runAtOut(dir, "git", "rev-parse", "HEAD")
			ref := "refs/git-remote-aws/test"
			runAt(dir, "git", "update-ref", ref, tip)
			git, err := exec.LookPath("git")
			if err != nil {
				t.Fatal(err)
			}
			bin := t.TempDir()
			marker := filepath.Join(bin, "mutation")
			script := fmt.Sprintf(`#!/bin/sh
real=%q
if [ "$1" = symbolic-ref ] && [ "$2" = --quiet ]; then
    if [ %q = type-check-failure ]; then
        echo 'type inspection failed' >&2
        exit 128
    fi
    "$real" "$@"
    status=$?
    "$real" -c core.filesRefLockTimeout=0 symbolic-ref "$3" refs/heads/main > %q.error 2>&1
    printf '%%s\n' "$?" > %q
    exit "$status"
fi
exec "$real" "$@"
`, git, scenario, marker, marker)
			if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
			err = removePinnedPushRef(t.Context(), ref, tip)
			if scenario == "type-check-locked" {
				if err != nil {
					t.Fatal(err)
				}
				status, readErr := os.ReadFile(marker)
				if readErr != nil || strings.TrimSpace(string(status)) != "1" {
					t.Fatalf("concurrent mutation was not rejected while inspecting type: %q, %v", status, readErr)
				}
				message, readErr := os.ReadFile(marker + ".error")
				if readErr != nil || !strings.Contains(string(message), "lock") {
					t.Fatalf("concurrent mutation did not fail on the transaction lock: %s, %v", message, readErr)
				}
				if refs := runAtOut(dir, git, "for-each-ref", "refs/git-remote-aws/"); refs != "" {
					t.Errorf("owned ref survived the committed deletion: %s", refs)
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), "type inspection failed") {
					t.Fatalf("type inspection failure was lost: %v", err)
				}
				if got := runAtOut(dir, git, "rev-parse", "--verify", ref); got != tip {
					t.Fatalf("failed type inspection changed the ref: %s", got)
				}
			}
			// Both commit and abort release the transaction lock.
			runAt(dir, git, "update-ref", "--no-deref", ref, tip)
			runAt(dir, git, "update-ref", "--no-deref", "-d", ref, tip)
		})
	}
}
