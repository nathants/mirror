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
	"mirror/internal/rootfs"
)

func TestConfiguredMirrorConsensus(t *testing.T) {
	x, y, z := checksumBytes([]byte("x")), checksumBytes([]byte("y")), checksumBytes([]byte("z"))
	tests := []struct {
		name       string
		configured int
		copies     []Copy
		want       string
		failure    bool
	}{
		{"two healthy defeat one double disagreement", 3, []Copy{{Root: "/a", ActualChecksum: x, Sidecar: recorded(x)}, {Root: "/b", ActualChecksum: x, Sidecar: recorded(x)}, {Root: "/c", ActualChecksum: y, Sidecar: recorded(z)}}, x, false},
		{"two mirrors can repair data from metadata evidence", 2, []Copy{{Root: "/a", ActualChecksum: x, Sidecar: recorded(x)}, {Root: "/b", ActualChecksum: y, Sidecar: recorded(x)}}, x, false},
		{"multiple majorities even with unequal support", 3, []Copy{{Root: "/a", ActualChecksum: x, Sidecar: recorded(y)}, {Root: "/b", ActualChecksum: x, Sidecar: recorded(y)}, {Root: "/c", ActualChecksum: x, Sidecar: recorded(x)}}, "", true},
		{"missing configured mirrors do not shrink denominator", 4, []Copy{{Root: "/a", ActualChecksum: x, Sidecar: recorded(x)}, {Root: "/b", ActualChecksum: y, Sidecar: recorded(x)}}, "", true},
		{"agreement is not doubled", 3, []Copy{{Root: "/a", ActualChecksum: x, Sidecar: recorded(x)}, {Root: "/b", ActualChecksum: y, Sidecar: nil}}, "", true},
		{"missing metadata uses data evidence", 2, []Copy{{Root: "/a", ActualChecksum: x, Sidecar: nil}, {Root: "/b", ActualChecksum: x, Sidecar: nil}}, x, false},
		{"metadata majority without data source refuses", 3, []Copy{{Root: "/a", ActualChecksum: y, Sidecar: recorded(x)}, {Root: "/b", ActualChecksum: z, Sidecar: recorded(x)}}, "", true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			now := time.Now().UTC()
			var roots []string
			for _, copy := range test.copies {
				roots = append(roots, copy.Root)
			}
			log, err := BuildLog(now, now, roots, test.configured, map[string][]Copy{"item": test.copies})
			if test.failure {
				if err == nil {
					t.Fatal("unsafe consensus accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(log.Repairs) != 1 || log.Repairs[0].Checksum != test.want {
				t.Fatalf("wrong repair: %+v", log.Repairs)
			}
		})
	}
}

func TestScanAndRepairMissingMalformedMetadataWithoutIngest(t *testing.T) {
	for _, value := range [][]byte{nil, {}, {0xff, 0, 0x80}, []byte("truncated")} {
		t.Run(string(checksumBytes(value)[:8]), func(t *testing.T) {
			roots := []string{t.TempDir(), t.TempDir(), t.TempDir()}
			good := checksumBytes([]byte("good"))
			for _, root := range roots {
				writeMirrorFile(t, filepath.Join(root, "item"), []byte("good"), good)
			}
			path := filepath.Join(roots[2], "item.b2sum")
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if value != nil {
				if err := os.WriteFile(path, value, 0600); err != nil {
					t.Fatal(err)
				}
			}
			observations, err := Scan(context.Background(), roots)
			if err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if value == nil {
				if !errors.Is(err, os.ErrNotExist) {
					t.Fatal("scan created missing metadata")
				}
			} else if err != nil || string(before) != string(value) {
				t.Fatalf("scan rewrote metadata: %q %v", before, err)
			}
			now := time.Now().UTC()
			log, err := BuildLog(now, now, roots, 3, observations)
			if err != nil {
				t.Fatal(err)
			}
			if err := Apply(context.Background(), log, roots, false, io.Discard); err != nil {
				t.Fatal(err)
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != good {
				t.Fatalf("metadata not repaired: %q %v", after, err)
			}
			if value != nil {
				backup := quarantined(t, roots[2], "item.b2sum")
				got, err := os.ReadFile(backup)
				if err != nil || string(got) != string(value) {
					t.Fatalf("evidence not retained: %q %v", got, err)
				}
			}
			// Crash artifacts and corrupt evidence cannot become normal observations.
			root, err := rootfs.Open(roots[2])
			if err != nil {
				t.Fatal(err)
			}
			temp, name, err := root.CreateTemp(rootfs.Internal+"/tmp", "interrupted-")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := temp.WriteString("partial copy"); err != nil {
				t.Fatal(err)
			}
			temp.Close()
			root.Close()
			observations, err = Scan(context.Background(), roots)
			if err != nil {
				t.Fatal(err)
			}
			if len(observations) != 1 {
				t.Fatalf("quarantine/temp entered scan: %v", observations)
			}
			if _, err := os.Stat(filepath.Join(roots[2], name)); err != nil {
				t.Fatal("scan removed crash evidence")
			}
		})
	}
}
func TestFreshPreviewIsObservationalOnDisks(t *testing.T) {
	home := t.TempDir()
	roots := []string{t.TempDir(), t.TempDir()}
	for _, root := range roots {
		if err := os.WriteFile(filepath.Join(root, "item"), []byte("same"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	options := commandOptions{snapshot: func(context.Context) (mountdisks.Snapshot, error) {
		return mountdisks.Snapshot{Roots: roots, ConfiguredMirrors: 2}, nil
	}, confirm: func(context.Context) error { t.Fatal("preview asked to mutate"); return nil }, output: io.Discard}
	if err := run(context.Background(), home, true, false, options); err != nil {
		t.Fatal(err)
	}
	for _, root := range roots {
		if _, err := os.Stat(filepath.Join(root, "item.b2sum")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("preview created sidecar")
		}
		info, err := os.Stat(filepath.Join(root, "item"))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0644 {
			t.Fatal("preview chmodded data")
		}
		if _, err := os.Stat(filepath.Join(root, rootfs.Internal)); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("preview created internal storage")
		}
	}
	log, err := Load(filepath.Join(home, ".mirror", repairLogName))
	if err != nil {
		t.Fatal(err)
	}
	if log.Status != StatusPending {
		t.Fatal("preview did not retain pending plan")
	}
}
func TestCancelledRepairRetainsPendingLogAndRelocks(t *testing.T) {
	home := t.TempDir()
	roots := []string{t.TempDir(), t.TempDir()}
	good := checksumBytes([]byte("good"))
	writeMirrorFile(t, filepath.Join(roots[0], "item"), []byte("good"), good)
	writeMirrorFile(t, filepath.Join(roots[1], "item"), []byte("bad"), good)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	writer := &cancelAfterOutput{cancel: cancel}
	options := commandOptions{snapshot: func(context.Context) (mountdisks.Snapshot, error) {
		return mountdisks.Snapshot{Roots: roots, ConfiguredMirrors: 2}, nil
	}, confirm: func(context.Context) error { return nil }, output: writer}
	err := run(ctx, home, false, false, options)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error: %v", err)
	}
	log, err := Load(filepath.Join(home, ".mirror", repairLogName))
	if err != nil {
		t.Fatal(err)
	}
	if log.Status != StatusPending {
		t.Fatal("cancelled repair marked completed")
	}
	for _, root := range roots {
		info, err := os.Stat(root)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0544 {
			t.Fatalf("root left unlocked: %o", info.Mode().Perm())
		}
		t.Cleanup(func() { _ = os.Chmod(root, 0700) })
	}
	options.output = io.Discard
	if err := run(context.Background(), home, false, true, options); err != nil {
		t.Fatal(err)
	}
	log, err = Load(filepath.Join(home, ".mirror", repairLogName))
	if err != nil {
		t.Fatal(err)
	}
	if log.Status != StatusCompleted {
		t.Fatal("resume did not complete")
	}
}

type cancelAfterOutput struct{ cancel context.CancelFunc }

func (w *cancelAfterOutput) Write(p []byte) (int, error) {
	if strings.Contains(string(p), "repaired file:") {
		w.cancel()
	}
	return len(p), nil
}

func TestReservedNamespaceCollisionStopsBeforeMutation(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, rootfs.Internal), 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, rootfs.Internal, "user-data")
	if err := os.WriteFile(path, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Scan(context.Background(), []string{root}); err == nil {
		t.Fatal("unmarked reserved user directory was adopted")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "keep" {
		t.Fatal("collision changed user content")
	}
}

func TestQuarantineSupportsMaximumSidecarFilename(t *testing.T) {
	roots := []string{t.TempDir(), t.TempDir()}
	name := strings.Repeat("x", 249)
	good := checksumBytes([]byte("good"))
	writeMirrorFile(t, filepath.Join(roots[0], name), []byte("good"), good)
	writeMirrorFile(t, filepath.Join(roots[1], name), []byte("bad"), good)
	observations, err := Scan(context.Background(), roots)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	log, err := BuildLog(now, now, roots, 2, observations)
	if err != nil {
		t.Fatal(err)
	}
	if err := Apply(context.Background(), log, roots, false, io.Discard); err != nil {
		t.Fatal(err)
	}
}

func TestScanCancellationJoinsEveryDiskReader(t *testing.T) {
	roots := []string{t.TempDir(), t.TempDir()}
	for _, root := range roots {
		for _, name := range []string{"a", "b", "c"} {
			if err := os.WriteFile(filepath.Join(root, name), []byte("data"), 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reported := 0
	_, err := scan(ctx, roots, func(string, string) { reported++; cancel() })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("scan cancellation: %v", err)
	}
	// scan owns/joined all workers; this non-atomic read is race-safe only if no
	// callback can still run after return (also exercised under go test -race).
	if reported < 1 {
		t.Fatal("cancellation callback never ran")
	}
}
func TestCancellationDuringReplacementNeverPublishesPartialData(t *testing.T) {
	path := t.TempDir()
	root, err := rootfs.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := os.WriteFile(filepath.Join(path, "item"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err = replace(ctx, root, "item", checksumBytes([]byte("old")), checksumBytes([]byte("new")), func(file *os.File) error {
		_, err := file.WriteString("new")
		cancel()
		return err
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("replacement cancellation: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(path, "item"))
	if err != nil || string(data) != "old" {
		t.Fatal("cancelled replacement published data")
	}
	entries, err := os.ReadDir(filepath.Join(path, rootfs.Internal, "tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatal("cancelled replacement leaked temporary file")
	}
}
