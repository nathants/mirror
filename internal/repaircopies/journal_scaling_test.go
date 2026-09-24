package repaircopies

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"mirror/internal/mountdisks"
)

func TestCheckpointWriteVolumeIsLinear(t *testing.T) {
	measure := func(n int) int64 {
		t.Helper()
		home := t.TempDir()
		roots := []string{t.TempDir(), t.TempDir()}
		for _, root := range roots {
			for i := range n {
				if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("item-%04d", i)), []byte("x"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			t.Cleanup(func() { _ = os.Chmod(root, 0700) })
		}
		lockTestRoots(t, roots)
		var written int64
		checkpoints := 0
		path := filepath.Join(home, ".mirror", repairLogName)
		options := Options{
			Snapshot: func(context.Context) (mountdisks.Snapshot, error) {
				return mountdisks.Snapshot{Roots: roots, ConfiguredMirrors: 2}, nil
			},
			Confirm: func(context.Context) error {
				info, err := os.Stat(path)
				if err == nil {
					written += info.Size() // Immutable evidence, persisted once.
				}
				return err
			},
			Output: io.Discard,
			Append: func(j *Journal, p Progress) error {
				before, err := os.Stat(path)
				if err != nil {
					return err
				}
				if err := j.Append(p); err != nil {
					return err
				}
				after, err := os.Stat(path)
				if err != nil {
					return err
				}
				added := after.Size() - before.Size()
				if !os.SameFile(before, after) || added <= 0 || added > 256 {
					t.Fatalf("checkpoint rewrote the plan instead of appending a small record: %d bytes", added)
				}
				written += added
				checkpoints++
				return nil
			},
		}
		if err := Run(context.Background(), home, false, false, options); err != nil {
			t.Fatal(err)
		}
		if checkpoints != 2*n+1 {
			t.Fatalf("missing durable checkpoints: %d for %d repairs", checkpoints, n)
		}
		log, err := Load(path)
		if err != nil || log.Status != StatusCompleted || len(log.Repairs) != n {
			t.Fatalf("journal did not replay the completed work: %+v %v", log, err)
		}
		return written
	}
	small, large := measure(16), measure(32)
	t.Logf("scan evidence plus progress: 16 repairs=%d bytes, 32 repairs=%d bytes", small, large)
	if large > small*5/2 {
		t.Fatalf("doubling repairs more than doubled journal work: %d -> %d bytes", small, large)
	}
}
