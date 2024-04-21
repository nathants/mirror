package main

import (
	"fmt"
	"github.com/alexflint/go-arg"
	"mirror/lib"
	"time"
)

type ArgsRepairCopies struct {
	Preview bool `arg:"-p,--preview"`
}

func main() {

	var args ArgsRepairCopies
	arg.MustParse(&args)

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
				srcPath := disk.AbsPath + "/" + path.RelPath
				checksumActual := lib.Blake2bChecksum(srcPath)
				results <- &lib.File{
					Relpath:        &path.RelPath,
					Disk:           &disk.AbsPath,
					ChecksumActual: &checksumActual,
					ChecksumFile:   file.ChecksumFile,
					Symlink:        file.Symlink,
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
		needsRepair := false
		checksumFiles := map[string]int{}
		checksumActuals := map[string]int{}
		for _, file := range fs {
			if *file.Relpath != path.RelPath {
				panic(fmt.Sprintln("path mismatch", *file.Relpath, path.RelPath))
			}
			if file.Symlink != nil {
				continue // symlink has no data to check
			}
			if *file.ChecksumFile != *file.ChecksumActual {
				needsRepair = true
			}
			checksumActuals[*file.ChecksumActual]++
			checksumFiles[*file.ChecksumFile]++
		}
		if len(checksumActuals) != 1 {
			needsRepair = true
		}
		if len(checksumFiles) != 1 {
			needsRepair = true
		}
		if needsRepair {
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
	lib.PromptProceed("going to repair these files from the mirror.")

	for path, fs := range toRepair {
		checksumActuals := map[string]int{}
		checksumFiles := map[string]int{}
		for _, file := range fs {
			checksumActuals[*file.ChecksumActual]++
			checksumFiles[*file.ChecksumFile]++
		}
		if len(checksumActuals) == 1 {
			// checksomeFiles got corrupted, rewrite them
		} else if len(checksumActuals) == 0 {
			panic("unreachable")
		} else {
			// mirrors data has diverged
			if len(disks) == 1 {
				panic("with only 1 mirror we cannot repair")
			} else {
				if len(checksumFiles) == 1 {
					// checksum files agree, find this object and repair from it.
					// TODO logic
				} else {
					// compare checksumFiles and checksumActuals and see if there is a most common checksum.
					// TODO list all scenarios we need to handle here. consider when there are 2 mirrors, 3 mirrors, or n mirrors.
					// TODO logic
				}
			}
			_ = path
		}
	}

}
