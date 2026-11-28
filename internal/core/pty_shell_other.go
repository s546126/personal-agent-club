//go:build !unix

package core

import (
	"os"
	"syscall"
)

func killProcessGroup(pid int, sig syscall.Signal) error {
	if pid <= 0 {
		return nil
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Signal(sig)
}

func processGroupAlive(pid int) bool {
	return false
}
