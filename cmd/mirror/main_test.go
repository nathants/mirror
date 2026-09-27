package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"mirror/internal/operationlock"
)

// The real entrypoint runs in a subprocess. Fake inventory executables have no
// access to any block device; PATH contains only this test binary's aliases.
func TestMain(m *testing.M) {
	if os.Getenv("MIRROR_TEST_HOME") != "" {
		name := filepath.Base(os.Args[0])
		if name == "lsblk" || name == "findmnt" {
			fakeInventory(name)
			return
		}
		if command := os.Getenv("MIRROR_TEST_COMMAND"); command != "" {
			os.Args = []string{"mirror", command}
			if command == "ensure-copies" || command == "repair-copies" {
				os.Args = append(os.Args, "--preview")
			}
			os.Exit(run())
		}
	}
	os.Exit(m.Run())
}
func testCommand(t *testing.T, command string) (*exec.Cmd, string, *bytes.Buffer) {
	t.Helper()
	home := t.TempDir()
	for _, dir := range []string{".mirror", "bin", "mnt.sde"} {
		if err := os.Mkdir(filepath.Join(home, dir), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(home, ".mirror/disks"), []byte("0f14e957-2298-43ce-8436-40e925af43fa\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "mnt.sde/item"), []byte("original bytes"), 0644); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"lsblk", "findmnt"} {
		if err := os.Symlink(self, filepath.Join(home, "bin", name)); err != nil {
			t.Fatal(err)
		}
	}
	child := exec.Command(self)
	for _, value := range os.Environ() {
		name, _, _ := strings.Cut(value, "=")
		if name != "SUDO_UID" && name != "SUDO_GID" && name != "SUDO_USER" {
			child.Env = append(child.Env, value)
		}
	}
	child.Env = append(child.Env, "HOME="+home, "PATH="+filepath.Join(home, "bin"), "MIRROR_TEST_HOME="+home, "MIRROR_TEST_COMMAND="+command)
	output := &bytes.Buffer{}
	child.Stdout = output
	child.Stderr = output
	return child, home, output
}
func TestSignalsCancelRealCopyRepairAndLockEntrypoints(t *testing.T) {
	for _, command := range []string{"ensure-copies", "repair-copies", "lock", "unlock"} {
		for _, signal := range []os.Signal{syscall.SIGTERM, os.Interrupt} {
			t.Run(command+"/"+signal.String(), func(t *testing.T) {
				child, home, output := testCommand(t, command)
				if err := child.Start(); err != nil {
					t.Fatal(err)
				}
				done := make(chan error, 1)
				go func() { done <- child.Wait() }()
				deadline := time.Now().Add(5 * time.Second)
				for {
					if _, err := os.Stat(filepath.Join(home, "ready")); err == nil {
						break
					}
					if time.Now().After(deadline) {
						_ = child.Process.Kill()
						<-done
						t.Fatalf("inventory not reached: %s", output.String())
					}
					time.Sleep(5 * time.Millisecond)
				}
				if err := child.Process.Signal(signal); err != nil {
					t.Fatal(err)
				}
				select {
				case err := <-done:
					if err == nil {
						t.Fatal("cancelled command reported success")
					}
				case <-time.After(3 * time.Second):
					_ = child.Process.Kill()
					<-done
					t.Fatal("entrypoint ignored cancellation")
				}
				if _, err := os.Stat(filepath.Join(home, "mnt.sde/item.b2sum")); !os.IsNotExist(err) {
					t.Fatal("wrote metadata after cancellation")
				}
				checkFailureLog(t, home, command, "context canceled")
				// The same lock inode can be acquired immediately after process cleanup.
				lock, err := operationlock.Acquire(context.Background(), home, os.Getuid(), os.Getgid())
				if err != nil {
					t.Fatal(err)
				}
				_ = lock.Close()
			})
		}
	}
}
func TestAllUnprivilegedCommandsUseSharedOperationLock(t *testing.T) {
	for _, command := range []string{"ensure-copies", "repair-copies", "lock", "unlock"} {
		t.Run(command, func(t *testing.T) {
			child, home, output := testCommand(t, command)
			lock, err := operationlock.Acquire(context.Background(), home, os.Getuid(), os.Getgid())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = lock.Close() }()
			if err := child.Run(); err == nil || !strings.Contains(output.String(), "another mirror operation") {
				t.Fatalf("did not refuse contention: %v %s", err, output.String())
			}
			if _, err := os.Stat(filepath.Join(home, "ready")); !os.IsNotExist(err) {
				t.Fatal("contended command reached inventory")
			}
			checkFailureLog(t, home, command, "another mirror operation")
		})
	}
}
func fakeInventory(name string) {
	home := os.Getenv("MIRROR_TEST_HOME")
	root := filepath.Join(home, "mnt.sde")
	if name == "findmnt" {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"filesystems": []any{map[string]any{"source": "/dev/mapper/sde", "target": root}}})
		return
	}
	if err := os.WriteFile(filepath.Join(home, "ready"), nil, 0600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	// The test cancels while this real child command is blocked. Bound a failed
	// test so the helper cannot survive indefinitely even if cleanup regresses.
	time.Sleep(10 * time.Second)
	os.Exit(2)
}

func checkFailureLog(t *testing.T, home, command, message string) {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(home, ".mirror-logs", "*.jsonl"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("command logs: %v, %v", paths, err)
	}
	data, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	type record struct {
		Kind     string
		Args     []string
		Text     string
		ExitCode *int `json:"exit_code"`
	}
	var records []record
	decoder := json.NewDecoder(bytes.NewReader(data))
	for {
		var value record
		if err := decoder.Decode(&value); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		records = append(records, value)
	}
	if len(records) < 3 || records[0].Kind != "start" || len(records[0].Args) == 0 || records[0].Args[0] != command {
		t.Fatalf("missing start/output: %+v", records)
	}
	found := false
	for _, record := range records {
		found = found || strings.Contains(record.Text, message)
	}
	last := records[len(records)-1]
	if !found || last.Kind != "finish" || last.ExitCode == nil || *last.ExitCode != 1 {
		t.Fatalf("missing failure diagnostics/status: %+v", records)
	}
}
