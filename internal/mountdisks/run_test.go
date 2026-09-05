package mountdisks

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
	name    string
	args    []string
	rawName string
	stdin   []byte
}

type fakeSystem struct {
	identifier         string
	diskPath           string
	mapperPath         string
	mountpoint         string
	filesystem         string
	mapperOpen         bool
	mounted            bool
	mountpointExists   bool
	mountpointNonempty bool
	nestedMountpoint   bool
	wrongMountSource   bool
	failChown          bool
	calls              []recordedCall
}

func newFakeSystem() *fakeSystem {
	return &fakeSystem{
		identifier: "0f14e957-2298-43ce-8436-40e925af43fa",
		diskPath:   "/dev/sde",
		mapperPath: "/dev/mapper/sde",
		mountpoint: "/home/test/mnt.sde",
		filesystem: "ext4",
	}
}

func (f *fakeSystem) Output(_ context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, recordedCall{
		name: name, args: append([]string(nil), args...), rawName: name,
	})
	switch name {
	case "lsblk":
		partition := blockDevice{
			Path:       f.diskPath + "1",
			KernelName: f.diskPath + "1",
			ParentName: f.diskPath,
			MajorMinor: "8:65",
			Size:       3_999_000_000,
			Type:       "part",
			Filesystem: "crypto_LUKS",
			UUID:       "luks-id",
			PartUUID:   "partition-id",
		}
		if f.mapperOpen {
			mapper := blockDevice{
				Path:        f.mapperPath,
				KernelName:  "/dev/dm-0",
				ParentName:  partition.Path,
				MajorMinor:  "254:0",
				Size:        3_998_000_000,
				Type:        "crypt",
				Filesystem:  f.filesystem,
				Mountpoints: []*string{},
			}
			if f.mounted {
				mountpoint := f.mountpoint
				mapper.Mountpoints = []*string{&mountpoint}
			}
			partition.Children = []blockDevice{mapper}
		}
		root := blockDevice{
			Path:       f.diskPath,
			KernelName: f.diskPath,
			MajorMinor: "8:64",
			Size:       4_000_787_030_016,
			Type:       "disk",
			PTUUID:     f.identifier,
			Model:      "ST4000DM004",
			Serial:     "ZFNTEST",
			WWN:        "0x5000test",
			Children:   []blockDevice{partition},
		}
		return json.Marshal(lsblkDocument{BlockDevices: []blockDevice{root}})
	case "blkid":
		return []byte(f.filesystem + "\n"), nil
	case "findmnt":
		if hasArgument(args, "--list") {
			var filesystems []mountRecord
			if f.mounted || f.wrongMountSource {
				source := f.mapperPath
				if f.wrongMountSource {
					source = "/dev/mapper/wrong"
				}
				filesystems = append(filesystems, mountRecord{Source: source, Target: f.mountpoint})
			}
			if f.nestedMountpoint {
				filesystems = append(filesystems, mountRecord{Source: "/dev/mapper/nested", Target: filepath.Join(f.mountpoint, "nested")})
			}
			return json.Marshal(findmntDocument{Filesystems: filesystems})
		}
		if hasArgument(args, "--target") && f.mounted {
			return []byte(f.mapperPath + "\n"), nil
		}
		return nil, fakeExitError{code: 1}
	default:
		return nil, errors.New("unexpected output command: " + name)
	}
}

func (f *fakeSystem) Run(_ context.Context, stdin io.Reader, _ io.Writer, _ io.Writer, name string, args ...string) error {
	var input []byte
	if stdin != nil && name == "cryptsetup" && len(args) > 0 && args[0] == "open" {
		var err error
		input, err = io.ReadAll(stdin)
		if err != nil {
			return err
		}
	}
	f.calls = append(f.calls, recordedCall{
		name: name, args: append([]string(nil), args...), rawName: name, stdin: input,
	})
	switch name {
	case "cryptsetup":
		if len(args) == 0 {
			return errors.New("missing cryptsetup action")
		}
		switch args[0] {
		case "open":
			f.mapperOpen = true
		case "close":
			f.mapperOpen = false
		default:
			return errors.New("unexpected cryptsetup action")
		}
	case "mount":
		f.mounted = true
	case "umount":
		f.mounted = false
	case "chown":
		if f.failChown {
			return fakeExitError{code: 1}
		}
	default:
		return errors.New("unexpected run command: " + name)
	}
	return nil
}

func (f *fakeSystem) Lstat(path string) (os.FileInfo, error) {
	switch path {
	case f.mapperPath:
		if f.mapperOpen {
			return fakeFileInfo{mode: os.ModeDevice}, nil
		}
	case f.mountpoint:
		if f.mountpointExists {
			return fakeFileInfo{mode: os.ModeDir | 0o755}, nil
		}
	}
	return nil, fs.ErrNotExist
}

func (f *fakeSystem) DirectoryEmpty(path string) (bool, error) {
	if path != f.mountpoint || !f.mountpointExists {
		return false, errors.New("unexpected directory emptiness check: " + path)
	}
	return !f.mountpointNonempty, nil
}

func (f *fakeSystem) MkdirAll(path string, _ os.FileMode) error {
	if path != f.mountpoint {
		return errors.New("unexpected mkdir path: " + path)
	}
	f.mountpointExists = true
	return nil
}

type fakeFileInfo struct{ mode os.FileMode }

func (f fakeFileInfo) Name() string       { return "fake" }
func (f fakeFileInfo) Size() int64        { return 0 }
func (f fakeFileInfo) Mode() os.FileMode  { return f.mode }
func (f fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (f fakeFileInfo) IsDir() bool        { return f.mode.IsDir() }
func (f fakeFileInfo) Sys() any           { return nil }

type fakeExitError struct{ code int }

func (e fakeExitError) Error() string { return "fake command failed" }
func (e fakeExitError) ExitCode() int { return e.code }

func hasArgument(args []string, wanted string) bool {
	for _, arg := range args {
		if arg == wanted {
			return true
		}
	}
	return false
}

func writeConfig(t *testing.T, identifiers ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".mirror")
	if err := os.WriteFile(path, []byte(strings.Join(identifiers, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func options(t *testing.T, password func() ([]byte, error), identifiers ...string) Options {
	t.Helper()
	return Options{
		ConfigPath:   writeConfig(t, identifiers...),
		HomeDir:      "/home/test",
		MapperDir:    "/dev/mapper",
		OwnerUID:     1234,
		OwnerGID:     5678,
		ReadPassword: password,
	}
}

func mutatingCalls(calls []recordedCall) []recordedCall {
	var result []recordedCall
	for _, call := range calls {
		switch call.name {
		case "cryptsetup", "mount", "umount", "chown":
			result = append(result, call)
		}
	}
	return result
}

func TestDeclinedMountDoesNotMutateOrReadPassword(t *testing.T) {
	system := newFakeSystem()
	passwordReads := 0
	option := options(t, func() ([]byte, error) {
		passwordReads++
		return []byte("secret"), nil
	}, system.identifier)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := Mount(context.Background(), system, strings.NewReader("no\n"), &stdout, &stderr, option)
	if !errors.Is(err, ErrDeclined) {
		t.Fatalf("error = %v, want ErrDeclined", err)
	}
	if passwordReads != 0 {
		t.Fatalf("password reader called %d times", passwordReads)
	}
	if calls := mutatingCalls(system.calls); len(calls) != 0 {
		t.Fatalf("declined mount issued mutating calls: %#v", calls)
	}
}

func TestMountClosedDiskPreservesPasswordBytesAndVerifiesState(t *testing.T) {
	system := newFakeSystem()
	password := []byte("secret  phrase * -n")
	option := options(t, func() ([]byte, error) {
		return append([]byte(nil), password...), nil
	}, system.identifier)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := Mount(context.Background(), system, strings.NewReader("mount\n"), &stdout, &stderr, option); err != nil {
		t.Fatalf("mount failed: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
	}
	if !system.mapperOpen || !system.mounted {
		t.Fatalf("final state mapperOpen=%v mounted=%v", system.mapperOpen, system.mounted)
	}
	calls := mutatingCalls(system.calls)
	if len(calls) != 3 {
		t.Fatalf("mutating calls = %#v, want open, mount, chown", calls)
	}
	if calls[0].name != "cryptsetup" || !reflect.DeepEqual(calls[0].args, []string{"open", "/dev/sde1", "sde", "--key-file", "-"}) {
		t.Fatalf("open call = %#v", calls[0])
	}
	if !bytes.Equal(calls[0].stdin, password) {
		t.Fatalf("password bytes = %q, want %q", calls[0].stdin, password)
	}
	if calls[1].name != "mount" || calls[2].name != "chown" {
		t.Fatalf("call order = %#v", calls)
	}
	if !strings.Contains(stdout.String(), "mounted /dev/sde1 at /home/test/mnt.sde") {
		t.Fatalf("missing success message: %s", stdout.String())
	}
}

func TestMountRejectsNonExt4AndLeavesMapperForManualRecovery(t *testing.T) {
	system := newFakeSystem()
	system.filesystem = "xfs"
	option := options(t, func() ([]byte, error) { return []byte("secret"), nil }, system.identifier)
	err := Mount(context.Background(), system, strings.NewReader("mount\n"), io.Discard, io.Discard, option)
	if err == nil || !strings.Contains(err.Error(), "expected ext4") {
		t.Fatalf("error = %v", err)
	}
	if !system.mapperOpen || system.mounted {
		t.Fatalf("unexpected state after rejection: mapperOpen=%v mounted=%v", system.mapperOpen, system.mounted)
	}
	if countCalls(system.calls, "mount", "") != 0 || countCalls(system.calls, "cryptsetup", "close") != 0 {
		t.Fatalf("mutating calls = %#v", mutatingCalls(system.calls))
	}
}

func TestMountAlreadyOpenMapperDoesNotReadPassword(t *testing.T) {
	system := newFakeSystem()
	system.mapperOpen = true
	option := options(t, func() ([]byte, error) {
		t.Fatal("password reader should not be called")
		return nil, nil
	}, system.identifier)
	if err := Mount(context.Background(), system, strings.NewReader("mount\n"), io.Discard, io.Discard, option); err != nil {
		t.Fatalf("mount failed: %v", err)
	}
	if countCalls(system.calls, "cryptsetup", "open") != 0 {
		t.Fatal("mount reopened an existing mapper")
	}
	if !system.mounted {
		t.Fatal("existing mapper was not mounted")
	}
}

func TestMountFailureLeavesStateForManualRecovery(t *testing.T) {
	system := newFakeSystem()
	system.failChown = true
	option := options(t, func() ([]byte, error) { return []byte("secret"), nil }, system.identifier)
	err := Mount(context.Background(), system, strings.NewReader("mount\n"), io.Discard, io.Discard, option)
	if err == nil {
		t.Fatal("mount unexpectedly succeeded")
	}
	if !system.mounted || !system.mapperOpen {
		t.Fatalf("failure automatically changed state: mapperOpen=%v mounted=%v", system.mapperOpen, system.mounted)
	}
	if countCalls(system.calls, "umount", "") != 0 || countCalls(system.calls, "cryptsetup", "close") != 0 {
		t.Fatalf("unexpected automatic rollback: %#v", mutatingCalls(system.calls))
	}
}

func TestWrongMountSourceFailsBeforeMutation(t *testing.T) {
	system := newFakeSystem()
	system.wrongMountSource = true
	option := options(t, func() ([]byte, error) { return []byte("secret"), nil }, system.identifier)
	err := Mount(context.Background(), system, strings.NewReader("mount\n"), io.Discard, io.Discard, option)
	if err == nil || !strings.Contains(err.Error(), "occupied") {
		t.Fatalf("error = %v", err)
	}
	if calls := mutatingCalls(system.calls); len(calls) != 0 {
		t.Fatalf("wrong mount source issued mutations: %#v", calls)
	}
}

func TestMountRejectsUnsafeDestinationBeforeMutation(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*fakeSystem)
	}{
		{
			name: "nonempty mountpoint",
			mutate: func(system *fakeSystem) {
				system.mountpointExists = true
				system.mountpointNonempty = true
			},
		},
		{
			name: "nested mount",
			mutate: func(system *fakeSystem) {
				system.mountpointExists = true
				system.nestedMountpoint = true
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			system := newFakeSystem()
			test.mutate(system)
			passwordReads := 0
			option := options(t, func() ([]byte, error) {
				passwordReads++
				return []byte("secret"), nil
			}, system.identifier)
			err := Mount(context.Background(), system, strings.NewReader("mount\n"), io.Discard, io.Discard, option)
			if err == nil {
				t.Fatal("mount unexpectedly succeeded")
			}
			if passwordReads != 0 {
				t.Fatalf("unsafe mountpoint caused %d password reads", passwordReads)
			}
			if calls := mutatingCalls(system.calls); len(calls) != 0 {
				t.Fatalf("unsafe mountpoint issued mutations: %#v", calls)
			}
		})
	}
}

func TestUnmountMountedAndOpenOnlyStates(t *testing.T) {
	for _, test := range []struct {
		name    string
		mounted bool
		want    []string
	}{
		{name: "mounted", mounted: true, want: []string{"umount", "cryptsetup"}},
		{name: "open but unmounted", mounted: false, want: []string{"cryptsetup"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			system := newFakeSystem()
			system.mapperOpen = true
			system.mounted = test.mounted
			option := options(t, nil, system.identifier)
			if err := Unmount(context.Background(), system, strings.NewReader("unmount\n"), io.Discard, io.Discard, option); err != nil {
				t.Fatalf("unmount failed: %v", err)
			}
			if system.mapperOpen || system.mounted {
				t.Fatalf("final state mapperOpen=%v mounted=%v", system.mapperOpen, system.mounted)
			}
			var got []string
			for _, call := range mutatingCalls(system.calls) {
				got = append(got, call.name)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("mutating call order = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestMountedRootsReturnsOnlyConfiguredMountedDisks(t *testing.T) {
	system := newFakeSystem()
	system.mapperOpen = true
	system.mounted = true
	missingID := "a74a2821-168e-4475-87a9-ab6f2e4e9714"
	configPath := writeConfig(t, system.identifier, missingID)
	state, err := MountedRoots(context.Background(), system, configPath, "/home/test", "/dev/mapper")
	if err != nil {
		t.Fatalf("discover mounted roots: %v", err)
	}
	if want := []string{system.mountpoint}; !reflect.DeepEqual(state.Roots, want) {
		t.Fatalf("roots = %#v, want %#v", state.Roots, want)
	}
	if want := []string{missingID}; !reflect.DeepEqual(state.Missing, want) {
		t.Fatalf("missing = %#v, want %#v", state.Missing, want)
	}
}

func countCalls(calls []recordedCall, name, argument string) int {
	count := 0
	for _, call := range calls {
		if call.name == name && (argument == "" || hasArgument(call.args, argument)) {
			count++
		}
	}
	return count
}

func TestMountWorkflowsNeverInvokeSudo(t *testing.T) {
	for _, command := range []string{"mount", "unmount"} {
		t.Run(command, func(t *testing.T) {
			system := newFakeSystem()
			var err error
			switch command {
			case "mount":
				err = Mount(
					context.Background(),
					system,
					strings.NewReader("mount\n"),
					io.Discard,
					io.Discard,
					options(t, func() ([]byte, error) { return []byte("secret"), nil }, system.identifier),
				)
			case "unmount":
				system.mapperOpen = true
				system.mounted = true
				err = Unmount(
					context.Background(),
					system,
					strings.NewReader("unmount\n"),
					io.Discard,
					io.Discard,
					options(t, nil, system.identifier),
				)
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, call := range system.calls {
				if call.rawName == "sudo" {
					t.Fatalf("%s invoked sudo internally: %#v", command, call)
				}
			}
		})
	}
}
