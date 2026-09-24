package ensurecopies

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsurePreservesSymlinkTargetsOpaquely(t *testing.T) {
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "untouched"), []byte("outside data"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ name, target string }{
		{"ordinary relative", "target"},
		{"dot relative", "./target"},
		{"unnormalized relative", "dir/../target"},
		{"parent relative", "../target"},
		{"absolute directory", outside},
		{"self loop", "link"},
	} {
		t.Run(test.name, func(t *testing.T) {
			roots := []string{t.TempDir(), t.TempDir()}
			path := "nested/link"
			if err := os.Mkdir(filepath.Join(roots[0], "nested"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(test.target, filepath.Join(roots[0], path)); err != nil {
				t.Fatal(err)
			}
			rs := testRoots(t, roots)
			actions, err := plan(context.Background(), rs)
			if err != nil {
				t.Fatal(err)
			}
			if len(actions) != 1 || actions[0].target == nil || *actions[0].target != test.target {
				t.Fatalf("did not plan exactly the opaque link: %+v", actions)
			}
			if err := apply(context.Background(), rs, actions, true, io.Discard); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Lstat(filepath.Join(roots[1], "nested")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("preview created destination parents: %v", err)
			}
			if err := apply(context.Background(), rs, actions, false, io.Discard); err != nil {
				t.Fatal(err)
			}
			target, err := os.Readlink(filepath.Join(roots[1], path))
			if err != nil || target != test.target {
				t.Fatalf("target not preserved exactly: %q %v", target, err)
			}
			entries, err := os.ReadDir(filepath.Join(roots[1], "nested"))
			if err != nil || len(entries) != 1 || entries[0].Name() != "link" {
				t.Fatalf("copied target content or created metadata: %v %v", entries, err)
			}
			actions, err = plan(context.Background(), rs)
			if err != nil || len(actions) != 0 {
				t.Fatalf("symlink replication not idempotent: %d actions, %v", len(actions), err)
			}
		})
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 1 || entries[0].Name() != "untouched" {
		t.Fatalf("symlink target directory changed: %v %v", entries, err)
	}
	data, err := os.ReadFile(filepath.Join(outside, "untouched"))
	if err != nil || string(data) != "outside data" {
		t.Fatalf("symlink target data changed: %q %v", data, err)
	}
}

func TestEnsureRejectsSymlinkTargetDisagreementAndDrift(t *testing.T) {
	for _, scenario := range []string{"disagreement", "drift"} {
		t.Run(scenario, func(t *testing.T) {
			roots := []string{t.TempDir(), t.TempDir(), t.TempDir()}
			source := filepath.Join(roots[0], "link")
			if err := os.Symlink("target", source); err != nil {
				t.Fatal(err)
			}
			if scenario == "disagreement" {
				if err := os.Symlink("./target", filepath.Join(roots[1], "link")); err != nil {
					t.Fatal(err)
				}
				if _, err := plan(context.Background(), testRoots(t, roots)); err == nil || !strings.Contains(err.Error(), "symlink disagreement") {
					t.Fatalf("distinct target strings considered equal: %v", err)
				}
			} else {
				rs := testRoots(t, roots)
				actions, err := plan(context.Background(), rs)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(source); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("./target", source); err != nil {
					t.Fatal(err)
				}
				if err := apply(context.Background(), rs, actions, false, io.Discard); err == nil || !strings.Contains(err.Error(), "source symlink changed") {
					t.Fatalf("publication accepted changed target string: %v", err)
				}
			}
			entries, err := os.ReadDir(roots[2])
			if err != nil || len(entries) != 0 {
				t.Fatalf("disagreement or drift mutated destination: %v %v", entries, err)
			}
		})
	}
}
