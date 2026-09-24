package repaircopies

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mirror/internal/mountdisks"
)

func TestRepairRevalidatesPreviouslyAbsentCopies(t *testing.T) {
	for _, kind := range []string{"data", "sidecar"} {
		for _, phase := range []string{"resume", "confirmation", "application", "completion"} {
			t.Run(kind+"/"+phase, func(t *testing.T) {
				home := t.TempDir()
				roots := []string{t.TempDir(), t.TempDir(), t.TempDir()}
				x, y := checksumBytes([]byte("x")), checksumBytes([]byte("y"))
				for _, name := range []string{"item", "later"} {
					writeMirrorFile(t, filepath.Join(roots[0], name), []byte("x"), x)
					writeMirrorFile(t, filepath.Join(roots[1], name), []byte("y"), x)
				}
				t.Cleanup(func() {
					for _, root := range roots {
						_ = os.Chmod(root, 0700)
					}
				})
				options := Options{
					Snapshot: func(context.Context) (mountdisks.Snapshot, error) {
						return mountdisks.Snapshot{Roots: roots, ConfiguredMirrors: 3}, nil
					},
					Confirm: func(context.Context) error { return nil },
					Output:  io.Discard,
				}
				ctx := context.Background()
				if err := Run(ctx, home, true, false, options); err != nil {
					t.Fatal(err)
				}

				lockTestRoots(t, roots)
				added := false
				introduceEvidence := func() {
					t.Helper()
					if added {
						return
					}
					path, data := filepath.Join(roots[2], "item"), "y"
					if kind == "sidecar" {
						path += checksumSuffix
						data = y
					}
					if err := os.Chmod(roots[2], 0744); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, []byte(data), 0600); err != nil {
						t.Fatal(err)
					}
					if err := os.Chmod(roots[2], 0544); err != nil {
						t.Fatal(err)
					}
					added = true
					if phase != "completion" {
						// The new vote makes both X and Y strict majorities.
						observations, err := Scan(ctx, testRoots(t, roots))
						if err != nil {
							t.Fatal(err)
						}
						now := time.Now().UTC()
						fresh, err := BuildLog(now, now, roots, 3, observations)
						if !errors.Is(err, ErrUnresolved) || len(fresh.Unresolved) != 1 || fresh.Unresolved[0].RelPath != "item" {
							t.Fatalf("new evidence did not invalidate consensus: %+v %v", fresh, err)
						}
					}
				}
				switch phase {
				case "resume":
					introduceEvidence()
				case "confirmation":
					options.Confirm = func(context.Context) error { introduceEvidence(); return nil }
				case "application":
					options.Append = func(j *Journal, p Progress) error {
						if err := j.Append(p); err != nil {
							return err
						}
						if p.Repair != nil && *p.Repair == 0 && p.State == RepairStarted {
							introduceEvidence()
						}
						return nil
					}
				case "completion":
					options.Output = absenceOutput(func(p []byte) (int, error) {
						if strings.Contains(string(p), "repaired file:") && strings.Contains(string(p), "/item\n") {
							introduceEvidence()
						}
						return len(p), nil
					})
				}
				if err := Run(ctx, home, false, phase != "confirmation", options); err == nil || !strings.Contains(err.Error(), "fresh scan") {
					t.Fatalf("new evidence did not stop the saved repair: %v", err)
				}
				if !added {
					t.Fatal("evidence-change hook was not reached")
				}
				wantData, wantState := "y", RepairPending
				if phase == "application" {
					wantState = RepairFailed
				} else if phase == "completion" {
					wantData, wantState = "x", RepairFailed
				}
				data, err := os.ReadFile(filepath.Join(roots[1], "item"))
				if err != nil || string(data) != wantData {
					t.Fatalf("unexpected publication or rollback: %q %v", data, err)
				}
				data, err = os.ReadFile(filepath.Join(roots[1], "later"))
				if err != nil || string(data) != "y" {
					t.Fatalf("continued repairing after evidence drift: %q %v", data, err)
				}
				log, err := Load(filepath.Join(home, ".mirror", repairLogName))
				if err != nil || log.Status != StatusPending || log.Repairs[0].State != wantState || log.Repairs[1].State != RepairPending {
					t.Fatalf("incorrect durable progress after drift: %+v %v", log, err)
				}
			})
		}
	}
}

type absenceOutput func([]byte) (int, error)

func (w absenceOutput) Write(p []byte) (int, error) { return w(p) }
