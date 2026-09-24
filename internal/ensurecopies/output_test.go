package ensurecopies

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

type failedOutput struct{ err error }

func (w failedOutput) Write([]byte) (int, error) { return 0, w.err }

func TestApplyStopsOnOutputFailure(t *testing.T) {
	for _, preview := range []bool{true, false} {
		t.Run(map[bool]string{true: "preview", false: "publication"}[preview], func(t *testing.T) {
			root := t.TempDir()
			for _, name := range []string{"a", "b"} {
				if err := os.WriteFile(filepath.Join(root, name), []byte("data"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			rs := testRoots(t, []string{root})
			actions, err := plan(context.Background(), rs)
			if err != nil {
				t.Fatal(err)
			}
			outputErr := errors.New("output unavailable")
			if err := apply(context.Background(), rs, actions, preview, failedOutput{outputErr}); !errors.Is(err, outputErr) {
				t.Fatalf("lost output error: %v", err)
			}
			_, err = os.Stat(filepath.Join(root, "a.b2sum"))
			if preview && !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("preview published: %v", err)
			}
			if !preview && err != nil {
				t.Fatalf("completed publication lost: %v", err)
			}
			if _, err := os.Stat(filepath.Join(root, "b.b2sum")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("continued after output failure: %v", err)
			}
		})
	}
}
