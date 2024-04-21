package main

import (
	"fmt"
	"github.com/alexflint/go-arg"
	"mirror/lib"
	"os"
)

type ArgsEnsureCopies struct {
	Preview bool `arg:"-p,--preview"`
}

func main() {

	var args ArgsEnsureCopies
	arg.MustParse(&args)

	disks := lib.ScanDisks()

	for disk, files := range disks {
		for path, file := range files {
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
