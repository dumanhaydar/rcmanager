package procs

import "os"

// skipSet is rcmanager's own pid plus whatever the caller excludes, so a scan
// never reports the process doing the scanning.
func skipSet(exclude []int) map[int]bool {
	skip := map[int]bool{os.Getpid(): true}
	for _, pid := range exclude {
		if pid > 0 {
			skip[pid] = true
		}
	}
	return skip
}
