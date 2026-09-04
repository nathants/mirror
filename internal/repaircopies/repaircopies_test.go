package repaircopies

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mirror/internal/rootfs"
)

func TestBuildLogChoosesUniqueMajorityAndSorts(t *testing.T) {
	good := checksumBytes([]byte("good"))
	bad := checksumBytes([]byte("bad"))
	started := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	scanned := started.Add(time.Hour)
	roots := []string{"/mirror/c", "/mirror/a", "/mirror/b"}
	observations := map[string][]Copy{
		"z/file": {
			{Root: "/mirror/c", ActualChecksum: bad, Sidecar: recorded(good)},
			{Root: "/mirror/a", ActualChecksum: good, Sidecar: recorded(good)},
			{Root: "/mirror/b", ActualChecksum: good, Sidecar: recorded(good)},
		},
		"a/healthy": {
			{Root: "/mirror/a", ActualChecksum: good, Sidecar: recorded(good)},
			{Root: "/mirror/b", ActualChecksum: good, Sidecar: recorded(good)},
		},
	}

	log, err := BuildLog(started, scanned, roots, len(roots), observations)
	if err != nil {
		t.Fatalf("BuildLog: %v", err)
	}
	if log.Status != StatusPending {
		t.Fatalf("status = %q, want %q", log.Status, StatusPending)
	}
	if got, want := strings.Join(log.Roots, ","), "/mirror/a,/mirror/b,/mirror/c"; got != want {
		t.Fatalf("roots = %q, want %q", got, want)
	}
	if len(log.Repairs) != 1 || log.Repairs[0].RelPath != "z/file" || log.Repairs[0].Checksum != good {
		t.Fatalf("repairs = %#v", log.Repairs)
	}
	if got, want := log.Repairs[0].Copies[0].Root, "/mirror/a"; got != want {
		t.Fatalf("first sorted copy = %q, want %q", got, want)
	}
}

func TestBuildLogRejectsMultipleMajorities(t *testing.T) {
	a := checksumBytes([]byte("a"))
	b := checksumBytes([]byte("b"))
	c := checksumBytes([]byte("c"))
	e := checksumBytes([]byte("e"))
	now := time.Now().UTC()

	_, err := BuildLog(now, now, []string{"/mirror/a"}, 1, map[string][]Copy{
		"file": {{Root: "/mirror/a", ActualChecksum: a, Sidecar: recorded(b)}},
	})
	if err == nil || !strings.Contains(err.Error(), "strict checksum majority") {
		t.Fatalf("tie error = %v", err)
	}

	_, err = BuildLog(now, now, []string{"/mirror/a", "/mirror/b", "/mirror/c"}, 3, map[string][]Copy{
		"file": {
			{Root: "/mirror/a", ActualChecksum: a, Sidecar: recorded(b)},
			{Root: "/mirror/b", ActualChecksum: a, Sidecar: recorded(c)},
			{Root: "/mirror/c", ActualChecksum: b, Sidecar: recorded(e)},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "strict checksum majority") {
		t.Fatalf("plurality error = %v", err)
	}
}

func TestSaveLoadLogIsAtomicPrivateAndStrict(t *testing.T) {
	good := checksumBytes([]byte("good"))
	bad := checksumBytes([]byte("bad"))
	now := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	log, err := BuildLog(now, now.Add(time.Minute), []string{"/mirror/a", "/mirror/b"}, 2, map[string][]Copy{
		"file": {
			{Root: "/mirror/a", ActualChecksum: good, Sidecar: recorded(good)},
			{Root: "/mirror/b", ActualChecksum: bad, Sidecar: recorded(good)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(t.TempDir(), ".mirror")
	path := filepath.Join(stateDir, "repair-last.json")
	if err := Save(path, log); err != nil {
		t.Fatalf("Save: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := info.Mode().Perm(), os.FileMode(0o600); got != want {
		t.Fatalf("log mode = %o, want %o", got, want)
	}
	stateInfo, err := os.Stat(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := stateInfo.Mode().Perm(), os.FileMode(0o700); got != want {
		t.Fatalf("state directory mode = %o, want %o", got, want)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded.Repairs) != 1 || loaded.Repairs[0].Checksum != good {
		t.Fatalf("loaded log = %#v", loaded)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, []byte("{}\n")...)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "trailing") {
		t.Fatalf("trailing JSON error = %v", err)
	}
}

func TestLoadRejectsTraversalAndPublicLog(t *testing.T) {
	good := checksumBytes([]byte("good"))
	now := time.Date(2026, 9, 4, 10, 0, 0, 0, time.UTC)
	log := Log{
		Version:           LogVersion,
		ConfiguredMirrors: 1,
		Status:            StatusPending,
		StartedAt:         now,
		ScannedAt:         now,
		Roots:             []string{"/mirror/a"},
		Repairs: []Repair{{
			RelPath:  "../escape",
			Checksum: good,
			Copies:   []Copy{{Root: "/mirror/a", ActualChecksum: good, Sidecar: recorded(good)}},
		}},
	}
	path := filepath.Join(t.TempDir(), "repair.json")
	if err := writeJSONForTest(path, log, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "relative path") {
		t.Fatalf("traversal error = %v", err)
	}

	log.Repairs[0].RelPath = "file"
	if err := writeJSONForTest(path, log, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "permissions") {
		t.Fatalf("public mode error = %v", err)
	}
}

func TestValidateAndApplyPendingRepairPreservesCorruptEvidence(t *testing.T) {
	rootA := filepath.Join(t.TempDir(), "a")
	rootB := filepath.Join(t.TempDir(), "b")
	rootC := filepath.Join(t.TempDir(), "c")
	for _, root := range []string{rootA, rootB, rootC} {
		if err := os.MkdirAll(filepath.Join(root, "books"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	goodData := []byte("known good contents")
	badData := []byte("corrupt contents")
	good := checksumBytes(goodData)
	bad := checksumBytes(badData)
	for _, root := range []string{rootA, rootB} {
		writeMirrorFile(t, filepath.Join(root, "books", "item"), goodData, good)
	}
	writeMirrorFile(t, filepath.Join(rootC, "books", "item"), badData, good)

	now := time.Now().UTC()
	log, err := BuildLog(now, now, []string{rootC, rootA, rootB}, 3, map[string][]Copy{
		"books/item": {
			{Root: rootA, ActualChecksum: good, Sidecar: recorded(good)},
			{Root: rootB, ActualChecksum: good, Sidecar: recorded(good)},
			{Root: rootC, ActualChecksum: bad, Sidecar: recorded(good)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateCurrentState(context.Background(), log, []string{rootA, rootB, rootC}); err != nil {
		t.Fatalf("ValidateCurrentState before apply: %v", err)
	}
	var output strings.Builder
	if err := Apply(context.Background(), log, []string{rootA, rootB, rootC}, false, &output); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	destination := filepath.Join(rootC, "books", "item")
	if got, err := os.ReadFile(destination); err != nil || string(got) != string(goodData) {
		t.Fatalf("destination = %q, err %v", got, err)
	}
	backup := quarantined(t, rootC, "books/item")
	if got, err := os.ReadFile(backup); err != nil || string(got) != string(badData) {
		t.Fatalf("backup = %q, err %v", got, err)
	}
	if !strings.Contains(output.String(), "repaired file:") {
		t.Fatalf("missing repair output: %s", output.String())
	}
	// A killed partially completed run is resumable: desired-state copies are
	// accepted as well as their exact logged pre-repair values.
	if err := ValidateCurrentState(context.Background(), log, []string{rootA, rootB, rootC}); err != nil {
		t.Fatalf("ValidateCurrentState after apply: %v", err)
	}
}

func TestValidateCurrentStateRejectsRootOrFileDrift(t *testing.T) {
	rootA := filepath.Join(t.TempDir(), "a")
	rootB := filepath.Join(t.TempDir(), "b")
	if err := os.MkdirAll(rootA, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(rootB, 0o755); err != nil {
		t.Fatal(err)
	}
	goodData := []byte("good")
	badData := []byte("bad")
	good := checksumBytes(goodData)
	bad := checksumBytes(badData)
	writeMirrorFile(t, filepath.Join(rootA, "item"), goodData, good)
	writeMirrorFile(t, filepath.Join(rootB, "item"), badData, good)
	now := time.Now().UTC()
	log, err := BuildLog(now, now, []string{rootA, rootB}, 2, map[string][]Copy{
		"item": {
			{Root: rootA, ActualChecksum: good, Sidecar: recorded(good)},
			{Root: rootB, ActualChecksum: bad, Sidecar: recorded(good)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateCurrentState(context.Background(), log, []string{rootA}); err == nil || !strings.Contains(err.Error(), "mounted roots changed") {
		t.Fatalf("root drift error = %v", err)
	}
	drifted := filepath.Join(rootB, "item")
	if err := os.Chmod(drifted, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(drifted, []byte("different"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := ValidateCurrentState(context.Background(), log, []string{rootA, rootB}); err == nil || !strings.Contains(err.Error(), "changed since the logged scan") {
		t.Fatalf("file drift error = %v", err)
	}
}

func TestMarkCompletedPreventsResume(t *testing.T) {
	now := time.Now().UTC()
	log := Log{Version: LogVersion, ConfiguredMirrors: 1, Status: StatusScanning, StartedAt: now, Roots: []string{"/mirror/a"}}
	if err := RequirePending(log); err == nil {
		t.Fatal("scanning log unexpectedly resumable")
	}
	log.Status = StatusCompleted
	log.ScannedAt = now
	log.CompletedAt = now.Add(time.Minute)
	log.Repairs = []Repair{{
		RelPath:  "file",
		Checksum: checksumBytes([]byte("good")),
		Copies: []Copy{{
			Root:           "/mirror/a",
			ActualChecksum: checksumBytes([]byte("bad")),
			Sidecar:        recorded(checksumBytes([]byte("good"))),
		}},
	}}
	if err := RequirePending(log); err == nil || !strings.Contains(err.Error(), "completed") {
		t.Fatalf("completed resume error = %v", err)
	}
}

func writeMirrorFile(t *testing.T, path string, data []byte, recorded string) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+checksumSuffix, []byte(recorded), 0o444); err != nil {
		t.Fatal(err)
	}
}

func writeJSONForTest(path string, log Log, mode os.FileMode) error {
	data, err := marshalLog(log)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, mode)
}

func TestApplyPreviewDoesNotMutate(t *testing.T) {
	rootA := filepath.Join(t.TempDir(), "a")
	rootB := filepath.Join(t.TempDir(), "b")
	if err := os.MkdirAll(rootA, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(rootB, 0o755); err != nil {
		t.Fatal(err)
	}
	goodData := []byte("good")
	badData := []byte("bad")
	good := checksumBytes(goodData)
	bad := checksumBytes(badData)
	writeMirrorFile(t, filepath.Join(rootA, "item"), goodData, good)
	writeMirrorFile(t, filepath.Join(rootB, "item"), badData, good)
	now := time.Now().UTC()
	log, err := BuildLog(now, now, []string{rootA, rootB}, 2, map[string][]Copy{
		"item": {
			{Root: rootA, ActualChecksum: good, Sidecar: recorded(good)},
			{Root: rootB, ActualChecksum: bad, Sidecar: recorded(good)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := Apply(context.Background(), log, []string{rootA, rootB}, true, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(filepath.Join(rootB, "item")); err != nil || string(got) != string(badData) {
		t.Fatalf("preview destination = %q, err %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(rootB, "item.corrupted.0")); !os.IsNotExist(err) {
		t.Fatalf("preview created backup: %v", err)
	}
}

func TestApplyRepairsCorruptSidecarAndPreservesIt(t *testing.T) {
	rootA := filepath.Join(t.TempDir(), "a")
	rootB := filepath.Join(t.TempDir(), "b")
	for _, root := range []string{rootA, rootB} {
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	data := []byte("good")
	good := checksumBytes(data)
	bad := checksumBytes([]byte("wrong sidecar value"))
	writeMirrorFile(t, filepath.Join(rootA, "item"), data, good)
	writeMirrorFile(t, filepath.Join(rootB, "item"), data, bad)
	now := time.Now().UTC()
	log, err := BuildLog(now, now, []string{rootA, rootB}, 2, map[string][]Copy{
		"item": {
			{Root: rootA, ActualChecksum: good, Sidecar: recorded(good)},
			{Root: rootB, ActualChecksum: good, Sidecar: recorded(bad)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := Apply(context.Background(), log, []string{rootA, rootB}, false, io.Discard); err != nil {
		t.Fatal(err)
	}
	sidecar := filepath.Join(rootB, "item") + checksumSuffix
	if got, err := os.ReadFile(sidecar); err != nil || string(got) != good {
		t.Fatalf("sidecar = %q, err %v", got, err)
	}
	if got, err := os.ReadFile(quarantined(t, rootB, "item.b2sum")); err != nil || string(got) != bad {
		t.Fatalf("sidecar backup = %q, err %v", got, err)
	}
}

func TestLogValidationRejectsRepairChecksumThatIsNotLoggedMajority(t *testing.T) {
	good := checksumBytes([]byte("good"))
	bad := checksumBytes([]byte("bad"))
	now := time.Now().UTC()
	log, err := BuildLog(now, now, []string{"/mirror/a", "/mirror/b", "/mirror/c"}, 3, map[string][]Copy{
		"item": {
			{Root: "/mirror/a", ActualChecksum: good, Sidecar: recorded(good)},
			{Root: "/mirror/b", ActualChecksum: good, Sidecar: recorded(good)},
			{Root: "/mirror/c", ActualChecksum: bad, Sidecar: recorded(good)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	log.Repairs[0].Checksum = bad
	if err := Save(filepath.Join(t.TempDir(), "repair.json"), log); err == nil || !strings.Contains(err.Error(), "strict majority") {
		t.Fatalf("tampered repair checksum error = %v", err)
	}
}

func recorded(value string) *Sidecar {
	result := &Sidecar{Hash: checksumBytes([]byte(value))}
	if validChecksum(value) {
		result.Value = value
	}
	return result
}

func quarantined(t *testing.T, root, path string) string {
	t.Helper()
	records, err := os.ReadDir(filepath.Join(root, rootfs.Internal, "quarantine"))
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for _, record := range records {
		candidate := filepath.Join(root, rootfs.Internal, "quarantine", record.Name(), path)
		if _, err := os.Stat(candidate); err == nil {
			found = append(found, candidate)
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	if len(found) != 1 {
		t.Fatalf("expected exactly one quarantine record for %s: %v", path, found)
	}
	return found[0]
}
