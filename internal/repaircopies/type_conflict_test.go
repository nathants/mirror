package repaircopies

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"mirror/internal/mountdisks"
)

func TestTypeConflictsRemainUnresolvedWithoutBlockingSafeRepairs(t *testing.T) {
	for _, test := range []struct {
		name, dataPath, conflictPath, kind string
		healthy                            bool
		findings                           int
	}{
		{"payload directory", "item", "item", "directory", false, 1},
		{"payload symlink", "item", "item", "symlink", false, 1},
		{"sidecar directory", "item", "item.b2sum", "directory", false, 1},
		{"ancestor symlink", "nested/item", "nested", "symlink", false, 1},
		{"ancestor regular file", "nested/item", "nested", "regular", false, 2},
		{"ancestor checksum sidecar", "nested.b2sum/item", "nested.b2sum", "regular", false, 2},
		{"healthy copies and directory", "item", "item", "directory", true, 1},
		{"healthy copies and symlink", "item", "item", "symlink", true, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			home, base := t.TempDir(), t.TempDir()
			roots := []string{filepath.Join(base, "a"), filepath.Join(base, "b"), filepath.Join(base, "c")}
			for _, root := range roots {
				if err := os.Mkdir(root, 0700); err != nil {
					t.Fatal(err)
				}
			}
			t.Cleanup(func() {
				_ = filepath.WalkDir(base, func(path string, entry os.DirEntry, err error) error {
					if err == nil && entry.IsDir() {
						return os.Chmod(path, 0700)
					}
					return err
				})
			})
			good := checksumBytes([]byte("good"))
			data := "bad"
			if test.healthy {
				data = "good"
			}
			for _, root := range roots[:2] {
				if err := os.MkdirAll(filepath.Join(root, filepath.Dir(test.dataPath)), 0700); err != nil {
					t.Fatal(err)
				}
			}
			writeMirrorFile(t, filepath.Join(roots[0], test.dataPath), []byte("good"), good)
			writeMirrorFile(t, filepath.Join(roots[1], test.dataPath), []byte(data), good)
			writeMirrorFile(t, filepath.Join(roots[0], "safe"), []byte("good"), good)
			writeMirrorFile(t, filepath.Join(roots[1], "safe"), []byte("bad"), good)
			conflictPath := filepath.Join(roots[2], test.conflictPath)
			switch test.kind {
			case "directory":
				if err := os.Mkdir(conflictPath, 0700); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink("opaque/../missing", conflictPath); err != nil {
					t.Fatal(err)
				}
			case "regular":
				if strings.HasSuffix(conflictPath, checksumSuffix) {
					if err := os.WriteFile(conflictPath, []byte("blocker"), 0600); err != nil {
						t.Fatal(err)
					}
				} else {
					writeMirrorFile(t, conflictPath, []byte("blocker"), checksumBytes([]byte("blocker")))
				}
			}
			var output bytes.Buffer
			confirmations := 0
			options := Options{
				Snapshot: func(context.Context) (mountdisks.Snapshot, error) {
					return mountdisks.Snapshot{Roots: roots, ConfiguredMirrors: 3}, nil
				},
				Confirm: func(context.Context) error { confirmations++; return nil },
				Output:  &output,
			}
			ctx := context.Background()
			if err := Run(ctx, home, true, false, options); !errors.Is(err, ErrUnresolved) {
				t.Fatalf("fresh scan misclassified a static type conflict: %v", err)
			}
			logPath := filepath.Join(home, ".mirror", repairLogName)
			scanned, err := Load(logPath)
			if err != nil || len(scanned.Unresolved) != test.findings || len(scanned.Repairs) != 1 || scanned.Repairs[0].RelPath != "safe" || confirmations != 0 {
				t.Fatalf("incorrect scan partition: %+v confirmations=%d err=%v", scanned, confirmations, err)
			}
			found := false
			for _, finding := range scanned.Unresolved {
				if finding.RelPath == test.dataPath {
					for _, copy := range finding.Copies {
						if copy.Root == roots[2] && copy.Conflict != nil {
							found = copy.Conflict.RelPath == test.conflictPath && copy.Conflict.Kind == test.kind && copy.ActualChecksum == "" && copy.Sidecar == nil
						}
					}
				}
			}
			if !found {
				t.Fatalf("scan lost the conflicting entry's root, path, or type: %+v", scanned.Unresolved)
			}
			if !strings.Contains(output.String(), "entry type conflict") || !strings.Contains(output.String(), conflictPath) || !strings.Contains(output.String(), test.kind) {
				t.Fatalf("report concealed type-conflict evidence: %s", &output)
			}
			lockTestRoots(t, roots)
			for _, preview := range []bool{true, false} {
				if err := Run(ctx, home, preview, true, options); !errors.Is(err, ErrUnresolved) {
					t.Fatalf("resume did not preserve the independent safe subset: %v", err)
				}
			}
			finished, err := Load(logPath)
			if err != nil || finished.Status != StatusUnresolved || finished.Repairs[0].State != RepairCompleted || !reflect.DeepEqual(finished.Unresolved, scanned.Unresolved) || confirmations != 1 {
				t.Fatalf("repair or replay changed conflict evidence: %+v confirmations=%d err=%v", finished, confirmations, err)
			}
			for path, want := range map[string]string{test.dataPath: data, "safe": "good"} {
				got, err := os.ReadFile(filepath.Join(roots[1], path))
				if err != nil || string(got) != want {
					t.Fatalf("incorrect publication at %s: %q %v", path, got, err)
				}
			}
			if test.kind == "symlink" {
				if got, err := os.Readlink(conflictPath); err != nil || got != "opaque/../missing" {
					t.Fatalf("repair changed/followed the conflicting link: %q %v", got, err)
				}
			} else if test.kind == "regular" {
				if got, err := os.ReadFile(conflictPath); err != nil || string(got) != "blocker" {
					t.Fatalf("repair changed the conflicting ancestor: %q %v", got, err)
				}
			} else if info, err := os.Stat(conflictPath); err != nil || !info.IsDir() {
				t.Fatalf("repair replaced the conflicting directory: %v %v", info, err)
			}

			// Manual resolution never promotes historical unresolved evidence on
			// resume. Only a new scan may authorize work on the formerly blocked path.
			if err := os.Chmod(roots[2], 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(conflictPath); err != nil {
				t.Fatal(err)
			}
			if test.kind == "regular" && !strings.HasSuffix(conflictPath, checksumSuffix) {
				if err := os.Remove(conflictPath + checksumSuffix); err != nil {
					t.Fatal(err)
				}
			}
			lockTestRoots(t, roots)
			if err := Run(ctx, home, false, true, options); !errors.Is(err, ErrUnresolved) || confirmations != 1 {
				t.Fatalf("resume reinterpreted conflict evidence: confirmations=%d err=%v", confirmations, err)
			}
			replayed, err := Load(logPath)
			if err != nil || !reflect.DeepEqual(replayed.Unresolved, scanned.Unresolved) {
				t.Fatalf("compaction lost historical conflict evidence: %+v %v", replayed, err)
			}
			if err := Run(ctx, home, false, false, options); err != nil {
				t.Fatalf("fresh scan after manual resolution failed: %v", err)
			}
			if got, err := os.ReadFile(filepath.Join(roots[1], test.dataPath)); err != nil || string(got) != "good" {
				t.Fatalf("fresh scan did not repair the resolved path: %q %v", got, err)
			}
		})
	}
}

func TestScanDoesNotInventDirectoryOrSymlinkOnlyCandidates(t *testing.T) {
	roots := []string{t.TempDir(), t.TempDir()}
	if err := os.MkdirAll(filepath.Join(roots[0], "empty", "child"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("opaque", filepath.Join(roots[0], "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(roots[1], "link"), 0700); err != nil {
		t.Fatal(err)
	}
	observations, err := Scan(context.Background(), testRoots(t, roots))
	if err != nil || len(observations) != 0 {
		t.Fatalf("non-payload paths created repair candidates: %+v %v", observations, err)
	}
	now := time.Now().UTC()
	log, err := BuildLog(now, now, roots, 2, observations)
	if err != nil || log.Status != StatusClean {
		t.Fatalf("directory/symlink-only scan was not clean: %+v %v", log, err)
	}
}

func TestLogRejectsInvalidTypeConflictEvidence(t *testing.T) {
	good := checksumBytes([]byte("good"))
	for _, test := range []struct {
		name   string
		change func(*Log)
	}{
		{"unknown type", func(log *Log) { log.Unresolved[0].Copies[2].Conflict.Kind = "unknown" }},
		{"unrelated path", func(log *Log) { log.Unresolved[0].Copies[2].Conflict.RelPath = "other" }},
		{"unsafe path", func(log *Log) { log.Unresolved[0].Copies[2].Conflict.RelPath = "../outside" }},
		{"directory ancestor is not an obstruction", func(log *Log) {
			log.Unresolved[0].Copies[2].Conflict = &TypeConflict{RelPath: "dir", Kind: entryDirectory}
		}},
		{"regular leaf is checksum evidence", func(log *Log) { log.Unresolved[0].Copies[2].Conflict.Kind = entryRegular }},
		{"conflict with data vote", func(log *Log) { log.Unresolved[0].Copies[2].ActualChecksum = good }},
		{"conflict with sidecar vote", func(log *Log) { log.Unresolved[0].Copies[2].Sidecar = recorded(good) }},
		{"promoted to repair target", func(log *Log) {
			finding := log.Unresolved[0]
			log.Status, log.Unresolved = StatusPending, nil
			log.Repairs = []Repair{{RelPath: finding.RelPath, Checksum: good, Copies: finding.Copies, State: RepairPending}}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Now().UTC()
			log, err := BuildLog(now, now, []string{"/a", "/b", "/c"}, 3, map[string][]Copy{"dir/item": {
				{Root: "/a", ActualChecksum: good, Sidecar: recorded(good)},
				{Root: "/b", ActualChecksum: good, Sidecar: recorded(good)},
				{Root: "/c", Conflict: &TypeConflict{RelPath: "dir/item", Kind: entrySymlink}},
			}})
			if !errors.Is(err, ErrUnresolved) || len(log.Unresolved) != 1 {
				t.Fatalf("valid type conflict not retained: %+v %v", log, err)
			}
			test.change(&log)
			path := filepath.Join(t.TempDir(), repairLogName)
			if err := writeJSONForTest(path, log, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil {
				t.Fatal("accepted invalid or actionable type-conflict evidence")
			}
		})
	}
}
