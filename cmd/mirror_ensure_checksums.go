package main

import (
	"fmt"
	"github.com/alexflint/go-arg"
	"mirror/lib"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type ArgsCleanupChecksums struct {
	Preview bool `arg:"-p,--preview"`
}

func main() {

	var args ArgsCleanupChecksums
	arg.MustParse(&args)

	res := lib.Warn("df -h | grep ^/dev/mapper/sd")
	if res.Err != nil {
		panic(res.Err)
	}
	fmt.Println(res.Stderr)
	for _, line := range strings.Split(res.Stdout, "\n") {
		//fmt.Println(line)
		parts := regexp.MustCompile(` +`).Split(line, 6)
		if len(parts) != 6 {
			fmt.Printf("%#v\n", parts)
			panic("bad split")
		}

		//mapper := parts[0]
		//size := parts[1]
		//used := parts[2]
		//available := parts[3]
		//usedPercent := parts[4]
		mount := parts[5]

		err := filepath.WalkDir(mount, func(path string, d os.DirEntry, err error) error {
			if err != nil && filepath.Base(path) != "lost+found" {
				return err
			}

			if d.IsDir() {
				// dir
				//fmt.Println(mount, "dir:", path)
			} else if d.Type().IsRegular() {
				// file
				if !strings.HasSuffix(path, lib.ChecksumSuffix) {
					checksumPath := path + lib.ChecksumSuffix
					if !lib.FileExists(checksumPath) {
						checksum := lib.Blake2bChecksum(path)
						fmt.Println(lib.PreviewString(args.Preview)+mount, "create checksum:", path)
						fmt.Println(lib.PreviewString(args.Preview)+" ", checksum)
						if !args.Preview {
							err := os.WriteFile(checksumPath, []byte(checksum), 0444)
							if err != nil {
								panic(err)
							}
						}
					}
				}
			} else if d.Type()&os.ModeSymlink != 0 {
				// symlink
				//fmt.Println(mount, "symlink:", path)
			} else {
				panic("unsupported type: " + path)
			}

			return nil
		})

		if err != nil {
			panic(err)
		}

	}
}
