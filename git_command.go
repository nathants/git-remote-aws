package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// Git can spawn pack-objects and other children that inherit its output pipes.
func gitCommand(ctx context.Context, args ...string) *exec.Cmd {
	return groupCommand(ctx, "git", args...)
}

// groupCommand runs name in its own process group. Cancellation kills the whole
// group, and waiting for pipes inherited by descendants is bounded.
func groupCommand(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = time.Second
	return cmd
}
