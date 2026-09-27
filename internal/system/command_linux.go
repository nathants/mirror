package system

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Serialize terminal ownership, including foreground restoration. Acquisition
// is cancellable; a second invocation must not steal a running command's tty.
var commandSlot = make(chan struct{}, 1)

// RunCommand runs an unstarted exec.Command, owning its new process group until
// no live member remains. Do not use CommandContext, Start/Wait concurrently, or
// set SysProcAttr. Dir, Env and streams retain os/exec semantics. Stream methods
// must not block indefinitely; WaitDelay cannot interrupt arbitrary Go I/O.
//
// This is lifecycle management for trusted, non-daemonizing tools, not a sandbox:
// descendants that deliberately change process group/session escape ownership.
// Linux /proc must expose the group's processes. Kernel-blocked exits or lost
// /proc visibility retain ownership (and the caller's operation lock), not a
// false claim of completed cleanup.
func RunCommand(ctx context.Context, cmd *exec.Cmd) (returnErr error) {
	if cmd.Process != nil || cmd.SysProcAttr != nil || cmd.Cancel != nil {
		return errors.New("owned command requires an unstarted exec.Command without SysProcAttr or Cancel")
	}
	select {
	case commandSlot <- struct{}{}:
		defer func() { <-commandSlot }()
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := liveGroup(unix.Getpgrp()); err != nil {
		return fmt.Errorf("preflight process visibility: %w", err)
	}
	// Our own PID is not a child. ECHILD proves this wait mode is available
	// without consuming any child status; refuse seccomp/unsupported failures.
	var info unix.Siginfo
	switch err := unix.Waitid(unix.P_PID, os.Getpid(), &info, unix.WEXITED|unix.WNOWAIT|unix.WNOHANG, nil); {
	case err == nil:
		return errors.New("preflight non-reaping wait: expected ECHILD, got success")
	case !errors.Is(err, unix.ECHILD):
		return fmt.Errorf("preflight non-reaping wait: expected ECHILD: %w", err)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	tty, err := commandTerminal()
	if err != nil {
		return err
	}
	if tty != nil {
		original, err := unix.IoctlGetTermios(int(tty.Fd()), unix.TCGETS)
		if err != nil {
			return errors.Join(err, tty.Close())
		}
		defer func() {
			returnErr = errors.Join(returnErr, restoreTerminal(tty, original), tty.Close())
		}()
		cmd.SysProcAttr.Foreground = true
		cmd.SysProcAttr.Ctty = int(tty.Fd())
	}
	cmd.WaitDelay = 250 * time.Millisecond
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	pid := cmd.Process.Pid
	// WNOWAIT pins the leader's PID/PGID until all group signalling and
	// verification are finished. Cmd.Wait must not race negative-PID kills:
	// reaping first could let the numeric group ID be reused by unrelated work.
	exited := make(chan error, 1)
	go func() {
		var info unix.Siginfo
		for {
			err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
			if !errors.Is(err, unix.EINTR) {
				exited <- err
				return
			}
		}
	}()
	var lifecycleErr, waitErr error
	select {
	case waitErr = <-exited:
	case <-ctx.Done():
		lifecycleErr = errors.Join(ctx.Err(), killGroup(pid))
		waitErr = <-exited
	}
	if waitErr != nil {
		// A foreign child reaper violates ownership. Never signal a numeric
		// group after losing the unreaped leader that pins its identity.
		return errors.Join(lifecycleErr, fmt.Errorf("lost command wait ownership; helper cleanup unverified: %w", waitErr), cmd.Wait())
	}
	// Also clean up abandoned helpers on normal parent exit. Such a command
	// did not complete synchronously and must not be reported as successful.
	var cleanupErr error
	for {
		live, inspectErr := liveGroup(pid)
		if inspectErr == nil && !live {
			break
		}
		if lifecycleErr == nil {
			lifecycleErr = errors.New("command left live helpers or process cleanup could not be verified")
		}
		if inspectErr != nil && cleanupErr == nil {
			cleanupErr = fmt.Errorf("inspect command group: %w", inspectErr)
		}
		// Keep the leader unreaped and retry rather than release the operation
		// lock while helpers might still be running. SIGKILL is not rollback.
		if err := killGroup(pid); err != nil && cleanupErr == nil {
			cleanupErr = fmt.Errorf("terminate command group: %w", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	return errors.Join(lifecycleErr, cleanupErr, ctx.Err(), cmd.Wait())
}

func killGroup(pid int) error {
	err := unix.Kill(-pid, unix.SIGKILL)
	if errors.Is(err, unix.ESRCH) {
		return nil
	}
	return err
}

// A final zombie cannot execute or retain pipes, but a zombie thread-group
// leader can still have live sibling threads. Orphans may remain final zombies
// until the host's init/subreaper reaps them.
func liveGroup(group int) (bool, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false, err
	}
	live := false
	for _, entry := range entries {
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue
		}
		data, err := os.ReadFile("/proc/" + entry.Name() + "/stat")
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ESRCH) {
			continue
		}
		if err != nil {
			return false, err
		}
		// comm is parenthesized and can contain whitespace or parentheses.
		end := strings.LastIndexByte(string(data), ')')
		if end < 0 {
			return false, fmt.Errorf("invalid process stat for %s", entry.Name())
		}
		fields := strings.Fields(string(data[end+1:]))
		if len(fields) < 18 {
			return false, fmt.Errorf("short process stat for %s", entry.Name())
		}
		pgid, err := strconv.Atoi(fields[2])
		if err != nil {
			return false, err
		}
		threads, err := strconv.Atoi(fields[17]) // num_threads, field 20
		if err != nil || threads < 1 {
			return false, fmt.Errorf("invalid thread count for %s", entry.Name())
		}
		if pgid == group && (threads > 1 || (fields[0] != "Z" && fields[0] != "X")) {
			live = true
		}
	}
	return live, nil
}

func commandTerminal() (*os.File, error) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR|syscall.O_NONBLOCK, 0)
	if errors.Is(err, unix.ENXIO) || errors.Is(err, unix.ENODEV) || errors.Is(err, os.ErrNotExist) {
		return nil, nil // no controlling terminal, including pipes/CI
	}
	if err != nil {
		return nil, fmt.Errorf("open controlling terminal: %w", err)
	}
	foreground, err := unix.IoctlGetInt(int(tty.Fd()), unix.TIOCGPGRP)
	if err != nil {
		_ = tty.Close()
		return nil, fmt.Errorf("command requires ownership of the foreground terminal: %w", err)
	}
	if foreground != unix.Getpgrp() {
		_ = tty.Close()
		return nil, fmt.Errorf("command requires ownership of the foreground terminal (group %d)", foreground)
	}
	return tty, nil
}

func restoreTerminal(tty *os.File, original *unix.Termios) error {
	// tcsetpgrp from the now-background parent would trigger SIGTTOU. Block it
	// only on this locked thread, without changing process-wide dispositions.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	var block, previous unix.Sigset_t
	block.Val[0] = 1 << (uint(unix.SIGTTOU) - 1)
	if err := unix.PthreadSigmask(unix.SIG_BLOCK, &block, &previous); err != nil {
		return fmt.Errorf("block SIGTTOU for terminal restoration: %w", err)
	}
	err := unix.IoctlSetPointerInt(int(tty.Fd()), unix.TIOCSPGRP, unix.Getpgrp())
	// A killed password reader cannot restore echo/canonical modes itself.
	modeErr := unix.IoctlSetTermios(int(tty.Fd()), unix.TCSETS, original)
	maskErr := unix.PthreadSigmask(unix.SIG_SETMASK, &previous, nil)
	if err != nil || modeErr != nil || maskErr != nil {
		return fmt.Errorf("restore foreground terminal: %w", errors.Join(err, modeErr, maskErr))
	}
	return nil
}
