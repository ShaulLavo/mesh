package tui

import "os"

func quietTerminal(*os.File) func() { return func() {} }
