package lib

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"golang.org/x/crypto/blake2b"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

const (
	Timeout        = 15 * time.Minute
	ChecksumSuffix = ".b2sum"
)

type WarnResult struct {
	Stdout string
	Stderr string
	Err    error
}

func Warn(format string, args ...interface{}) *WarnResult {
	str := fmt.Sprintf(format, args...)
	str = fmt.Sprintf("set -eou pipefail; %s", str)
	cmd := exec.Command("bash", "-c", str)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	result := make(chan *WarnResult)
	go func() {
		// defer func() {}()
		err := cmd.Run()
		result <- &WarnResult{
			strings.TrimRight(stdout.String(), "\n"),
			strings.TrimRight(stderr.String(), "\n"),
			err,
		}
	}()
	select {
	case r := <-result:
		return r
	case <-time.After(Timeout):
		_ = cmd.Process.Kill()
		return &WarnResult{
			"",
			"",
			errors.New("cmd Timeout"),
		}
	}
}

func Last[T any](s []T) T {
	if len(s) == 0 {
		panic("empty")
	}
	return s[len(s)-1]
}

func FileExists(filename string) bool {
	info, err := os.Stat(filename)
	if os.IsNotExist(err) {
		return false
	}
	return !info.IsDir()
}

func Blake2bChecksum(filePath string) string {
	file, err := os.Open(filePath)
	if err != nil {
		panic(err)
	}

	defer file.Close()

	hasher, err := blake2b.New512(nil)
	if err != nil {
		panic(err)
	}

	_, err = io.Copy(hasher, file)
	if err != nil {
		panic(err)
	}

	hash := hasher.Sum(nil)

	return hex.EncodeToString(hash)
}

func PreviewString(preview bool) string {
	if !preview {
		return ""
	}
	return "preview: "
}
