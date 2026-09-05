package repaircopies

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestScanRejectsInvalidChecksumNamespace(t *testing.T) {
	for _, kind := range []string{"double suffix", "empty data name", "directory sidecar", "symlink sidecar", "checksum symlink"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			name := "item.b2sum"
			var err error
			switch kind {
			case "double suffix":
				name += checksumSuffix
			case "empty data name":
				name = checksumSuffix
			case "directory sidecar":
				err = os.Mkdir(filepath.Join(root, "item"), 0700)
			case "symlink sidecar":
				err = os.Symlink("../target", filepath.Join(root, "item"))
			case "checksum symlink":
				err = os.Symlink("../target", filepath.Join(root, name))
			}
			if err != nil {
				t.Fatal(err)
			}
			if kind != "checksum symlink" {
				if err := os.WriteFile(filepath.Join(root, name), []byte("metadata"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := Scan(context.Background(), []string{root}); err == nil || !strings.Contains(err.Error(), ".b2sum is reserved") {
				t.Fatalf("invalid namespace accepted or unexplained: %v", err)
			}
		})
	}
}

func TestChecksumSuffixIsNotReservedInDirectoryNames(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "directory.b2sum", "item")
	if err := os.Mkdir(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	writeMirrorFile(t, path, []byte("data"), checksumBytes([]byte("data")))
	observations, err := Scan(context.Background(), []string{root})
	if err != nil || len(observations["directory.b2sum/item"]) != 1 {
		t.Fatalf("directory suffix confused with file metadata: %+v %v", observations, err)
	}
}

func TestChecksumNamedPayloadRejectedAtPublicationAndLogBoundaries(t *testing.T) {
	ctx := context.Background()
	src, dst := t.TempDir(), t.TempDir()
	path := "payload.b2sum"
	checksum := checksumBytes([]byte("payload"))
	if err := os.WriteFile(filepath.Join(src, path), []byte("payload"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, publish := range []func() error{
		func() error { return PublishMissing(ctx, src, dst, path, checksum) },
		func() error { return PublishMissingSidecar(ctx, src, path, checksum) },
	} {
		if err := publish(); err == nil || !strings.Contains(err.Error(), ".b2sum is reserved") {
			t.Fatalf("publication accepted reserved payload name: %v", err)
		}
	}
	entries, err := os.ReadDir(dst)
	if err != nil || len(entries) != 0 {
		t.Fatalf("rejected publication mutated destination: %v %v", entries, err)
	}
	entries, err = os.ReadDir(src)
	if err != nil || len(entries) != 1 || entries[0].Name() != path {
		t.Fatalf("rejected metadata publication mutated source: %v %v", entries, err)
	}
	now := time.Now().UTC()
	observations := map[string][]Copy{path: {{Root: src, ActualChecksum: checksum}}}
	if _, err := BuildLog(now, now, []string{src}, 1, observations); err == nil || !strings.Contains(err.Error(), ".b2sum is reserved") {
		t.Fatalf("log builder accepted reserved payload name: %v", err)
	}
	log := Log{Version: LogVersion, Status: StatusPending, StartedAt: now, ScannedAt: now, Roots: []string{src}, ConfiguredMirrors: 1,
		Repairs: []Repair{{RelPath: path, Checksum: checksum, Copies: observations[path]}}}
	if err := Apply(ctx, log, []string{src}, false, io.Discard); err == nil || !strings.Contains(err.Error(), ".b2sum is reserved") {
		t.Fatalf("resume accepted reserved payload name: %v", err)
	}
}
