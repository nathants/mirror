package main

import (
	"fmt"
	"os"
	"os/exec"
)

func main() {
	if os.Geteuid() != 0 {
		fatal("image preparation must run as root")
	}
	commands := [][]string{
		{"apt-get", "update"},
		{"apt-get", "-y", "dist-upgrade"},
		{"apt-get", "install", "-y", "cryptsetup"},
		{"apt-get", "clean"},
	}
	for _, command := range commands {
		fmt.Printf("==> %v\n", command)
		cmd := exec.Command(command[0], command[1:]...)
		cmd.Env = append(os.Environ(), "DEBIAN_FRONTEND=noninteractive", "LC_ALL=C")
		cmd.Stdin = os.Stdin
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			fatal("%v failed: %v", command, err)
		}
	}
	if err := os.RemoveAll("/var/lib/apt/lists"); err != nil {
		fatal("remove apt lists: %v", err)
	}
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "fatal: "+format+"\n", args...)
	os.Exit(1)
}
