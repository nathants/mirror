package system

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"
)

// Exec invokes operating-system commands directly with exact argument vectors.
type Exec struct{}

func (Exec) Output(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, commandError(name, args, stderr.String(), err)
	}
	return stdout.Bytes(), nil
}

func (Exec) Run(
	ctx context.Context,
	stdin io.Reader,
	stdout io.Writer,
	stderr io.Writer,
	name string,
	args ...string,
) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return commandError(name, args, "", err)
	}
	return nil
}

func (Exec) Lstat(path string) (os.FileInfo, error) {
	return os.Lstat(path)
}

func (Exec) DirectoryEmpty(path string) (empty bool, returnErr error) {
	directory, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer func() {
		if err := directory.Close(); err != nil && returnErr == nil {
			returnErr = err
		}
	}()
	_, err = directory.Readdirnames(1)
	if errors.Is(err, io.EOF) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return false, nil
}

func (Exec) MkdirAll(path string, mode os.FileMode) error {
	return os.MkdirAll(path, mode)
}

func (Exec) Sleep(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type commandFailure struct {
	command string
	code    int
	cause   error
}

func (e *commandFailure) Error() string {
	return e.command
}

func (e *commandFailure) Unwrap() error {
	return e.cause
}

func (e *commandFailure) ExitCode() int {
	return e.code
}

func commandError(name string, args []string, stderr string, err error) error {
	message := fmt.Sprintf("%s %q failed", name, args)
	if stderr != "" {
		message += ": " + stderr
	}
	code := -1
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		code = exitErr.ExitCode()
	}
	return &commandFailure{
		command: message,
		code:    code,
		cause:   err,
	}
}
