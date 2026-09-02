//go:build linux

package main

// driver_proctable_linux.go — the `process` driver's query against the runtime, which on Linux is
// /proc. See driver.go for the rule this exists to satisfy and driver_process.go for what it means
// here.

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
)

// labelledProc is one process carrying a Worker label. ppid is read because a label is INHERITED,
// so the scan has to be able to tell a half from its own children (driver_process.go).
type labelledProc struct {
	pid   int
	ppid  int
	label string
}

// labelledProcs walks /proc and returns every process whose environment carries workerLabelVar.
//
// THE ENVIRONMENT IS READ FROM THE KERNEL, not from anything kontra wrote: /proc/<pid>/environ is
// the region execve set up, which is why a process cannot be talked out of its label and why one
// that died has none.
//
// Unreadable is skipped, silently and deliberately. A process owned by another user answers EACCES
// and a process that exited between the ReadDir and the ReadFile answers ESRCH; neither is an error
// about this machine, and failing the whole scan on one of them would make `list` fail more often
// the busier the box is.
func labelledProcs() ([]labelledProc, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	prefix := []byte(workerLabelVar + "=")
	var out []labelledProc
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("/proc", e.Name(), "environ"))
		if err != nil || len(raw) == 0 {
			continue
		}
		var label string
		// environ is NUL-separated, with a trailing NUL. A variable's VALUE may contain anything
		// except NUL, so splitting on it is exact where splitting on newlines or spaces is not.
		for _, kv := range bytes.Split(raw, []byte{0}) {
			if bytes.HasPrefix(kv, prefix) {
				label = string(kv[len(prefix):])
				break
			}
		}
		if label == "" {
			continue
		}
		out = append(out, labelledProc{pid: pid, ppid: parentOf(e.Name()), label: label})
	}
	return out, nil
}

// parentOf reads PPid from /proc/<pid>/status.
//
// status AND NOT stat, which is the field-index answer everyone reaches for first and is wrong here:
// stat's second field is the executable name in parentheses, unquoted, and it may contain spaces and
// close-parens — so `Fields(stat)[3]` is the ppid only for processes whose name is well behaved. A
// worker's is `python3` today and could be anything tomorrow.
//
// 0 when it cannot be read: an unknown parent is never equal to a label, so the process is treated
// as a root, which is the safe direction — a half reported twice is visible, a half not reported at
// all is the invisible failure.
func parentOf(pid string) int {
	raw, err := os.ReadFile(filepath.Join("/proc", pid, "status"))
	if err != nil {
		return 0
	}
	for _, line := range bytes.Split(raw, []byte{'\n'}) {
		if !bytes.HasPrefix(line, []byte("PPid:")) {
			continue
		}
		ppid, err := strconv.Atoi(string(bytes.TrimSpace(line[len("PPid:"):])))
		if err != nil {
			return 0
		}
		return ppid
	}
	return 0
}
