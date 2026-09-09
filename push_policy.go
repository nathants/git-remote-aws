package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/nathants/go-libsodium"
)

// Check the index and the actual working file separately. Git's stat cache and
// assume-unchanged/skip-worktree flags must not hide uncommitted recipient edits.
func requireCommittedRecipients(tip string) error {
	root, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return fmt.Errorf("locate .publickeys worktree: %w", err)
	}
	worktree := strings.TrimSuffix(string(root), "\n")
	index, err := exec.Command("git", "rev-parse", "--git-path", "index").Output()
	if err != nil {
		return fmt.Errorf("locate .publickeys index: %w", err)
	}
	// Callers using git commit-tree need not create an index.
	// An existing index, including an explicitly staged deletion, must agree.
	if _, err := os.Lstat(strings.TrimSuffix(string(index), "\n")); err == nil {
		// An ordinary literal filename also works when our caller pins
		// GIT_LITERAL_PATHSPECS=1. Pathspec magic would become a different name.
		if err := exec.Command("git", "-C", worktree, "diff", "--cached", "--quiet", "--no-ext-diff", "--no-textconv", tip, "--", ".publickeys").Run(); err != nil {
			return fmt.Errorf(".publickeys index must match the pushed commit; commit recipient changes before pushing: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect .publickeys index: %w", err)
	}
	tree, err := exec.Command("git", "ls-tree", "--full-tree", tip, "--", ".publickeys").Output()
	if err != nil {
		return fmt.Errorf("inspect committed .publickeys: %w", err)
	}
	fields := strings.Fields(string(tree))
	if len(fields) != 4 || fields[1] != "blob" || fields[3] != ".publickeys" || (fields[0] != "100644" && fields[0] != "100755") {
		return fmt.Errorf("committed .publickeys must be a regular file")
	}
	filename := filepath.Join(worktree, ".publickeys")
	info, err := os.Lstat(filename)
	if err != nil {
		return fmt.Errorf("inspect working .publickeys: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > libsodium.MaxKeyChainsBytes || (info.Mode().Perm()&0100 != 0) != (fields[0] == "100755") {
		return fmt.Errorf("working .publickeys type, mode, or size does not match a valid committed policy")
	}
	actual, err := exec.Command("git", "hash-object", "--no-filters", "--", filename).Output()
	if err != nil {
		return fmt.Errorf("hash working .publickeys: %w", err)
	}
	if strings.TrimSpace(string(actual)) != fields[2] {
		return fmt.Errorf(".publickeys has uncommitted changes; commit recipient changes before pushing")
	}
	return nil
}

func createPushBundle(ctx context.Context, filename, target, expectedTip string) error {
	output, err := exec.CommandContext(ctx, "git", "bundle", "create", filename, target).CombinedOutput()
	if err != nil {
		return fmt.Errorf("create Git bundle: %w: %s", err, output)
	}
	// The branch may move while this push is running. Never encrypt or upload
	// a bundle whose content tip differs from its identity and recipient policy.
	heads, err := exec.CommandContext(ctx, "git", "bundle", "list-heads", filename).Output()
	if err != nil {
		return fmt.Errorf("inspect generated Git bundle: %w", err)
	}
	fields := strings.Fields(string(heads))
	if len(fields) != 2 || fields[0] != expectedTip {
		return fmt.Errorf("branch tip changed during bundle creation; retry the push")
	}
	return nil
}
