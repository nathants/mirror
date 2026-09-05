package repaircopies

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mirror/internal/mountdisks"
)

func partialFixture(t *testing.T) (string, []string, commandOptions) {
	t.Helper()
	home := t.TempDir()
	roots := []string{t.TempDir(), t.TempDir()}
	for _, root := range roots {
		if err := os.Chmod(root, 0700); err != nil {
			t.Fatal(err)
		}
	}
	x, y, good := checksumBytes([]byte("x")), checksumBytes([]byte("y")), checksumBytes([]byte("good"))
	for _, name := range []string{"a-unresolved", "z-unresolved"} {
		writeMirrorFile(t, filepath.Join(roots[0], name), []byte("x"), x)
		writeMirrorFile(t, filepath.Join(roots[1], name), []byte("y"), y)
	}
	writeMirrorFile(t, filepath.Join(roots[0], "m-safe"), []byte("good"), good)
	writeMirrorFile(t, filepath.Join(roots[1], "m-safe"), []byte("bad"), good)
	t.Cleanup(func() {
		for _, root := range roots {
			_ = os.Chmod(root, 0700)
		}
	})
	return home, roots, commandOptions{snapshot: func(context.Context) (mountdisks.Snapshot, error) {
		return mountdisks.Snapshot{Roots: roots, ConfiguredMirrors: 2}, nil
	}}
}

func TestRunRepairsSafeSubsetAndRetainsEveryUnresolvedFinding(t *testing.T) {
	home, roots, options := partialFixture(t)
	var output strings.Builder
	options.output = &output
	confirmed := false
	options.confirm = func(context.Context) error {
		confirmed = true
		data, err := os.ReadFile(filepath.Join(home, ".mirror", repairLogName))
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"a-unresolved", "m-safe", "z-unresolved"} {
			if !strings.Contains(string(data), name) || !strings.Contains(output.String(), name) {
				t.Fatalf("confirmation preceded durable and visible findings for %s", name)
			}
		}
		return nil
	}
	if err := run(context.Background(), home, false, false, options); err == nil {
		t.Fatal("unresolved findings reported as success")
	}
	if !confirmed {
		t.Error("unresolved files prevented confirmation of the safe subset")
	}
	if data, err := os.ReadFile(filepath.Join(roots[1], "m-safe")); err != nil || string(data) != "good" {
		t.Errorf("safe repair was discarded: %q %v", data, err)
	}
	log, err := Load(filepath.Join(home, ".mirror", repairLogName))
	if err != nil {
		t.Fatal(err)
	}
	if log.Status != StatusUnresolved || len(log.Unresolved) != 2 || len(log.Repairs) != 1 || log.Repairs[0].State != RepairCompleted || log.Error != "" {
		t.Errorf("completed scan lost or remaining issues hidden: %+v", log)
	}
	if !strings.Contains(output.String(), "repaired=1 unresolved=2 failed/uncertain=0 unattempted=0") {
		t.Errorf("summary concealed partial success: %s", output.String())
	}
	options.confirm = func(context.Context) error { t.Fatal("resume tried to reapply completed repairs"); return nil }
	if err := run(context.Background(), home, false, true, options); err == nil {
		t.Fatal("resume concealed remaining unresolved findings")
	}
	for _, name := range []string{"a-unresolved", "z-unresolved"} {
		if !strings.Contains(output.String(), name) {
			t.Errorf("report omitted %s", name)
		}
		for i, root := range roots {
			want := []string{"x", "y"}[i]
			data, err := os.ReadFile(filepath.Join(root, name))
			if err != nil || string(data) != want {
				t.Errorf("unresolved evidence changed: %q %v", data, err)
			}
		}
	}
}

func TestPartialPreviewRetainsAllFindingsWithoutMutation(t *testing.T) {
	home, roots, options := partialFixture(t)
	var output strings.Builder
	options.output = &output
	options.confirm = func(context.Context) error { t.Fatal("preview requested confirmation"); return nil }
	if err := run(context.Background(), home, true, false, options); err == nil {
		t.Fatal("preview hid unresolved findings")
	}
	log, err := Load(filepath.Join(home, ".mirror", repairLogName))
	if err != nil {
		t.Fatal(err)
	}
	if log.Status != StatusPending || len(log.Repairs) != 1 {
		t.Errorf("preview discarded the safe subset: %+v", log)
	}
	for _, name := range []string{"a-unresolved", "m-safe", "z-unresolved"} {
		if !strings.Contains(output.String(), name) {
			t.Errorf("preview omitted %s", name)
		}
	}
	if data, err := os.ReadFile(filepath.Join(roots[1], "m-safe")); err != nil || string(data) != "bad" {
		t.Errorf("preview changed data: %q %v", data, err)
	}
	for _, root := range roots {
		info, err := os.Stat(root)
		if err != nil || info.Mode().Perm() != 0700 {
			t.Fatalf("preview changed root modes: %v %v", info, err)
		}
	}
}
