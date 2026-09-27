package mountdisks

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
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

func TestMountWorkflowsStopOnOutputFailure(t *testing.T) {
	for _, operation := range []string{"mount", "unmount"} {
		for _, stage := range []string{"missing", "heading", "table", "prompt", "decline", "success"} {
			t.Run(operation+"/"+stage, func(t *testing.T) {
				fake := newFakeSystem()
				fake.mapperOpen = operation == "unmount"
				fake.mounted = operation == "unmount"
				reads := 0
				option := options(t, func() ([]byte, error) { reads++; return []byte("secret"), nil }, fake.identifier, "a74a2821-168e-4475-87a9-ab6f2e4e9714")
				input := strings.NewReader(operation + "\n")
				matches := map[string]string{"missing": "drive not found", "heading": "going to", "table": "DISK", "prompt": "to confirm:", "decline": "aborting", "success": "mounted /dev/"}
				if operation == "unmount" {
					matches["success"] = "closed /dev/"
				}
				writer := &failingDisplay{match: matches[stage]}
				var out, stderr io.Writer = writer, io.Discard
				if stage == "missing" {
					out, stderr = io.Discard, writer
				}
				if stage == "decline" {
					input = strings.NewReader("no\n")
				}
				run := Mount
				if operation == "unmount" {
					run = Unmount
				}
				err := run(context.Background(), fake, input, out, stderr, option)
				if !writer.seen || !errors.Is(err, errDisplay) || errors.Is(err, ErrDeclined) {
					t.Errorf("display failure hidden as success/silent decline: seen=%v error=%v", writer.seen, err)
				}
				if stage != "success" {
					if len(mutatingCalls(fake.calls)) != 0 || reads != 0 {
						t.Errorf("failed display allowed password/mutation: reads=%d calls=%#v", reads, mutatingCalls(fake.calls))
					}
					if stage != "decline" && input.Len() != len(operation+"\n") {
						t.Error("confirmation consumed despite failed display")
					}
				} else if fake.mapperOpen != (operation == "mount") || fake.mounted != (operation == "mount") {
					t.Error("failed final output rolled back completed state")
				}
			})
		}
	}
}
