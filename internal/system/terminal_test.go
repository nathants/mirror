package system

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestExecTerminalHelper(t *testing.T) {
	mode := os.Getenv("MIRROR_TERMINAL_HELPER")
	if mode == "" {
		return
	}
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer tty.Close()
	if mode == "read" || mode == "hold" {
		foreground, err := unix.IoctlGetInt(int(tty.Fd()), unix.TIOCGPGRP)
		if err != nil || foreground != unix.Getpgrp() || unix.Getpgrp() != os.Getpid() {
			t.Fatalf("child does not own terminal: %d %v", foreground, err)
		}
		termios, err := unix.IoctlGetTermios(int(tty.Fd()), unix.TCGETS)
		if err != nil {
			t.Fatal(err)
		}
		original := *termios
		termios.Lflag &^= unix.ECHO
		if err := unix.IoctlSetTermios(int(tty.Fd()), unix.TCSETS, termios); err != nil {
			t.Fatal(err)
		}
		defer unix.IoctlSetTermios(int(tty.Fd()), unix.TCSETS, &original)
		if mode == "hold" {
			fmt.Fprint(os.Stdout, "hold-ready")
			time.Sleep(3 * time.Second)
			return
		}
		fmt.Fprintln(tty, "test password:")
		data := make([]byte, 64)
		n, err := tty.Read(data)
		if err != nil || string(data[:n]) != "private-test-password\n" {
			t.Fatal("failed controlling-terminal password read")
		}
		return
	}
	if mode != "session" {
		t.Fatal("unknown terminal helper mode")
	}
	original, err := unix.IoctlGetTermios(int(tty.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	checkForeground := func() {
		foreground, err := unix.IoctlGetInt(int(tty.Fd()), unix.TIOCGPGRP)
		if err != nil || foreground != unix.Getpgrp() {
			t.Fatalf("parent terminal not restored: %d %v", foreground, err)
		}
		current, err := unix.IoctlGetTermios(int(tty.Fd()), unix.TCGETS)
		if err != nil || *current != *original {
			t.Fatalf("terminal modes not restored: %v", err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// Captured streams still need the controlling terminal for /dev/tty reads.
	t.Setenv("MIRROR_TERMINAL_HELPER", "read")
	if output, err := (Exec{}).Output(ctx, os.Args[0], "-test.run=^TestExecTerminalHelper$"); err != nil {
		t.Fatalf("terminal read: %v %s", err, output)
	}
	checkForeground()
	t.Setenv("MIRROR_TERMINAL_HELPER", "hold")
	cancelCtx, stop := context.WithCancel(ctx)
	defer stop()
	ready := &terminalCancelWriter{cancel: stop}
	if err := (Exec{}).Run(cancelCtx, os.Stdin, ready, ready, os.Args[0], "-test.run=^TestExecTerminalHelper$"); !errors.Is(err, context.Canceled) || !strings.Contains(ready.output.String(), "hold-ready") {
		t.Fatalf("terminal command did not cancel after disabling echo: %v; %s", err, ready.output.String())
	}
	checkForeground()
	// Exec failure after foreground handoff must restore it too.
	bad := t.TempDir() + "/not-executable-format"
	if err := os.WriteFile(bad, []byte("invalid executable\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := (Exec{}).Output(ctx, bad); err == nil {
		t.Fatal("invalid executable succeeded")
	}
	checkForeground()
	fmt.Fprintln(tty, "terminal-session-ok")
}

type terminalCancelWriter struct {
	output bytes.Buffer
	cancel context.CancelFunc
}

func (w *terminalCancelWriter) Write(data []byte) (int, error) {
	n, err := w.output.Write(data)
	if strings.Contains(w.output.String(), "hold-ready") {
		w.cancel()
	}
	return n, err
}

func TestExecPreservesControllingTerminal(t *testing.T) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	if err := unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		t.Fatal(err)
	}
	number, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", number), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer slave.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Only this test fixture creates a session; the actual nested commands use
	// Exec. Closing the PTY on failure hangs up that isolated session.
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestExecTerminalHelper$")
	cmd.Env = append(os.Environ(), "MIRROR_TERMINAL_HELPER=session", "GORACE=atexit_sleep_ms=0")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	cmd.Stdin = slave
	var diagnostic bytes.Buffer
	cmd.Stdout, cmd.Stderr = &diagnostic, &diagnostic
	cmd.WaitDelay = time.Second
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = slave.Close()
	if err := master.SetDeadline(time.Now().Add(4 * time.Second)); err != nil {
		cancel()
		_ = master.Close()
		_ = cmd.Wait()
		t.Fatal(err)
	}
	var output bytes.Buffer
	answered := false
	buffer := make([]byte, 1024)
	for !strings.Contains(output.String(), "terminal-session-ok") {
		n, readErr := master.Read(buffer)
		output.Write(buffer[:n])
		if !answered && strings.Contains(output.String(), "test password:") {
			if _, err := io.WriteString(master, "private-test-password\n"); err != nil {
				readErr = err
			}
			answered = true
		}
		if readErr != nil {
			cancel()
			_ = master.Close()
			err := cmd.Wait()
			t.Fatalf("terminal session: read %v wait %v; output %q; diagnostics %s", readErr, err, output.String(), diagnostic.String())
		}
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("terminal session: %v; %s", err, diagnostic.String())
	}
	if !answered || strings.Contains(output.String(), "private-test-password") {
		t.Fatalf("password missing or echoed: %q", output.String())
	}
}

func TestExecCancelledBeforeStart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := (Exec{}).Output(ctx, "/does-not-exist")
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("expected visible cancellation before executable lookup: %v", err)
	}
}
