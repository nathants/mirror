package ensurecopies

import (
	"context"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/blake2b"
)

func TestEnsureSidecarCollisionFailsCompletePreflight(t *testing.T) {
	sum := blake2b.Sum512([]byte("payload"))
	checksum := hex.EncodeToString(sum[:])
	for _, kind := range []string{"matching", "conflicting", "malformed", "directory"} {
		t.Run(kind, func(t *testing.T) {
			roots := []string{t.TempDir(), t.TempDir()}
			for _, name := range []string{"a", "z"} {
				if err := os.WriteFile(filepath.Join(roots[0], name), []byte("payload"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			contents := checksum
			if kind == "conflicting" {
				contents = strings.Repeat("0", len(checksum))
			} else if kind == "malformed" {
				contents = "surviving malformed evidence"
			}
			path := filepath.Join(roots[1], "z.b2sum")
			var err error
			if kind == "directory" {
				err = os.Mkdir(path, 0700)
			} else {
				err = os.WriteFile(path, []byte(contents), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			rs := testRoots(t, roots)
			actions, err := plan(context.Background(), rs)
			if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "must be absent") {
				t.Fatalf("known sidecar obstruction missed before publication: %d actions, err=%v", len(actions), err)
			}
			if len(actions) != 0 {
				t.Fatalf("failed preflight returned a partial plan: %+v", actions)
			}
			entries, err := os.ReadDir(roots[1])
			if err != nil || len(entries) != 1 || entries[0].Name() != "z.b2sum" {
				t.Fatalf("preflight published earlier data or private state: %v %v", entries, err)
			}
			for _, name := range []string{"a", "z"} {
				if _, err := os.Lstat(filepath.Join(roots[0], name+".b2sum")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("preflight ingested earlier metadata: %v", err)
				}
			}
			if kind != "directory" {
				data, err := os.ReadFile(path)
				if err != nil || string(data) != contents {
					t.Fatalf("preflight changed sidecar evidence: %q %v", data, err)
				}
			}
		})
	}
}

func TestEnsurePublicationStillRejectsSidecarAppearingAfterPlan(t *testing.T) {
	roots := []string{t.TempDir(), t.TempDir()}
	if err := os.WriteFile(filepath.Join(roots[0], "item"), []byte("payload"), 0600); err != nil {
		t.Fatal(err)
	}
	rs := testRoots(t, roots)
	actions, err := plan(context.Background(), rs)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(roots[1], "item.b2sum")
	if err := os.WriteFile(path, []byte("new evidence"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := apply(context.Background(), rs, actions, false, io.Discard); err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("publication trusted stale preflight: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(roots[1], "item")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("publication filled data despite new sidecar: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "new evidence" {
		t.Fatalf("publication overwrote new sidecar: %q %v", data, err)
	}
}

func TestEnsureSymlinkDoesNotReserveAnUnusedSidecarDestination(t *testing.T) {
	roots := []string{t.TempDir(), t.TempDir()}
	if err := os.Symlink("../target", filepath.Join(roots[0], "link")); err != nil {
		t.Fatal(err)
	}
	// This directory is not metadata and does not obstruct the link itself.
	if err := os.Mkdir(filepath.Join(roots[1], "link.b2sum"), 0700); err != nil {
		t.Fatal(err)
	}
	rs := testRoots(t, roots)
	actions, err := plan(context.Background(), rs)
	if err != nil {
		t.Fatal(err)
	}
	if err := apply(context.Background(), rs, actions, false, io.Discard); err != nil {
		t.Fatal(err)
	}
	target, err := os.Readlink(filepath.Join(roots[1], "link"))
	if err != nil || target != "../target" {
		t.Fatalf("link publication failed: %q %v", target, err)
	}
}
