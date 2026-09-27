package actionlog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mirror/internal/operationlock"
)

type fakeBackend struct {
	run         func(context.Context, io.Reader, string, []string) error
	observation func(context.Context, string) ([]byte, error)
}

func (f fakeBackend) Run(ctx context.Context, input io.Reader, _, _ io.Writer, name string, args ...string) error {
	return f.run(ctx, input, name, args)
}
func (f fakeBackend) Output(ctx context.Context, name string, _ ...string) ([]byte, error) {
	return f.observation(ctx, name)
}
func (fakeBackend) Lstat(string) (os.FileInfo, error)                { return nil, os.ErrNotExist }
func (fakeBackend) DirectoryEmpty(string) (bool, error)              { return true, nil }
func (fakeBackend) MkdirAll(string, os.FileMode) error               { return nil }
func (fakeBackend) Sleep(ctx context.Context, _ time.Duration) error { return ctx.Err() }

func openTestLog(t *testing.T, operation string, output io.Writer) (*Log, string) {
	t.Helper()
	home := t.TempDir()
	lock, err := operationlock.Acquire(context.Background(), home, os.Getuid(), os.Getgid())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lock.Close() })
	log, err := Open(home, operation, os.Getuid(), os.Getgid(), output)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	return log, home
}

func readEvents(t *testing.T, path string) []event {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	decoder := json.NewDecoder(file)
	var events []event
	for {
		var value event
		if err := decoder.Decode(&value); errors.Is(err, io.EOF) {
			return events
		} else if err != nil {
			t.Fatal(err)
		}
		events = append(events, value)
	}
}

func TestActionStartIsPersistedBeforeInvocationAndPasswordsAreNotLogged(t *testing.T) {
	var output bytes.Buffer
	log, _ := openTestLog(t, "mount", &output)
	const password = "never-log-this-passphrase"
	called := false
	backend := fakeBackend{run: func(_ context.Context, input io.Reader, name string, _ []string) error {
		called = true
		events := readEvents(t, log.Path())
		last := events[len(events)-1]
		if last.Kind != "action" || last.Status != "started" || last.Name != name || last.Action != 1 {
			t.Fatalf("action began without a recorded start: %+v", last)
		}
		data, err := io.ReadAll(input)
		if err != nil || string(data) != password {
			t.Fatal("logging changed stdin")
		}
		return nil
	}}
	system := &System{Backend: backend, Log: log, Context: context.Background()}
	if err := system.Run(context.Background(), strings.NewReader(password), io.Discard, io.Discard, "cryptsetup", "open", "/dev/sde1", "sde", "--key-file", "-"); err != nil {
		t.Fatal(err)
	}
	if err := system.Finish(nil); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(log.Path())
	if err != nil {
		t.Fatal(err)
	}
	if !called || bytes.Contains(data, []byte(password)) || strings.Contains(output.String(), password) {
		t.Fatal("action was skipped or password leaked")
	}
	events := readEvents(t, log.Path())
	if len(events) != 4 || events[2].Status != "succeeded" || events[3].Kind != "operation" || events[3].Status != "succeeded" {
		t.Fatalf("incorrect action results: %+v", events)
	}
}

func TestFailureAfterSideEffectRecordsUncertaintyAndIndependentObservations(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(map[bool]string{false: "observed", true: "unknown"}[unknown], func(t *testing.T) {
			var output bytes.Buffer
			log, _ := openTestLog(t, "mount", &output)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			mounted := false
			observed := 0
			backend := fakeBackend{
				run: func(context.Context, io.Reader, string, []string) error {
					mounted = true
					cancel()
					return context.Canceled
				},
				observation: func(ctx context.Context, name string) ([]byte, error) {
					observed++
					deadline, bounded := ctx.Deadline()
					if ctx.Err() != nil || !bounded || time.Until(deadline) > 30*time.Second {
						t.Fatal("inspection did not get an independent bounded context")
					}
					if unknown {
						return nil, errors.New("inventory unavailable")
					}
					if name == "lsblk" {
						return []byte(`{"blockdevices":[{"path":"/dev/sde","serial":"known-disk"}]}`), nil
					}
					return []byte(`{"filesystems":[{"source":"/dev/mapper/sde","target":"/home/test/mnt.sde"}]}`), nil
				},
			}
			system := &System{Backend: backend, Log: log, Context: ctx}
			err := system.Run(ctx, nil, io.Discard, io.Discard, "mount", "/dev/mapper/sde", "/home/test/mnt.sde")
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("lost action failure: %v", err)
			}
			if err := system.Finish(err); err != nil {
				t.Fatal(err)
			}
			if !mounted || observed != 2 || !strings.Contains(output.String(), "No automatic rollback") {
				t.Fatalf("missing manual recovery diagnostics: mounted=%v observations=%d output=%s", mounted, observed, output.String())
			}
			events := readEvents(t, log.Path())
			if events[2].Status != "failed/uncertain" || events[len(events)-1].Status != "failed/uncertain" {
				t.Fatalf("failed action treated as no change: %+v", events)
			}
			for _, e := range events[3:5] {
				if e.Kind != "failure-diagnostic" || (unknown && (e.Status != "unknown" || len(e.Data) != 0)) || (!unknown && (e.Status != "observed" || len(e.Data) == 0)) {
					t.Fatalf("invented or omitted observed state: %+v", e)
				}
			}
		})
	}
}

func TestLogWriteFailurePreventsAction(t *testing.T) {
	log, _ := openTestLog(t, "format", io.Discard)
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	system := &System{Log: log, Context: context.Background(), Backend: fakeBackend{
		run: func(context.Context, io.Reader, string, []string) error {
			t.Fatal("ran without durable start record")
			return nil
		},
		observation: func(context.Context, string) ([]byte, error) { return nil, errors.New("unavailable") },
	}}
	if err := system.Run(context.Background(), nil, io.Discard, io.Discard, "fdisk", "/dev/sdz"); err == nil {
		t.Fatal("log failure ignored")
	}
}

func TestPrivateLogsAreRetainedAcrossInvocations(t *testing.T) {
	first, home := openTestLog(t, "mount", io.Discard)
	before, err := os.ReadFile(first.Path())
	if err != nil {
		t.Fatal(err)
	}
	second, err := Open(home, "unmount", os.Getuid(), os.Getgid(), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()
	if first.Path() == second.Path() {
		t.Fatal("overwrote prior operation log")
	}
	after, err := os.ReadFile(first.Path())
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("prior operation log changed")
	}
	for _, path := range []string{first.Path(), second.Path()} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("nonprivate action log: %v", err)
		}
	}
	info, err := os.Stat(filepath.Join(home, directory))
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("nonprivate action log directory: %v", err)
	}
}

func TestLogRejectsSymlinkDirectory(t *testing.T) {
	home := t.TempDir()
	if err := os.Mkdir(filepath.Join(home, ".mirror"), 0700); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(home, directory)); err != nil {
		t.Fatal(err)
	}
	log, err := Open(home, "format", os.Getuid(), os.Getgid(), io.Discard)
	if log != nil {
		_ = log.Close()
	}
	if err == nil {
		t.Fatal("followed action log directory symlink")
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 0 {
		t.Fatal("wrote outside state directory")
	}
}
