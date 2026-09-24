package repaircopies

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
			home := t.TempDir()
			var roots []string
			for i := 0; i < count; i++ {
				root := t.TempDir()
				roots = append(roots, root)
				if err := os.Mkdir(filepath.Join(root, "books"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, "books/item"), []byte("payload"), 0600); err != nil {
					t.Fatal(err)
				}
				if count > 1 {
					for _, rel := range []string{".", "books"} {
						if err := os.Chmod(filepath.Join(root, rel), 0544); err != nil {
							t.Fatal(err)
						}
					}
				}
				t.Cleanup(func() {
					_ = os.Chmod(root, 0700)
					_ = os.Chmod(filepath.Join(root, "books"), 0700)
				})
			}
			publications := 0
			if err := Run(context.Background(), home, false, false, Options{
				Snapshot: func(context.Context) (mountdisks.Snapshot, error) {
					return mountdisks.Snapshot{Roots: roots, ConfiguredMirrors: count}, nil
				},
				Confirm: func(context.Context) error { return nil },
				Output: absenceOutput(func(p []byte) (int, error) {
					if strings.HasPrefix(string(p), "repaired checksumfile:") {
						publications++
						for _, root := range roots {
							mode := os.FileMode(0544)
							if strings.Contains(string(p), root+"/") {
								mode = 0744
							}
							for _, rel := range []string{".", "books"} {
								info, err := os.Stat(filepath.Join(root, rel))
								if err != nil || info.Mode().Perm() != mode {
									t.Fatalf("%s/%s: info=%v error=%v want mode=%o", root, rel, info, err, mode)
								}
							}
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
					info, err := os.Stat(filepath.Join(root, rel))
					if err != nil || info.Mode().Perm() != 0544 {
						t.Fatalf("%s/%s left unlocked: %v %v", root, rel, info, err)
					}
				}
				data, err := os.ReadFile(filepath.Join(root, "books/item.b2sum"))
				if err != nil || string(data) != checksumBytes([]byte("payload")) {
					t.Fatalf("sidecar=%q error=%v", data, err)
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

func TestDiskBatchFailureResumesOnlyRemainingWriters(t *testing.T) {
	for _, failure := range []string{"output", "cancellation", "relock"} {
		t.Run(failure, func(t *testing.T) {
			home := t.TempDir()
			roots := []string{t.TempDir(), t.TempDir()}
			for _, root := range roots {
				if err := os.WriteFile(filepath.Join(root, "item"), []byte("payload"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			lockTestRoots(t, roots)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			path := filepath.Join(home, ".mirror", repairLogName)
			marker := filepath.Join(roots[0], ".mirror-internal", "FORMAT")
			var markerBytes []byte
			publications := 0
			options := Options{
				Snapshot: func(context.Context) (mountdisks.Snapshot, error) {
					return mountdisks.Snapshot{Roots: roots, ConfiguredMirrors: 2}, nil
				},
				Confirm: func(context.Context) error { return nil },
				Output: absenceOutput(func(p []byte) (int, error) {
					if !strings.HasPrefix(string(p), "repaired checksumfile:") {
						return len(p), nil
					}
					publications++
					log, err := Load(path)
					if err != nil || log.Repairs[0].State != RepairStarted {
						t.Fatalf("missing durable start: %+v %v", log, err)
					}
					info, err := os.Stat(roots[1])
					if err != nil || info.Mode().Perm() != 0544 {
						t.Fatalf("next disk unlocked early: %v %v", info, err)
					}
					switch failure {
					case "output":
						return 0, fmt.Errorf("injected output failure")
					case "cancellation":
						cancel()
					case "relock":
						markerBytes, err = os.ReadFile(marker)
						if err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(marker, []byte("changed"), 0600); err != nil {
							t.Fatal(err)
						}
					}
					return len(p), nil
				}),
			}
			if err := Run(ctx, home, false, false, options); err == nil {
				t.Fatal("failure reported success")
			}
			if publications != 1 {
				t.Fatalf("continued to next writer: %d publications", publications)
			}
			if _, err := os.Stat(filepath.Join(roots[1], "item.b2sum")); !os.IsNotExist(err) {
				t.Fatalf("next disk was written: %v", err)
			}
			for i, root := range roots {
				if failure == "relock" && i == 0 {
					continue
				}
				info, err := os.Stat(root)
				if err != nil || info.Mode().Perm() != 0544 {
					t.Fatalf("unlocked after failure: %v %v", info, err)
				}
			}
			log, err := Load(path)
			if err != nil || log.Status != StatusPending || log.Repairs[0].State == RepairCompleted || log.Error == "" {
				t.Fatalf("partial batch lost from journal: %+v %v", log, err)
			}
			if failure == "relock" {
				// Manual resolution is required before resuming uncertain cleanup.
				if err := os.WriteFile(marker, markerBytes, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Chmod(roots[0], 0500); err != nil {
				t.Fatal(err)
			}
			completedSidecar := filepath.Join(roots[0], "item.b2sum")
			before, err := os.Stat(completedSidecar)
			if err != nil {
				t.Fatal(err)
			}
			options.Output = absenceOutput(func(p []byte) (int, error) {
				if strings.HasPrefix(string(p), "repaired checksumfile:") {
					if strings.Contains(string(p), roots[0]+"/") {
						t.Fatal("resume repeated the completed disk's publication")
					}
					info, err := os.Stat(roots[0])
					if err != nil || info.Mode().Perm() != 0500 {
						t.Fatalf("resume unlocked a now-read-only source: %v %v", info, err)
					}
				}
				return len(p), nil
			})
			if err := Run(context.Background(), home, false, true, options); err != nil {
				t.Fatal(err)
			}
			after, err := os.Stat(completedSidecar)
			if err != nil || !os.SameFile(before, after) {
				t.Fatalf("completed sidecar replaced: %v", err)
			}
			info, err := os.Stat(roots[0])
			if err != nil || info.Mode().Perm() != 0500 {
				t.Fatalf("source mode changed on resume: %v %v", info, err)
			}
			log, err = Load(path)
			if err != nil || log.Status != StatusCompleted || log.Repairs[0].State != RepairCompleted {
				t.Fatalf("resume did not finish: %+v %v", log, err)
			}
		})
	}
}

func TestRepairBatchesDataDestinationsAndPreservesHealthyReader(t *testing.T) {
	home := t.TempDir()
	roots := []string{t.TempDir(), t.TempDir(), t.TempDir()}
	good := checksumBytes([]byte("good"))
	for i, root := range roots {
		if err := os.Mkdir(filepath.Join(root, "books"), 0700); err != nil {
			t.Fatal(err)
		}
		data := "good"
		if i > 0 {
			data = fmt.Sprintf("damaged-%d", i)
		}
		for _, name := range []string{"a", "b"} {
			writeMirrorFile(t, filepath.Join(root, "books", name), []byte(data), good)
		}
	}
	lockTestRoots(t, roots)
	for _, rel := range []string{".", "books"} {
		if err := os.Chmod(filepath.Join(roots[0], rel), 0500); err != nil {
			t.Fatal(err)
		}
	}
	publications := 0
	if err := Run(context.Background(), home, false, false, Options{
		Snapshot: func(context.Context) (mountdisks.Snapshot, error) {
			return mountdisks.Snapshot{Roots: roots, ConfiguredMirrors: 3}, nil
		},
		Confirm: func(context.Context) error { return nil },
		Output: absenceOutput(func(p []byte) (int, error) {
			if strings.HasPrefix(string(p), "repaired file:") {
				active := 1 + publications/2
				if active >= len(roots) || !strings.Contains(string(p), roots[active]+"/") {
					t.Fatalf("not batched by disk: %q", p)
				}
				for i, root := range roots {
					mode := os.FileMode(0544)
					if i == 0 {
						mode = 0500
					} else if i == active {
						mode = 0744
					}
					for _, rel := range []string{".", "books"} {
						info, err := os.Stat(filepath.Join(root, rel))
						if err != nil || info.Mode().Perm() != mode {
							t.Fatalf("%s/%s: %v %v want mode %o", root, rel, info, err, mode)
						}
					}
				}
				publications++
			}
			return len(p), nil
		}),
	}); err != nil {
		t.Fatal(err)
	}
	if publications != 4 {
		t.Fatalf("publications=%d", publications)
	}
	for i, root := range roots {
		for _, name := range []string{"a", "b"} {
			path := filepath.Join("books", name)
			data, err := os.ReadFile(filepath.Join(root, path))
			if err != nil || string(data) != "good" {
				t.Fatalf("unrepaired payload: %q %v", data, err)
			}
			if i > 0 {
				data, err = os.ReadFile(quarantined(t, root, path))
				if err != nil || string(data) != fmt.Sprintf("damaged-%d", i) {
					t.Fatalf("lost evidence: %q %v", data, err)
				}
			}
		}
		want := os.FileMode(0544)
		if i == 0 {
			want = 0500
		}
		info, err := os.Stat(root)
		if err != nil || info.Mode().Perm() != want {
			t.Fatalf("post-repair mode: %v %v", info, err)
		}
	}
	if _, err := os.Stat(filepath.Join(roots[0], ".mirror-internal")); !os.IsNotExist(err) {
		t.Fatalf("created internal state on healthy reader: %v", err)
	}
}
