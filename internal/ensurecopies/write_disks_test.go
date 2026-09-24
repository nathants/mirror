package ensurecopies

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mirror/internal/mountdisks"
)

func TestRunWritesOneDiskAtATime(t *testing.T) {
	for _, count := range []int{1, 2, 3} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			var roots []string
			for i := 0; i < count; i++ {
				root := t.TempDir()
				roots = append(roots, root)
				if err := os.Mkdir(filepath.Join(root, "books"), 0700); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					_ = os.Chmod(root, 0700)
					_ = os.Chmod(filepath.Join(root, "books"), 0700)
				})
			}
			if err := os.WriteFile(filepath.Join(roots[0], "books/item"), []byte("payload"), 0600); err != nil {
				t.Fatal(err)
			}
			if count > 1 {
				for _, root := range roots {
					for _, rel := range []string{".", "books"} {
						if err := os.Chmod(filepath.Join(root, rel), 0544); err != nil {
							t.Fatal(err)
						}
					}
				}
			}
			publications := 0
			if err := Run(context.Background(), false, Options{
				Snapshot: func(context.Context) (mountdisks.Snapshot, error) {
					return mountdisks.Snapshot{Roots: roots, ConfiguredMirrors: count}, nil
				},
				Output: inspectingOutput(func(p []byte) (int, error) {
					publications++
					for _, root := range roots {
						mode := os.FileMode(0544)
						if strings.Contains(string(p), " on "+root+" ") {
							mode = 0744
						}
						for _, rel := range []string{".", "books"} {
							requireMode(t, filepath.Join(root, rel), mode)
						}
					}
					return len(p), nil
				}),
			}); err != nil {
				t.Fatal(err)
			}
			if publications != count {
				t.Fatalf("publications=%d want=%d", publications, count)
			}
			for _, root := range roots {
				for _, rel := range []string{".", "books"} {
					requireMode(t, filepath.Join(root, rel), 0544)
				}
				data, err := os.ReadFile(filepath.Join(root, "books/item"))
				if err != nil || string(data) != "payload" {
					t.Fatalf("payload=%q error=%v", data, err)
				}
				if _, err := os.Stat(filepath.Join(root, "books/item.b2sum")); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func lockTestRoots(t *testing.T, roots []string) {
	t.Helper()
	for _, root := range roots {
		chmod := func(mode os.FileMode) error {
			return filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if path == filepath.Join(root, ".mirror-internal") || path == filepath.Join(root, "lost+found") {
					return filepath.SkipDir
				}
				if entry.IsDir() {
					return os.Chmod(path, mode)
				}
				return nil
			})
		}
		if err := chmod(0544); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = chmod(0700) })
	}
}

func TestRunDoesNotUnlockNextDiskAfterRelockFailure(t *testing.T) {
	roots := workflowRoots(t)
	lockTestRoots(t, roots)
	publications := 0
	err := Run(context.Background(), false, Options{
		Snapshot: func(context.Context) (mountdisks.Snapshot, error) {
			return mountdisks.Snapshot{Roots: roots, ConfiguredMirrors: 2}, nil
		},
		Output: inspectingOutput(func(p []byte) (int, error) {
			publications++
			// Invalid private-state observations make relocking uncertain.
			// A successful publication must not mask this cleanup failure.
			if err := os.WriteFile(filepath.Join(roots[0], ".mirror-internal", "FORMAT"), []byte("changed"), 0600); err != nil {
				t.Fatal(err)
			}
			return len(p), nil
		}),
	})
	if err == nil || !strings.Contains(err.Error(), "reserved directory collision") {
		t.Fatalf("relock failure not reported: %v", err)
	}
	if publications != 1 {
		t.Fatalf("continued after cleanup failure: %d", publications)
	}
	for _, rel := range []string{".", "books"} {
		requireMode(t, filepath.Join(roots[1], rel), 0544)
	}
	requireAbsent(t, filepath.Join(roots[1], "books/item"))
	requireAbsent(t, filepath.Join(roots[1], "link"))
}
