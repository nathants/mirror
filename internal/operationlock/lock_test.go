package operationlock

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLockIsExclusiveAndReleasedOnClose(t *testing.T) {
	home := t.TempDir()
	first, err := Acquire(context.Background(), home, os.Getuid(), os.Getgid())
	if err != nil {
		t.Fatal(err)
	}
	second, err := Acquire(context.Background(), home, os.Getuid(), os.Getgid())
	if second != nil {
		second.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "another mirror operation") {
		t.Fatalf("concurrent lock: %v", err)
	}
	info, err := first.Stat()
	if err != nil {
		t.Fatal(err)
	}
	first.Close()
	next, err := Acquire(context.Background(), home, os.Getuid(), os.Getgid())
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	after, err := next.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(info, after) {
		t.Fatal("lock inode replaced")
	}
}
func TestLockRejectsSymlinksAndHardlinks(t *testing.T) {
	for _, hardlink := range []bool{false, true} {
		t.Run(map[bool]string{false: "symlink", true: "hardlink"}[hardlink], func(t *testing.T) {
			home := t.TempDir()
			if err := os.Mkdir(filepath.Join(home, ".mirror"), 0700); err != nil {
				t.Fatal(err)
			}
			outside := filepath.Join(t.TempDir(), "outside")
			if err := os.WriteFile(outside, []byte("keep"), 0600); err != nil {
				t.Fatal(err)
			}
			var err error
			if hardlink {
				err = os.Link(outside, filepath.Join(home, lockPath))
			} else {
				err = os.Symlink(outside, filepath.Join(home, lockPath))
			}
			if err != nil {
				t.Fatal(err)
			}
			lock, err := Acquire(context.Background(), home, os.Getuid(), os.Getgid())
			if lock != nil {
				lock.Close()
			}
			if err == nil {
				t.Fatal("unsafe lock accepted")
			}
			data, err := os.ReadFile(outside)
			if err != nil || string(data) != "keep" {
				t.Fatal("outside lock target modified")
			}
		})
	}
}
