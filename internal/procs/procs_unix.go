//go:build !linux

package procs

import (
	"os/exec"
	"strconv"
	"strings"
)

// On systems without /proc, ps(1) is the portable way to read another
// process's argv. It is what makes the signature interlock work on macOS and
// the BSDs rather than degrading to "refuse everything".

func readCmdline(pid int) []string {
	out, err := exec.Command("ps", "-ww", "-o", "args=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return nil
	}
	line := strings.TrimSpace(string(out))
	if line == "" {
		return nil
	}
	return strings.Fields(line)
}

// ListProcesses returns every readable process with its argv.
func ListProcesses(excludePids ...int) []Proc {
	skip := skipSet(excludePids)
	out, err := exec.Command("ps", "-axww", "-o", "pid=,args=").Output()
	if err != nil {
		return nil
	}
	var procs []Proc
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil || skip[pid] {
			continue
		}
		procs = append(procs, Proc{Pid: pid, Argv: fields[1:]})
	}
	return procs
}

// ParentOf returns the direct parent of pid, or 0.
func ParentOf(pid int) int {
	out, err := exec.Command("ps", "-o", "ppid=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0
	}
	ppid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0
	}
	return ppid
}

func relations() []relation {
	out, err := exec.Command("ps", "-axo", "pid=,ppid=").Output()
	if err != nil {
		return nil
	}
	var rels []relation
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		pid, err1 := strconv.Atoi(fields[0])
		ppid, err2 := strconv.Atoi(fields[1])
		if err1 != nil || err2 != nil {
			continue
		}
		rels = append(rels, relation{pid: pid, ppid: ppid})
	}
	return rels
}
