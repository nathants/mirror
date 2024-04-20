package main

import (
	"fmt"
	"mirror/lib"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/alexflint/go-arg"
)

type ArgsEnsureChecksums struct {
	Preview bool `arg:"-p,--preview"`
}

func main() {

	var args ArgsEnsureChecksums
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
				if strings.HasSuffix(path, lib.ChecksumSuffix) {
					filePath := strings.TrimSuffix(path, lib.ChecksumSuffix)
					if !lib.FileExists(filePath) {
						if !args.Preview {
							err := os.Remove(path)
							if err != nil {
								panic(err)
							}
						}
						fmt.Println(lib.PreviewString(args.Preview)+"rm checksum without file:", path)
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
