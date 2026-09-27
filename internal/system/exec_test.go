package system

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// A Linux process can have a zombie thread-group leader while other threads
// still execute. Exit only the init/main OS thread in this isolated fixture.
func init() {
	if os.Getenv("MIRROR_EXEC_HELPER") != "zombie-thread" {
		return
	}
	runtime.LockOSThread()
	go func() {
		for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
			data, _ := os.ReadFile("/proc/self/stat")
			if strings.HasPrefix(string(data)[strings.LastIndexByte(string(data), ')')+1:], " Z ") {
				if os.WriteFile(os.Getenv("MIRROR_EXEC_READY"), []byte(strconv.Itoa(os.Getpid())), 0600) != nil {
					os.Exit(95)
				}
				time.Sleep(3 * time.Second)
				os.Exit(0)
			}
			time.Sleep(time.Millisecond)
		}
		os.Exit(96)
	}()
	_, _, _ = syscall.RawSyscall(unix.SYS_EXIT, 0, 0, 0)
	os.Exit(97)
}

// Helpers are this test binary, never storage tools. The timeout also bounds
// their lifetime on an unfixed runner; tests explicitly kill recorded survivors.
func TestExecHelper(_ *testing.T) {
	mode := os.Getenv("MIRROR_EXEC_HELPER")
	if mode == "" {
		return
	}
	switch mode {
	case "hold":
		if err := os.WriteFile(os.Getenv("MIRROR_EXEC_READY"), []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
			os.Exit(90)
		}
		time.Sleep(3 * time.Second)
	case "spawn", "orphan", "escaped", "orphan-zombie":
		child := exec.Command(os.Args[0], "-test.run=^TestExecHelper$")
		childMode := "hold"
		if mode == "orphan-zombie" {
			childMode = "zombie-thread"
		}
		child.Env = append(os.Environ(), "MIRROR_EXEC_HELPER="+childMode)
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if mode == "escaped" {
			child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		}
		if child.Start() != nil {
			os.Exit(91)
		}
		if mode != "spawn" {
			for range 500 {
				if _, err := os.Stat(os.Getenv("MIRROR_EXEC_READY")); err == nil {
					os.Exit(0)
				}
				time.Sleep(time.Millisecond)
			}
			os.Exit(92)
		}
		_ = child.Wait()
	case "echo":
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			os.Exit(93)
		}
		_, _ = fmt.Fprintf(os.Stdout, "%q|%s|%s", os.Args[len(os.Args)-1], os.Getenv("LC_ALL"), data)
		fmt.Fprint(os.Stderr, "diagnostic")
		os.Exit(23)
	default:
		os.Exit(94)
	}
	os.Exit(0)
}

func TestExecOwnsHelperLifetime(t *testing.T) {
	t.Setenv("GORACE", "atexit_sleep_ms=0")
	for _, capture := range []bool{true, false} {
		modes := []string{"spawn", "orphan", "orphan-zombie"}
		if capture {
			modes = append(modes, "escaped") // pipe deadline, not group containment
		}
		for _, mode := range modes {
			t.Run(fmt.Sprintf("capture=%t/%s", capture, mode), func(t *testing.T) {
				ready := filepath.Join(t.TempDir(), "ready")
				t.Setenv("MIRROR_EXEC_HELPER", mode)
				t.Setenv("MIRROR_EXEC_READY", ready)
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				file, err := os.CreateTemp(t.TempDir(), "output")
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = file.Close() }()
				done := make(chan error, 1)
				go func() {
					if capture {
						_, err := (Exec{}).Output(ctx, os.Args[0], "-test.run=^TestExecHelper$")
						done <- err
					} else {
						done <- (Exec{}).Run(ctx, nil, file, file, os.Args[0], "-test.run=^TestExecHelper$")
					}
				}()
				var pid int
				for deadline := time.Now().Add(4 * time.Second); time.Now().Before(deadline); {
					data, _ := os.ReadFile(ready)
					pid, _ = strconv.Atoi(string(data))
					if pid != 0 {
						break
					}
					time.Sleep(5 * time.Millisecond)
				}
				if pid == 0 {
					cancel()
					t.Fatalf("helper not ready: %v", <-done)
				}
				pidfd, err := unix.PidfdOpen(pid, 0)
				if err == nil {
					defer func() { _ = unix.Close(pidfd) }()
					defer func() { _ = unix.PidfdSendSignal(pidfd, unix.SIGKILL, nil, 0) }()
				} else if !errors.Is(err, unix.ESRCH) {
					cancel()
					<-done
					t.Fatal(err)
				}
				started := time.Now()
				if mode == "spawn" {
					cancel()
				}
				err = <-done
				if err == nil {
					t.Error("cancelled command or abandoned helper reported success")
				}
				if elapsed := time.Since(started); elapsed > time.Second {
					t.Errorf("waited %s for an inherited pipe instead of terminating the helper", elapsed)
				}
				if mode == "escaped" {
					if !errors.Is(err, exec.ErrWaitDelay) {
						t.Fatalf("expected bounded drain for an escaped pipe holder: %v", err)
					}
					return // explicitly killed through the fixture's pidfd defer
				}
				if mode == "spawn" && !errors.Is(err, context.Canceled) {
					t.Errorf("lost cancellation cause: %v", err)
				}
				if pidfd >= 0 {
					// pidfd becomes readable only when the entire process exits,
					// not merely when its main thread becomes a zombie.
					poll := []unix.PollFd{{Fd: int32(pidfd), Events: unix.POLLIN}}
					if n, err := unix.Poll(poll, 0); err != nil || n != 1 {
						t.Errorf("helper %d is still live after Exec returned: poll %d %v", pid, n, err)
					}
				}
			})
		}
	}
}

func TestExecPreservesStreamsArgumentsAndExit(t *testing.T) {
	t.Setenv("MIRROR_EXEC_HELPER", "echo")
	t.Setenv("LC_ALL", "not-C")
	const argument = "literal ; $HOME ' spaces"
	var stdout, stderr bytes.Buffer
	err := (Exec{}).Run(context.Background(), strings.NewReader("input"), &stdout, &stderr, os.Args[0], "-test.run=^TestExecHelper$", "--", argument)
	var exit interface{ ExitCode() int }
	if !errors.As(err, &exit) || exit.ExitCode() != 23 {
		t.Fatalf("lost exit code: %v", err)
	}
	if stdout.String() != fmt.Sprintf("%q|C|input", argument) || stderr.String() != "diagnostic" {
		t.Fatalf("changed streams: %q / %q", stdout.String(), stderr.String())
	}
	_, err = (Exec{}).Output(context.Background(), os.Args[0], "-test.run=^TestExecHelper$", "--", argument)
	if !errors.As(err, &exit) || exit.ExitCode() != 23 || !strings.Contains(err.Error(), "diagnostic") {
		t.Fatalf("lost captured failure details: %v", err)
	}
}
