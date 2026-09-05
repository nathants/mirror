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

	"mirror/internal/mountdisks"
)

func progressFixture(t *testing.T) (string, []string, Options) {
	t.Helper()
	home := t.TempDir()
	roots := []string{t.TempDir(), t.TempDir()}
	good := checksumBytes([]byte("good"))
	for _, name := range []string{"a", "b", "c"} {
		writeMirrorFile(t, filepath.Join(roots[0], name), []byte("good"), good)
		writeMirrorFile(t, filepath.Join(roots[1], name), []byte("bad"), good)
	}
	t.Cleanup(func() {
		for _, root := range roots {
			_ = os.Chmod(root, 0700)
		}
	})
	return home, roots, Options{Snapshot: func(context.Context) (mountdisks.Snapshot, error) {
		return mountdisks.Snapshot{Roots: roots, ConfiguredMirrors: 2}, nil
	}, Confirm: func(context.Context) error { return nil }, Output: io.Discard}
}

type progressWriter struct {
	strings.Builder
	beforeFailure func()
}

func (w *progressWriter) Write(data []byte) (int, error) {
	if strings.Contains(string(data), "repaired file:") && strings.Contains(string(data), "/b\n") {
		w.beforeFailure()
		return 0, errors.New("injected output failure after publication")
	}
	return w.Builder.Write(data)
}

func TestOperationalFailureRetainsProgressAndStopsLaterRepairs(t *testing.T) {
	for _, drift := range []bool{false, true} {
		t.Run(map[bool]string{false: "resume", true: "completed data drift"}[drift], func(t *testing.T) {
			home, roots, options := progressFixture(t)
			path := filepath.Join(home, ".mirror", repairLogName)
			writer := &progressWriter{beforeFailure: func() {
				// A is verified complete; B's durable start predates its publication.
				log, err := Load(path)
				if err != nil || log.Repairs[0].State != RepairCompleted || log.Repairs[1].State != RepairStarted || log.Repairs[2].State != RepairPending {
					t.Fatalf("publication lacked a durable start: %+v %v", log, err)
				}
			}}
			options.Output = writer
			if err := Run(context.Background(), home, false, false, options); err == nil || !strings.Contains(err.Error(), "injected output failure") {
				t.Fatalf("operational failure lost: %v", err)
			}
			log, err := Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if log.Status != StatusPending || log.Repairs[0].State != RepairCompleted || log.Repairs[1].State != RepairFailed || log.Repairs[1].Error == "" || log.Repairs[2].State != RepairPending || log.Error == "" {
				t.Fatalf("partial progress not retained: %+v", log)
			}
			if !strings.Contains(writer.String(), "repaired=1 unresolved=0 failed/uncertain=1 unattempted=1") {
				t.Fatalf("summary hides progress: %s", writer.String())
			}
			for i, name := range []string{"a", "b", "c"} {
				want := []string{"good", "good", "bad"}[i]
				data, err := os.ReadFile(filepath.Join(roots[1], name))
				if err != nil || string(data) != want {
					t.Fatalf("unexpected data after stop for %s: %q %v", name, data, err)
				}
			}
			for _, root := range roots {
				info, err := os.Stat(root)
				if err != nil || info.Mode().Perm() != 0544 {
					t.Fatalf("failure left root unlocked: %v %v", info, err)
				}
			}
			if drift {
				file := filepath.Join(roots[1], "a")
				if err := os.Chmod(file, 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(file, []byte("bad"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			options.Output = io.Discard
			err = Run(context.Background(), home, false, true, options)
			if drift {
				if err == nil || !strings.Contains(err.Error(), "not repaired") {
					t.Fatalf("completed copy accepted its old scanned bytes: %v", err)
				}
				if data, err := os.ReadFile(filepath.Join(roots[1], "c")); err != nil || string(data) != "bad" {
					t.Fatal("resume mutated later work despite completed-file drift")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			log, err = Load(path)
			if err != nil || log.Status != StatusCompleted || log.Error != "" {
				t.Fatalf("resume did not finish: %+v %v", log, err)
			}
			for _, repair := range log.Repairs {
				if repair.State != RepairCompleted || repair.Error != "" {
					t.Fatalf("resume lost per-file completion: %+v", repair)
				}
				// Exactly one evidence record per replaced file, even across resume.
				quarantined(t, roots[1], repair.RelPath)
			}
		})
	}
}

func TestCheckpointFailureStopsBeforeNextMutation(t *testing.T) {
	for _, failState := range []string{RepairStarted, RepairCompleted} {
		t.Run(failState, func(t *testing.T) {
			home, roots, options := progressFixture(t)
			failed := false
			options.Save = func(path string, log Log) error {
				if len(log.Repairs) > 1 && log.Repairs[1].State == failState {
					failed = true
					return errors.New("injected checkpoint failure")
				}
				return Save(path, log)
			}
			if err := Run(context.Background(), home, false, false, options); err == nil || !strings.Contains(err.Error(), "injected checkpoint failure") || !failed {
				t.Fatalf("checkpoint failure did not stop execution: %v", err)
			}
			log, err := Load(filepath.Join(home, ".mirror", repairLogName))
			if err != nil {
				t.Fatal(err)
			}
			wantState, wantData := RepairPending, "bad"
			if failState == RepairCompleted {
				wantState, wantData = RepairStarted, "good"
			}
			if log.Repairs[0].State != RepairCompleted || log.Repairs[1].State != wantState || log.Repairs[2].State != RepairPending {
				t.Fatalf("failed checkpoint misrepresented durable progress: %+v", log.Repairs)
			}
			if data, err := os.ReadFile(filepath.Join(roots[1], "b")); err != nil || string(data) != wantData {
				t.Fatalf("incorrect mutation around failed checkpoint: %q %v", data, err)
			}
			if data, err := os.ReadFile(filepath.Join(roots[1], "c")); err != nil || string(data) != "bad" {
				t.Fatal("execution continued after checkpoint failure")
			}
		})
	}
}

func TestOnlyUnresolvedFindingsRemainReportableOnResume(t *testing.T) {
	home, roots, options := partialFixture(t)
	for _, root := range roots {
		for _, name := range []string{"m-safe", "m-safe.b2sum"} {
			if err := os.Remove(filepath.Join(root, name)); err != nil {
				t.Fatal(err)
			}
		}
	}
	options.Confirm = func(context.Context) error { t.Fatal("unresolved-only run requested confirmation"); return nil }
	var output strings.Builder
	options.Output = &output
	for _, resume := range []bool{false, true} {
		output.Reset()
		if err := Run(context.Background(), home, false, resume, options); !errors.Is(err, ErrUnresolved) {
			t.Fatalf("unresolved-only result: %v", err)
		}
		if resume && strings.Contains(output.String(), "scanning ") {
			t.Fatal("resume repeated the scan")
		}
		log, err := Load(filepath.Join(home, ".mirror", repairLogName))
		if err != nil || log.Status != StatusUnresolved || len(log.Unresolved) != 2 || len(log.Repairs) != 0 || log.Error != "" {
			t.Fatalf("unresolved-only findings lost: %+v %v", log, err)
		}
		for _, root := range roots {
			info, err := os.Stat(root)
			if err != nil || info.Mode().Perm() != 0700 {
				t.Fatal("unresolved-only run changed directory modes")
			}
		}
	}
}

func TestUnresolvedEvidenceLogRoundTripAndValidation(t *testing.T) {
	home, roots, _ := partialFixture(t)
	observations, err := Scan(context.Background(), roots)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	log, err := BuildLog(now, now, roots, 2, observations)
	if !errors.Is(err, ErrUnresolved) || len(log.Unresolved) != 2 || len(log.Repairs) != 1 {
		t.Fatalf("BuildLog discarded completed evidence: %+v %v", log, err)
	}
	path := filepath.Join(home, ".mirror", repairLogName)
	if err := Save(path, log); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil || !reflect.DeepEqual(log, loaded) {
		t.Fatalf("round trip lost evidence or states: %+v %v", loaded, err)
	}
	for _, invalid := range []string{"reason", "duplicate", "repair collision", "completed", "progress state"} {
		t.Run(invalid, func(t *testing.T) {
			bad := log
			bad.Repairs = append([]Repair(nil), log.Repairs...)
			bad.Unresolved = append([]Unresolved(nil), log.Unresolved...)
			switch invalid {
			case "reason":
				bad.Unresolved[0].Reason = "invented reason"
			case "duplicate":
				bad.Unresolved[1] = bad.Unresolved[0]
			case "repair collision":
				bad.Unresolved[0].RelPath = bad.Repairs[0].RelPath
			case "completed":
				bad.Status = StatusCompleted
				bad.CompletedAt = now
			case "progress state":
				bad.Repairs[0].State = "invented state"
			}
			if err := Save(path, bad); err == nil {
				t.Fatalf("invalid %s accepted", invalid)
			}
		})
	}
	log.Version = 3
	if err := writeJSONForTest(path, log, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "fresh scan") {
		t.Fatalf("old log omitting unresolved findings/progress accepted: %v", err)
	}
}

type rejectPlanWriter struct{}

func (rejectPlanWriter) Write(data []byte) (int, error) {
	if strings.HasPrefix(string(data), "repairable (") {
		return 0, errors.New("injected plan output failure")
	}
	return len(data), nil
}

func TestFailedPlanReportCannotReachConfirmationOrMutation(t *testing.T) {
	home, roots, options := progressFixture(t)
	options.Output = rejectPlanWriter{}
	options.Confirm = func(context.Context) error { t.Fatal("confirmation followed failed report"); return nil }
	if err := Run(context.Background(), home, false, false, options); err == nil || !strings.Contains(err.Error(), "injected plan output failure") {
		t.Fatalf("report failure ignored: %v", err)
	}
	log, err := Load(filepath.Join(home, ".mirror", repairLogName))
	if err != nil || log.Error == "" {
		t.Fatalf("report failure lost from log: %+v %v", log, err)
	}
	for _, repair := range log.Repairs {
		if repair.State != RepairPending {
			t.Fatalf("report failure started repairs: %+v", repair)
		}
		if data, err := os.ReadFile(filepath.Join(roots[1], repair.RelPath)); err != nil || string(data) != "bad" {
			t.Fatal("report failure changed data")
		}
	}
}
