package mountdisks

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"mirror/internal/actionlog"
	"mirror/internal/operationlock"
)

type uncertainCommandSystem struct {
	*fakeSystem
	failName, failAction string
	cancel               context.CancelFunc
}

func (f uncertainCommandSystem) Sleep(ctx context.Context, _ time.Duration) error { return ctx.Err() }
func (f uncertainCommandSystem) Run(ctx context.Context, input io.Reader, out, stderr io.Writer, name string, args ...string) error {
	if err := f.fakeSystem.Run(ctx, input, out, stderr, name, args...); err != nil {
		return err
	}
	if name == f.failName && (f.failAction == "" || (len(args) > 0 && args[0] == f.failAction)) {
		f.cancel() // The syscall took effect before the command failed.
		return context.Canceled
	}
	return nil
}

func TestActionLogExplainsPartialMountAndUnmountWithoutRollback(t *testing.T) {
	for _, test := range []struct {
		operation, name, action string
		open, mounted           bool
	}{
		{"mount", "cryptsetup", "open", true, false},
		{"mount", "mount", "", true, true},
		{"unmount", "umount", "", true, false},
		{"unmount", "cryptsetup", "close", false, false},
	} {
		t.Run(test.operation+"/"+test.name+"/"+test.action, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			fake := newFakeSystem()
			if test.operation == "unmount" {
				fake.mapperOpen = true
				fake.mounted = true
			}
			backend := uncertainCommandSystem{fakeSystem: fake, failName: test.name, failAction: test.action, cancel: cancel}
			home := t.TempDir()
			lock, err := operationlock.Acquire(ctx, home, os.Getuid(), os.Getgid())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = lock.Close() }()
			var output bytes.Buffer
			log, err := actionlog.Open(home, test.operation, os.Getuid(), os.Getgid(), &output)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = log.Close() }()
			observed := &actionlog.System{Backend: backend, Log: log, Context: ctx}
			option := options(t, func() ([]byte, error) { return []byte("private passphrase"), nil }, fake.identifier)
			if test.operation == "mount" {
				err = Mount(ctx, observed, strings.NewReader("mount\n"), io.Discard, io.Discard, option)
			} else {
				err = Unmount(ctx, observed, strings.NewReader("unmount\n"), io.Discard, io.Discard, option)
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("lost uncertain failure: %v", err)
			}
			if err := observed.Finish(err); err != nil {
				t.Fatal(err)
			}
			if fake.mapperOpen != test.open || fake.mounted != test.mounted {
				t.Fatalf("failure triggered unrequested mutation: mapperOpen=%v mounted=%v", fake.mapperOpen, fake.mounted)
			}
			if test.operation == "mount" && (countCalls(fake.calls, "umount", "") != 0 || countCalls(fake.calls, "cryptsetup", "close") != 0) {
				t.Fatal("mount performed automatic rollback")
			}
			if test.name == "umount" && countCalls(fake.calls, "cryptsetup", "close") != 0 {
				t.Fatal("unmount continued mutations after failure")
			}
			data, err := os.ReadFile(log.Path())
			if err != nil {
				t.Fatal(err)
			}
			for _, evidence := range []string{"failed/uncertain", "failure-diagnostic", fake.identifier, "ZFNTEST", "No automatic rollback"} {
				if !strings.Contains(string(data), evidence) {
					t.Fatalf("missing %q in durable log: %s", evidence, data)
				}
			}
			if strings.Contains(string(data), "private passphrase") {
				t.Fatal("password leaked into action log")
			}
			if !strings.Contains(output.String(), "No automatic rollback") {
				t.Fatal("manual recovery warning not displayed")
			}
		})
	}
}
