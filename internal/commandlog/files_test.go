package commandlog

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestRetentionDeletesOnlyExpiredInactiveLogs(t *testing.T) {
	home := t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)
	cutoff := now.Add(-14 * 24 * time.Hour)
	var warnings bytes.Buffer
	log, err := open(home, os.Geteuid(), os.Getegid(), []string{"lock"}, &warnings, now)
	if err != nil {
		t.Fatal(err)
	}
	activePath := log.file.(*os.File).Name()
	defer log.Close(0)
	if err := os.Chtimes(activePath, cutoff.Add(-time.Hour), cutoff.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, directory)
	info, err := os.Stat(dir)
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("unsafe log directory: %v, %v", info, err)
	}
	newName := func(i int) string {
		return fmt.Sprintf("mirror-%s-%032x.jsonl", now.Format(timestampLayout), i)
	}
	paths := map[string]string{}
	for i, test := range []struct {
		name string
		age  time.Duration
	}{
		{"expired", -time.Second},
		{"boundary", 0},
		{"recent", time.Second},
		{"future", 30 * 24 * time.Hour},
	} {
		path := filepath.Join(dir, newName(i))
		if err := os.WriteFile(path, []byte(test.name), 0600); err != nil {
			t.Fatal(err)
		}
		stamp := cutoff.Add(test.age)
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
		paths[test.name] = path
	}
	outside := filepath.Join(home, "outside")
	if err := os.WriteFile(outside, []byte("unrelated"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(outside, cutoff.Add(-time.Hour), cutoff.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	// Names alone never authorize following links, unlinking hardlinks, or
	// recursively deleting a directory. Unrelated files are not our logs.
	for _, test := range []struct {
		name   string
		create func(string) error
	}{
		{"symlink", func(path string) error { return os.Symlink(outside, path) }},
		{"hardlink", func(path string) error { return os.Link(outside, path) }},
		{"directory", func(path string) error { return os.Mkdir(path, 0700) }},
		{"fifo", func(path string) error { return unix.Mkfifo(path, 0600) }},
		{"unrelated", func(path string) error { return os.WriteFile(path, []byte("keep"), 0600) }},
	} {
		path := filepath.Join(dir, newName(len(paths)))
		if test.name == "unrelated" {
			path = filepath.Join(dir, "notes.jsonl")
		}
		if err := test.create(path); err != nil {
			t.Fatal(err)
		}
		if test.name != "symlink" {
			if err := os.Chtimes(path, cutoff.Add(-time.Hour), cutoff.Add(-time.Hour)); err != nil {
				t.Fatal(err)
			}
		}
		paths[test.name] = path
	}
	// Mandatory recovery evidence is outside the debug retention namespace.
	recovery := filepath.Join(home, ".mirror", "operations", "format-old")
	if err := os.MkdirAll(filepath.Dir(recovery), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(recovery, []byte("retain evidence"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(recovery, cutoff.Add(-time.Hour), cutoff.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	paths["active"] = activePath
	paths["outside"] = outside
	paths["recovery"] = recovery

	next, err := open(home, os.Geteuid(), os.Getegid(), []string{"unlock"}, &warnings, now)
	if err != nil {
		t.Fatal(err)
	}
	next.Close(0)
	if warnings.Len() != 0 {
		t.Fatalf("unexpected warning: %s", &warnings)
	}
	for kind, path := range paths {
		_, err := os.Lstat(path)
		if kind == "expired" {
			if !os.IsNotExist(err) {
				t.Fatalf("expired log survived: %v", err)
			}
		} else if err != nil {
			t.Fatalf("deleted %s: %v", kind, err)
		}
	}
	if got, err := os.ReadFile(outside); err != nil || string(got) != "unrelated" {
		t.Fatalf("touched symlink/hardlink target: %q, %v", got, err)
	}
}

func TestLogDirectoryRejectsUnsafePathsWithoutChangingThem(t *testing.T) {
	for _, kind := range []string{"symlink", "non-private", "different owner", "file", "home symlink"} {
		t.Run(kind, func(t *testing.T) {
			home := t.TempDir()
			path := filepath.Join(home, directory)
			uid := os.Geteuid()
			switch kind {
			case "symlink":
				target := t.TempDir()
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			case "non-private", "different owner":
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
				if kind == "non-private" {
					if err := os.Chmod(path, 0755); err != nil {
						t.Fatal(err)
					}
				} else {
					uid++
				}
			case "file":
				if err := os.WriteFile(path, []byte("do not overwrite"), 0600); err != nil {
					t.Fatal(err)
				}
			case "home symlink":
				alias := filepath.Join(t.TempDir(), "home")
				if err := os.Symlink(home, alias); err != nil {
					t.Fatal(err)
				}
				home = alias
			}
			if log, err := Open(home, uid, os.Getegid(), []string{"lock"}, io.Discard); err == nil {
				log.Close(0)
				t.Fatal("accepted unsafe log directory")
			}
			entries, err := os.ReadDir(home)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "home symlink" {
				if len(entries) != 0 {
					t.Fatal("created a log through home symlink")
				}
			} else {
				if len(entries) != 1 {
					t.Fatalf("unexpected home contents: %v", entries)
				}
				info, err := os.Lstat(path)
				if err != nil {
					t.Fatal(err)
				}
				if kind == "non-private" && info.Mode().Perm() != 0755 {
					t.Fatal("silently changed existing directory permissions")
				}
				if kind == "symlink" && info.Mode()&os.ModeSymlink == 0 {
					t.Fatal("replaced symlink")
				}
				if kind == "file" {
					if data, err := os.ReadFile(path); err != nil || string(data) != "do not overwrite" {
						t.Fatal("changed existing non-directory")
					}
				} else if contents, err := os.ReadDir(path); err != nil || len(contents) != 0 {
					t.Fatalf("wrote into unsafe directory: %v, %v", contents, err)
				}
			}
		})
	}
}

func TestRetentionAndCreationUseValidatedDirectoryDescriptor(t *testing.T) {
	home := t.TempDir()
	now := time.Now()
	dir, err := openDirectory(home, os.Geteuid(), os.Getegid())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dir.Close() }()
	file, err := createFile(dir, os.Geteuid(), os.Getegid(), now)
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Base(file.Name())
	_ = file.Close()
	old := now.Add(-15 * 24 * time.Hour)
	if err := os.Chtimes(file.Name(), old, old); err != nil {
		t.Fatal(err)
	}
	original := filepath.Join(home, "original-logs")
	if err := os.Rename(filepath.Join(home, directory), original); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(home, directory), 0700); err != nil {
		t.Fatal(err)
	}
	replacement := filepath.Join(home, directory, name)
	if err := os.WriteFile(replacement, []byte("replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(replacement, old, old); err != nil {
		t.Fatal(err)
	}
	if err := prune(dir, os.Geteuid(), now.Add(-14*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(original, name)); !os.IsNotExist(err) {
		t.Fatalf("did not prune original: %v", err)
	}
	if data, err := os.ReadFile(replacement); err != nil || string(data) != "replacement" {
		t.Fatalf("touched replacement: %q, %v", data, err)
	}
	created, err := createFile(dir, os.Geteuid(), os.Getegid(), now)
	if err != nil {
		t.Fatal(err)
	}
	_ = created.Close()
	if _, err := os.Stat(filepath.Join(original, filepath.Base(created.Name()))); err != nil {
		t.Fatalf("new log escaped the validated directory: %v", err)
	}
}

func TestConcurrentInvocationsDoNotOverwriteEachOther(t *testing.T) {
	home := t.TempDir()
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			log, err := Open(home, os.Geteuid(), os.Getegid(), []string{"--help"}, io.Discard)
			if err != nil {
				t.Error(err)
				return
			}
			log.Close(0)
		}()
	}
	wg.Wait()
	entries, err := os.ReadDir(filepath.Join(home, directory))
	if err != nil || len(entries) != 10 {
		t.Fatalf("lost concurrent logs: %v, %v", entries, err)
	}
	for _, entry := range entries {
		events := readEvents(t, filepath.Join(home, directory, entry.Name()))
		if len(events) != 2 || events[0].Kind != "start" || events[1].Kind != "finish" {
			t.Fatalf("overwrote/interleaved invocation: %+v", events)
		}
	}
}

func TestRetentionFailureWarnsAndStillRecordsCurrentCommand(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses the unreadable-file fixture")
	}
	home := t.TempDir()
	old, err := Open(home, os.Geteuid(), os.Getegid(), []string{"lock"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	path := old.file.(*os.File).Name()
	old.Close(0)
	stamp := time.Now().Add(-15 * 24 * time.Hour)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	var warnings, stdout bytes.Buffer
	current, err := Open(home, os.Geteuid(), os.Getegid(), []string{"unlock"}, &warnings)
	if err != nil {
		t.Fatalf("retention failure disabled current logging: %v", err)
	}
	if _, err := io.WriteString(current.Writer("stdout", &stdout), "current output"); err != nil {
		t.Fatal(err)
	}
	current.Close(0)
	if !bytes.Contains(warnings.Bytes(), []byte("warning: command log retention:")) || stdout.String() != "current output" {
		t.Fatalf("missing retention warning or command output: %q / %q", &warnings, &stdout)
	}
	events := readEvents(t, current.file.(*os.File).Name())
	if len(events) != 3 || events[1].Text != "current output" || events[2].ExitCode == nil || *events[2].ExitCode != 0 {
		t.Fatalf("retention failure lost current diagnostics: %+v", events)
	}
}
