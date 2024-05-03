package main

import (
	"fmt"
	"github.com/alexflint/go-arg"
	"mirror/lib"
	"os"
	"sort"
	"time"
)

type ArgsRepairCopies struct {
	Preview bool `arg:"-p,--preview"`
}

type Checksum struct {
	Checksum string
	Count    int
}

func main() {

	var args ArgsRepairCopies
	arg.MustParse(&args)

	lib.LockDirs(false)
	defer lib.LockDirs(true)

	disks := lib.ScanDisks()

	results := make(chan *lib.File, 128)

	totalCount := 0
	for _, files := range disks {
		for range files {
			totalCount++
		}
	}

	for disk, files := range disks {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					lib.LogRecover(r)
				}
			}()
			for path, file := range files {
				if file.Symlink != nil {
					continue // symlinks have no data to check
				}
				srcPath := disk.AbsPath + "/" + path.RelPath
				checksumActual := lib.Blake2bChecksum(srcPath)
				results <- &lib.File{
					Relpath:        &path.RelPath,
					Disk:           &disk.AbsPath,
					ChecksumActual: &checksumActual,
					ChecksumFile:   file.ChecksumFile,
				}
			}
			results <- nil
		}()
	}

	files := map[lib.Path][]lib.File{}
	workCount := 0
	stopCount := 0
	startTime := time.Now()
	for {
		if stopCount == len(disks) {
			break
		}
		file := <-results
		if file == nil {
			stopCount++
		} else {
			workCount++
			elapsed := time.Since(startTime).Minutes()
			progress := float64(workCount) / float64(totalCount)
			estimatedTotalTime := elapsed / progress
			eta := estimatedTotalTime - elapsed
			fmt.Printf("\r\033[Kscanning: %.1f%% %.1f minutes remaining", progress*100, eta)
			if file.Relpath == nil {
				panic(fmt.Sprint("\nrelpath was nil"))
			}
			if file.Disk == nil {
				panic(fmt.Sprint("\ndisk was nil for: ", *file.Relpath))
			}
			if file.Relpath == nil {
				panic(fmt.Sprint("\nrelpath was nil for: ", *file.Disk+"/"+*file.Relpath))
			}
			if file.ChecksumActual == nil && file.Symlink == nil {
				panic(fmt.Sprint("\nchecksum-actual was nil for: ", *file.Disk+"/"+*file.Relpath))
			}
			if file.ChecksumFile == nil && file.Symlink == nil {
				panic(fmt.Sprint("\nchecksum-file was nil for: ", *file.Disk+"/"+*file.Relpath))
			}
			path := lib.Path{*file.Relpath}
			files[path] = append(files[path], *file)
		}
	}

	toRepair := map[lib.Path][]lib.File{}
	for path, fs := range files {
		counts := map[string]int{}
		for _, file := range fs {
			if *file.Relpath != path.RelPath {
				panic(fmt.Sprintln("path mismatch", *file.Relpath, path.RelPath))
			}
			if file.Symlink != nil {
				continue // symlink has no data to check
			}
			counts[*file.ChecksumActual]++
			counts[*file.ChecksumFile]++
		}
		if len(counts) != 1 {
			toRepair[path] = fs
			fmt.Println()
			fmt.Println(path.RelPath)
			for _, file := range fs {
				fmt.Println("  disk:", *file.Disk)
				fmt.Println("    file:  ", *file.ChecksumFile)
				fmt.Println("    actual:", *file.ChecksumActual)
			}
		}
	}
	if len(toRepair) == 0 {
		elapsed := time.Since(startTime).Minutes()
		fmt.Println("total runtime:", elapsed, "minutes")
		return
	}

	err := lib.PromptProceed("\ngoing to repair these files from the mirror.\n")
	if err != nil {
		panic(err)
	}

	for path, fs := range toRepair {
		counts := map[string]int{}
		var checksums []Checksum
		for _, file := range fs {
			counts[*file.ChecksumActual]++
			counts[*file.ChecksumFile]++
		}
		for checksum, count := range counts {
			checksums = append(checksums, Checksum{
				Checksum: checksum,
				Count:    count,
			})
		}
		sort.Slice(checksums, func(i, j int) bool {
			return checksums[i].Count < checksums[j].Count
		})
		mostCommonChecksum := lib.Last(checksums)
		var fileToRepairFrom *lib.File
		for _, file := range fs {
			if *file.ChecksumActual == mostCommonChecksum.Checksum {
				fileToRepairFrom = &file
			}
		}
		if fileToRepairFrom == nil {
			panic(fmt.Sprintln("no file found to repair from for:", path.RelPath, mostCommonChecksum.Checksum))
		}
		for _, file := range fs {
			if *file.ChecksumActual != mostCommonChecksum.Checksum {
				srcPath := *fileToRepairFrom.Disk + "/" + *fileToRepairFrom.Relpath
				dstPath := *file.Disk + "/" + *file.Relpath
				if lib.FileExists(dstPath) {
					corruptedPath := ""
					count := 0
					for {
						corruptedPath = dstPath + ".corrupted." + string(count)
						if !lib.FileExists(corruptedPath) {
							break
						}
					}
					if !args.Preview {
						err := os.Rename(dstPath, corruptedPath)
						if err != nil {
							panic(err)
						}

					}
					fmt.Println(lib.PreviewString(args.Preview)+"move corrupted file:", dstPath, "to:", corruptedPath)
				}
				if !args.Preview {
					lib.CopyFile(srcPath, dstPath)
				}
				fmt.Println(lib.PreviewString(args.Preview)+"repaired file:", dstPath, "to:", mostCommonChecksum.Checksum)
			}
			if *file.ChecksumFile != mostCommonChecksum.Checksum {
				dstPath := *file.Disk + "/" + *file.Relpath + lib.ChecksumSuffix
				if lib.FileExists(dstPath) {
					corruptedPath := ""
					count := 0
					for {
						corruptedPath = dstPath + ".corrupted." + string(count)
						if !lib.FileExists(corruptedPath) {
							break
						}
					}
					if !args.Preview {
						err := os.Rename(dstPath, corruptedPath)
						if err != nil {
							panic(err)
						}

					}
					fmt.Println(lib.PreviewString(args.Preview)+"move corrupted checksumfile:", dstPath, "to:", corruptedPath)
				}
				if !args.Preview {
					lib.WriteFile(dstPath, mostCommonChecksum.Checksum)
				}
				fmt.Println(lib.PreviewString(args.Preview)+"repaired checksumfile:", dstPath, "to:", mostCommonChecksum.Checksum)
			}
		}

	}

	elapsed := time.Since(startTime).Minutes()
	fmt.Println("total runtime:", elapsed, "minutes")

}
