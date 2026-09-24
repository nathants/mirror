package repaircopies

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mirror/internal/mountdisks"
)

func TestRepairKeepsPlanningRootsThroughApplication(t *testing.T) {
	for _, phase := range []string{"confirmation", "after unlock"} {
		t.Run(phase, func(t *testing.T) {
			home, base := t.TempDir(), t.TempDir()
			roots := []string{filepath.Join(base, "a"), filepath.Join(base, "b")}
			original := roots[1] + "-original"
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
			good := checksumBytes([]byte("good"))
			writeMirrorFile(t, filepath.Join(roots[0], "item"), []byte("good"), good)
			writeMirrorFile(t, filepath.Join(roots[1], "item"), []byte("bad"), good)
			replaced := false
			replace := func() {
				t.Helper()
				if err := os.Rename(roots[1], original); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(roots[1], 0700); err != nil {
					t.Fatal(err)
				}
				writeMirrorFile(t, filepath.Join(roots[1], "item"), []byte("bad"), good)
				replaced = true
			}
			err := Run(context.Background(), home, false, false, Options{
				Output: io.Discard,
				Confirm: func(context.Context) error {
					if phase == "confirmation" {
						replace()
					}
					return nil
				},
				Snapshot: func(context.Context) (mountdisks.Snapshot, error) {
					info, err := os.Stat(roots[1])
					if err != nil {
						t.Fatal(err)
					}
					if phase == "after unlock" && !replaced && info.Mode().Perm() == 0744 {
						replace()
					}
					return mountdisks.Snapshot{Roots: roots, ConfiguredMirrors: 2}, nil
				},
			})
			if !replaced || err == nil || !strings.Contains(err.Error(), "mirror root changed") {
				t.Fatalf("root drift not rejected: replaced=%v err=%v", replaced, err)
			}
			for _, root := range []string{original, roots[1]} {
				data, err := os.ReadFile(filepath.Join(root, "item"))
				if err != nil || string(data) != "bad" {
					t.Fatalf("published after root drift: %s: %q %v", root, data, err)
				}
				info, err := os.Stat(root)
				want := os.FileMode(0700)
				if root == original && phase == "after unlock" {
					want = 0544
				}
				if err != nil || info.Mode().Perm() != want {
					t.Fatalf("wrong cleanup at %s: info=%v err=%v", root, info, err)
				}
			}
		})
	}
}
