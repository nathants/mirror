package repaircopies

import (
	"context"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
	"mirror/internal/mountdisks"
)

func TestMetadataRepairDoesNotRehashToRediscoverSources(t *testing.T) {
	for _, resume := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh", true: "resume"}[resume], func(t *testing.T) {
			home, base := t.TempDir(), t.TempDir()
			roots := []string{filepath.Join(base, "a"), filepath.Join(base, "b")}
			for _, root := range roots {
				if err := os.Mkdir(root, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, "item"), []byte("payload"), 0600); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(root, 0700) })
			}
			lockTestRoots(t, roots)
			options := Options{
				Snapshot: func(context.Context) (mountdisks.Snapshot, error) {
					return mountdisks.Snapshot{Roots: roots, ConfiguredMirrors: 2}, nil
				},
				Confirm: func(context.Context) error { return nil }, Output: io.Discard,
			}
			if resume {
				if err := Run(context.Background(), home, true, false, options); err != nil {
					t.Fatal(err)
				}
			}
			fd, err := unix.InotifyInit1(unix.IN_NONBLOCK | unix.IN_CLOEXEC)
			if err != nil {
				t.Fatal(err)
			}
			defer unix.Close(fd)
			watches := make(map[int]string)
			for _, root := range roots {
				// Including closes prevents adjacent identical opens from being
				// coalesced. Observe the real implementation, not a hash stub.
				wd, err := unix.InotifyAddWatch(fd, filepath.Join(root, "item"), unix.IN_OPEN|unix.IN_CLOSE_NOWRITE)
				if err != nil {
					t.Fatal(err)
				}
				watches[wd] = root
			}
			if err := Run(context.Background(), home, false, resume, options); err != nil {
				t.Fatal(err)
			}
			counts := make(map[string]int)
			buffer := make([]byte, 65536)
			for {
				n, err := unix.Read(fd, buffer)
				if err == unix.EAGAIN {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				for offset := 0; offset+unix.SizeofInotifyEvent <= n; {
					wd := int(int32(binary.NativeEndian.Uint32(buffer[offset:])))
					mask := binary.NativeEndian.Uint32(buffer[offset+4:])
					length := int(binary.NativeEndian.Uint32(buffer[offset+12:]))
					if mask&unix.IN_Q_OVERFLOW != 0 {
						t.Fatal("inotify queue overflow")
					}
					if mask&unix.IN_OPEN != 0 {
						counts[watches[wd]]++
					}
					offset += unix.SizeofInotifyEvent + length
				}
			}
			// Confirmation/resume validation, one immediate file validation per
			// destination's write window, metadata-publication validation, durable
			// completion, and final verification; fresh runs also scan once.
			limit := 5 + len(roots)
			if resume {
				limit--
			}
			for _, root := range roots {
				if counts[root] == 0 || counts[root] > limit {
					t.Errorf("payload on %s opened %d times, budget %d", filepath.Base(root), counts[root], limit)
				}
				data, err := os.ReadFile(filepath.Join(root, "item.b2sum"))
				if err != nil || string(data) != checksumBytes([]byte("payload")) {
					t.Fatalf("repair did not publish verified metadata: %q %v", data, err)
				}
			}
		})
	}
}
