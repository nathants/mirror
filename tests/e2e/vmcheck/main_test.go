package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCheckProbeDoesNotRecreateOrRewriteData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "probe")
	if err := checkProbe(path); err == nil {
		t.Error("missing persisted payload was recreated instead of rejected")
	}
	if err := os.WriteFile(path, []byte("wrong persisted data"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := checkProbe(path); err == nil {
		t.Error("corrupt persisted payload was overwritten instead of rejected")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "wrong persisted data" {
		t.Errorf("verification rewrote data: %q", data)
	}
}
