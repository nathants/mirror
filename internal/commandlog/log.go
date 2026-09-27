// Package commandlog keeps best-effort command diagnostics, separate from the
// durable action and repair journals that authorize storage work.
package commandlog

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
	"unicode/utf8"
)

type event struct {
	Time       time.Time `json:"time"`
	Kind       string    `json:"kind"`
	PID        int       `json:"pid,omitempty"`
	Args       []string  `json:"args,omitempty"`
	ArgsBase64 [][]byte  `json:"args_base64,omitempty"`
	Stream     string    `json:"stream,omitempty"`
	Text       string    `json:"text,omitempty"`
	Bytes      []byte    `json:"bytes_base64,omitempty"`
	ExitCode   *int      `json:"exit_code,omitempty"`
}

type logFile interface {
	io.Writer
	Sync() error
	Close() error
}

// Log serializes stdout/stderr records into a private per-invocation JSONL file.
// A disk failure warns once and disables further records, never command output.
type Log struct {
	mu       sync.Mutex
	file     logFile
	warnings io.Writer
	failed   bool
}

// Open prunes inactive logs last modified more than 14 days ago. It never prunes
// recovery journals, follows symlinks, or adopts a non-private log directory.
// Pruning failures warn but do not prevent opening the current invocation's log.
func Open(home string, uid, gid int, args []string, warnings io.Writer) (*Log, error) {
	return open(home, uid, gid, args, warnings, time.Now())
}

func open(home string, uid, gid int, args []string, warnings io.Writer, now time.Time) (*Log, error) {
	dir, err := openDirectory(home, uid, gid)
	if err != nil {
		return nil, err
	}
	defer func() { _ = dir.Close() }()
	if err := prune(dir, uid, now.Add(-14*24*time.Hour)); err != nil {
		Warn(warnings, fmt.Errorf("command log retention: %w", err))
	}
	file, err := createFile(dir, uid, gid, now)
	if err != nil {
		return nil, err
	}
	log := &Log{file: file, warnings: warnings}
	log.record(event{Kind: "start", PID: os.Getpid(), Args: args})
	return log, nil
}

// Writer duplicates Mirror's own output without suppressing terminal errors.
// It does not wrap stdin; external actions must use Unwrap to avoid logging
// password prompts, echoed input, or other external output streams.
func (l *Log) Writer(stream string, destination io.Writer) io.Writer {
	return &writer{log: l, stream: stream, destination: destination}
}

// Close records the exit code and flushes the log. Failures are warnings only.
func (l *Log) Close(exitCode int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.record(event{Kind: "finish", ExitCode: &exitCode})
	l.fail(errors.Join(l.file.Sync(), l.file.Close()))
}

// record and fail are called with mu held, or before publishing the Log.
func (l *Log) record(value event) {
	if l.failed {
		return
	}
	value.Time = time.Now().UTC()
	// JSON strings replace invalid UTF-8. Preserve opaque Linux pathname and
	// argument bytes instead; []byte fields marshal as base64 strings. Each
	// output write is independent, including a write that splits a UTF-8 rune.
	if !utf8.ValidString(value.Text) {
		value.Bytes = []byte(value.Text)
		value.Text = ""
	}
	for _, arg := range value.Args {
		if !utf8.ValidString(arg) {
			value.ArgsBase64 = make([][]byte, len(value.Args))
			for i, arg := range value.Args {
				value.ArgsBase64[i] = []byte(arg)
			}
			value.Args = nil
			break
		}
	}
	data, err := json.Marshal(value)
	if err == nil {
		data = append(data, '\n')
		var n int
		n, err = l.file.Write(data)
		if err == nil && n != len(data) {
			err = io.ErrShortWrite
		}
	}
	l.fail(err)
}

func (l *Log) fail(err error) {
	if err != nil && !l.failed {
		l.failed = true
		Warn(l.warnings, fmt.Errorf("command logging disabled: %w", err))
	}
}

type writer struct {
	log         *Log
	stream      string
	destination io.Writer
}

func (w *writer) Write(data []byte) (int, error) {
	w.log.mu.Lock()
	defer w.log.mu.Unlock()
	w.log.record(event{Kind: "output", Stream: w.stream, Text: string(data)})
	n, err := w.destination.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	return n, err
}

// Unwrap bypasses only this package's recording writer. Returning the original
// *os.File also preserves external commands' terminal and redirection behavior.
func Unwrap(output io.Writer) io.Writer {
	for {
		w, ok := output.(*writer)
		if !ok {
			return output
		}
		output = w.destination
	}
}
