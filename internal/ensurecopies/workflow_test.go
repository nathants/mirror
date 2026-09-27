package ensurecopies

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mirror/internal/mountdisks"
)

func workflowRoots(t *testing.T) []string {
	t.Helper()
	roots := []string{t.TempDir(), t.TempDir()}
	for _, root := range roots {
		if err := os.Chmod(root, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(root, "books"), 0755); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = os.Chmod(root, 0700)
			_ = os.Chmod(filepath.Join(root, "books"), 0700)
		})
	}
	if err := os.WriteFile(filepath.Join(roots[0], "books/item"), []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../missing", filepath.Join(roots[0], "link")); err != nil {
		t.Fatal(err)
	}
	return roots
}

func requireMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != want {
		t.Fatalf("%s mode=%o want=%o", path, info.Mode().Perm(), want)
	}
}

func requireAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s not absent: %v", path, err)
	}
}

func TestRunPreviewPublicationAndIdempotence(t *testing.T) {
	roots := workflowRoots(t)
	ctx := t.Context()
	var output bytes.Buffer
	calls := 0
	options := Options{Output: &output, Snapshot: func(got context.Context) (mountdisks.Snapshot, error) {
		if got != ctx {
			t.Fatal("lost caller context")
		}
		calls++
		return mountdisks.Snapshot{Roots: roots, ConfiguredMirrors: 2}, nil
	}}
	if err := Run(ctx, true, options); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || strings.Count(output.String(), "preview: publish ") != 3 {
		t.Fatalf("calls=%d output=%q", calls, &output)
	}
	for _, root := range roots {
		requireMode(t, root, 0700)
		requireMode(t, filepath.Join(root, "books"), 0755)
		requireAbsent(t, filepath.Join(root, "books/item.b2sum"))
		requireAbsent(t, filepath.Join(root, ".mirror-internal"))
	}
	requireAbsent(t, filepath.Join(roots[1], "books/item"))
	requireAbsent(t, filepath.Join(roots[1], "link"))
	lockTestRoots(t, roots)
	output.Reset()
	calls = 0
	if err := Run(ctx, false, options); err != nil {
		t.Fatal(err)
	}
	if calls != 5 || strings.Count(output.String(), "published ") != 3 {
		t.Fatalf("calls=%d output=%q", calls, &output)
	}
	for _, root := range roots {
		data, err := os.ReadFile(filepath.Join(root, "books/item"))
		if err != nil || string(data) != "data" {
			t.Fatalf("data=%q error=%v", data, err)
		}
		target, err := os.Readlink(filepath.Join(root, "link"))
		if err != nil || target != "../missing" {
			t.Fatalf("target=%q error=%v", target, err)
		}
		requireMode(t, root, 0544)
		requireMode(t, filepath.Join(root, "books"), 0544)
	}
	// A fresh real scan verifies both published data/sidecar pairs.
	output.Reset()
	calls = 0
	if err := Run(ctx, false, options); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || output.Len() != 0 {
		t.Fatalf("not idempotent: calls=%d output=%q", calls, &output)
	}
}

func TestRunStopsAndRelocksAfterOutputFailureOrCancellation(t *testing.T) {
	for _, cancelRun := range []bool{false, true} {
		t.Run(map[bool]string{false: "output", true: "cancellation"}[cancelRun], func(t *testing.T) {
			roots := workflowRoots(t)
			lockTestRoots(t, roots)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			want := errors.New("output unavailable")
			var writer io.Writer = failedOutput{want}
			if cancelRun {
				writer = cancelOutput{cancel}
				want = context.Canceled
			}
			err := Run(ctx, false, Options{Output: writer, Snapshot: func(context.Context) (mountdisks.Snapshot, error) {
				return mountdisks.Snapshot{Roots: roots, ConfiguredMirrors: 2}, nil
			}})
			if !errors.Is(err, want) {
				t.Fatalf("lost failure: %v", err)
			}
			if _, err := os.Stat(filepath.Join(roots[0], "books/item.b2sum")); err != nil {
				t.Fatalf("completed action undone: %v", err)
			}
			requireAbsent(t, filepath.Join(roots[1], "books/item"))
			requireAbsent(t, filepath.Join(roots[1], "link"))
			for _, root := range roots {
				requireMode(t, root, 0544)
				requireMode(t, filepath.Join(root, "books"), 0544)
			}
		})
	}
}

type cancelOutput struct{ cancel context.CancelFunc }

func (w cancelOutput) Write(p []byte) (int, error) { w.cancel(); return len(p), nil }

func TestRunRevalidatesBeforeUnlockOrPublication(t *testing.T) {
	for _, failure := range []string{"initial inventory", "revalidation inventory", "root drift", "configured count drift"} {
		t.Run(failure, func(t *testing.T) {
			roots := workflowRoots(t)
			lockTestRoots(t, roots)
			inventoryErr := errors.New("inventory failed")
			calls := 0
			err := Run(context.Background(), false, Options{Output: io.Discard, Snapshot: func(context.Context) (mountdisks.Snapshot, error) {
				calls++
				state := mountdisks.Snapshot{Roots: roots, ConfiguredMirrors: 2}
				if failure == "initial inventory" || (calls == 2 && failure == "revalidation inventory") {
					return state, inventoryErr
				}
				if calls == 2 {
					if failure == "root drift" {
						state.Roots = roots[:1]
					}
					if failure == "configured count drift" {
						state.ConfiguredMirrors++
					}
				}
				return state, nil
			}})
			if err == nil {
				t.Fatal("accepted invalid inventory")
			}
			if strings.Contains(failure, "inventory") && !errors.Is(err, inventoryErr) {
				t.Fatalf("lost inventory error: %v", err)
			}
			for _, root := range roots {
				requireMode(t, root, 0544)
				requireMode(t, filepath.Join(root, "books"), 0544)
				requireAbsent(t, filepath.Join(root, "books/item.b2sum"))
			}
			requireAbsent(t, filepath.Join(roots[1], "books/item"))
		})
	}
}

func TestRunRequiresDependencies(t *testing.T) {
	for _, options := range []Options{{}, {Output: io.Discard}, {Snapshot: func(context.Context) (mountdisks.Snapshot, error) {
		t.Fatal("inventory before output validation")
		return mountdisks.Snapshot{}, nil
	}}} {
		if err := Run(context.Background(), true, options); err == nil {
			t.Fatal("missing dependency accepted")
		}
	}
}
