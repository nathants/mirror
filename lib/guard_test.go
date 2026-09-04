package lib

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"mirror/internal/rootfs"
)

func guardTree(t *testing.T, path string) {
	t.Helper()
	for _, rel := range []string{"a", "b/child"} {
		if err := os.MkdirAll(filepath.Join(path, rel), 0700); err != nil {
			t.Fatal(err)
		}
	}
}

func assertGuardMode(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != mode {
		t.Errorf("mode at %s = %o, want %o", path, info.Mode().Perm(), mode)
	}
}

func makeGuardTreeWritable(path string) {
	for _, rel := range []string{".", "a", "b", "b/child", "new", "new/child"} {
		_ = os.Chmod(filepath.Join(path, rel), 0700)
	}
}

func TestGuardCleanupStaysOnOriginalFilesystem(t *testing.T) {
	for _, replacement := range []string{"directory", "symlink", "absent"} {
		t.Run(replacement, func(t *testing.T) {
			base := t.TempDir()
			path, original, outside := filepath.Join(base, "mirror"), filepath.Join(base, "original"), filepath.Join(base, "outside")
			guardTree(t, path)
			guardTree(t, outside)
			t.Cleanup(func() { makeGuardTreeWritable(path); makeGuardTreeWritable(original) })
			root, err := rootfs.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := root.EnsureInternal(); err != nil {
				t.Fatal(err)
			}
			root.Close()
			guard, err := OpenDirectoryGuard(context.Background(), []string{path})
			if err != nil {
				t.Fatal(err)
			}
			defer guard.Close()
			if err := guard.SetLocked(context.Background(), false); err != nil {
				t.Fatal(err)
			}
			// Include directories published during the operation in relocking.
			if err := os.MkdirAll(filepath.Join(path, "new/child"), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(path, original); err != nil {
				t.Fatal(err)
			}
			switch replacement {
			case "directory":
				guardTree(t, path)
			case "symlink":
				if err := os.Symlink(outside, path); err != nil {
					t.Fatal(err)
				}
			}
			if err := guard.Relock(context.Background()); err == nil {
				t.Fatal("cleanup did not report root drift")
			}
			for _, rel := range []string{".", "a", "b", "b/child", "new", "new/child"} {
				assertGuardMode(t, filepath.Join(original, rel), 0544)
			}
			assertGuardMode(t, filepath.Join(original, rootfs.Internal), 0700)
			assertGuardMode(t, filepath.Join(original, rootfs.Internal, "tmp"), 0700)
			assertGuardMode(t, filepath.Join(original, rootfs.Internal, "quarantine"), 0700)
			for _, rel := range []string{".", "a", "b", "b/child"} {
				assertGuardMode(t, filepath.Join(outside, rel), 0700)
				if replacement == "directory" {
					assertGuardMode(t, filepath.Join(path, rel), 0700)
				}
			}
		})
	}
}

func TestGuardChecksAllRootsBeforeNormalMutation(t *testing.T) {
	base := t.TempDir()
	paths := []string{filepath.Join(base, "a"), filepath.Join(base, "b")}
	for _, path := range paths {
		guardTree(t, path)
	}
	guard, err := OpenDirectoryGuard(context.Background(), paths)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	if err := os.Rename(paths[1], paths[1]+"-original"); err != nil {
		t.Fatal(err)
	}
	guardTree(t, paths[1])
	for _, locked := range []bool{true, false} {
		if err := guard.SetLocked(context.Background(), locked); err == nil {
			t.Fatal("normal operation accepted root replacement")
		}
		for _, path := range []string{paths[0], paths[1], paths[1] + "-original"} {
			assertGuardMode(t, path, 0700)
			assertGuardMode(t, filepath.Join(path, "a"), 0700)
		}
	}
}

// Cancel deterministically after the first real directory chmod, not during
// setup or inventory, to exercise recovery of a partially completed unlock.
type cancelAfterDirectoryUnlock struct {
	context.Context
	cancel context.CancelFunc
	path   string
}

func (c cancelAfterDirectoryUnlock) Err() error {
	if info, err := os.Stat(c.path); err == nil && info.Mode().Perm() == 0744 {
		c.cancel()
	}
	return c.Context.Err()
}

func TestGuardRelocksPartialUnlockWithIndependentContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mirror")
	guardTree(t, path)
	t.Cleanup(func() { makeGuardTreeWritable(path) })
	guard, err := OpenDirectoryGuard(context.Background(), []string{path})
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	partial := cancelAfterDirectoryUnlock{Context: ctx, cancel: cancel, path: filepath.Join(path, "a")}
	if err := guard.SetLocked(partial, false); !errors.Is(err, context.Canceled) {
		t.Fatalf("unlock did not cancel: %v", err)
	}
	assertGuardMode(t, filepath.Join(path, "a"), 0744)
	assertGuardMode(t, path, 0700)
	if err := guard.Relock(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{".", "a", "b", "b/child"} {
		assertGuardMode(t, filepath.Join(path, rel), 0544)
	}
	if err := guard.Close(); err != nil {
		t.Fatal(err)
	}
	if err := guard.SetLocked(context.Background(), false); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("closed guard remained usable: %v", err)
	}
}

func TestGuardPreflightRejectsUnmarkedInternalState(t *testing.T) {
	paths := []string{t.TempDir(), t.TempDir()}
	for _, path := range paths {
		if err := os.Chmod(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(paths[1], rootfs.Internal), 0700); err != nil {
		t.Fatal(err)
	}
	guard, err := OpenDirectoryGuard(context.Background(), paths)
	if guard != nil {
		guard.Close()
	}
	if err == nil {
		t.Fatal("guard adopted unmarked private namespace")
	}
	for _, path := range paths {
		assertGuardMode(t, path, 0700)
	}
}
