package main

import (
	"github.com/alexflint/go-arg"
	"mirror/lib"
)

type ArgsLockCopies struct {
}

func main() {

	var args ArgsLockCopies
	arg.MustParse(&args)

	lib.LockDirs(true)

}
