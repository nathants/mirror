package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mirror/internal/actionlog"
	"mirror/internal/operationlock"
)

func TestDiskOperationLogsRemainInsideCallerLock(t *testing.T) {
	home := t.TempDir()
	caller := callerIdentity{HomeDir: home, UID: os.Getuid(), GID: os.Getgid()}
	var output bytes.Buffer
	path := ""
	err := withLoggedDiskOperation(context.Background(), caller, "mount", &output, func(observed *actionlog.System) error {
		lock, err := operationlock.Acquire(context.Background(), home, os.Getuid(), os.Getgid())
		if lock != nil {
			lock.Close()
		}
		if err == nil {
			t.Fatal("action logging ran outside the operation lock")
		}
		path = observed.Log.Path()
		return observed.MkdirAll(filepath.Join(home, "test-mountpoint"), 0700)
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{`"kind":"operation"`, `"kind":"action"`, `"name":"mkdir"`, `"status":"started"`, `"status":"succeeded"`} {
		if !strings.Contains(string(data), text) {
			t.Fatalf("missing %s: %s", text, data)
		}
	}
	if !strings.Contains(output.String(), path) {
		t.Fatal("caller was not told where the retained log lives")
	}
	lock, err := operationlock.Acquire(context.Background(), home, os.Getuid(), os.Getgid())
	if err != nil {
		t.Fatal(err)
	}
	lock.Close()
}
