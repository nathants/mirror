package ensurecopies

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mirror/internal/mountdisks"
)

type inspectingOutput func([]byte) (int, error)

func (w inspectingOutput) Write(p []byte) (int, error) { return w(p) }

func ingestSource(t *testing.T, root string) {
	t.Helper()
	if err := Run(context.Background(), false, Options{
		Output: io.Discard,
		Snapshot: func(context.Context) (mountdisks.Snapshot, error) {
			return mountdisks.Snapshot{Roots: []string{root}, ConfiguredMirrors: 2}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRunUnlocksOnlyWriteDestinations(t *testing.T) {
	for _, contents := range []string{"ingested", "missing sidecar", "symlink only"} {
		for _, outcome := range []string{"success", "output failure", "cancellation"} {
			t.Run(contents+"/"+outcome, func(t *testing.T) {
				roots := workflowRoots(t)
				switch contents {
				case "ingested":
					// Ingest with only the source mounted, then attach the other disk.
					ingestSource(t, roots[0])
				case "symlink only":
					if err := os.Remove(filepath.Join(roots[0], "books/item")); err != nil {
						t.Fatal(err)
					}
				}
				for i, root := range roots {
					mode := os.FileMode(0544)
					if i == 0 {
						// A distinct locked mode detects unwanted relocking too.
						mode = 0500
					}
					for _, rel := range []string{".", "books"} {
						if err := os.Chmod(filepath.Join(root, rel), mode); err != nil {
							t.Fatal(err)
						}
					}
				}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				outputErr := errors.New("output unavailable")
				writes := 0
				err := Run(ctx, false, Options{
					Snapshot: func(context.Context) (mountdisks.Snapshot, error) {
						return mountdisks.Snapshot{Roots: roots, ConfiguredMirrors: 2}, nil
					},
					Output: inspectingOutput(func(p []byte) (int, error) {
						writes++
						sourceMode := os.FileMode(0500)
						if contents == "missing sidecar" {
							sourceMode = 0744
						}
						for _, rel := range []string{".", "books"} {
							requireMode(t, filepath.Join(roots[0], rel), sourceMode)
							requireMode(t, filepath.Join(roots[1], rel), 0744)
						}
						if outcome == "output failure" {
							return 0, outputErr
						}
						if outcome == "cancellation" {
							cancel()
						}
						return len(p), nil
					}),
				})
				var want error
				switch outcome {
				case "output failure":
					want = outputErr
				case "cancellation":
					want = context.Canceled
				}
				if !errors.Is(err, want) {
					t.Fatalf("error=%v want=%v", err, want)
				}
				if writes == 0 || (want != nil && writes != 1) {
					t.Fatalf("unexpected progress writes: %d", writes)
				}
				sourceMode := os.FileMode(0500)
				if contents == "missing sidecar" {
					sourceMode = 0544
				}
				for _, rel := range []string{".", "books"} {
					requireMode(t, filepath.Join(roots[0], rel), sourceMode)
					requireMode(t, filepath.Join(roots[1], rel), 0544)
				}
				if contents == "missing sidecar" {
					if _, err := os.Stat(filepath.Join(roots[0], "books/item.b2sum")); err != nil {
						t.Fatalf("missing completed sidecar: %v", err)
					}
					if want != nil {
						requireAbsent(t, filepath.Join(roots[1], "books/item"))
					}
				}
				if contents == "ingested" || (contents == "missing sidecar" && want == nil) {
					data, err := os.ReadFile(filepath.Join(roots[1], "books/item"))
					if err != nil || string(data) != "data" {
						t.Fatalf("completed copy: data=%q error=%v", data, err)
					}
				}
				if contents == "symlink only" || want == nil {
					target, err := os.Readlink(filepath.Join(roots[1], "link"))
					if err != nil || target != "../missing" {
						t.Fatalf("copied link: target=%q error=%v", target, err)
					}
				} else {
					requireAbsent(t, filepath.Join(roots[1], "link"))
				}
			})
		}
	}
}

func TestRunRejectsReadOnlySourceIdentityReplacement(t *testing.T) {
	for _, phase := range []string{"revalidation", "after publication"} {
		t.Run(phase, func(t *testing.T) {
			roots := workflowRoots(t)
			ingestSource(t, roots[0])
			for _, rel := range []string{".", "books"} {
				if err := os.Chmod(filepath.Join(roots[0], rel), 0500); err != nil {
					t.Fatal(err)
				}
			}
			original := roots[0] + "-original"
			replaced := false
			defer func() {
				if replaced {
					if err := os.RemoveAll(roots[0]); err != nil {
						t.Error(err)
					}
					if err := os.Rename(original, roots[0]); err != nil {
						t.Error(err)
					}
				}
			}()
			replaceSource := func() {
				t.Helper()
				if err := os.Rename(roots[0], original); err != nil {
					t.Fatal(err)
				}
				replaced = true
				// Same path, bytes, and target, but a different root identity.
				if err := os.MkdirAll(filepath.Join(roots[0], "books"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(roots[0], "books/item"), []byte("data"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("../missing", filepath.Join(roots[0], "link")); err != nil {
					t.Fatal(err)
				}
			}
			calls, publications := 0, 0
			err := Run(context.Background(), false, Options{
				Snapshot: func(context.Context) (mountdisks.Snapshot, error) {
					calls++
					if calls == 2 && phase == "revalidation" {
						replaceSource()
					}
					return mountdisks.Snapshot{Roots: roots, ConfiguredMirrors: 2}, nil
				},
				Output: inspectingOutput(func(p []byte) (int, error) {
					publications++
					if phase == "after publication" && publications == 2 {
						replaceSource()
					}
					return len(p), nil
				}),
			})
			if !replaced || err == nil || !strings.Contains(err.Error(), "mirror root changed") {
				t.Fatalf("source replacement not rejected: replaced=%v error=%v", replaced, err)
			}
			for _, rel := range []string{".", "books"} {
				requireMode(t, filepath.Join(original, rel), 0500)
				requireMode(t, filepath.Join(roots[0], rel), 0700)
				requireMode(t, filepath.Join(roots[1], rel), 0544)
			}
			if phase == "revalidation" {
				if publications != 0 {
					t.Fatalf("published despite source replacement: %d", publications)
				}
				requireAbsent(t, filepath.Join(roots[1], "books/item"))
				requireAbsent(t, filepath.Join(roots[1], "link"))
			} else {
				data, err := os.ReadFile(filepath.Join(roots[1], "books/item"))
				if err != nil || string(data) != "data" {
					t.Fatalf("completed copy: data=%q error=%v", data, err)
				}
				target, err := os.Readlink(filepath.Join(roots[1], "link"))
				if err != nil || target != "../missing" {
					t.Fatalf("completed link: target=%q error=%v", target, err)
				}
			}
		})
	}
}

func TestRunRevalidatesReadOnlySourceBeforeUnlock(t *testing.T) {
	roots := workflowRoots(t)
	ingestSource(t, roots[0])
	calls := 0
	err := Run(context.Background(), false, Options{
		Output: io.Discard,
		Snapshot: func(context.Context) (mountdisks.Snapshot, error) {
			calls++
			current := roots
			if calls == 2 {
				current = roots[1:]
			}
			return mountdisks.Snapshot{Roots: current, ConfiguredMirrors: 2}, nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "mounted mirror state changed") {
		t.Fatalf("source drift not rejected: %v", err)
	}
	requireMode(t, roots[0], 0544)
	requireMode(t, roots[1], 0700)
	requireMode(t, filepath.Join(roots[1], "books"), 0755)
	requireAbsent(t, filepath.Join(roots[1], "books/item"))
	requireAbsent(t, filepath.Join(roots[1], "link"))
}
