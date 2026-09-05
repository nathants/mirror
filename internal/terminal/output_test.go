package terminal

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

var errPromptOutput = errors.New("prompt output unavailable")

type promptOutput struct {
	tty       *os.File
	newline   bool
	interrupt bool
}

func (w promptOutput) Write(p []byte) (int, error) {
	if w.newline && string(p) != "\n" {
		// Tell the PTY driver that ReadPassword has entered raw/no-echo mode.
		marker := "password-ready"
		if w.interrupt {
			marker = "interrupt-ready"
		}
		_, err := fmt.Fprintln(w.tty, marker)
		return len(p), err
	}
	return 0, errPromptOutput
}

func TestTerminalOutputHelper(t *testing.T) {
	if os.Getenv("MIRROR_TERMINAL_OUTPUT_HELPER") != "1" {
		return
	}
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR|unix.O_NONBLOCK, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer tty.Close()
	original, err := unix.IoctlGetTermios(int(tty.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatal(err)
	}
	checkModes := func() {
		current, err := unix.IoctlGetTermios(int(tty.Fd()), unix.TCGETS)
		if err != nil || *current != *original {
			t.Fatal("output error left terminal modes changed")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	err = Confirm(ctx, promptOutput{tty: tty}, "repair plan")
	cancel()
	if !errors.Is(err, errPromptOutput) {
		t.Errorf("Confirm ignored failed prompt and waited for input: %v", err)
	}
	checkModes()
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	password, err := ReadPassword(ctx, promptOutput{tty: tty}, "password:")
	cancel()
	if !errors.Is(err, errPromptOutput) || password != nil {
		t.Errorf("password prompt failure ignored: %v", err)
	}
	checkModes()
	for _, interrupt := range []bool{false, true} {
		ctx, cancel = context.WithTimeout(context.Background(), time.Second)
		password, err = ReadPassword(ctx, promptOutput{tty: tty, newline: true, interrupt: interrupt}, "password:")
		cancel()
		if !errors.Is(err, errPromptOutput) || password != nil {
			t.Errorf("newline failure returned password/success: %v", err)
		}
		if interrupt && !errors.Is(err, ErrInterrupted) {
			t.Errorf("newline failure lost input interruption: %v", err)
		}
		clear(password)
		checkModes()
	}
	fmt.Fprintln(tty, "output-tests-finished")
}

func TestTerminalOutputFailuresRestoreModesAndDiscardPassword(t *testing.T) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY|unix.O_NONBLOCK, 0)
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
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", number), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer slave.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestTerminalOutputHelper$")
	cmd.Env = append(os.Environ(), "MIRROR_TERMINAL_OUTPUT_HELPER=1", "GORACE=atexit_sleep_ms=0")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	cmd.Stdin = slave
	var diagnostics bytes.Buffer
	cmd.Stdout, cmd.Stderr = &diagnostics, &diagnostics
	cmd.WaitDelay = time.Second
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = slave.Close()
	if err := master.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		cancel()
		_ = master.Close()
		_ = cmd.Wait()
		t.Fatal(err)
	}
	var output strings.Builder
	buffer := make([]byte, 1024)
	passwordSent, interruptSent := false, false
	for !strings.Contains(output.String(), "output-tests-finished") {
		n, readErr := master.Read(buffer)
		output.Write(buffer[:n])
		if !passwordSent && strings.Contains(output.String(), "password-ready") {
			_, err = io.WriteString(master, "private-test-password\n")
			passwordSent = true
		}
		if !interruptSent && strings.Contains(output.String(), "interrupt-ready") {
			_, err = io.WriteString(master, "\x03")
			interruptSent = true
		}
		if readErr != nil || err != nil {
			cancel()
			_ = master.Close()
			waitErr := cmd.Wait()
			t.Fatalf("PTY failed: %v %v %v; %s", readErr, err, waitErr, diagnostics.String())
		}
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("terminal output checks: %v; %s", err, diagnostics.String())
	}
	if !passwordSent || !interruptSent || strings.Contains(output.String(), "private-test-password") {
		t.Fatalf("missing interaction or echoed password: %q", output.String())
	}
}
