package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mirror/internal/mountdisks"
	"mirror/internal/operationlock"
	"mirror/internal/repaircopies"
)

func TestRepairRunUsesCLIOutput(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// Resume a nonexistent journal: exercise the real CLI without inventory or
	// a controlling terminal, and verify its first diagnostic uses our stream.
	var output, diagnostic bytes.Buffer
	code := Run(context.Background(), []string{"repair-copies", "--resume"}, strings.NewReader(""), &output, &diagnostic)
	if code != 1 || !strings.Contains(output.String(), "repair log: "+filepath.Join(home, ".mirror", "repair-last.jsonl")) {
		t.Fatalf("code=%d output=%q diagnostic=%q", code, &output, &diagnostic)
	}
}

// These tests substitute only inventory/terminal I/O. Dispatch, locking, scan,
// publication, journal persistence, and directory guards are production code.
func TestCopyHandlersRunRealWorkflowsThroughCLIStreams(t *testing.T) {
	for _, command := range []string{"ensure-copies", "repair-copies"} {
		t.Run(command, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("PATH", t.TempDir())
			roots := []string{t.TempDir(), t.TempDir()}
			for i, root := range roots {
				if err := os.Chmod(root, 0700); err != nil {
					t.Fatal(err)
				}
				if i == 0 || command == "repair-copies" {
					if err := os.WriteFile(filepath.Join(root, "item"), []byte("data"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				t.Cleanup(func() { _ = os.Chmod(root, 0700) })
			}
			ctx := t.Context()
			var output, diagnostic bytes.Buffer
			input := strings.NewReader("do not read piped confirmation")
			assertLock := func() {
				lock, err := operationlock.Acquire(ctx, home, os.Geteuid(), os.Getegid())
				if err == nil {
					_ = lock.Close()
					t.Fatal("workflow lacks operation lock")
				}
				if !strings.Contains(err.Error(), "another mirror operation") {
					t.Fatal(err)
				}
			}
			calls, confirmations := 0, 0
			confirmationErr := errors.New("confirmation declined")
			h := commandHandlers(input, &output, &diagnostic, copyDependencies{
				snapshot: func(got context.Context, gotHome string) (mountdisks.Snapshot, error) {
					if got != ctx || gotHome != home {
						t.Fatal("lost caller context/home")
					}
					assertLock()
					calls++
					return mountdisks.Snapshot{Roots: roots, ConfiguredMirrors: 2}, nil
				},
				confirm: func(got context.Context, writer io.Writer, message string) error {
					if got != ctx || writer != &output {
						t.Fatal("lost confirmation context/output")
					}
					if !strings.Contains(message, "unresolved files will be left untouched") {
						t.Fatalf("unsafe prompt: %q", message)
					}
					assertLock()
					confirmations++
					if _, err := fmt.Fprintln(writer, message); err != nil {
						return err
					}
					return confirmationErr
				},
			})
			exec := func(args ...string) int {
				return execute(ctx, args, input, &output, &diagnostic, platform{}, h)
			}
			if code := exec(command, "--preview"); code != 0 {
				t.Fatalf("preview code=%d diagnostic=%q", code, &diagnostic)
			}
			if calls == 0 || confirmations != 0 || !strings.Contains(output.String(), "preview") {
				t.Fatalf("calls=%d confirmations=%d output=%q", calls, confirmations, &output)
			}
			for _, root := range roots {
				if _, err := os.Stat(filepath.Join(root, "item.b2sum")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("preview published: %v", err)
				}
				info, err := os.Stat(root)
				if err != nil || info.Mode().Perm() != 0700 {
					t.Fatalf("preview changed root: %v %v", info, err)
				}
			}
			for _, root := range roots {
				if err := os.Chmod(root, 0544); err != nil {
					t.Fatal(err)
				}
			}
			output.Reset()
			if command == "repair-copies" {
				if code := exec(command, "--resume"); code != 1 || !strings.Contains(diagnostic.String(), confirmationErr.Error()) {
					t.Fatalf("decline code=%d diagnostic=%q", code, &diagnostic)
				}
				for _, root := range roots {
					if _, err := os.Stat(filepath.Join(root, "item.b2sum")); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("decline published: %v", err)
					}
				}
				if confirmations != 1 {
					t.Fatalf("confirmations=%d", confirmations)
				}
				confirmationErr = nil
				output.Reset()
				diagnostic.Reset()
				if code := exec(command, "--resume"); code != 0 {
					t.Fatalf("resume code=%d diagnostic=%q", code, &diagnostic)
				}
				if confirmations != 2 || strings.Count(output.String(), "repaired checksumfile:") != 2 || !strings.Contains(output.String(), "repaired=1 unresolved=0") || strings.Contains(output.String(), "scanning ") {
					t.Fatalf("confirmations=%d output=%q", confirmations, &output)
				}
				log, err := repaircopies.Load(filepath.Join(home, ".mirror", "repair-last.jsonl"))
				if err != nil || log.Status != repaircopies.StatusCompleted {
					t.Fatalf("log status=%s error=%v", log.Status, err)
				}
			} else {
				if code := exec(command); code != 0 {
					t.Fatalf("apply code=%d diagnostic=%q", code, &diagnostic)
				}
				if confirmations != 0 || !strings.Contains(output.String(), "published item") {
					t.Fatalf("confirmations=%d output=%q", confirmations, &output)
				}
			}
			for _, root := range roots {
				data, err := os.ReadFile(filepath.Join(root, "item"))
				if err != nil || string(data) != "data" {
					t.Fatalf("data=%q error=%v", data, err)
				}
				if _, err := os.Stat(filepath.Join(root, "item.b2sum")); err != nil {
					t.Fatal(err)
				}
				info, err := os.Stat(root)
				if err != nil || info.Mode().Perm() != 0544 {
					t.Fatalf("not relocked: %v %v", info, err)
				}
			}
			if input.Len() != len("do not read piped confirmation") {
				t.Fatal("consumed piped input instead of explicit terminal boundary")
			}
			lock, err := operationlock.Acquire(ctx, home, os.Geteuid(), os.Getegid())
			if err != nil {
				t.Fatalf("lock leaked: %v", err)
			}
			if err := lock.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
