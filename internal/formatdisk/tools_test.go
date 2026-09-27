package formatdisk

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"slices"
	"strings"
	"testing"
)

type toolSystem struct {
	*fakeSystem
	failTool  string
	failure   error
	probes    []recordedCall
	recheck   bool
	unbounded bool
}

func (s *toolSystem) Output(ctx context.Context, name string, args ...string) ([]byte, error) {
	if name == s.failTool && (!s.recheck || len(s.probes) >= 8) {
		return nil, s.failure
	}
	if slices.Contains(args, "--version") || (name == "mkfs.ext4" && slices.Contains(args, "-V")) {
		if _, ok := ctx.Deadline(); !ok {
			s.unbounded = true
		}
		s.probes = append(s.probes, recordedCall{name: name, args: append([]string(nil), args...)})
	}
	return s.fakeSystem.Output(ctx, name, args...)
}

func (s *toolSystem) Run(ctx context.Context, in io.Reader, out, errOut io.Writer, name string, args ...string) error {
	if name == s.failTool {
		return s.failure
	}
	return s.fakeSystem.Run(ctx, in, out, errOut, name, args...)
}

func TestMissingFormatToolsFailBeforeConfirmationOrErase(t *testing.T) {
	for _, name := range []string{"lsblk", "findmnt", "fdisk", "cryptsetup", "mkfs.ext4", "blkid", "mount", "chown"} {
		t.Run(name, func(t *testing.T) {
			system := &toolSystem{fakeSystem: newFakeSystem(), failTool: name, failure: errors.New("injected missing executable")}
			var output bytes.Buffer
			err := Run(context.Background(), system, strings.NewReader("/dev/sdz\n/dev/sdz\n"), &output, io.Discard, Options{HomeDir: "/home/test"})
			if err == nil || !strings.Contains(err.Error(), name) {
				t.Fatalf("missing %s not explained: %v", name, err)
			}
			if strings.Contains(output.String(), "Type the exact device path") || system.partitioned || len(destructiveCalls(system.calls)) != 0 || system.mountPathMode != 0 {
				t.Fatalf("missing tool reached confirmation or mutation: %s, %v", output.String(), destructiveCalls(system.calls))
			}
		})
	}
}

func TestFormatRechecksToolsBeforeDiskRevalidation(t *testing.T) {
	system := &toolSystem{fakeSystem: newFakeSystem(), failTool: "cryptsetup", failure: errors.New("injected vanished executable"), recheck: true}
	err := Run(context.Background(), system, strings.NewReader("/dev/sdz\n/dev/sdz\n"), io.Discard, io.Discard, Options{HomeDir: "/home/test"})
	if err == nil || !strings.Contains(err.Error(), "vanished executable") {
		t.Fatalf("tool disappearance missed: %v", err)
	}
	if system.partitioned || len(destructiveCalls(system.calls)) != 0 {
		t.Fatal("formatter erased disk after a tool disappeared during confirmation")
	}
}

func TestUnsupportedFormatToolFlagsFailBeforeErase(t *testing.T) {
	system := &toolSystem{fakeSystem: newFakeSystem(), failTool: "cryptsetup", failure: errors.New("unrecognized option --perf-no_read_workqueue")}
	err := Run(context.Background(), system, strings.NewReader("/dev/sdz\n/dev/sdz\n"), io.Discard, io.Discard, Options{HomeDir: "/home/test"})
	if err == nil || !strings.Contains(err.Error(), "unrecognized option") || system.partitioned {
		t.Fatalf("unsupported tool reached erase: partitioned=%v err=%v", system.partitioned, err)
	}
}

func TestFormatToolProbesAreCompleteDeviceFreeAndBounded(t *testing.T) {
	system := &toolSystem{fakeSystem: newFakeSystem()}
	if err := Run(context.Background(), system, strings.NewReader("/dev/sdz\n/dev/sdz\n"), io.Discard, io.Discard, Options{HomeDir: "/home/test"}); err != nil {
		t.Fatal(err)
	}
	want := []recordedCall{
		{name: "lsblk", args: []string{"--version"}},
		{name: "findmnt", args: []string{"--version"}},
		{name: "fdisk", args: []string{"-w", "always", "-W", "always", "--version"}},
		{name: "cryptsetup", args: []string{"--type", "luks2", "--key-size=512", "--perf-no_read_workqueue", "--perf-no_write_workqueue", "--version"}},
		{name: "mkfs.ext4", args: []string{"-m", "0", "-V"}},
		{name: "blkid", args: []string{"-p", "-s", "TYPE", "-o", "value", "--version"}},
		{name: "mount", args: []string{"--version"}},
		{name: "chown", args: []string{"--version"}},
	}
	want = append(want, want...) // Initial preflight and post-confirmation recheck.
	if !reflect.DeepEqual(system.probes, want) || system.unbounded {
		t.Fatalf("incorrect or unbounded probes: %+v, unbounded=%v", system.probes, system.unbounded)
	}
}

func TestCancelledFormatDoesNotProbeOrErase(t *testing.T) {
	system := &toolSystem{fakeSystem: newFakeSystem()}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Run(ctx, system, strings.NewReader("/dev/sdz\n/dev/sdz\n"), io.Discard, io.Discard, Options{HomeDir: "/home/test"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation ignored: %v", err)
	}
	if len(system.probes) != 0 || len(system.calls) != 0 {
		t.Fatal("cancelled formatter issued commands")
	}
}
