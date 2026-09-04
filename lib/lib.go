package lib

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	"github.com/pkg/term"
	"golang.org/x/crypto/blake2b"
	"mirror/internal/mountdisks"
	"mirror/internal/system"
)

const (
	commandTimeout = 15 * time.Minute
	ChecksumSuffix = ".b2sum"
)

// MountedMirrorRoots returns the currently mounted roots selected by the configured disk PTUUIDs.
func MountedMirrorRoots() []string {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		panic(fmt.Errorf("determine home directory: %w", err))
	}
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	roots, _, err := mountdisks.MountedRoots(ctx, system.Exec{}, filepath.Join(homeDir, ".mirror", "disks"), homeDir, "/dev/mapper")
	if err != nil {
		panic(err)
	}
	if len(roots) == 0 {
		panic("no configured mirror filesystems are mounted")
	}
	return roots
}

func Last[T any](s []T) T {
	if len(s) == 0 {
		panic("empty")
	}
	return s[len(s)-1]
}

func FileExists(filename string) bool {
	info, err := os.Stat(filename)
	if err != nil {
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

func CopySymlink(src, dst string) {
	target, err := os.Readlink(src)
	if err != nil {
		panic(err)
	}
	err = os.Symlink(target, dst)
	if err != nil {
		panic(err)
	}
}

func CopyFile(src, dst string) {
	EnsureChmod(dst, 0744)
	sourceFile, err := os.Open(src)
	if err != nil {
		panic(err)
	}
	defer sourceFile.Close()
	destFile, err := os.Create(dst)
	if err != nil {
		panic(err)
	}

	defer destFile.Close()
	_, err = io.Copy(destFile, sourceFile)
	if err != nil {
		panic(err)
	}
	EnsureChmod(dst, 0444)
}

var home string

func UseTilda(path string) string {
	if home == "" {
		usr, err := user.Current()
		if err != nil {
			panic(err)
		}
		home = usr.HomeDir
	}
	return strings.Replace(path, home, "~", 1)
}

func EnsureDirs(path string) {
	err := os.MkdirAll(filepath.Dir(path), 0755)
	if err != nil {
		panic(err)
	}
}

type Disk struct {
	AbsPath string
}

type Path struct {
	RelPath string
}

type File struct {
	ChecksumFile   *string
	ChecksumActual *string
	Symlink        *string
	Disk           *string
	Relpath        *string
}

func EnsureChmod(path string, mode os.FileMode) {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		panic(err)
	}
	if info.Mode().Perm() != mode {
		err := os.Chmod(path, mode)
		if err != nil {
			panic(err)
		}
	}
}

func LockDirs(locked bool) {
	for _, disk := range MountedMirrorRoots() {
		if locked {
			EnsureChmod(disk, 0544)
		} else {
			EnsureChmod(disk, 0744)
		}

		err := filepath.WalkDir(disk, func(absPath string, d os.DirEntry, err error) error {

			if err != nil && filepath.Base(absPath) != "lost+found" {
				return err
			}
			if d.IsDir() && filepath.Base(absPath) != "lost+found" {
				if locked {
					EnsureChmod(absPath, 0544)
				} else {
					EnsureChmod(absPath, 0744)
				}
			}
			return nil
		})
		if err != nil {
			panic(err)
		}

	}
}

func ScanDisks() map[Disk]map[Path]File {
	disks := map[Disk]map[Path]File{}
	for _, disk := range MountedMirrorRoots() {
		disks[Disk{disk}] = make(map[Path]File)

		err := filepath.WalkDir(disk, func(absPath string, d os.DirEntry, err error) error {

			if err != nil && filepath.Base(absPath) != "lost+found" {
				return err
			}

			if d.IsDir() {
				// dir
			} else if d.Type().IsRegular() {
				// file
				if !strings.HasSuffix(absPath, ChecksumSuffix) {
					EnsureChmod(absPath, 0444)
					checksumAbsPath := absPath + ChecksumSuffix
					if !FileExists(checksumAbsPath) {
						checksum := Blake2bChecksum(absPath)
						WriteFile(checksumAbsPath, checksum)
						fmt.Println("created checksum file:", absPath, checksum)
					}
					EnsureChmod(checksumAbsPath, 0444)
					data, err := os.ReadFile(checksumAbsPath)
					if err != nil {
						panic(err)
					}
					checksum := string(data)
					relPath := strings.Replace(absPath, disk+"/", "", 1)
					disks[Disk{disk}][Path{relPath}] = File{
						ChecksumFile: &checksum,
					}
				}
			} else if d.Type()&os.ModeSymlink != 0 {
				// symlink
				link, err := os.Readlink(absPath)
				if err != nil {
					panic(err)
				}
				if !strings.HasPrefix(link, "../") {
					panic("only relative symlinks are supported, not: " + link)
				}
				relPath := strings.Replace(absPath, disk+"/", "", 1)
				disks[Disk{disk}][Path{relPath}] = File{
					Symlink: &link,
				}
			} else {
				panic("unsupported type: " + absPath)
			}
			return nil
		})

		if err != nil {
			panic(err)
		}

	}

	return disks

}

func LogRecover(r interface{}) {
	stack := string(debug.Stack())
	fmt.Println(r)
	fmt.Println(stack)
	panic(r)
}

// ErrPromptInterrupted reports Control-C while reading a raw confirmation byte.
var ErrPromptInterrupted = errors.New("prompt interrupted")

func getch() (string, error) {
	t, err := term.Open("/dev/tty")
	if err != nil {
		return "", err
	}
	err = term.RawMode(t)
	if err != nil {
		_ = t.Close()
		return "", err
	}
	defer func() {
		_ = t.Restore()
		_ = t.Close()
	}()
	return readPromptByte(t)
}

func readPromptByte(reader io.Reader) (string, error) {
	var one [1]byte
	n, err := reader.Read(one[:])
	if err != nil {
		return "", err
	}
	if n != 1 {
		return "", io.ErrNoProgress
	}
	if one[0] == 3 {
		return "", ErrPromptInterrupted
	}
	return string(one[:]), nil
}

func PromptProceed(prompt string) error {
	fmt.Println(prompt)
	fmt.Println("proceed? y/n")
	ch, err := getch()
	if err != nil {
		return err
	}
	if ch != "y" {
		return fmt.Errorf("prompt failed")
	}
	return nil
}

func WriteFile(path string, value string) {
	if FileExists(path) {
		EnsureChmod(path, 0644)
	}
	err := os.WriteFile(path, []byte(value), 0444)
	if err != nil {
		panic(err)
	}
}
