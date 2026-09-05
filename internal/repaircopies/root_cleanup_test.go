package repaircopies

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"mirror/internal/mountdisks"
)

func TestRepairCleanupRelocksOriginalRootNotReplacement(t *testing.T) {
	home, base := t.TempDir(), t.TempDir()
	roots := []string{filepath.Join(base, "a"), filepath.Join(base, "b")}
	original := roots[1] + "-original"
	for _, root := range roots {
		if err := os.MkdirAll(filepath.Join(root, "books"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		for _, root := range append(append([]string(nil), roots...), original) {
			_ = os.Chmod(root, 0700)
			_ = os.Chmod(filepath.Join(root, "books"), 0700)
		}
	})
	good := checksumBytes([]byte("good"))
	writeMirrorFile(t, filepath.Join(roots[0], "books/item"), []byte("good"), good)
	writeMirrorFile(t, filepath.Join(roots[1], "books/item"), []byte("bad"), good)
	replaced := false
	options := Options{
		Output:  io.Discard,
		Confirm: func(context.Context) error { return nil },
		Snapshot: func(context.Context) (mountdisks.Snapshot, error) {
			info, err := os.Stat(roots[1])
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() == 0744 && !replaced {
				// Simulate the mount path exposing unrelated directories after
				// the original mirror was unlocked, without touching any mount.
				if err := os.Rename(roots[1], original); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Join(roots[1], "books"), 0700); err != nil {
					t.Fatal(err)
				}
				replaced = true
				return mountdisks.Snapshot{}, errors.New("mount disappeared")
			}
			return mountdisks.Snapshot{Roots: roots, ConfiguredMirrors: 2}, nil
		},
	}
	if err := Run(context.Background(), home, false, false, options); err == nil {
		t.Fatal("expected root-loss error")
	}
	if !replaced {
		t.Fatal("did not exercise cleanup after unlock")
	}
	for _, check := range []struct {
		root string
		mode os.FileMode
	}{{roots[0], 0544}, {original, 0544}, {roots[1], 0700}} {
		for _, rel := range []string{".", "books"} {
			path := filepath.Join(check.root, rel)
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != check.mode {
				t.Errorf("cleanup mode at %s = %o, want %o", path, info.Mode().Perm(), check.mode)
			}
		}
	}
	if data, err := os.ReadFile(filepath.Join(original, "books/item")); err != nil || string(data) != "bad" {
		t.Fatalf("root-loss rejection changed data: %q %v", data, err)
	}
	log, err := Load(filepath.Join(home, ".mirror", repairLogName))
	if err != nil || log.Status != StatusPending {
		t.Fatalf("root-loss rejection lost pending log: %s %v", log.Status, err)
	}
}
