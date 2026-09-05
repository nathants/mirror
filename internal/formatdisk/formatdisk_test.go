package formatdisk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type recordedCall struct {
	kind    string
	rawName string
	name    string
	args    []string
	stdin   string
}

type fakeSystem struct {
	devicePath           string
	deviceType           string
	serial               string
	wwn                  string
	readOnly             bool
	filesystem           string
	mountpoint           string
	holderType           string
	mountpointBusy       bool
	nestedMountpoint     bool
	mountPathNonempty    bool
	mapperInitiallyOpen  bool
	changeOnRevalidate   bool
	changeAfterPartition bool
	failOpenCount        int
	wrongMountedSource   bool

	calls         []recordedCall
	inspectCount  int
	partitioned   bool
	mapperOpen    bool
	mounted       bool
	mapperPath    string
	mountPath     string
	mountPathMode os.FileMode
}

func newFakeSystem() *fakeSystem {
	return &fakeSystem{
		devicePath: "/dev/sdz",
		deviceType: "disk",
		serial:     "ZFNTEST",
		wwn:        "0x5000test",
		mapperPath: "/dev/mapper/sdz",
		mountPath:  "/home/test/mnt.sdz",
	}
}

func (f *fakeSystem) Output(_ context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, recordedCall{kind: "output", rawName: name, name: name, args: append([]string(nil), args...)})

	switch name {
	case "lsblk":
		if contains(args, "--nodeps") {
			return marshalLSBLK(f.rootDevice()), nil
		}
		f.inspectCount++
		root := f.rootDevice()
		if f.changeOnRevalidate && f.inspectCount == 2 {
			root.Serial = "DIFFERENT"
		}
		if f.changeAfterPartition && f.partitioned {
			root.Serial = "REPLACEMENT"
		}
		devices := []blockDevice{root}
		partitionPath := f.devicePath + "1"
		if f.deviceType == "loop" {
			partitionPath = f.devicePath + "p1"
		}
		partition := blockDevice{
			Path:       partitionPath,
			KernelName: partitionPath,
			ParentName: f.devicePath,
			MajorMinor: "8:241",
			Size:       990_000_000,
			Type:       "part",
			Filesystem: f.filesystem,
		}
		if f.mountpoint != "" {
			mountpoint := f.mountpoint
			partition.Mountpoints = []*string{&mountpoint}
		}
		devices = append(devices, partition)
		if f.holderType != "" {
			devices = append(devices, blockDevice{
				Path:       "/dev/mapper/holder",
				KernelName: "/dev/dm-0",
				ParentName: partitionPath,
				MajorMinor: "254:0",
				Size:       980_000_000,
				Type:       f.holderType,
			})
		}
		return marshalLSBLK(devices...), nil
	case "findmnt":
		if contains(args, "--json") {
			root := mountRecord{Target: "/"}
			if f.mountpointBusy {
				root.Children = append(root.Children, mountRecord{Target: f.mountPath})
			}
			if f.nestedMountpoint {
				root.Children = append(root.Children, mountRecord{Target: filepath.Join(f.mountPath, "nested")})
			}
			return marshalFindmnt(root), nil
		}
		if contains(args, "--target") && f.mounted {
			if f.wrongMountedSource {
				return []byte("/dev/mapper/wrong\n"), nil
			}
			return []byte(f.mapperPath + "\n"), nil
		}
		return nil, fakeExitError{code: 1}
	case "blkid":
		return []byte("ext4\n"), nil
	default:
		return nil, errors.New("unexpected output command: " + name)
	}
}

func (f *fakeSystem) Run(
	_ context.Context,
	stdin io.Reader,
	_ io.Writer,
	_ io.Writer,
	name string,
	args ...string,
) error {
	input := ""
	if name == "fdisk" && stdin != nil {
		data, err := io.ReadAll(stdin)
		if err != nil {
			return err
		}
		input = string(data)
	}
	f.calls = append(f.calls, recordedCall{
		kind:    "run",
		rawName: name,
		name:    name,
		args:    append([]string(nil), args...),
		stdin:   input,
	})

	switch name {
	case "fdisk":
		f.partitioned = true
		return nil
	case "cryptsetup":
		if len(args) == 0 {
			return errors.New("missing cryptsetup action")
		}
		action := args[0]
		if action == "-v" && len(args) > 1 {
			action = args[1]
		}
		switch action {
		case "luksFormat", "isLuks":
			return nil
		case "open":
			if f.failOpenCount > 0 {
				f.failOpenCount--
				return fakeExitError{code: 2}
			}
			f.mapperOpen = true
			return nil
		case "close":
			f.mapperOpen = false
			return nil
		default:
			return errors.New("unexpected cryptsetup action: " + action)
		}
	case "chown":
		return nil
	case "mkfs.ext4":
		return nil
	case "mount":
		f.mounted = true
		return nil
	case "umount":
		f.mounted = false
		return nil
	default:
		return errors.New("unexpected run command: " + name)
	}
}

func (f *fakeSystem) Lstat(path string) (os.FileInfo, error) {
	switch path {
	case f.mapperPath:
		if f.mapperInitiallyOpen || f.mapperOpen {
			return fakeFileInfo{mode: os.ModeDevice}, nil
		}
	case f.mountPath:
		if f.mountPathMode != 0 {
			return fakeFileInfo{mode: f.mountPathMode}, nil
		}
	}
	return nil, fs.ErrNotExist
}

func (f *fakeSystem) DirectoryEmpty(path string) (bool, error) {
	if path != f.mountPath || f.mountPathMode == 0 {
		return false, errors.New("unexpected directory emptiness check: " + path)
	}
	return !f.mountPathNonempty, nil
}

func (f *fakeSystem) MkdirAll(path string, _ os.FileMode) error {
	if path != f.mountPath {
		return errors.New("unexpected mkdir path: " + path)
	}
	f.mountPathMode = os.ModeDir | 0o755
	return nil
}

func (f *fakeSystem) Sleep(context.Context, time.Duration) error {
	return nil
}

func (f *fakeSystem) rootDevice() blockDevice {
	return blockDevice{
		Path:       f.devicePath,
		KernelName: f.devicePath,
		MajorMinor: "8:240",
		Size:       1_000_000_000,
		Type:       f.deviceType,
		Model:      "TEST DISK",
		Serial:     f.serial,
		WWN:        f.wwn,
		ReadOnly:   f.readOnly,
	}
}

type fakeExitError struct {
	code int
}

func (e fakeExitError) Error() string {
	return "fake command failed"
}

func (e fakeExitError) ExitCode() int {
	return e.code
}

type fakeFileInfo struct{ mode os.FileMode }

func (fakeFileInfo) Name() string        { return "sda" }
func (fakeFileInfo) Size() int64         { return 0 }
func (f fakeFileInfo) Mode() os.FileMode { return f.mode }
func (fakeFileInfo) ModTime() time.Time  { return time.Time{} }
func (f fakeFileInfo) IsDir() bool       { return f.mode.IsDir() }
func (fakeFileInfo) Sys() any            { return nil }

func marshalLSBLK(devices ...blockDevice) []byte {
	data, err := json.Marshal(lsblkDocument{BlockDevices: devices})
	if err != nil {
		panic(err)
	}
	return data
}

func marshalFindmnt(filesystems ...mountRecord) []byte {
	data, err := json.Marshal(findmntDocument{Filesystems: filesystems})
	if err != nil {
		panic(err)
	}
	return data
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func runFake(input string, system *fakeSystem, configure ...func(*Options)) (string, string, error) {
	option := Options{
		HomeDir:               "/home/test",
		MapperDir:             "/dev/mapper",
		OwnerUID:              1234,
		OwnerGID:              5678,
		PartitionPollAttempts: 2,
	}
	for _, change := range configure {
		change(&option)
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := Run(context.Background(), system, strings.NewReader(input), &stdout, &stderr, option)
	return stdout.String(), stderr.String(), err
}

func destructiveCalls(calls []recordedCall) []recordedCall {
	var result []recordedCall
	for _, call := range calls {
		switch call.name {
		case "chown", "fdisk", "mkfs.ext4", "mount", "umount":
			result = append(result, call)
		case "cryptsetup":
			if contains(call.args, "luksFormat") || contains(call.args, "open") || contains(call.args, "close") {
				result = append(result, call)
			}
		}
	}
	return result
}

func countCalls(calls []recordedCall, name string, argument string) int {
	count := 0
	for _, call := range calls {
		if call.name == name && (argument == "" || contains(call.args, argument)) {
			count++
		}
	}
	return count
}

func TestRejectedDevicesPerformNoDestructiveCommands(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		mutate func(*fakeSystem)
	}{
		{
			name:  "confirmation declined",
			input: "/dev/sdz\nn\n",
		},
		{
			name:  "non sd path",
			input: "/dev/nvme0n1\n",
		},
		{
			name:  "partition path",
			input: "/dev/sdz1\n",
		},
		{
			name:  "not a whole disk",
			input: "/dev/sdz\n",
			mutate: func(system *fakeSystem) {
				system.deviceType = "part"
			},
		},
		{
			name:  "mounted child",
			input: "/dev/sdz\n",
			mutate: func(system *fakeSystem) {
				system.mountpoint = "/data"
			},
		},
		{
			name:  "swap child",
			input: "/dev/sdz\n",
			mutate: func(system *fakeSystem) {
				system.filesystem = "swap"
			},
		},
		{
			name:  "active holder",
			input: "/dev/sdz\n",
			mutate: func(system *fakeSystem) {
				system.holderType = "crypt"
			},
		},
		{
			name:  "read-only disk",
			input: "/dev/sdz\n",
			mutate: func(system *fakeSystem) {
				system.readOnly = true
			},
		},
		{
			name:  "missing stable identity",
			input: "/dev/sdz\n",
			mutate: func(system *fakeSystem) {
				system.serial = ""
				system.wwn = ""
			},
		},
		{
			name:  "destination mountpoint is a symlink",
			input: "/dev/sdz\n",
			mutate: func(system *fakeSystem) {
				system.mountPathMode = os.ModeSymlink
			},
		},
		{
			name:  "busy destination mountpoint",
			input: "/dev/sdz\n",
			mutate: func(system *fakeSystem) {
				system.mountpointBusy = true
			},
		},
		{
			name:  "destination mountpoint contains a file",
			input: "/dev/sdz\n/dev/sdz\n",
			mutate: func(system *fakeSystem) {
				system.mountPathMode = os.ModeDir | 0o755
				system.mountPathNonempty = true
			},
		},
		{
			name:  "destination mountpoint contains a nested mount",
			input: "/dev/sdz\n/dev/sdz\n",
			mutate: func(system *fakeSystem) {
				system.mountPathMode = os.ModeDir | 0o755
				system.nestedMountpoint = true
			},
		},
		{
			name:  "busy mapper name",
			input: "/dev/sdz\n",
			mutate: func(system *fakeSystem) {
				system.mapperInitiallyOpen = true
			},
		},
		{
			name:  "identity changes during confirmation",
			input: "/dev/sdz\n/dev/sdz\n",
			mutate: func(system *fakeSystem) {
				system.changeOnRevalidate = true
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			system := newFakeSystem()
			if test.mutate != nil {
				test.mutate(system)
			}
			_, _, err := runFake(test.input, system)
			if err == nil {
				t.Fatal("formatter unexpectedly succeeded")
			}
			if calls := destructiveCalls(system.calls); len(calls) != 0 {
				t.Fatalf("formatter issued destructive calls: %#v", calls)
			}
		})
	}
}

func TestReplacementAfterPartitioningStopsBeforeLUKS(t *testing.T) {
	system := newFakeSystem()
	system.changeAfterPartition = true
	_, _, err := runFake("/dev/sdz\n/dev/sdz\n", system)
	if err == nil || !strings.Contains(err.Error(), "identity changed after partitioning") {
		t.Fatalf("error = %v", err)
	}
	if count := countCalls(system.calls, "fdisk", ""); count != 1 {
		t.Fatalf("fdisk calls = %d, want 1", count)
	}
	if count := countCalls(system.calls, "cryptsetup", "luksFormat"); count != 0 {
		t.Fatalf("replacement disk received %d luksFormat calls", count)
	}
}

func TestSuccessfulFormatUsesExactOrderAndArguments(t *testing.T) {
	system := newFakeSystem()
	stdout, stderr, err := runFake("/dev/sdz\n/dev/sdz\n", system)
	if err != nil {
		t.Fatalf("format failed: %v\nstdout:\n%s\nstderr:\n%s", err, stdout, stderr)
	}
	if system.inspectCount < 3 {
		t.Fatalf("expected initial inspection, revalidation, and post-fdisk inspection; got %d", system.inspectCount)
	}
	if count := countCalls(system.calls, "fdisk", ""); count != 1 {
		t.Fatalf("expected one fdisk call, got %d", count)
	}
	if count := countCalls(system.calls, "cryptsetup", "luksFormat"); count != 1 {
		t.Fatalf("expected one luksFormat call, got %d", count)
	}
	if count := countCalls(system.calls, "cryptsetup", "open"); count != 1 {
		t.Fatalf("expected one open call, got %d", count)
	}
	if count := countCalls(system.calls, "mkfs.ext4", ""); count != 1 {
		t.Fatalf("expected one mkfs.ext4 call, got %d", count)
	}
	if count := countCalls(system.calls, "mount", ""); count != 1 {
		t.Fatalf("expected one mount call, got %d", count)
	}
	if count := countCalls(system.calls, "chown", ""); count != 1 {
		t.Fatalf("expected one chown call, got %d", count)
	}
	var fdiskCall *recordedCall
	var mkfsCall *recordedCall
	var chownCall *recordedCall
	for index := range system.calls {
		switch system.calls[index].name {
		case "fdisk":
			fdiskCall = &system.calls[index]
		case "mkfs.ext4":
			mkfsCall = &system.calls[index]
		case "chown":
			chownCall = &system.calls[index]
		}
	}
	if fdiskCall == nil {
		t.Fatal("fdisk was not called")
	}
	if fdiskCall.rawName != "fdisk" {
		t.Fatalf("fdisk was not invoked directly: %#v", *fdiskCall)
	}
	if got, want := strings.Join(fdiskCall.args, " "), "-w always -W always /dev/sdz"; got != want {
		t.Fatalf("fdisk args = %q, want %q", got, want)
	}
	if got, want := fdiskCall.stdin, "g\nn\n1\n\n\n\nw\n"; got != want {
		t.Fatalf("fdisk stdin = %q, want %q", got, want)
	}
	if mkfsCall == nil || !reflect.DeepEqual(mkfsCall.args, []string{"-m", "0", "/dev/mapper/sdz"}) {
		t.Fatalf("mkfs.ext4 call = %#v", mkfsCall)
	}
	if chownCall == nil || !reflect.DeepEqual(chownCall.args, []string{"1234:5678", "/home/test/mnt.sdz"}) {
		t.Fatalf("chown call = %#v", chownCall)
	}
	if !system.mounted {
		t.Fatal("successful format did not leave the new filesystem mounted")
	}
	if !strings.Contains(stdout, "formatted and mounted /dev/sdz at /home/test/mnt.sdz") {
		t.Fatalf("missing success report in stdout:\n%s", stdout)
	}
}

func TestOpenRetryNeverReformatsLUKS(t *testing.T) {
	system := newFakeSystem()
	system.failOpenCount = 1
	_, _, err := runFake("/dev/sdz\n/dev/sdz\ny\n", system)
	if err != nil {
		t.Fatalf("format with successful open retry failed: %v", err)
	}
	if count := countCalls(system.calls, "cryptsetup", "luksFormat"); count != 1 {
		t.Fatalf("open retry invoked luksFormat %d times", count)
	}
	if count := countCalls(system.calls, "cryptsetup", "open"); count != 2 {
		t.Fatalf("open retry invoked open %d times, want 2", count)
	}
}

func TestDeclinedOpenRetryNeverReformatsLUKS(t *testing.T) {
	system := newFakeSystem()
	system.failOpenCount = 1
	_, _, err := runFake("/dev/sdz\n/dev/sdz\nn\n", system)
	if err == nil {
		t.Fatal("declining the open retry unexpectedly succeeded")
	}
	if count := countCalls(system.calls, "cryptsetup", "luksFormat"); count != 1 {
		t.Fatalf("declining open retry invoked luksFormat %d times", count)
	}
}

func TestFailedMountVerificationLeavesStateForManualRecovery(t *testing.T) {
	system := newFakeSystem()
	system.wrongMountedSource = true
	_, _, err := runFake("/dev/sdz\n/dev/sdz\n", system)
	if err == nil {
		t.Fatal("wrong mounted source unexpectedly succeeded")
	}
	if !system.mounted || !system.mapperOpen {
		t.Fatal("failure automatically changed mounted or mapper state")
	}
	if count := countCalls(system.calls, "umount", ""); count != 0 {
		t.Fatalf("failure called umount %d times, want 0", count)
	}
	if count := countCalls(system.calls, "cryptsetup", "close"); count != 0 {
		t.Fatalf("failure called cryptsetup close %d times, want 0", count)
	}
}

func TestNonSCSIBypassExistsOnlyInInjectedTestOptions(t *testing.T) {
	system := newFakeSystem()
	system.devicePath = "/dev/loop7"
	system.deviceType = "loop"
	system.serial = ""
	system.wwn = ""
	system.mapperPath = "/test/mapper/loop7"
	system.mountPath = "/test/home/mnt.loop7"

	configure := func(option *Options) {
		option.HomeDir = "/test/home"
		option.MapperDir = "/test/mapper"
		option.AllowNonSCSIBlockForTesting = true
	}
	_, _, err := runFake("/dev/loop7\n/dev/loop7\n", system, configure)
	if err != nil {
		t.Fatalf("test-only loop device failed: %v", err)
	}
	for _, call := range system.calls {
		if call.name == "cryptsetup" && contains(call.args, "luksFormat") {
			if got := call.args[len(call.args)-1]; got != "/dev/loop7p1" {
				t.Fatalf("loop partition path = %q, want /dev/loop7p1", got)
			}
			return
		}
	}
	t.Fatal("luksFormat was not called")
}

func TestFormatterNeverInvokesSudo(t *testing.T) {
	system := newFakeSystem()
	if _, _, err := runFake("/dev/sdz\n/dev/sdz\n", system); err != nil {
		t.Fatal(err)
	}
	for _, call := range system.calls {
		if call.rawName == "sudo" {
			t.Fatalf("formatter invoked sudo internally: %#v", call)
		}
	}
}
