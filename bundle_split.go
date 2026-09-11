package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gofrs/uuid/v5"
)

const defaultBundleSize int64 = 1 << 30

func pushBundleSize(ctx context.Context) (int64, error) {
	output, err := gitCommand(ctx, "config", "--type=int", "--get", "remote-aws.bundleSize").CombinedOutput()
	if cause := context.Cause(ctx); cause != nil {
		return 0, cause
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return defaultBundleSize, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read remote-aws.bundleSize: %w: %s", err, output)
	}
	size, err := strconv.ParseInt(strings.TrimSpace(string(output)), 10, 64)
	if err != nil || size <= 0 {
		return 0, fmt.Errorf("remote-aws.bundleSize must be a positive byte count (Git k/m/g suffixes are supported)")
	}
	return size, nil
}

type pushBundleBuilder struct {
	ctx       context.Context
	directory string
	target    int64
	consume   func(filename string) error
}

func (b *pushBundleBuilder) create(base, tip string) error {
	// Boundaries form one ancestry chain. Merged side histories travel with the
	// first-parent step that introduces them, never as unrelated bundle heads.
	target := tip
	if base != "" {
		target = base + ".." + tip
	}
	output, err := gitCommand(b.ctx, "rev-list", "--first-parent", "--reverse", target).Output()
	if err != nil {
		return fmt.Errorf("plan Git bundle boundaries: %w", err)
	}
	commits := strings.Fields(string(output))
	if len(commits) == 0 || last(commits) != tip {
		return fmt.Errorf("no complete ancestry path for Git bundle %s", target)
	}
	if base != "" {
		// Keep only the suffix containing the remote base. Combining Git's
		// --ancestry-path with --first-parent misses a base buried in side
		// history, since that walk never visits the connecting side commits.
		// Containment is monotone along this chain; check its first commit
		// first for the common linear case, then binary-search merge histories.
		low, high := 0, len(commits)
		for low < high {
			middle := low + (high-low)/2
			if low == 0 {
				middle = 0
			}
			err := gitCommand(b.ctx, "merge-base", "--is-ancestor", base, commits[middle]).Run()
			if cause := context.Cause(b.ctx); cause != nil {
				return cause
			}
			var exit *exec.ExitError
			if err == nil {
				high = middle
			} else if errors.As(err, &exit) && exit.ExitCode() == 1 {
				low = middle + 1
			} else {
				return fmt.Errorf("check Git bundle boundary ancestry: %w", err)
			}
		}
		if low == len(commits) {
			return fmt.Errorf("no bundle boundary contains remote base %s", base)
		}
		commits = commits[low:]
	}
	return b.createRange(base, commits)
}

func (b *pushBundleBuilder) createRange(base string, commits []string) error {
	if err := context.Cause(b.ctx); err != nil {
		return err
	}
	tip := last(commits)
	target := tip
	if base != "" {
		target = base + ".." + tip
	}
	if len(commits) > 1 {
		// Existing object storage is a cheap estimate, not a promise about a new
		// pack's deltas or compression. Split before packing a huge history, and
		// verify the resulting file size below. Each split halves the commit
		// range, avoiding commit-by-commit repacking of an ever-growing bundle.
		output, err := gitCommand(b.ctx, "rev-list", "--objects", "--disk-usage", target).Output()
		if err != nil {
			return fmt.Errorf("estimate Git bundle size: %w", err)
		}
		estimate, err := strconv.ParseInt(strings.TrimSpace(string(output)), 10, 64)
		if err != nil || estimate < 0 {
			return fmt.Errorf("invalid Git bundle size estimate: %q", output)
		}
		if estimate > b.target {
			return b.splitRange(base, commits)
		}
	}
	start := base
	if start == "" {
		start = strings.Repeat("0", len(tip))
	}
	filename := filepath.Join(b.directory, start+".."+tip)
	fmt.Fprintln(os.Stderr, "git bundle:", filepath.Base(filename))
	if err := createPinnedPushBundle(b.ctx, filename, base, tip); err != nil {
		return err
	}
	info, err := os.Stat(filename)
	if err != nil {
		return err
	}
	if info.Size() > b.target && len(commits) > 1 {
		if err := os.Remove(filename); err != nil {
			return err
		}
		return b.splitRange(base, commits)
	}
	if info.Size() > b.target {
		fmt.Fprintf(os.Stderr, "bundle exceeds %d-byte target: indivisible first-parent increment (%d bytes)\n", b.target, info.Size())
	}
	if err := b.consume(filename); err != nil {
		return err
	}
	return os.Remove(filename)
}

func (b *pushBundleBuilder) splitRange(base string, commits []string) error {
	middle := len(commits) / 2
	if err := b.createRange(base, commits[:middle]); err != nil {
		return err
	}
	return b.createRange(commits[middle-1], commits[middle:])
}

func createPinnedPushBundle(ctx context.Context, filename, base, tip string) (err error) {
	id, err := uuid.NewV4()
	if err != nil {
		return err
	}
	ref := "refs/git-remote-aws/" + id.String()
	// Git bundle requires a named ref, not a raw object ID. A private ref pins
	// each boundary without moving the user's branch. Conditional updates and
	// deletion cannot clobber any concurrent ref edit, even during cleanup.
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		output, cleanupErr := gitCommand(cleanup, "update-ref", "--no-deref", "-d", ref, tip).CombinedOutput()
		if cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("remove temporary bundle ref %s: %w: %s", ref, cleanupErr, output))
		}
	}()
	output, err := gitCommand(ctx, "update-ref", "--no-deref", ref, tip, strings.Repeat("0", len(tip))).CombinedOutput()
	if err != nil {
		return fmt.Errorf("pin Git bundle boundary: %w: %s", err, output)
	}
	target := ref
	if base != "" {
		target = base + ".." + ref
	}
	return createPushBundle(ctx, filename, target, tip)
}
