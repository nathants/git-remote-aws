package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
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
	// each boundary without moving the user's branch. Only a confirmed create
	// grants cleanup ownership; a failed or interrupted create may belong to
	// another writer, even if its object ID matches.
	output, err := gitCommand(ctx, "update-ref", "--no-deref", ref, tip, strings.Repeat("0", len(tip))).CombinedOutput()
	if err != nil {
		return fmt.Errorf("pin Git bundle boundary %s (inspect this ref if creation was interrupted): %w: %s", ref, err, output)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if cleanupErr := removePinnedPushRef(cleanup, ref, tip); cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("remove temporary bundle ref %s: %w", ref, cleanupErr))
		}
	}()
	target := ref
	if base != "" {
		target = base + ".." + ref
	}
	return createPushBundle(ctx, filename, target, tip)
}

func removePinnedPushRef(ctx context.Context, ref, tip string) (err error) {
	// An old-OID guard still follows symbolic refs even with --no-deref. Hold
	// Git's transaction lock while checking the ref's type, then commit only a
	// direct ref deletion. Closing stdin without commit aborts the transaction.
	cmd := gitCommand(ctx, "update-ref", "--no-deref", "--stdin")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	input, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	defer func() { _ = input.Close() }()
	output, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	defer func() { _ = output.Close() }()
	if err := cmd.Start(); err != nil {
		return err
	}
	defer func() {
		_ = input.Close()
		if waitErr := cmd.Wait(); waitErr != nil {
			err = errors.Join(err, fmt.Errorf("run Git ref cleanup transaction: %w: %s", waitErr, stderr.String()))
		}
	}()
	reader := bufio.NewReader(output)
	exchange := func(command, response string) error {
		if _, err := io.WriteString(input, command+"\n"); err != nil {
			return err
		}
		line, err := reader.ReadString('\n')
		if err != nil {
			return err
		}
		if line != response+"\n" {
			return fmt.Errorf("unexpected Git ref transaction response: %q", line)
		}
		return nil
	}
	if err := exchange("start", "start: ok"); err != nil {
		return err
	}
	if err := exchange("delete "+ref+" "+tip+"\nprepare", "prepare: ok"); err != nil {
		return err
	}
	symbolic, err := gitCommand(ctx, "symbolic-ref", "--quiet", ref).CombinedOutput()
	if err == nil {
		return fmt.Errorf("temporary bundle ref became symbolic: %s", strings.TrimSpace(string(symbolic)))
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		return fmt.Errorf("inspect temporary bundle ref type: %w: %s", err, symbolic)
	}
	return exchange("commit", "commit: ok")
}
