package formatdisk

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"mirror/internal/actionlog"
	"mirror/internal/operationlock"
)

var errDisplay = errors.New("display unavailable")

type failingDisplay struct {
	match string
	seen  bool
}

func (w *failingDisplay) Write(p []byte) (int, error) {
	if strings.Contains(string(p), w.match) {
		w.seen = true
		return 0, errDisplay
	}
	return len(p), nil
}

func TestFormatOutputFailureStopsBeforeDestruction(t *testing.T) {
	for _, match := range []string{"detected whole disks:", "PATH", "which whole disk", "selected disk:", "crypt name:", "mountpoint:", "WARNING:", "Type the exact device", "confirmed and revalidated"} {
		t.Run(match, func(t *testing.T) {
			fake := newFakeSystem()
			out := &failingDisplay{match: match}
			input := strings.NewReader("/dev/sdz\n/dev/sdz\n")
			err := Run(context.Background(), fake, input, out, io.Discard, Options{HomeDir: "/home/test", PartitionPollAttempts: 2})
			if !out.seen || !errors.Is(err, errDisplay) {
				t.Errorf("lost display failure: seen=%v error=%v", out.seen, err)
			}
			if calls := destructiveCalls(fake.calls); len(calls) != 0 {
				t.Errorf("failed display allowed destruction: %#v", calls)
			}
			if match != "confirmed and revalidated" && input.Len() < len("/dev/sdz\n") {
				t.Error("confirmation consumed despite failed plan/prompt")
			}
		})
	}
}

func TestFormatRetryOutputAndReadFailuresPreserveOpenError(t *testing.T) {
	for _, stage := range []string{"diagnostic", "prompt", "read", "declined"} {
		t.Run(stage, func(t *testing.T) {
			fake := newFakeSystem()
			fake.failOpenCount = 1
			var out, stderr io.Writer = io.Discard, io.Discard
			input := "/dev/sdz\n/dev/sdz\ny\n"
			switch stage {
			case "diagnostic":
				stderr = &failingDisplay{match: "opening LUKS container failed"}
			case "prompt":
				out = &failingDisplay{match: "retry opening"}
			case "read":
				input = "/dev/sdz\n/dev/sdz\n"
			case "declined":
				input = "/dev/sdz\n/dev/sdz\nn\n"
			}
			err := Run(context.Background(), fake, strings.NewReader(input), out, stderr, Options{HomeDir: "/home/test", PartitionPollAttempts: 2})
			var openErr fakeExitError
			if !errors.As(err, &openErr) || openErr.ExitCode() != 2 {
				t.Errorf("original open error was lost: %v", err)
			}
			if (stage == "diagnostic" || stage == "prompt") && !errors.Is(err, errDisplay) {
				t.Errorf("output error was lost: %v", err)
			}
			if stage == "read" && !errors.Is(err, io.EOF) {
				t.Errorf("response error was lost: %v", err)
			}
			if countCalls(fake.calls, "cryptsetup", "open") != 1 || countCalls(fake.calls, "cryptsetup", "luksFormat") != 1 || countCalls(fake.calls, "mkfs.ext4", "") != 0 {
				t.Errorf("failed retry interaction continued storage work: %#v", destructiveCalls(fake.calls))
			}
		})
	}
}

func TestFormatFinalAndDeclineOutputFailuresAreErrors(t *testing.T) {
	for _, decline := range []bool{false, true} {
		fake := newFakeSystem()
		input := "/dev/sdz\n/dev/sdz\n"
		match := "formatted and mounted"
		if decline {
			input = "/dev/sdz\nno\n"
			match = "aborting"
		}
		out := &failingDisplay{match: match}
		err := Run(context.Background(), fake, strings.NewReader(input), out, io.Discard, Options{HomeDir: "/home/test", PartitionPollAttempts: 2})
		if !out.seen || !errors.Is(err, errDisplay) || errors.Is(err, ErrDeclined) {
			t.Errorf("output failure hidden as success/silent decline: %v", err)
		}
		if !decline && (!fake.mapperOpen || !fake.mounted || countCalls(fake.calls, "umount", "") != 0) {
			t.Error("failed final output rolled back completed storage work")
		}
	}
}

func TestFormatFinalOutputFailureIsJournaledAsUncertain(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	lock, err := operationlock.Acquire(ctx, home, os.Getuid(), os.Getgid())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Close() }()
	log, err := actionlog.Open(home, "format", os.Getuid(), os.Getgid(), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = log.Close() }()
	fake := newFakeSystem()
	observed := &actionlog.System{Backend: fake, Log: log, Context: ctx}
	err = Run(ctx, observed, strings.NewReader("/dev/sdz\n/dev/sdz\n"), &failingDisplay{match: "formatted and mounted"}, io.Discard, Options{HomeDir: "/home/test", PartitionPollAttempts: 2})
	if !errors.Is(err, errDisplay) {
		t.Fatalf("missing final output error: %v", err)
	}
	if err := observed.Finish(err); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(log.Path())
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	var result struct{ Kind, Status, Error string }
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &result); err != nil {
		t.Fatal(err)
	}
	if result.Kind != "operation" || result.Status != "failed/uncertain" || !strings.Contains(result.Error, errDisplay.Error()) {
		t.Fatalf("output failure journaled as successful: %+v", result)
	}
	if !fake.mapperOpen || !fake.mounted || countCalls(fake.calls, "umount", "") != 0 {
		t.Fatal("diagnostics rolled back state after final output failure")
	}
}
