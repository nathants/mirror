package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

func main() {
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		fatal("could not locate check.go")
	}
	repo := filepath.Clean(filepath.Join(filepath.Dir(sourceFile), ".."))
	if _, err := exec.LookPath("libcheck"); err != nil {
		fatal("libcheck not found; install it with: go install github.com/nathants/libcheck@latest")
	}

	checks := [][]string{
		{"libcheck", "check"},
		{"make", "build"},
		{"go", "test", "./..."},
		{"go", "test", "-race", "./..."},
		// Standalone automation is build-constrained to avoid multiple mains in
		// bin. Explicit file arguments keep it covered without executing a VM.
		{"go", "test", "./bin/bake-e2e-image.go"},
		{"go", "test", "-race", "./bin/bake-e2e-image.go"},
		{"go", "mod", "verify"},
		{"git", "diff", "--check"},
	}
	for _, check := range checks {
		fmt.Printf("==> %s\n", strings.Join(check, " "))
		command := exec.Command(check[0], check[1:]...)
		command.Dir = repo
		command.Env = fastTestEnvironment()
		command.Stdin = os.Stdin
		command.Stdout = os.Stdout
		command.Stderr = os.Stderr
		if err := command.Run(); err != nil {
			fatal("%s: %v", strings.Join(check, " "), err)
		}
	}

	entries, err := os.ReadDir(filepath.Join(repo, "bin"))
	if err != nil {
		fatal("read bin: %v", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		path := filepath.Join(repo, "bin", entry.Name())
		info, err := entry.Info()
		if err != nil {
			fatal("stat %s: %v", path, err)
		}
		if !info.Mode().IsRegular() {
			fatal("unexpected non-regular entry in bin: %s", path)
		}
		if info.Mode()&0o111 == 0 {
			fatal("thin wrapper is not executable: %s", path)
		}
		fmt.Printf("==> bash -n bin/%s\n", entry.Name())
		command := exec.Command("bash", "-n", path)
		command.Dir = repo
		command.Stdout = os.Stdout
		command.Stderr = os.Stderr
		if err := command.Run(); err != nil {
			fatal("bash -n %s: %v", path, err)
		}
	}
	fmt.Println("PASS")
}

func fastTestEnvironment() []string {
	blocked := map[string]struct{}{
		"MIRROR_E2E_VM":   {},
		"MIRROR_E2E_KEEP": {},
	}
	var environment []string
	for _, value := range os.Environ() {
		name, _, _ := strings.Cut(value, "=")
		if _, found := blocked[name]; !found {
			environment = append(environment, value)
		}
	}
	return environment
}

func fatal(format string, arguments ...any) {
	fmt.Fprintf(os.Stderr, "fatal: "+format+"\n", arguments...)
	os.Exit(1)
}
