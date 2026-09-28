package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

type invocation struct {
	Directory string
	Arguments []string
}

// The test executable doubles as a harmless go stub. TestMain runs before
// testing parses flags, so even empty and flag-like arguments are opaque.
func TestMain(m *testing.M) {
	if os.Getenv("MIRROR_TEST_WRAPPER_STUB") == "1" {
		cwd, err := os.Getwd()
		if err == nil {
			err = json.NewEncoder(os.Stdout).Encode(invocation{cwd, os.Args[1:]})
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(37)
	}
	os.Exit(m.Run())
}

func TestBakeE2EImageWrapperForwardsWorkingDirectoryArgumentsAndExitStatus(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	stub, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	repo := filepath.Join(base, "repository with spaces [and glob characters]")
	stubDir := filepath.Join(base, "stubs")
	outside := filepath.Join(base, "unrelated working directory")
	for _, dir := range []string{filepath.Join(repo, "bin"), stubDir, outside} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(stubDir, "go"), stub, 0700); err != nil {
		t.Fatal(err)
	}
	// Execute the checked-in wrapper, unchanged, in a disposable layout. The
	// go stub shadows the real toolchain, so no image bake can run here.
	data, err := os.ReadFile("bake-e2e-image")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(repo, "bin", "bake-e2e-image")
	if err := os.WriteFile(path, data, 0700); err != nil {
		t.Fatal(err)
	}
	for _, relative := range []bool{false, true} {
		for _, args := range [][]string{nil, {"", "two words", "line\nbreak", "'\"$HOME", "*?[abc]", "--flag", "back\\slash"}} {
			t.Run(fmt.Sprintf("relative=%v/args=%d", relative, len(args)), func(t *testing.T) {
				program := path
				if relative {
					relativePath, err := filepath.Rel(outside, path)
					if err != nil {
						t.Fatal(err)
					}
					program = relativePath
				}
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				cmd := exec.CommandContext(ctx, program, args...)
				cmd.Dir = outside
				cmd.Env = append(os.Environ(), "MIRROR_TEST_WRAPPER_STUB=1", "PATH="+stubDir+":"+os.Getenv("PATH"))
				output, err := cmd.CombinedOutput()
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() != 37 {
					t.Fatalf("wrapper did not propagate stub exit: %v, output=%q", err, output)
				}
				var got invocation
				if err := json.Unmarshal(output, &got); err != nil {
					t.Fatalf("stub invocation missing: %v, output=%q", err, output)
				}
				want := append([]string{"run", "./bin/bake-e2e-image.go"}, args...)
				if got.Directory != repo || !slices.Equal(got.Arguments, want) {
					t.Fatalf("invocation=%+v; want directory=%q args=%q", got, repo, want)
				}
			})
		}
	}
}
