package commandlog

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
)

// Run warning and ordinary output failures in a subprocess so SIGPIPE cannot
// kill the test runner. The parent attaches stderr to a pipe with no readers.
func TestBrokenStderrHelper(t *testing.T) {
	mode := os.Getenv("MIRROR_LOG_BROKEN_STDERR")
	if mode == "" {
		return
	}
	file := &failingFile{}
	log := &Log{file: file, warnings: os.Stderr}
	switch mode {
	case "write warning":
		file.writeErr = errors.New("log disk full")
		if _, err := io.WriteString(log.Writer("stdout", os.Stdout), "command continued\n"); err != nil {
			t.Fatal(err)
		}
		log.Close(0)
	case "close warning":
		file.closeErr = errors.New("log close failed")
		log.Close(0)
		fmt.Fprintln(os.Stdout, "command continued")
	case "ordinary output":
		// Warning suppression must not change SIGPIPE handling for real output.
		_, _ = io.WriteString(log.Writer("stderr", os.Stderr), "real command diagnostic\n")
		t.Fatal("normal stderr unexpectedly survived SIGPIPE")
	default:
		t.Fatal("unknown helper mode")
	}
}

func TestWarningsCannotTerminateCommandOnBrokenStderr(t *testing.T) {
	for _, mode := range []string{"write warning", "close warning", "ordinary output"} {
		t.Run(mode, func(t *testing.T) {
			read, write, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			read.Close()
			defer write.Close()
			cmd := exec.Command(os.Args[0], "-test.run=^TestBrokenStderrHelper$")
			cmd.Env = append(os.Environ(), "MIRROR_LOG_BROKEN_STDERR="+mode, "GORACE=atexit_sleep_ms=0")
			var stdout bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, write
			err = cmd.Run()
			if mode == "ordinary output" {
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.Sys().(syscall.WaitStatus).Signal() != syscall.SIGPIPE {
					t.Fatalf("normal output behavior changed: %v, %q", err, &stdout)
				}
			} else if err != nil || !strings.Contains(stdout.String(), "command continued") {
				t.Fatalf("debug warning terminated the command: %v, %q", err, &stdout)
			}
		})
	}
}

func TestWarningPreservesOriginalOutputDescriptor(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "warnings")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	logFile := &failingFile{}
	log := &Log{file: logFile, warnings: io.Discard}
	Warn(log.Writer("stderr", file), errors.New("logging unavailable"))
	if _, err := io.WriteString(file, "normal output\n"); err != nil {
		t.Fatalf("warning closed the original output: %v", err)
	}
	if logFile.Len() != 0 {
		t.Fatal("warning recursively entered the logger")
	}
	data, err := os.ReadFile(file.Name())
	if err != nil || string(data) != "warning: logging unavailable\nnormal output\n" {
		t.Fatalf("changed warning/output: %q, %v", data, err)
	}
}
