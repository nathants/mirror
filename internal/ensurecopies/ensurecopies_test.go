package ensurecopies

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"mirror/internal/rootfs"
)

func TestEnsurePublishesEachDestinationOnceAndSkipsInternalState(t *testing.T) {
	paths := []string{t.TempDir(), t.TempDir(), t.TempDir()}
	for _, path := range paths[:2] {
		if err := os.WriteFile(filepath.Join(path, "item"), []byte("same"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("../target", filepath.Join(path, "link")); err != nil {
			t.Fatal(err)
		}
	}
	root, err := rootfs.Open(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := root.EnsureInternal(); err != nil {
		t.Fatal(err)
	}
	file, _, err := root.CreateTemp(rootfs.Internal+"/tmp", "interrupted-")
	if err != nil {
		t.Fatal(err)
	}
	file.WriteString("partial")
	file.Close()
	root.Close()
	rs := testRoots(t, paths)
	actions, err := plan(context.Background(), rs)
	if err != nil {
		t.Fatal(err)
	}
	if err := apply(context.Background(), rs, actions, true, io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		if _, err := os.Stat(filepath.Join(path, "item.b2sum")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("preview ingested metadata")
		}
	}
	if err := apply(context.Background(), rs, actions, false, io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		data, err := os.ReadFile(filepath.Join(path, "item"))
		if err != nil || string(data) != "same" {
			t.Fatalf("wrong copied data: %q %v", data, err)
		}
		target, err := os.Readlink(filepath.Join(path, "link"))
		if err != nil || target != "../target" {
			t.Fatal("link not copied")
		}
	}
	actions, err = plan(context.Background(), rs)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 0 {
		t.Fatalf("ensure not idempotent or internal state replicated: %+v", actions)
	}
}
func TestEnsureConflictsFailBeforePublication(t *testing.T) {
	paths := []string{t.TempDir(), t.TempDir(), t.TempDir()}
	for i, path := range paths[:2] {
		if err := os.WriteFile(filepath.Join(path, "item"), []byte{byte(i)}, 0644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := plan(context.Background(), testRoots(t, paths)); err == nil {
		t.Fatal("disagreeing sources accepted")
	}
	if _, err := os.Stat(filepath.Join(paths[2], "item")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("conflict published data")
	}
}

func testRoots(t *testing.T, paths []string) *rootfs.Set {
	t.Helper()
	roots, err := rootfs.OpenSet(context.Background(), paths)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := roots.Close(); err != nil {
			t.Error(err)
		}
	})
	return roots
}
