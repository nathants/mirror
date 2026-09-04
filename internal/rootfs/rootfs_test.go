package rootfs

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestNoFollowAndRootReplacement(t *testing.T) {
	base := t.TempDir()
	path := filepath.Join(base, "mirror")
	outside := filepath.Join(base, "outside")
	for _, dir := range []string{path, outside} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(outside, "item"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	root, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := os.Symlink(outside, filepath.Join(path, "link")); err != nil {
		t.Fatal(err)
	}
	file, err := root.OpenFile("link/item", unix.O_WRONLY|unix.O_TRUNC, 0)
	if file != nil {
		file.Close()
	}
	if err == nil {
		t.Fatal("followed ancestor symlink")
	}
	data, err := os.ReadFile(filepath.Join(outside, "item"))
	if err != nil || string(data) != "keep" {
		t.Fatal("outside data altered")
	}
	if err := os.Rename(path, path+"-old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := root.Check(); err == nil {
		t.Fatal("root replacement not detected")
	}
}
func TestInternalMarkerAndNoReplace(t *testing.T) {
	path := t.TempDir()
	root, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := root.EnsureInternal(); err != nil {
		t.Fatal(err)
	}
	first, firstPath, err := root.CreateTemp(Internal+"/tmp", "file-")
	if err != nil {
		t.Fatal(err)
	}
	first.Close()
	second, secondPath, err := root.CreateTemp(Internal+"/tmp", "file-")
	if err != nil {
		t.Fatal(err)
	}
	second.Close()
	if err := root.Rename(firstPath, secondPath, true); !errors.Is(err, os.ErrExist) {
		t.Fatalf("no-replace overwrote destination: %v", err)
	}
	markerPath := filepath.Join(path, Internal, "FORMAT")
	if err := os.WriteFile(markerPath, []byte("wrong marker"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := root.CheckInternal(); err == nil {
		t.Fatal("wrong marker accepted")
	}
}
