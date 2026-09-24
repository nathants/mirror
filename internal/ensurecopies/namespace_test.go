package ensurecopies

import (
	"context"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/blake2b"
)

func TestEnsureRejectsChecksumOnlyPathWithoutDataSource(t *testing.T) {
	sum := blake2b.Sum512([]byte("missing data"))
	for _, contents := range []string{"application payload", hex.EncodeToString(sum[:])} {
		t.Run(contents[:8], func(t *testing.T) {
			roots := []string{t.TempDir(), t.TempDir()}
			path := filepath.Join(roots[0], "database.b2sum")
			if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
				t.Fatal(err)
			}
			rs := testRoots(t, roots)
			actions, err := plan(context.Background(), rs)
			if err == nil || !strings.Contains(err.Error(), ".b2sum is reserved") || !strings.Contains(err.Error(), "database") || !strings.Contains(err.Error(), "no data source") {
				t.Fatalf("checksum-named payload silently omitted: actions=%+v err=%v", actions, err)
			}
			if len(actions) != 0 {
				t.Fatalf("failed preflight returned actions: %+v", actions)
			}
			data, err := os.ReadFile(path)
			if err != nil || string(data) != contents {
				t.Fatalf("preflight changed evidence: %q %v", data, err)
			}
			entries, err := os.ReadDir(roots[1])
			if err != nil || len(entries) != 0 {
				t.Fatalf("preflight mutated destination: %v %v", entries, err)
			}
		})
	}
}

func TestEnsureRejectsChecksumNamedSymlink(t *testing.T) {
	roots := []string{t.TempDir(), t.TempDir()}
	if err := os.Symlink("../target", filepath.Join(roots[0], "link.b2sum")); err != nil {
		t.Fatal(err)
	}
	if _, err := plan(context.Background(), testRoots(t, roots)); err == nil || !strings.Contains(err.Error(), ".b2sum is reserved") {
		t.Fatalf("checksum-named symlink accepted as payload: %v", err)
	}
}
