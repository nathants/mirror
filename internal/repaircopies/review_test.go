package repaircopies

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReviewResumeRejectsSymlinkAncestor(t *testing.T) {
	base := t.TempDir()
	a, b := filepath.Join(base, "a"), filepath.Join(base, "b")
	for _, root := range []string{a, b} {
		if err := os.MkdirAll(filepath.Join(root, "books"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	good, bad := checksumBytes([]byte("good")), checksumBytes([]byte("bad"))
	writeMirrorFile(t, filepath.Join(a, "books/item"), []byte("good"), good)
	writeMirrorFile(t, filepath.Join(b, "books/item"), []byte("bad"), good)
	now := time.Now().UTC()
	log, err := BuildLog(now, now, []string{a, b}, 2, map[string][]Copy{
		"books/item": {{Root: a, ActualChecksum: good, Sidecar: recorded(good)}, {Root: b, ActualChecksum: bad, Sidecar: recorded(good)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(base, "outside")
	if err := os.Rename(filepath.Join(b, "books"), outside); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(b, "books")); err != nil {
		t.Fatal(err)
	}
	err = Apply(context.Background(), log, testRoots(t, []string{a, b}), false, io.Discard)
	got, readErr := os.ReadFile(filepath.Join(outside, "item"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if err == nil || string(got) != "bad" {
		t.Fatalf("resume followed an ancestor symlink outside root: Apply error=%v, outside contents=%q (want rejection and untouched bad bytes)", err, got)
	}
}

func TestReviewRepairMalformedSidecarWithTwoGoodReplicas(t *testing.T) {
	good := checksumBytes([]byte("good"))
	now := time.Now().UTC()
	log, err := BuildLog(now, now, []string{"/mirror/a", "/mirror/b", "/mirror/c"}, 3, map[string][]Copy{
		"item": {
			{Root: "/mirror/a", ActualChecksum: good, Sidecar: recorded(good)},
			{Root: "/mirror/b", ActualChecksum: good, Sidecar: recorded(good)},
			{Root: "/mirror/c", ActualChecksum: good, Sidecar: recorded("corrupt sidecar bytes")},
		},
	})
	if err != nil || len(log.Repairs) != 1 {
		t.Fatalf("three agreeing data copies plus two good sidecars cannot repair one malformed sidecar: err=%v, repairs=%d", err, len(log.Repairs))
	}
}

// The output writer simulates an outside edit after a replacement has completed.
type restoreScannedBytes struct {
	path string
	done bool
}

func (w *restoreScannedBytes) Write(p []byte) (int, error) {
	if !w.done && strings.Contains(string(p), "repaired file:") {
		w.done = true
		if err := os.Chmod(w.path, 0644); err != nil {
			return 0, err
		}
		if err := os.WriteFile(w.path, []byte("bad"), 0444); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}
func TestReviewCompletionRequiresDesiredBytes(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	good, bad := checksumBytes([]byte("good")), checksumBytes([]byte("bad"))
	writeMirrorFile(t, filepath.Join(a, "item"), []byte("good"), good)
	writeMirrorFile(t, filepath.Join(b, "item"), []byte("bad"), good)
	now := time.Now().UTC()
	log, err := BuildLog(now, now, []string{a, b}, 2, map[string][]Copy{"item": {
		{Root: a, ActualChecksum: good, Sidecar: recorded(good)},
		{Root: b, ActualChecksum: bad, Sidecar: recorded(good)},
	}})
	if err != nil {
		t.Fatal(err)
	}
	writer := &restoreScannedBytes{path: filepath.Join(b, "item")}
	if err := Apply(context.Background(), log, testRoots(t, []string{a, b}), false, writer); err == nil {
		t.Fatal("Apply reported completion with original corrupt bytes restored")
	}
}
