package main

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

func main() {
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		fatal("could not locate check.go")
	}
	repo := filepath.Clean(filepath.Join(filepath.Dir(sourceFile), ".."))

	var goFiles []string
	if err := filepath.WalkDir(repo, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() && entry.Name() == ".git" {
			return filepath.SkipDir
		}
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".go") {
			relative, err := filepath.Rel(repo, path)
			if err != nil {
				return err
			}
			goFiles = append(goFiles, relative)
		}
		return nil
	}); err != nil {
		fatal("find Go files: %v", err)
	}
	sort.Strings(goFiles)
	format := exec.Command("gofmt", append([]string{"-l"}, goFiles...)...)
	format.Dir = repo
	var unformatted bytes.Buffer
	format.Stdout = &unformatted
	format.Stderr = os.Stderr
	if err := format.Run(); err != nil {
		fatal("gofmt: %v", err)
	}
	if unformatted.Len() != 0 {
		fatal("Go files need formatting:\n%s", unformatted.String())
	}

	checks := [][]string{
		{"go", "test", "./..."},
		{"go", "test", "-race", "./..."},
		{"go", "vet", "./..."},
		{"staticcheck", "./..."},
		{"go", "mod", "verify"},
		{"go", "mod", "tidy", "-diff"},
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

	const wrapperPrefix = "#!/bin/bash\nset -euo pipefail\ncd -- \"$(dirname -- \"${BASH_SOURCE[0]}\")/..\"\n"
	expectedWrappers := map[string]string{
		"bake-e2e-image": wrapperPrefix + "exec go run ./cmd/bake-e2e-image \"$@\"\n",
		"mirror":         wrapperPrefix + "exec ./mirror \"$@\"\n",
		"mirror-build":   wrapperPrefix + "exec go build -trimpath -o ./mirror ./cmd/mirror\n",
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
		expected, found := expectedWrappers[entry.Name()]
		if !found {
			fatal("unexpected executable in bin: %s", path)
		}
		delete(expectedWrappers, entry.Name())
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
		data, err := os.ReadFile(path)
		if err != nil {
			fatal("read %s: %v", path, err)
		}
		if string(data) != expected {
			fatal("bin/%s is not the required thin wrapper", entry.Name())
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
	if len(expectedWrappers) != 0 {
		var missing []string
		for name := range expectedWrappers {
			missing = append(missing, name)
		}
		sort.Strings(missing)
		fatal("missing required bin wrappers: %s", strings.Join(missing, ", "))
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
