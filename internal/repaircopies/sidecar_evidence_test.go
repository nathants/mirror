package repaircopies

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"mirror/internal/rootfs"
)

func TestOrphanSidecarMakesRepairAmbiguous(t *testing.T) {
	roots := []string{t.TempDir(), t.TempDir(), t.TempDir()}
	x, y := checksumBytes([]byte("x")), checksumBytes([]byte("y"))
	writeMirrorFile(t, filepath.Join(roots[0], "item"), []byte("x"), x)
	writeMirrorFile(t, filepath.Join(roots[1], "item"), []byte("y"), x)
	if err := os.WriteFile(filepath.Join(roots[2], "item.b2sum"), []byte(y), 0600); err != nil {
		t.Fatal(err)
	}
	observations, err := Scan(context.Background(), testRoots(t, roots))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	// X is supported by A's data/sidecar and B's sidecar. Y is supported
	// by B's data and C's surviving sidecar. Neither may win uniquely.
	if _, err := BuildLog(now, now, roots, 3, observations); err == nil || !strings.Contains(err.Error(), "multiple checksums") {
		t.Fatalf("sidecar-only contrary evidence was ignored: %v", err)
	}
}

// Sort the evidence-only root first to exercise source selection independently
// of observation order. Its vote is necessary to repair the damaged data copy.
func sidecarEvidenceFixture(t *testing.T) (Log, []string) {
	t.Helper()
	base := t.TempDir()
	roots := []string{filepath.Join(base, "a-evidence"), filepath.Join(base, "b-source"), filepath.Join(base, "c-damaged")}
	for _, root := range roots {
		if err := os.Mkdir(root, 0700); err != nil {
			t.Fatal(err)
		}
	}
	good := checksumBytes([]byte("good"))
	if err := os.WriteFile(filepath.Join(roots[0], "item.b2sum"), []byte(good), 0600); err != nil {
		t.Fatal(err)
	}
	writeMirrorFile(t, filepath.Join(roots[1], "item"), []byte("good"), good)
	writeMirrorFile(t, filepath.Join(roots[2], "item"), []byte("bad"), checksumBytes([]byte("wrong")))
	observations, err := Scan(context.Background(), testRoots(t, roots))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	log, err := BuildLog(now, now, roots, 3, observations)
	if err != nil {
		t.Fatal(err)
	}
	if len(log.Repairs) != 1 || len(log.Repairs[0].Copies) != 3 || log.Repairs[0].Copies[0].ActualChecksum != "" {
		t.Fatalf("sidecar-only evidence not retained: %+v", log)
	}
	return log, roots
}

func TestSidecarOnlyEvidenceSurvivesLogResumeAndCompletion(t *testing.T) {
	log, roots := sidecarEvidenceFixture(t)
	path := filepath.Join(t.TempDir(), "repair.json")
	if err := Save(path, log); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(log, loaded) {
		t.Fatal("save/load lost scanned evidence")
	}
	var report strings.Builder
	if err := PrintRepairs(&report, loaded); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(report.String(), "missing (sidecar evidence only; not a repair target)") {
		t.Fatalf("report conceals missing data: %s", report.String())
	}
	if err := Apply(context.Background(), loaded, testRoots(t, roots), true, io.Discard); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(roots[2], "item")); err != nil || string(data) != "bad" {
		t.Fatal("preview changed data")
	}
	j, err := OpenJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	for range 2 { // Desired target state remains resumable; evidence stays unchanged.
		if err := ValidateCurrentState(context.Background(), loaded, testRoots(t, roots)); err != nil {
			t.Fatal(err)
		}
		if err := apply(context.Background(), j.log, testRoots(t, roots), false, io.Discard, func(i int, state string, failure error) error {
			p := Progress{Repair: &i, State: state}
			if failure != nil {
				p.Error = failure.Error()
			}
			return j.Append(p)
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Lstat(filepath.Join(roots[0], "item")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("repair filled an evidence-only data path: %v", err)
	}
	if _, err := os.Stat(filepath.Join(roots[0], rootfs.Internal)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("repair mutated the evidence-only root")
	}
	for _, root := range roots {
		data, err := os.ReadFile(filepath.Join(root, "item.b2sum"))
		if err != nil || string(data) != log.Repairs[0].Checksum {
			t.Fatalf("unexpected sidecar: %q %v", data, err)
		}
	}
	if data, err := os.ReadFile(filepath.Join(roots[2], "item")); err != nil || string(data) != "good" {
		t.Fatalf("data not repaired using sidecar-only vote: %q %v", data, err)
	}
	completedAt := time.Now().UTC()
	if err := j.Append(Progress{FinishAt: &completedAt}); err != nil {
		t.Fatal(err)
	}
	completed, err := Load(path)
	if err != nil || completed.Status != StatusCompleted {
		t.Fatalf("completion lost: %s %v", completed.Status, err)
	}
}

func TestSidecarOnlyEvidenceDriftRejectsResumeBeforeMutation(t *testing.T) {
	for _, change := range []string{"changed sidecar", "missing sidecar", "new data", "new symlink"} {
		t.Run(change, func(t *testing.T) {
			log, roots := sidecarEvidenceFixture(t)
			path := filepath.Join(roots[0], "item")
			var err error
			switch change {
			case "changed sidecar":
				err = os.WriteFile(path+checksumSuffix, []byte("changed evidence"), 0600)
			case "missing sidecar":
				err = os.Remove(path + checksumSuffix)
			case "new data":
				err = os.WriteFile(path, []byte("good"), 0600)
			case "new symlink":
				err = os.Symlink("../missing", path)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := ValidateCurrentState(context.Background(), log, testRoots(t, roots)); err == nil {
				t.Fatal("resume accepted evidence drift")
			}
			if err := Apply(context.Background(), log, testRoots(t, roots), false, io.Discard); err == nil {
				t.Fatal("apply accepted evidence drift")
			}
			if data, err := os.ReadFile(filepath.Join(roots[2], "item")); err != nil || string(data) != "bad" {
				t.Fatal("evidence drift mutated a destination")
			}
		})
	}
}

func TestSidecarOnlyConsensusEdges(t *testing.T) {
	x, y, z := checksumBytes([]byte("x")), checksumBytes([]byte("y")), checksumBytes([]byte("z"))
	for _, test := range []struct {
		name       string
		copies     []Copy
		configured int
		wantError  string
	}{
		{"no source", []Copy{{Root: "/a", Sidecar: recorded(x)}, {Root: "/b", Sidecar: recorded(x)}}, 2, "no data copy"},
		{"empty evidence", []Copy{{Root: "/a"}}, 1, "neither data nor sidecar"},
		{"malformed does not vote", []Copy{{Root: "/a", ActualChecksum: x, Sidecar: recorded(x)}, {Root: "/b", ActualChecksum: y, Sidecar: recorded(z)}, {Root: "/c", Sidecar: recorded("bad metadata")}}, 3, "no unique strict"},
		{"absent mirrors still count", []Copy{{Root: "/a", ActualChecksum: x, Sidecar: recorded(x)}, {Root: "/b", ActualChecksum: y, Sidecar: recorded(z)}, {Root: "/c", Sidecar: recorded(x)}}, 4, "no unique strict"},
		{"healthy data cannot hide contrary majority", []Copy{{Root: "/a", ActualChecksum: x, Sidecar: recorded(x)}, {Root: "/b", Sidecar: recorded(y)}, {Root: "/c", Sidecar: recorded(y)}}, 3, "no data copy"},
		{"healthy data with minority contrary evidence", []Copy{{Root: "/a", ActualChecksum: x, Sidecar: recorded(x)}, {Root: "/b", ActualChecksum: x, Sidecar: recorded(x)}, {Root: "/c", Sidecar: recorded(y)}}, 3, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			var roots []string
			for _, copy := range test.copies {
				roots = append(roots, copy.Root)
			}
			now := time.Now().UTC()
			log, err := BuildLog(now, now, roots, test.configured, map[string][]Copy{"item": test.copies})
			if test.wantError == "" {
				if err != nil || log.Status != StatusClean {
					t.Fatalf("scheduled a repair of evidence-only metadata: %+v %v", log, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("wanted %q, got %v", test.wantError, err)
			}
		})
	}
}

func TestOldLogsRequireFreshSidecarEvidenceScan(t *testing.T) {
	log, _ := sidecarEvidenceFixture(t)
	for _, version := range []int{1, 2} {
		log.Version = version
		path := filepath.Join(t.TempDir(), "old-log.json")
		if err := writeJSONForTest(path, log, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "run a fresh scan") {
			t.Fatalf("version %d can omit evidence but was accepted: %v", version, err)
		}
	}
}

type changeEvidenceAfterRepair struct{ path string }

func (w changeEvidenceAfterRepair) Write(data []byte) (int, error) {
	if strings.Contains(string(data), "repaired file:") {
		if err := os.WriteFile(w.path, []byte("changed evidence"), 0600); err != nil {
			return 0, err
		}
	}
	return len(data), nil
}

func TestCompletionRevalidatesSidecarOnlyEvidence(t *testing.T) {
	log, roots := sidecarEvidenceFixture(t)
	writer := changeEvidenceAfterRepair{path: filepath.Join(roots[0], "item.b2sum")}
	if err := Apply(context.Background(), log, testRoots(t, roots), false, writer); err == nil || !strings.Contains(err.Error(), "sidecar-only evidence changed") {
		t.Fatalf("completed with changed evidence: %v", err)
	}
}

func TestMalformedSidecarOnlyEvidenceIsPreservedWithoutVoting(t *testing.T) {
	log, roots := sidecarEvidenceFixture(t)
	path := filepath.Join(roots[0], "item.b2sum")
	if err := os.WriteFile(path, []byte("malformed"), 0600); err != nil {
		t.Fatal(err)
	}
	// Supply the second good vote from the damaged data's own sidecar,
	// rather than the malformed evidence-only sidecar.
	damagedSidecar := filepath.Join(roots[2], "item.b2sum")
	if err := os.Chmod(damagedSidecar, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(damagedSidecar, []byte(log.Repairs[0].Checksum), 0600); err != nil {
		t.Fatal(err)
	}
	observations, err := Scan(context.Background(), testRoots(t, roots))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	log, err = BuildLog(now, now, roots, 3, observations)
	if err != nil {
		t.Fatal(err)
	}
	if copy := log.Repairs[0].Copies[0]; copy.Sidecar == nil || copy.Sidecar.Value != "" || copy.Sidecar.Hash != checksumBytes([]byte("malformed")) {
		t.Fatalf("malformed evidence lost or promoted to a vote: %+v", copy)
	}
	if err := Apply(context.Background(), log, testRoots(t, roots), false, io.Discard); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "malformed" {
		t.Fatalf("evidence-only bytes changed: %q %v", data, err)
	}
}
