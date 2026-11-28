//go:build linux

package core

import (
	"os"
	"strconv"
	"strings"
	"syscall"
)

// killProcessGroup signals both the session leader's process group (-pid) and any other
// processes sharing the same POSIX session ID (sid == pid), such as background jobs created
// under interactive shell job control.
func killProcessGroup(pid int, sig syscall.Signal) error {
	if pid <= 0 {
		return nil
	}
	err := syscall.Kill(-pid, sig)
	for _, member := range sessionPIDs(pid) {
		_ = syscall.Kill(member, sig)
	}
	return err
}

func processGroupAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	if err := syscall.Kill(-pid, 0); err == nil || err == syscall.EPERM {
		return true
	}
	return len(sessionPIDs(pid)) > 0
}

// sessionPIDs returns all PIDs whose /proc/<pid>/stat session id equals sid.
func sessionPIDs(sid int) []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var out []int
	for _, ent := range entries {
		if !ent.IsDir() {
			continue
		}
		p, err := strconv.Atoi(ent.Name())
		if err != nil || p <= 1 {
			continue
		}
		data, err := os.ReadFile("/proc/" + ent.Name() + "/stat")
		if err != nil {
			continue
		}
		// Format: pid (comm) state ppid pgrp session ...
		// comm may contain spaces or parentheses, so split after the last ')'.
		idx := strings.LastIndexByte(string(data), ')')
		if idx < 0 || idx+2 >= len(data) {
			continue
		}
		fields := strings.Fields(string(data[idx+2:]))
		// fields[0]=state, [1]=ppid, [2]=pgrp, [3]=session
		if len(fields) >= 4 {
			if s, err := strconv.Atoi(fields[3]); err == nil && s == sid {
				out = append(out, p)
			}
		}
	}
	return out
}
