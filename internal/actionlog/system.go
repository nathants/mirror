package actionlog

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"mirror/internal/inventory"
)

// Backend is the shared OS boundary used by format and mount. Read-only path
// checks and sleeps remain delegated; external observations are retained, while
// every command and directory creation gets a durable start/result pair.
type Backend interface {
	Output(context.Context, string, ...string) ([]byte, error)
	Run(context.Context, io.Reader, io.Writer, io.Writer, string, ...string) error
	Lstat(string) (os.FileInfo, error)
	DirectoryEmpty(string) (bool, error)
	MkdirAll(string, os.FileMode) error
	Sleep(context.Context, time.Duration) error
}

type System struct {
	Backend
	Log       *Log
	Context   context.Context
	diagnosed bool
}

func (s *System) Run(ctx context.Context, input io.Reader, output, stderr io.Writer, name string, args ...string) error {
	s.diagnosed = false
	err := s.Log.action(ctx, name, args, func() error { return s.Backend.Run(ctx, input, output, stderr, name, args...) })
	if err != nil && s.Log.actions > 0 {
		return errors.Join(err, s.inspect())
	}
	return err
}

func (s *System) Output(ctx context.Context, name string, args ...string) ([]byte, error) {
	s.diagnosed = false
	data, err := s.Backend.Output(ctx, name, args...)
	return data, errors.Join(err, s.Log.observation(name, args, data, err, false))
}

func (s *System) MkdirAll(path string, mode os.FileMode) error {
	s.diagnosed = false
	return s.Log.action(s.Context, "mkdir", []string{path, fmt.Sprintf("%04o", mode.Perm())}, func() error {
		return s.Backend.MkdirAll(path, mode)
	})
}

// Finish records the workflow outcome, including verification/prompt failures
// after successful external actions. It returns logging/diagnostic errors only;
// the caller must separately propagate the workflow result.
func (s *System) Finish(result error) error {
	status := "succeeded"
	if result != nil {
		status = "stopped-before-actions"
		if s.Log.actions > 0 {
			status = "failed/uncertain"
		}
	}
	value := event{Kind: "operation", Status: status}
	if result != nil {
		value.Error = result.Error()
	}
	var diagnosticErr error
	if result != nil && s.Log.actions > 0 && !s.diagnosed {
		diagnosticErr = s.inspect()
	}
	return errors.Join(diagnosticErr, s.Log.record(value))
}

func (s *System) inspect() error {
	s.diagnosed = true
	message := "No automatic rollback was attempted. A failed action may have taken effect; mounted filesystems and decrypted mappers may remain active. Review the action log and verify disk identity before manual intervention. Do not rerun format just to retry opening or mounting."
	_, outputErr := fmt.Fprintln(s.Log.output, message)
	// Use independent bounded read-only commands even when the operation was
	// cancelled. Validate representation with the shared parsers, not storage
	// eligibility or an inferred success/recovery state, and retain raw evidence.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var failures []error
	failures = append(failures, outputErr)
	for _, command := range [][]string{
		{"lsblk", "--json", "--bytes", "--paths", "--output", inventory.BlockColumns},
		{"findmnt", "--json", "--list", "--output", "SOURCE,TARGET"},
	} {
		data, err := s.Backend.Output(ctx, command[0], command[1:]...)
		if err == nil {
			switch command[0] {
			case "lsblk":
				_, err = inventory.ParseBlocks(data)
			case "findmnt":
				_, err = inventory.ParseMounts(data)
			}
		}
		failures = append(failures, s.Log.observation(command[0], command[1:], data, err, true))
	}
	return errors.Join(failures...)
}
