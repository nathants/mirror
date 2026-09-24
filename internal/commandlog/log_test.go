package commandlog

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"

	"golang.org/x/sys/unix"
)

func readEvents(t *testing.T, path string) []event {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var events []event
	decoder := json.NewDecoder(bytes.NewReader(data))
	for {
		var value event
		if err := decoder.Decode(&value); err == io.EOF {
			return events
		} else if err != nil {
			t.Fatal(err)
		}
		if value.Time.IsZero() {
			t.Fatal("record has no timestamp")
		}
		events = append(events, value)
	}
}

func TestLogPreservesOutputAndRecordsLifecycle(t *testing.T) {
	home := t.TempDir()
	var stdout, stderr bytes.Buffer
	args := []string{"ensure-copies", "--preview"}
	log, err := Open(home, os.Geteuid(), os.Getegid(), args, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	file := log.file.(*os.File)
	path := file.Name()
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		t.Fatal(err)
	}
	if int(stat.Uid) != os.Geteuid() || int(stat.Gid) != os.Getegid() || stat.Mode&07777 != 0600 {
		t.Fatalf("unsafe log permissions/ownership: %+v", stat)
	}
	out, errOut := log.Writer("stdout", &stdout), log.Writer("stderr", &stderr)
	for _, write := range []struct {
		out  io.Writer
		text string
	}{{out, "plan\nprompt: "}, {errOut, "warning\n"}, {out, "done\n"}} {
		if n, err := io.WriteString(write.out, write.text); n != len(write.text) || err != nil {
			t.Fatalf("write = %d, %v", n, err)
		}
	}
	// Prompts and partial lines must be visible on disk without a final flush.
	if events := readEvents(t, path); len(events) != 4 {
		t.Fatalf("output buffered until command exit: %+v", events)
	}
	log.Close(1)
	if stdout.String() != "plan\nprompt: done\n" || stderr.String() != "warning\n" {
		t.Fatalf("changed command output: %q / %q", &stdout, &stderr)
	}
	events := readEvents(t, path)
	if len(events) != 5 || events[0].Kind != "start" || events[0].PID != os.Getpid() || !reflect.DeepEqual(events[0].Args, args) {
		t.Fatalf("missing invocation metadata: %+v", events)
	}
	if events[1].Stream != "stdout" || events[1].Text != "plan\nprompt: " || events[2].Stream != "stderr" || events[2].Text != "warning\n" || events[3].Text != "done\n" {
		t.Fatalf("lost stream contents/order: %+v", events)
	}
	if events[4].Kind != "finish" || events[4].ExitCode == nil || *events[4].ExitCode != 1 {
		t.Fatalf("lost exit status: %+v", events[4])
	}
}

type failingFile struct {
	bytes.Buffer
	writeErr, syncErr, closeErr error
	short                       bool
	writes, syncs, closes       int
}

func (f *failingFile) Write(data []byte) (int, error) {
	f.writes++
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	if f.short {
		return len(data) - 1, nil
	}
	return f.Buffer.Write(data)
}
func (f *failingFile) Sync() error  { f.syncs++; return f.syncErr }
func (f *failingFile) Close() error { f.closes++; return f.closeErr }

func TestDiskFailuresWarnWithoutFailingOutput(t *testing.T) {
	failure := errors.New("injected disk failure")
	for _, test := range []struct {
		name string
		file failingFile
	}{
		{name: "write", file: failingFile{writeErr: failure}},
		{name: "short write", file: failingFile{short: true}},
		{name: "sync", file: failingFile{syncErr: failure}},
		{name: "close", file: failingFile{closeErr: failure}},
		{name: "all", file: failingFile{writeErr: failure, syncErr: failure, closeErr: failure}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output, warnings bytes.Buffer
			log := &Log{file: &test.file, warnings: &warnings}
			for i := 0; i < 2; i++ {
				if n, err := io.WriteString(log.Writer("stdout", &output), "visible\n"); n != 8 || err != nil {
					t.Fatalf("logging failure escaped to the command: %d, %v", n, err)
				}
			}
			log.Close(0)
			if output.String() != "visible\nvisible\n" || strings.Count(warnings.String(), "warning:") != 1 {
				t.Fatalf("lost output or repeated warning: %q / %q", &output, &warnings)
			}
			if (test.file.writeErr != nil || test.file.short) && test.file.writes != 1 {
				t.Fatal("continued appending after an incomplete record")
			}
			if test.file.syncs != 1 || test.file.closes != 1 {
				t.Fatal("failed logger was not flushed/closed")
			}
		})
	}
}

type failedOutput struct {
	err error
}

func (w failedOutput) Write([]byte) (int, error) { return 0, w.err }

func TestTerminalFailuresStillReachTheCommand(t *testing.T) {
	failure := errors.New("injected terminal failure")
	for _, err := range []error{failure, nil} {
		file := &failingFile{}
		log := &Log{file: file, warnings: io.Discard}
		n, got := io.WriteString(log.Writer("stdout", failedOutput{err}), "plan and prompt")
		want := err
		if want == nil {
			want = io.ErrShortWrite
		}
		if n != 0 || !errors.Is(got, want) {
			t.Fatalf("terminal failure hidden: %d, %v", n, got)
		}
		if !strings.Contains(file.String(), "plan and prompt") {
			t.Fatal("lost diagnostic when terminal failed")
		}
		log.Close(1)
	}
}

func TestConcurrentStreamsRemainComplete(t *testing.T) {
	home := t.TempDir()
	var output bytes.Buffer
	log, err := Open(home, os.Geteuid(), os.Getegid(), []string{"test"}, &output)
	if err != nil {
		t.Fatal(err)
	}
	path := log.file.(*os.File).Name()
	var wg sync.WaitGroup
	for _, stream := range []string{"stdout", "stderr"} {
		wg.Add(1)
		go func(stream string) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				if _, err := io.WriteString(log.Writer(stream, &output), stream+"\n"); err != nil {
					t.Error(err)
				}
			}
		}(stream)
	}
	wg.Wait()
	log.Close(0)
	counts := map[string]int{}
	for _, value := range readEvents(t, path) {
		if value.Kind == "output" {
			if value.Text != value.Stream+"\n" {
				t.Fatalf("interleaved output record: %+v", value)
			}
			counts[value.Stream]++
		}
	}
	if counts["stdout"] != 100 || counts["stderr"] != 100 || output.Len() != 1400 {
		t.Fatalf("lost concurrent output: %v, %d bytes", counts, output.Len())
	}
}

func TestUnwrapPreservesOriginalFileDescriptor(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "terminal-output")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	log := &Log{file: &failingFile{}, warnings: io.Discard}
	wrapped := log.Writer("stdout", log.Writer("stdout", file))
	if Unwrap(wrapped) != file || Unwrap(file) != file {
		t.Fatal("external commands would receive a pipe instead of the original file descriptor")
	}
}
