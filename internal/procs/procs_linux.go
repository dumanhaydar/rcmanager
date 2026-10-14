//go:build linux

package procs

import (
	"os"
	"strconv"
	"strings"
)

func readCmdline(pid int) []string {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil {
		return nil
	}
	parts := []string{}
	for _, p := range strings.Split(string(raw), "\x00") {
		if p != "" {
			parts = append(parts, p)
		}
	}
	if len(parts) == 0 {
		return nil
	}
	return parts
}

// ListProcesses returns every readable process with its argv.
func ListProcesses(excludePids ...int) []Proc {
	skip := skipSet(excludePids)
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var out []Proc
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || skip[pid] {
			continue
		}
		if argv := readCmdline(pid); len(argv) > 0 {
			out = append(out, Proc{Pid: pid, Argv: argv})
		}
	}
	return out
}

// ParentOf returns the direct parent of pid, or 0.
func ParentOf(pid int) int {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0
	}
	// comm can contain spaces and parens, so parse after the final ')'.
	s := string(raw)
	i := strings.LastIndex(s, ")")
	if i == -1 || i+2 >= len(s) {
		return 0
	}
	fields := strings.Fields(s[i+2:])
	if len(fields) < 2 {
		return 0
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0
	}
	return ppid
}

func relations() []relation {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var out []relation
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if ppid := ParentOf(pid); ppid != 0 {
			out = append(out, relation{pid: pid, ppid: ppid})
		}
	}
	return out
}
