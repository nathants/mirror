package ensurecopies

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
	"mirror/internal/mountdisks"
)

func TestRunChecksLocksBeforeReadingPayloads(t *testing.T) {
	for _, work := range []string{"writes", "no writes"} {
		for _, unlocked := range []string{".", "books"} {
			t.Run(work+"/"+unlocked, func(t *testing.T) {
				roots := workflowRoots(t)
				lockTestRoots(t, roots)
				options := Options{
					Snapshot: func(context.Context) (mountdisks.Snapshot, error) {
						return mountdisks.Snapshot{Roots: roots, ConfiguredMirrors: len(roots)}, nil
					},
					Output: io.Discard,
				}
				if work == "no writes" {
					if err := Run(context.Background(), false, options); err != nil {
						t.Fatal(err)
					}
				}
				writable := filepath.Join(roots[1], unlocked)
				if err := os.Chmod(writable, 0744); err != nil {
					t.Fatal(err)
				}
				fd, err := unix.InotifyInit1(unix.IN_NONBLOCK | unix.IN_CLOEXEC)
				if err != nil {
					t.Fatal(err)
				}
				defer unix.Close(fd)
				if _, err := unix.InotifyAddWatch(fd, filepath.Join(roots[0], "books/item"), unix.IN_OPEN); err != nil {
					t.Fatal(err)
				}
				if err := Run(context.Background(), false, options); err == nil || !strings.Contains(err.Error(), "run mirror lock") {
					t.Errorf("unlocked input not rejected: %v", err)
				}
				// A mode preflight must not open payloads for the checksum scan.
				if n, err := unix.Read(fd, make([]byte, 4096)); !errors.Is(err, unix.EAGAIN) {
					t.Errorf("payload opened before lock rejection: event bytes=%d error=%v", n, err)
				}
				requireMode(t, writable, 0744)
				requireMode(t, roots[0], 0544)
				if work == "writes" {
					requireAbsent(t, filepath.Join(roots[0], "books/item.b2sum"))
					requireAbsent(t, filepath.Join(roots[1], "books/item"))
				}
			})
		}
	}
}
