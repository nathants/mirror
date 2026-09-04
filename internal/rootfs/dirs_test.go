package rootfs

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestPinnedDirectoryWalkSkipsPrivateStateAndSymlinks(t *testing.T) {
	path, outside := t.TempDir(), t.TempDir()
	for _, rel := range []string{"data/child", "lost+found"} {
		if err := os.MkdirAll(filepath.Join(path, rel), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(path, "link")); err != nil {
		t.Fatal(err)
	}
	root, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := root.EnsureInternal(); err != nil {
		t.Fatal(err)
	}
	var visited []string
	if err := root.WalkPinnedDirs(context.Background(), func(path string, dir *os.File) error {
		info, err := dir.Stat()
		if err != nil {
			return err
		}
		if !info.IsDir() {
			t.Fatal("visited a non-directory")
		}
		visited = append(visited, path)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"data/child", "data", "."}; !reflect.DeepEqual(visited, want) {
		t.Fatalf("walk included private state/symlinks or lost postorder: %v", visited)
	}
}

func TestPinnedDirectoryWalkUsesOpenedDirectoryForMutation(t *testing.T) {
	base := t.TempDir()
	path, original, outside := filepath.Join(base, "mirror"), filepath.Join(base, "original"), filepath.Join(base, "outside")
	for _, dir := range []string{filepath.Join(path, "a/child"), filepath.Join(path, "a/sibling"), filepath.Join(outside, "sibling")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	root, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	err = root.WalkPinnedDirs(context.Background(), func(rel string, dir *os.File) error {
		if rel == "a/child" {
			if err := os.Rename(filepath.Join(path, "a"), original); err != nil {
				return err
			}
			if err := os.Symlink(outside, filepath.Join(path, "a")); err != nil {
				return err
			}
		}
		// These descriptors refer to originally opened directories even after
		// an ancestor is redirected. No chmod may land on the symlink target.
		return dir.Chmod(0750)
	})
	if err == nil {
		t.Fatal("walk followed a redirected ancestor to the remaining sibling")
	}
	for _, rel := range []string{".", "sibling"} {
		info, err := os.Stat(filepath.Join(outside, rel))
		if err != nil || info.Mode().Perm() != 0700 {
			t.Fatalf("symlink target changed: %v", err)
		}
	}
	info, err := os.Stat(original)
	if err != nil || info.Mode().Perm() != 0750 {
		t.Fatalf("parent mutation did not use retained descriptor: %v", err)
	}
}
