package actionlog

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
)

func TestFailureDiagnosticsValidateSchemaAndRetainRawEvidence(t *testing.T) {
	for _, test := range []struct {
		name, blocks, mounts, status string
		failure                      error
	}{
		{"null blocks and empty mounts", `{"blockdevices":null}`, `{"filesystems":[]}`, "unknown", nil},
		{"missing schema", `{}`, `{}`, "unknown", nil},
		{"invalid paths", `{"blockdevices":[{"path":"relative"}]}`, `{"filesystems":[{"target":"relative"}]}`, "unknown", nil},
		{"malformed JSON", `{"blockdevices":`, `not JSON`, "unknown", nil},
		{"valid but ineligible", `{"blockdevices":[{"path":"/dev/loop0","type":"loop","ro":true}]}`, `{"filesystems":[{"source":"tmpfs","target":"/"}]}`, "observed", nil},
		{"command failure with output", `{"blockdevices":[]}`, `{"filesystems":[{"source":"tmpfs","target":"/"}]}`, "unknown", errors.New("inventory exited unsuccessfully")},
	} {
		t.Run(test.name, func(t *testing.T) {
			log, _ := openTestLog(t, "mount", io.Discard)
			actionErr := errors.New("mount result uncertain")
			system := &System{Log: log, Context: context.Background(), Backend: fakeBackend{
				run: func(context.Context, io.Reader, string, []string) error { return actionErr },
				observation: func(_ context.Context, name string) ([]byte, error) {
					data := test.blocks
					if name == "findmnt" {
						data = test.mounts
					}
					return []byte(data), test.failure
				},
			}}
			if err := system.Run(context.Background(), nil, io.Discard, io.Discard, "mount", "/dev/mapper/test", "/mirror/test"); !errors.Is(err, actionErr) {
				t.Fatalf("lost uncertain action error: %v", err)
			}
			seen := 0
			for _, event := range readEvents(t, log.Path()) {
				if event.Kind != "failure-diagnostic" {
					continue
				}
				seen++
				if event.Status != test.status || (event.Error != "") != (test.status == "unknown") {
					t.Errorf("incorrect observation classification: %+v", event)
				}
				want := test.blocks
				if event.Name == "findmnt" {
					want = test.mounts
				}
				if json.Valid([]byte(want)) {
					if string(event.Data) != want || event.Text != "" {
						t.Errorf("lost structured evidence: %+v", event)
					}
				} else if event.Text != want || len(event.Data) != 0 {
					t.Errorf("lost raw invalid evidence: %+v", event)
				}
			}
			if seen != 2 {
				t.Fatalf("missing diagnostics: %d", seen)
			}
		})
	}
}
