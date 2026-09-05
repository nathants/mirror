//go:build ignore

// Run with go run ./bin/bake-e2e-image.go; checked explicitly by bin/check.go.
package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

const imageName = "mirror-e2e"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() (returnErr error) {
	if len(os.Args) != 1 {
		return fmt.Errorf("usage: bake-e2e-image")
	}
	if _, err := exec.LookPath("vm"); err != nil {
		return fmt.Errorf("vm command is unavailable: %w", err)
	}
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		return fmt.Errorf("could not locate source repository")
	}
	repo := filepath.Clean(filepath.Join(filepath.Dir(sourceFile), ".."))
	work, err := os.MkdirTemp("", "mirror-e2e-image.")
	if err != nil {
		return fmt.Errorf("create temporary directory: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(work); err != nil && returnErr == nil {
			returnErr = fmt.Errorf("remove temporary directory: %w", err)
		}
	}()

	prep := filepath.Join(work, "mirror-e2e-imageprep")
	buildEnv := overrideEnvironment(map[string]string{
		"CGO_ENABLED": "0",
		"GOOS":        "linux",
		"GOARCH":      "amd64",
	})
	if err := runCommand(repo, buildEnv, nil, "go", "build", "-trimpath", "-o", prep, "./tests/e2e/imageprep"); err != nil {
		return err
	}

	builder := "mirror-e2e-build-" + strconv.Itoa(os.Getpid())
	created := false
	defer func() {
		if !created {
			return
		}
		fmt.Fprintf(os.Stderr, "cleaning up builder VM %s\n", builder)
		command := exec.Command("vm", "rm", builder)
		command.Stdout = os.Stderr
		command.Stderr = os.Stderr
		if err := command.Run(); err != nil && returnErr == nil {
			returnErr = fmt.Errorf("clean up builder VM %s: %w", builder, err)
		}
	}()

	if err := runCommand(repo, nil, nil, "vm", "new", "--image", "sid", "--no-ephemeral", "--mem", "2048", "--cpus", "2", "--disk", "8", builder); err != nil {
		return err
	}
	created = true
	commands := []struct {
		name string
		args []string
	}{
		{name: "vm", args: []string{"wait-ssh", "--timeout", "120", builder}},
		{name: "vm", args: []string{"scp", builder, prep, ":/tmp/mirror-e2e-imageprep"}},
		{name: "vm", args: []string{"ssh", "--cmd", "sudo -n -- /tmp/mirror-e2e-imageprep", builder}},
		{name: "vm", args: []string{"stop", builder}},
		{name: "vm", args: []string{"bake", "--replace", "--name", imageName, builder}},
		{name: "vm", args: []string{"rm", builder}},
	}
	for _, command := range commands {
		if err := runCommand(repo, nil, nil, command.name, command.args...); err != nil {
			return err
		}
	}
	created = false
	fmt.Printf("baked VM image %q\n", imageName)
	return nil
}

func runCommand(directory string, environment []string, stdin io.Reader, name string, args ...string) error {
	fmt.Printf("==> %s %v\n", name, args)
	command := exec.Command(name, args...)
	command.Dir = directory
	if environment != nil {
		command.Env = environment
	}
	command.Stdin = stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("%s %v failed: %w", name, args, err)
	}
	return nil
}

func overrideEnvironment(overrides map[string]string) []string {
	environment := make([]string, 0, len(os.Environ())+len(overrides))
	for _, value := range os.Environ() {
		name, _, _ := strings.Cut(value, "=")
		if _, replaced := overrides[name]; !replaced {
			environment = append(environment, value)
		}
	}
	for name, value := range overrides {
		environment = append(environment, name+"="+value)
	}
	return environment
}
