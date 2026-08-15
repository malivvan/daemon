// Package daemon provides utilities to run a Go program as a background daemon process.
package daemon

import (
	"os"
	"path/filepath"
)

var _path = func() string {
	path, err := filepath.Abs(os.Args[0])
	if err != nil {
		return ""
	}
	return path
}()

func Wait(pid int) {
	process, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	_, err = process.Wait()
	if err != nil {
		return
	}
}
