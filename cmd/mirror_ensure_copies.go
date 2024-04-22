package main

import (
	"fmt"
	"github.com/alexflint/go-arg"
	"mirror/lib"
	"os"
	"time"
)

type ArgsEnsureCopies struct {
	Preview bool `arg:"-p,--preview"`
}

func main() {

	var args ArgsEnsureCopies
	arg.MustParse(&args)

	lib.LockDirs(false)
	defer lib.LockDirs(true)

	disks := lib.ScanDisks()

	totalCount := 0
	for _, files := range disks {
		for range files {
			totalCount++
		}
	}

	workCount := 0
	startTime := time.Now()
	lastTime := time.Now()

	for disk, files := range disks {
		for path, file := range files {

			workCount++
			if time.Since(lastTime) > time.Second*1 {
				lastTime = time.Now()
				elapsed := time.Since(startTime).Minutes()
				progress := float64(workCount) / float64(totalCount)
				estimatedTotalTime := elapsed / progress
				eta := estimatedTotalTime - elapsed
				fmt.Printf("\r\033[Kscanning: %.1f%% %.1f minutes remaining", progress*100, eta)
			}

			for dstDisk, dstFiles := range disks {

				if disk == dstDisk {
					continue
				}

				_, ok := dstFiles[path]
				if !ok {

					if file.ChecksumFile != nil {

						checksumPath := path.RelPath + lib.ChecksumSuffix
						checksumSrcPath := disk.AbsPath + "/" + checksumPath
						checksumDstPath := dstDisk.AbsPath + "/" + checksumPath

						if !lib.FileExists(checksumSrcPath) {
							panic("checksum does not exist: " + checksumSrcPath)
						}

						srcPath := disk.AbsPath + "/" + path.RelPath
						dstPath := dstDisk.AbsPath + "/" + path.RelPath

						if !args.Preview {
							lib.EnsureDirs(dstPath)
							lib.CopyFile(srcPath, dstPath)
							lib.CopyFile(checksumSrcPath, checksumDstPath)
						}

						fmt.Println(
							lib.PreviewString(args.Preview)+"copied file",
							path.RelPath,
							"to",
							lib.UseTilda(dstDisk.AbsPath),
						)

					} else if file.Symlink != nil {

						srcPath := disk.AbsPath + "/" + path.RelPath
						dstPath := dstDisk.AbsPath + "/" + path.RelPath

						target, err := os.Readlink(srcPath)
						if err != nil {
							fmt.Println("failed to read symlink", srcPath)
							panic(err)
						}

						if !args.Preview {
							lib.EnsureDirs(dstPath)
							lib.CopySymlink(srcPath, dstPath)
						}

						fmt.Println(
							lib.PreviewString(args.Preview)+"copied symlink",
							path.RelPath,
							"->",
							target,
							"to",
							lib.UseTilda(dstDisk.AbsPath),
						)

					} else {
						panic("unreachable for: " + path.RelPath)
					}
				}

			}
		}
	}
}
