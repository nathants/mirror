package main

import (
	"github.com/alexflint/go-arg"
	"mirror/lib"
)

type ArgsUnlockCopies struct {
}

func main() {

	var args ArgsUnlockCopies
	arg.MustParse(&args)

	lib.LockDirs(false)

}
