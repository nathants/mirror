package repaircopies

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mirror/internal/mountdisks"
)

func TestImmediatePublicationChecksRejectDriftAfterFileValidation(t *testing.T) {
	for _, changed := range []string{"source", "destination data", "destination sidecar", "metadata-only destination", "repaired destination data"} {
		t.Run(changed, func(t *testing.T) {
			home, base := t.TempDir(), t.TempDir()
			roots := []string{filepath.Join(base, "a"), filepath.Join(base, "b"), filepath.Join(base, "c")}
			for _, root := range roots {
				if err := os.Mkdir(root, 0700); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(root, 0700) })
			}
			good := checksumBytes([]byte("good"))
			for _, name := range []string{"item", "later"} {
				if err := os.WriteFile(filepath.Join(roots[0], name), []byte("good"), 0600); err != nil {
					t.Fatal(err)
				}
				data := "bad"
				if changed == "metadata-only destination" && name == "item" {
					data = "good"
				}
				writeMirrorFile(t, filepath.Join(roots[1], name), []byte(data), "malformed sidecar")
				writeMirrorFile(t, filepath.Join(roots[2], name), []byte("good"), good)
			}
			boundary := "repaired checksumfile: " + filepath.Join(roots[0], "item.b2sum")
			if changed == "repaired destination data" {
				boundary = "repaired file: " + filepath.Join(roots[1], "item")
			}
			drifted := false
			err := Run(context.Background(), home, false, false, Options{
				Snapshot: func(context.Context) (mountdisks.Snapshot, error) {
					return mountdisks.Snapshot{Roots: roots, ConfiguredMirrors: 3}, nil
				},
				Confirm: func(context.Context) error { return nil },
				Output: absenceOutput(func(p []byte) (int, error) {
					if !drifted && strings.Contains(string(p), boundary) {
						// File validation has finished, but an earlier publication
						// separates it from the next data or metadata mutation.
						path := filepath.Join(roots[1], "item")
						if changed == "source" {
							path = filepath.Join(roots[0], "item")
						} else if changed == "destination sidecar" {
							path += checksumSuffix
						}
						if err := os.Chmod(path, 0600); err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(path, []byte("drift"), 0600); err != nil {
							t.Fatal(err)
						}
						drifted = true
					}
					return len(p), nil
				}),
			})
			if err == nil || !drifted {
				t.Fatalf("late drift escaped publication checks: drifted=%v err=%v", drifted, err)
			}
			wantData, wantSidecar := "bad", "malformed sidecar"
			switch changed {
			case "destination data", "metadata-only destination", "repaired destination data":
				wantData = "drift"
			case "destination sidecar":
				wantData, wantSidecar = "good", "drift" // Completed data is not rolled back.
			}
			for name, want := range map[string]string{"item": wantData, "item.b2sum": wantSidecar, "later": "bad"} {
				data, err := os.ReadFile(filepath.Join(roots[1], name))
				if err != nil || string(data) != want {
					t.Fatalf("unexpected publication at %s: %q %v", name, data, err)
				}
			}
			log, err := Load(filepath.Join(home, ".mirror", repairLogName))
			if err != nil || log.Repairs[0].State != RepairFailed || log.Repairs[1].State != RepairPending {
				t.Fatalf("late drift did not stop later work: %+v %v", log, err)
			}
		})
	}
}
