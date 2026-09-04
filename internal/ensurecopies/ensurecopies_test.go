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
	actions, err := plan(context.Background(), paths)
	if err != nil {
		t.Fatal(err)
	}
	if err := apply(context.Background(), actions, true, io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		if _, err := os.Stat(filepath.Join(path, "item.b2sum")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("preview ingested metadata")
		}
	}
	if err := apply(context.Background(), actions, false, io.Discard); err != nil {
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
	actions, err = plan(context.Background(), paths)
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
	if _, err := plan(context.Background(), paths); err == nil {
		t.Fatal("disagreeing sources accepted")
	}
	if _, err := os.Stat(filepath.Join(paths[2], "item")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("conflict published data")
	}
}

func TestSidecarOnlyEvidenceIsNotAnEnsureDataCopy(t *testing.T) {
	paths := []string{t.TempDir(), t.TempDir()}
	if err := os.WriteFile(filepath.Join(paths[0], "item.b2sum"), []byte("surviving metadata"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(paths[1], "item"), []byte("source data"), 0600); err != nil {
		t.Fatal(err)
	}
	actions, err := plan(context.Background(), paths)
	if err != nil {
		t.Fatal(err)
	}
	var missingCopies int
	for _, action := range actions {
		if !action.metadata {
			missingCopies++
			if action.source != paths[1] || action.destination != paths[0] || action.checksum == "" {
				t.Fatalf("sidecar-only observation treated as a data copy: %+v", action)
			}
		}
	}
	if missingCopies != 1 {
		t.Fatalf("missing data incorrectly considered present: %+v", actions)
	}
	// Publication must still refuse the existing sidecar. Recovery of that
	// collision is a separate ensure policy, not a side effect of repair voting.
	if err := apply(context.Background(), actions, false, io.Discard); err == nil {
		t.Fatal("ensure overwrote surviving metadata")
	}
	data, err := os.ReadFile(filepath.Join(paths[0], "item.b2sum"))
	if err != nil || string(data) != "surviving metadata" {
		t.Fatalf("surviving metadata changed: %q %v", data, err)
	}
}
