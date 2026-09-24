package ensurecopies

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mirror/internal/mountdisks"
)

func TestRunStopsPublicationOnRootReplacement(t *testing.T) {
	for _, kind := range []string{"data", "metadata", "link"} {
		for _, replacedIndex := range []int{0, 1} {
			name := kind + "/" + []string{"source", "destination"}[replacedIndex]
			t.Run(name, func(t *testing.T) {
				base := t.TempDir()
				roots := []string{filepath.Join(base, "source"), filepath.Join(base, "destination")}
				original := roots[replacedIndex] + "-original"
				for _, root := range roots {
					if err := os.Mkdir(root, 0700); err != nil {
						t.Fatal(err)
					}
				}
				t.Cleanup(func() {
					for _, root := range append(roots, original) {
						_ = os.Chmod(root, 0700)
					}
				})
				for _, path := range []string{"a", "b"} {
					if kind == "link" {
						if err := os.Symlink("../opaque-target", filepath.Join(roots[0], path)); err != nil {
							t.Fatal(err)
						}
					} else {
						for i, root := range roots {
							if i == 1 && kind == "data" {
								continue
							}
							if err := os.WriteFile(filepath.Join(root, path), []byte("payload"), 0600); err != nil {
								t.Fatal(err)
							}
						}
					}
				}
				if kind == "data" {
					ingestSource(t, roots[0])
				}
				lockTestRoots(t, roots)
				if kind == "link" {
					if err := os.Chmod(roots[0], 0500); err != nil {
						t.Fatal(err)
					}
				}
				publications := 0
				err := Run(context.Background(), false, Options{
					Snapshot: func(context.Context) (mountdisks.Snapshot, error) {
						return mountdisks.Snapshot{Roots: roots, ConfiguredMirrors: 2}, nil
					},
					Output: inspectingOutput(func(p []byte) (int, error) {
						publications++
						if publications == 1 {
							if err := os.Rename(roots[replacedIndex], original); err != nil {
								t.Fatal(err)
							}
							// Use matching payloads/targets so a late reopen cannot
							// accidentally catch drift through content validation.
							if err := os.Mkdir(roots[replacedIndex], 0700); err != nil {
								t.Fatal(err)
							}
							if replacedIndex == 0 || kind == "metadata" {
								for _, path := range []string{"a", "b"} {
									var err error
									if kind == "link" {
										err = os.Symlink("../opaque-target", filepath.Join(roots[replacedIndex], path))
									} else {
										err = os.WriteFile(filepath.Join(roots[replacedIndex], path), []byte("payload"), 0600)
									}
									if err != nil {
										t.Fatal(err)
									}
								}
							}
						}
						return len(p), nil
					}),
				})
				if err == nil || !strings.Contains(err.Error(), "mirror root changed") {
					t.Fatalf("root replacement not rejected: %v", err)
				}
				if publications != 1 {
					t.Errorf("continued publishing after root replacement: %d actions", publications)
				}
				requireMode(t, roots[replacedIndex], 0700)
				wantOriginalMode := os.FileMode(0544)
				if kind == "link" && replacedIndex == 0 {
					wantOriginalMode = 0500 // source-only directories are never chmodded
				}
				requireMode(t, original, wantOriginalMode)
				requireAbsent(t, filepath.Join(roots[replacedIndex], "b.b2sum"))
				if kind != "metadata" {
					requireAbsent(t, filepath.Join(roots[1], "b"))
				}
				if kind == "metadata" {
					requireAbsent(t, filepath.Join(roots[0], "b.b2sum"))
					requireAbsent(t, filepath.Join(roots[1], "b.b2sum"))
				}
			})
		}
	}
}

func TestRunRetainsRootsFromPlanning(t *testing.T) {
	roots := workflowRoots(t)
	ingestSource(t, roots[0])
	lockTestRoots(t, roots)
	original := roots[1] + "-original"
	defer func() {
		_ = os.RemoveAll(roots[1])
		_ = os.Rename(original, roots[1])
	}()
	calls := 0
	err := Run(context.Background(), false, Options{
		Output: io.Discard,
		Snapshot: func(context.Context) (mountdisks.Snapshot, error) {
			calls++
			if calls == 2 {
				if err := os.Rename(roots[1], original); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(roots[1], 0700); err != nil {
					t.Fatal(err)
				}
			}
			return mountdisks.Snapshot{Roots: roots, ConfiguredMirrors: 2}, nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "mirror root changed") {
		t.Fatalf("planning root replacement not rejected: %v", err)
	}
	requireAbsent(t, filepath.Join(roots[1], "books"))
	requireMode(t, roots[1], 0700)
}
