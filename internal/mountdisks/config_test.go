package mountdisks

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestConfigFIFOIsRejectedWithoutWaitingForWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "disks")
	if err := unix.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	system := newFakeSystem()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := MountedRoots(ctx, system, path, "/home/test", "/dev/mapper")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "regular file") {
			t.Fatalf("FIFO was not rejected as an invalid file type: %v", err)
		}
	case <-time.After(200 * time.Millisecond):
		cancel()
		// Release the old blocking implementation before reporting the failure.
		// Nonblocking writer open avoids introducing another stuck open if the
		// loader has already returned or is still waiting to be scheduled.
		for {
			select {
			case err := <-done:
				t.Fatalf("config loader waited instead of rejecting FIFO: %v", err)
			default:
			}
			fd, err := unix.Open(path, unix.O_WRONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
			if err == nil {
				_ = unix.Close(fd)
				<-done
				t.Fatal("config loader blocked waiting for a FIFO writer despite cancellation")
			}
			if !errors.Is(err, unix.ENXIO) {
				t.Fatal(err)
			}
			time.Sleep(time.Millisecond)
		}
	}
	if len(system.calls) != 0 {
		t.Fatalf("invalid config reached inventory: %#v", system.calls)
	}
}

func TestConfigRejectsUnsafeInputsBeforeInventory(t *testing.T) {
	for _, kind := range []string{"directory", "socket", "symlink", "ancestor-symlink", "oversized", "missing", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			system := newFakeSystem()
			dir := t.TempDir()
			path := filepath.Join(dir, "disks")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch kind {
			case "directory":
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			case "socket":
				listener, err := net.Listen("unix", path)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = listener.Close() }()
			case "symlink":
				if err := os.Symlink(writeConfig(t, system.identifier), path); err != nil {
					t.Fatal(err)
				}
			case "ancestor-symlink":
				target := writeConfig(t, system.identifier)
				link := filepath.Join(dir, "linked-parent")
				if err := os.Symlink(filepath.Dir(target), link); err != nil {
					t.Fatal(err)
				}
				path = filepath.Join(link, filepath.Base(target))
			case "oversized":
				// Valid syntax, but too much metadata; must not be silently parsed.
				data := system.identifier + "\n#" + strings.Repeat("x", 64*1024)
				if err := os.WriteFile(path, []byte(data), 0600); err != nil {
					t.Fatal(err)
				}
			case "cancelled":
				path = writeConfig(t, system.identifier)
				cancel()
			}
			state, err := MountedRoots(ctx, system, path, "/home/test", "/dev/mapper")
			if err == nil || state.ConfiguredMirrors != 0 {
				t.Errorf("unsafe input accepted: state=%+v error=%v", state, err)
			}
			if kind == "cancelled" && !errors.Is(err, context.Canceled) {
				t.Errorf("lost cancellation: %v", err)
			}
			if kind == "oversized" && (err == nil || !strings.Contains(err.Error(), "65536")) {
				t.Errorf("missing size-limit diagnostic: %v", err)
			}
			if len(system.calls) != 0 {
				t.Errorf("unsafe input reached inventory: %#v", system.calls)
			}
		})
	}
}

func TestConfigSyntaxAndLimit(t *testing.T) {
	const identifier = "0f14e957-2298-43ce-8436-40e925af43fa"
	const other = "a74a2821-168e-4475-87a9-ab6f2e4e9714"
	for _, test := range []struct {
		name, data, wantError string
	}{
		{"comments-whitespace-case", "\r\n  # comment\r\n\t" + strings.ToUpper(identifier) + "  \r\n\n" + other, ""},
		{"exact-limit", identifier + "\n" + other + "\n#" + strings.Repeat("x", 65536-2*37-1), ""},
		{"duplicate", identifier + "\n" + strings.ToUpper(identifier), "duplicate"},
		{"invalid", "# comment\n\nnot-a-uuid\n", "line 3"},
		{"no-identifiers", "# comment\n\n", "no disk identifiers"},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "disks")
			if err := os.WriteFile(path, []byte(test.data), 0600); err != nil {
				t.Fatal(err)
			}
			system := newFakeSystem()
			state, err := MountedRoots(context.Background(), system, path, "/home/test", "/dev/mapper")
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) || len(system.calls) != 0 {
					t.Fatalf("invalid syntax: %v; calls=%#v", err, system.calls)
				}
				return
			}
			if err != nil || state.ConfiguredMirrors != 2 || !reflect.DeepEqual(state.Missing, []string{other}) {
				t.Fatalf("changed config parsing: %+v; %v", state, err)
			}
		})
	}
}

func TestConfigRevalidatedAfterConfirmation(t *testing.T) {
	for _, operation := range []string{"mount", "unmount"} {
		t.Run(operation, func(t *testing.T) {
			system := newFakeSystem()
			system.mapperOpen = true
			system.mounted = operation == "unmount"
			option := options(t, nil, system.identifier)
			input := &configChangeReader{Reader: strings.NewReader(operation + "\n"), change: func() {
				if err := os.Remove(option.ConfigPath); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(writeConfig(t, system.identifier), option.ConfigPath); err != nil {
					t.Fatal(err)
				}
			}}
			run := Mount
			if operation == "unmount" {
				run = Unmount
			}
			err := run(context.Background(), system, input, io.Discard, io.Discard, option)
			if err == nil || !strings.Contains(err.Error(), "revalidate") {
				t.Errorf("config replacement not rejected during revalidation: %v", err)
			}
			if calls := mutatingCalls(system.calls); len(calls) != 0 {
				t.Errorf("unsafe config reached mutation: %#v", calls)
			}
		})
	}
}

type configChangeReader struct {
	io.Reader
	change func()
}

func (r *configChangeReader) Read(data []byte) (int, error) {
	if r.change != nil {
		r.change()
		r.change = nil
	}
	return r.Reader.Read(data)
}

func TestConfigReadBoundsStreamAndChecksCancellation(t *testing.T) {
	const identifier = "0f14e957-2298-43ce-8436-40e925af43fa"
	// The streamed limit must hold independently of the earlier stat check,
	// including for files that grow or report a synthetic size.
	data := identifier + "\n#" + strings.Repeat("x", 2*maxConfigBytes)
	reader := strings.NewReader(data)
	ids, err := readConfig(context.Background(), "disks", reader)
	if err == nil || !strings.Contains(err.Error(), "65536") || len(ids) != 0 {
		t.Fatalf("oversized stream accepted: %v %v", ids, err)
	}
	if consumed := len(data) - reader.Len(); consumed != maxConfigBytes+1 {
		t.Fatalf("stream read %d bytes, want only limit plus one", consumed)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	input := &configChangeReader{Reader: strings.NewReader(identifier), change: cancel}
	ids, err = readConfig(ctx, "disks", input)
	if !errors.Is(err, context.Canceled) || len(ids) != 0 {
		t.Fatalf("cancellation during a read accepted partial config: %v %v", ids, err)
	}
}
