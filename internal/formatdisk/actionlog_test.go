package formatdisk

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"mirror/internal/actionlog"
	"mirror/internal/operationlock"
)

type uncertainFormatSystem struct{ *fakeSystem }

func (f uncertainFormatSystem) Run(ctx context.Context, input io.Reader, out, stderr io.Writer, name string, args ...string) error {
	if err := f.fakeSystem.Run(ctx, input, out, stderr, name, args...); err != nil {
		return err
	}
	if name == "mount" {
		return errors.New("mount returned an error after taking effect")
	}
	return nil
}

func TestFormatLogsPartialActionsWithoutUnmounting(t *testing.T) {
	home := t.TempDir()
	lock, err := operationlock.Acquire(context.Background(), home, os.Getuid(), os.Getgid())
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	log, err := actionlog.Open(home, "format", os.Getuid(), os.Getgid(), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	fake := newFakeSystem()
	observed := &actionlog.System{Backend: uncertainFormatSystem{fake}, Log: log, Context: context.Background()}
	err = Run(context.Background(), observed, strings.NewReader("/dev/sdz\n/dev/sdz\n"), io.Discard, io.Discard, Options{HomeDir: "/home/test", OwnerUID: 1234, OwnerGID: 5678, PartitionPollAttempts: 2})
	if err == nil {
		t.Fatal("expected partial mount failure")
	}
	if err := observed.Finish(err); err != nil {
		t.Fatal(err)
	}
	if !fake.mounted || !fake.mapperOpen {
		t.Fatal("format automatically undid state after failure")
	}
	if countCalls(fake.calls, "umount", "") != 0 || countCalls(fake.calls, "cryptsetup", "close") != 0 {
		t.Fatal("format attempted rollback")
	}
	if countCalls(fake.calls, "cryptsetup", "luksFormat") != 1 {
		t.Fatal("format repeated destructive initialization")
	}
	data, err := os.ReadFile(log.Path())
	if err != nil {
		t.Fatal(err)
	}
	for _, evidence := range []string{"fdisk", "luksFormat", "mkfs.ext4", "mkdir", "mount", "failed/uncertain", "failure-diagnostic", "ZFNTEST"} {
		if !strings.Contains(string(data), evidence) {
			t.Fatalf("missing %q in format action log: %s", evidence, data)
		}
	}
}
