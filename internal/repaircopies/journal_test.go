package repaircopies

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func journalFixture(t *testing.T) (string, Log) {
	t.Helper()
	x, y := checksumBytes([]byte("x")), checksumBytes([]byte("y"))
	observations := map[string][]Copy{}
	for _, path := range []string{"a", "b"} {
		observations[path] = []Copy{{Root: "/mirror/a", ActualChecksum: x}, {Root: "/mirror/b", ActualChecksum: x}}
	}
	observations["unresolved"] = []Copy{{Root: "/mirror/a", ActualChecksum: x}, {Root: "/mirror/b", ActualChecksum: y}}
	now := time.Now().UTC()
	log, err := BuildLog(now, now, []string{"/mirror/a", "/mirror/b"}, 2, observations)
	if !errors.Is(err, ErrUnresolved) {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), repairLogName)
	if err := Save(path, log); err != nil {
		t.Fatal(err)
	}
	return path, log
}

func readJournalBytes(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestJournalReplaysAndCompactsTornFinalRecord(t *testing.T) {
	path, _ := journalFixture(t)
	j, err := OpenJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	a, b := 0, 1
	for _, p := range []Progress{{Repair: &a, State: RepairStarted}, {Repair: &a, State: RepairCompleted}, {Repair: &b, State: RepairStarted}} {
		if err := j.Append(p); err != nil {
			t.Fatal(err)
		}
	}
	before := readJournalBytes(t, path)
	want, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := j.Append(Progress{Repair: &b, State: RepairCompleted}); err != nil {
		t.Fatal(err)
	}
	full := readJournalBytes(t, path)
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	// Cut a real completion record at every byte, including just its newline.
	// Every prefix must preserve the durable start as uncertain, not completed.
	for end := len(before); end < len(full); end++ {
		if err := os.WriteFile(path, full[:end], 0600); err != nil {
			t.Fatal(err)
		}
		loaded, err := Load(path)
		if err != nil || !reflect.DeepEqual(loaded, want) {
			t.Fatalf("torn tail at %d changed durable state: %+v %v", end, loaded, err)
		}
		if !bytes.Equal(readJournalBytes(t, path), full[:end]) {
			t.Fatal("observational load modified the journal")
		}
		resumed, err := OpenJournal(path)
		if err != nil {
			t.Fatal(err)
		}
		compacted := readJournalBytes(t, path)
		if len(compacted) >= len(before) || !reflect.DeepEqual(resumed.log, want) {
			t.Fatal("compaction lost state/evidence or retained redundant records")
		}
		if err := resumed.Append(Progress{Repair: &a, State: RepairStarted}); err == nil {
			t.Fatal("completed file was allowed to restart")
		}
		for _, p := range []Progress{{Repair: &b, State: RepairStarted}, {Repair: &b, State: RepairCompleted}} {
			if err := resumed.Append(p); err != nil {
				t.Fatal(err)
			}
		}
		finished := time.Now().UTC()
		if err := resumed.Append(Progress{FinishAt: &finished}); err != nil {
			t.Fatal(err)
		}
		if err := resumed.Close(); err != nil {
			t.Fatal(err)
		}
		loaded, err = Load(path)
		if err != nil || loaded.Status != StatusUnresolved || loaded.Repairs[0].State != RepairCompleted || loaded.Repairs[1].State != RepairCompleted || !reflect.DeepEqual(loaded.Unresolved, want.Unresolved) {
			t.Fatalf("resumed records lost safe completion or unresolved evidence: %+v %v", loaded, err)
		}
	}
}

func TestJournalRejectsInvalidCompleteRecords(t *testing.T) {
	for _, record := range []string{
		`{`,
		`{}`,
		`{"sequence":2,"repair":0,"state":"started"}`,
		`{"sequence":1,"repair":0,"state":"completed"}`,
		`{"sequence":1,"repair":0,"state":"failed/uncertain","error":"failure"}`,
		`{"sequence":1,"repair":-1,"state":"started"}`,
		`{"sequence":1,"repair":2,"state":"started"}`,
		`{"sequence":1,"repair":0,"state":"pending"}`,
		`{"sequence":1,"repair":0,"state":"started","error":"failure"}`,
		`{"sequence":1,"repair":0,"state":"started","unknown":true}`,
		`{"sequence":1,"finish_at":"2026-09-24T06:00:00Z"}`,
		`{"sequence":1,"repair":0,"state":"started"} {}`,
	} {
		t.Run(record, func(t *testing.T) {
			path, _ := journalFixture(t)
			data := append(readJournalBytes(t, path), []byte(record+"\n")...)
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil {
				t.Fatal("corrupt complete record was treated as an interrupted tail")
			}
			if j, err := OpenJournal(path); err == nil {
				_ = j.Close()
				t.Fatal("corrupt records were compacted away")
			}
			if !bytes.Equal(data, readJournalBytes(t, path)) {
				t.Fatal("rejected replay changed evidence")
			}
		})
	}
}

func TestJournalWriteFailurePoisonsFurtherAppends(t *testing.T) {
	path, log := journalFixture(t)
	j, err := OpenJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := j.file.Close(); err != nil {
		t.Fatal(err)
	}
	// Stat/identity checks still work; the actual write fails on this descriptor.
	j.file, err = os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	i := 0
	if err := j.Append(Progress{Repair: &i, State: RepairStarted}); err == nil || !strings.Contains(err.Error(), "uncertain") {
		t.Fatalf("write failure not reported as uncertain: %v", err)
	}
	_ = j.file.Close()
	j.file, err = os.OpenFile(path, os.O_RDWR|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = j.Close() }()
	if err := j.Append(Progress{Repair: &i, State: RepairStarted}); err == nil {
		t.Fatal("journal continued after a persistence failure")
	}
	loaded, err := Load(path)
	if err != nil || !reflect.DeepEqual(loaded, log) || !reflect.DeepEqual(j.log, log) {
		t.Fatalf("failed checkpoint advanced state: %+v %v", loaded, err)
	}
}

func TestJournalRefusesChangedFileAndIncompleteSnapshot(t *testing.T) {
	for _, change := range []string{"replacement", "external append", "incomplete snapshot"} {
		t.Run(change, func(t *testing.T) {
			path, _ := journalFixture(t)
			j, err := OpenJournal(path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = j.Close() }()
			original := readJournalBytes(t, path)
			switch change {
			case "replacement":
				if err := os.Rename(path, path+"-original"); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, original, 0600); err != nil {
					t.Fatal(err)
				}
			case "external append":
				if err := os.WriteFile(path, append(original, 'x'), 0600); err != nil {
					t.Fatal(err)
				}
			case "incomplete snapshot":
				if err := os.WriteFile(path, original[:len(original)-1], 0600); err != nil {
					t.Fatal(err)
				}
				if _, err := Load(path); err == nil {
					t.Fatal("incomplete snapshot accepted")
				}
			}
			i := 0
			if err := j.Append(Progress{Repair: &i, State: RepairStarted}); err == nil {
				t.Fatal("changed journal accepted a checkpoint")
			}
		})
	}
}

func TestJournalFailedAndCompletedTransitionsSurviveCompaction(t *testing.T) {
	path, _ := journalFixture(t)
	j, err := OpenJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	i := 0
	for _, p := range []Progress{{Repair: &i, State: RepairStarted}, {Repair: &i, State: RepairFailed, Error: "publication uncertain"}, {Error: "operation stopped"}} {
		if err := j.Append(p); err != nil {
			t.Fatal(err)
		}
	}
	want, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = j.Close()
	j, err = OpenJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = j.Close() }()
	if !reflect.DeepEqual(j.log, want) {
		t.Fatal("compaction lost failure evidence")
	}
	// Reject records with valid JSON but invalid state transitions before writing.
	before := readJournalBytes(t, path)
	for _, p := range []Progress{{Repair: &i, State: RepairCompleted}, {}, {State: RepairStarted, Error: "no target"}} {
		if err := j.Append(p); err == nil {
			data, _ := json.Marshal(p)
			t.Fatalf("accepted invalid progress: %s", data)
		}
	}
	if !bytes.Equal(before, readJournalBytes(t, path)) {
		t.Fatal("invalid progress was written")
	}
}
