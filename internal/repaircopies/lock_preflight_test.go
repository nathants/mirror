package repaircopies

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
	"mirror/internal/mountdisks"
)

func TestChecksLocksBeforeReadingPayloadsOrConfirming(t *testing.T) {
	for _, entry := range []string{"fresh", "resume", "apply"} {
		for _, unlocked := range []string{".", "books"} {
			t.Run(entry+"/"+unlocked, func(t *testing.T) {
				home := t.TempDir()
				roots := []string{t.TempDir(), t.TempDir()}
				for _, root := range roots {
					if err := os.Mkdir(filepath.Join(root, "books"), 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(root, "books/item"), []byte("payload"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				lockTestRoots(t, roots)
				options := Options{
					Snapshot: func(context.Context) (mountdisks.Snapshot, error) {
						return mountdisks.Snapshot{Roots: roots, ConfiguredMirrors: len(roots)}, nil
					},
					Confirm: func(context.Context) error {
						t.Error("confirmation before rejecting unlocked inputs")
						return nil
					},
					Output: io.Discard,
				}
				if err := Run(context.Background(), home, true, false, options); err != nil {
					t.Fatal(err)
				}
				logPath := filepath.Join(home, ".mirror", repairLogName)
				previousLog, err := os.ReadFile(logPath)
				if err != nil {
					t.Fatal(err)
				}
				writable := filepath.Join(roots[1], unlocked)
				if err := os.Chmod(writable, 0744); err != nil {
					t.Fatal(err)
				}
				fd, err := unix.InotifyInit1(unix.IN_NONBLOCK | unix.IN_CLOEXEC)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = unix.Close(fd) }()
				for _, root := range roots {
					if _, err := unix.InotifyAddWatch(fd, filepath.Join(root, "books/item"), unix.IN_OPEN); err != nil {
						t.Fatal(err)
					}
				}
				if entry == "apply" {
					log, loadErr := Load(logPath)
					if loadErr != nil {
						t.Fatal(loadErr)
					}
					err = Apply(context.Background(), log, testRoots(t, roots), false, io.Discard)
				} else {
					err = Run(context.Background(), home, false, entry == "resume", options)
				}
				if err == nil || !strings.Contains(err.Error(), "run mirror lock") {
					t.Errorf("unlocked input not rejected: %v", err)
				}
				if n, err := unix.Read(fd, make([]byte, 4096)); !errors.Is(err, unix.EAGAIN) {
					t.Errorf("payload opened before lock rejection: event bytes=%d error=%v", n, err)
				}
				if entry == "fresh" {
					currentLog, err := os.ReadFile(logPath)
					if err != nil || string(currentLog) != string(previousLog) {
						t.Fatalf("failed preflight replaced the previous repair evidence: %v", err)
					}
				}
				info, err := os.Stat(writable)
				if err != nil || info.Mode().Perm() != 0744 {
					t.Fatalf("preflight changed writable input: %v %v", info, err)
				}
				for _, root := range roots {
					if _, err := os.Stat(filepath.Join(root, "books/item.b2sum")); !os.IsNotExist(err) {
						t.Fatalf("preflight published metadata: %v", err)
					}
				}
			})
		}
	}
}

func TestCleanRunLockPrerequisite(t *testing.T) {
	for _, count := range []int{1, 2} {
		for _, preview := range []bool{false, true} {
			t.Run(fmt.Sprintf("mirrors=%d/preview=%t", count, preview), func(t *testing.T) {
				var roots []string
				for range count {
					root := t.TempDir()
					roots = append(roots, root)
					if err := os.Chmod(root, 0700); err != nil {
						t.Fatal(err)
					}
					writeMirrorFile(t, filepath.Join(root, "item"), []byte("payload"), checksumBytes([]byte("payload")))
				}
				err := Run(context.Background(), t.TempDir(), preview, false, Options{
					Snapshot: func(context.Context) (mountdisks.Snapshot, error) {
						return mountdisks.Snapshot{Roots: roots, ConfiguredMirrors: count}, nil
					},
					Confirm: func(context.Context) error { t.Fatal("clean run asked for confirmation"); return nil },
					Output:  io.Discard,
				})
				if count > 1 && !preview {
					if err == nil || !strings.Contains(err.Error(), "run mirror lock") {
						t.Fatalf("unlocked no-op run accepted: %v", err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
				for _, root := range roots {
					info, err := os.Stat(root)
					if err != nil || info.Mode().Perm() != 0700 {
						t.Fatalf("no-op changed modes: %v %v", info, err)
					}
				}
			})
		}
	}
}
