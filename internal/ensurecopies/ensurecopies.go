// Package ensurecopies copies missing files and checksum sidecars across mounted mirror disks.
package ensurecopies

import (
	"fmt"
	"os"

	"mirror/lib"
)

func Run(preview bool) error {
	disks := lib.ScanDisks()

	lib.LockDirs(false)
	defer lib.LockDirs(true)

	for disk, files := range disks {
		for path, file := range files {
			for destinationDisk, destinationFiles := range disks {
				if disk == destinationDisk {
					continue
				}
				if _, exists := destinationFiles[path]; exists {
					continue
				}

				sourcePath := disk.AbsPath + "/" + path.RelPath
				destinationPath := destinationDisk.AbsPath + "/" + path.RelPath
				switch {
				case file.ChecksumFile != nil:
					checksumPath := path.RelPath + lib.ChecksumSuffix
					checksumSourcePath := disk.AbsPath + "/" + checksumPath
					checksumDestinationPath := destinationDisk.AbsPath + "/" + checksumPath
					if !lib.FileExists(checksumSourcePath) {
						return fmt.Errorf("checksum does not exist: %s", checksumSourcePath)
					}
					if !preview {
						lib.EnsureDirs(destinationPath)
						lib.CopyFile(sourcePath, destinationPath)
						lib.CopyFile(checksumSourcePath, checksumDestinationPath)
					}
					fmt.Println(
						lib.PreviewString(preview)+"copied file",
						path.RelPath,
						"to",
						lib.UseTilda(destinationDisk.AbsPath),
					)
				case file.Symlink != nil:
					target, err := os.Readlink(sourcePath)
					if err != nil {
						return fmt.Errorf("read symlink %s: %w", sourcePath, err)
					}
					if !preview {
						lib.EnsureDirs(destinationPath)
						lib.CopySymlink(sourcePath, destinationPath)
					}
					fmt.Println(
						lib.PreviewString(preview)+"copied symlink",
						path.RelPath,
						"->",
						target,
						"to",
						lib.UseTilda(destinationDisk.AbsPath),
					)
				default:
					return fmt.Errorf("unsupported scanned file: %s", path.RelPath)
				}
			}
		}
	}
	return nil
}
