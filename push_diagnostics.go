package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Diagnostics never fetch, mutate refs, or print commit bodies/key material.
// They describe locally available ancestry, not whether somebody rewrote it.
func logPushComparison(ctx context.Context, ref, local, remote string) {
	fmt.Fprintf(os.Stderr, "push comparison: ref=%s local=%s remote=%s\n", ref, local, remote)
	contains, known := gitBranchContains(ctx, local, remote)
	if contains {
		return
	}
	if !known {
		fmt.Fprintln(os.Stderr, "push comparison: remote tip unavailable locally; fetch to compare ancestry")
		return
	}
	counts, err := gitCommand(ctx, "rev-list", "--left-right", "--count", local+"..."+remote).Output()
	if err != nil {
		fmt.Fprintf(os.Stderr, "push comparison: ancestry counts unavailable: %v\n", err)
		return
	}
	fields := strings.Fields(string(counts))
	bases, err := gitCommand(ctx, "merge-base", "--all", local, remote).Output()
	base := strings.Join(strings.Fields(string(bases)), ",")
	if err != nil {
		if exit, ok := errors.AsType[*exec.ExitError](err); ok && exit.ExitCode() == 1 {
			base = "none"
		} else {
			fmt.Fprintf(os.Stderr, "push comparison: merge bases unavailable: %v\n", err)
			return
		}
	}
	fmt.Fprintf(os.Stderr, "push comparison: non-fast-forward in local history; local-only=%s remote-only=%s merge-bases=%s\n", fields[0], fields[1], base)
}
