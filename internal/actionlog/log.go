// Package actionlog retains disk-operation progress for explicit manual recovery.
package actionlog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
	"mirror/internal/rootfs"
)

const directory = ".mirror/operations"

type event struct {
	Time   time.Time       `json:"time"`
	Kind   string          `json:"kind"`
	Status string          `json:"status"`
	Action int             `json:"action,omitempty"`
	Name   string          `json:"name,omitempty"`
	Args   []string        `json:"args,omitempty"`
	Error  string          `json:"error,omitempty"`
	Data   json.RawMessage `json:"data,omitempty"`
	Text   string          `json:"text,omitempty"`
}

// Log is append-only. Each event is synced before proceeding; a last started
// action without a result is explicitly uncertain, not evidence of no change.
// Stdin and external command output streams (including passwords) are not copied.
type Log struct {
	file    *os.File
	path    string
	output  io.Writer
	actions int
}

// Open requires the caller-home operation lock to be held. Each operation gets
// its own retained file; subsequent invocations never overwrite earlier progress.
func Open(home, operation string, uid, gid int, output io.Writer) (*Log, error) {
	if output == nil || (operation != "format" && operation != "mount" && operation != "unmount") {
		return nil, errors.New("invalid action log options")
	}
	root, err := rootfs.Open(home)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	created := false
	if err := root.Mkdir(directory, 0700); err == nil {
		created = true
	} else if !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	dir, err := root.OpenFile(directory, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		return nil, err
	}
	if created {
		err = dir.Chown(uid, gid)
	}
	var stat unix.Stat_t
	if err == nil {
		err = unix.Fstat(int(dir.Fd()), &stat)
	}
	if err := errors.Join(err, dir.Close()); err != nil {
		return nil, err
	}
	if int(stat.Uid) != uid || stat.Mode&0777 != 0700 {
		return nil, errors.New("action log directory must be private and owned by the caller")
	}
	file, path, err := root.CreateTemp(directory, operation+"-")
	if err != nil {
		return nil, err
	}
	if err := errors.Join(file.Chown(uid, gid), file.Sync(), root.SyncDir(directory)); err != nil {
		file.Close()
		return nil, err
	}
	log := &Log{file: file, path: filepath.Join(home, path), output: output}
	if err := log.record(event{Kind: "operation", Name: operation, Status: "started", Text: "No automatic rollback. An action with no result is uncertain; inspect current state before manual intervention."}); err != nil {
		file.Close()
		return nil, err
	}
	if _, err := fmt.Fprintf(output, "action log: %s\n", log.path); err != nil {
		file.Close()
		return nil, err
	}
	return log, nil
}

func (l *Log) Path() string { return l.path }
func (l *Log) Close() error { return l.file.Close() }

func (l *Log) record(value event) error {
	value.Time = time.Now().UTC()
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if _, err := l.file.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("write action log %s: %w", l.path, err)
	}
	if err := l.file.Sync(); err != nil {
		return fmt.Errorf("sync action log %s: %w", l.path, err)
	}
	return nil
}

func (l *Log) action(ctx context.Context, name string, args []string, run func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	l.actions++
	value := event{Kind: "action", Action: l.actions, Name: name, Args: args, Status: "started"}
	if err := l.record(value); err != nil {
		return err // No action without a durable intent record.
	}
	if _, err := fmt.Fprintf(l.output, "action %d started: %s %q\n", value.Action, name, args); err != nil {
		return err
	}
	err := ctx.Err()
	if err == nil {
		err = run()
	}
	value.Status = "succeeded"
	if err != nil {
		value.Status = "failed/uncertain"
		value.Error = err.Error()
	}
	logErr := l.record(value)
	_, outputErr := fmt.Fprintf(l.output, "action %d %s: %s %q\n", value.Action, value.Status, name, args)
	return errors.Join(err, logErr, outputErr)
}

func (l *Log) observation(name string, args []string, data []byte, observedErr error, diagnostic bool) error {
	kind := "observation"
	if diagnostic {
		kind = "failure-diagnostic"
	}
	value := event{Kind: kind, Name: name, Args: args, Status: "observed"}
	if observedErr != nil {
		value.Status = "unknown"
		value.Error = observedErr.Error()
	} else if json.Valid(data) {
		value.Data = data
	} else {
		value.Text = string(data)
		if diagnostic {
			value.Status = "unknown"
			value.Error = "read-only inventory did not return valid JSON"
		}
	}
	err := l.record(value)
	if diagnostic {
		_, outputErr := fmt.Fprintf(l.output, "failure inspection %s: %s (details in %s)\n", name, value.Status, l.path)
		err = errors.Join(err, outputErr)
	}
	return err
}
