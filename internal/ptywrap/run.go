package ptywrap

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"syscall"
)

// Run is the wrapper process: allocate a pty, run the command on it, and pump
// bytes between the pty and this process's own stdio.
//
// It returns the exit code to leave with, so a crash-looping agent still looks
// like a crash-looping agent to the supervisor.
func Run(argv []string) int {
	// Everything after `--` is the real command.
	for i, a := range argv {
		if a == "--" {
			argv = argv[i+1:]
			break
		}
	}
	if len(argv) == 0 {
		fmt.Fprintln(os.Stderr, "rcmanager "+Subcommand+": nothing to run")
		return 2
	}

	master, slaveName, err := openPty()
	if err != nil {
		fmt.Fprintln(os.Stderr, "rcmanager "+Subcommand+": could not allocate a pty: "+err.Error())
		return 1
	}
	defer master.Close()

	slave, err := os.OpenFile(slaveName, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		fmt.Fprintln(os.Stderr, "rcmanager "+Subcommand+": could not open "+slaveName+": "+err.Error())
		return 1
	}

	setWinsize(master, envInt("LINES", 40), envInt("COLUMNS", 120))

	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	// The child leads a new session with the pty as its controlling terminal:
	// that is what makes a TUI believe it is talking to a person.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}

	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "rcmanager "+Subcommand+": "+err.Error())
		_ = slave.Close()
		return 1
	}
	// Only the child needs the slave; holding it here would keep the pty from
	// ever reporting end-of-file.
	_ = slave.Close()

	// A signal to the wrapper is meant for the agent: pass it on rather than
	// dying and leaving the child behind.
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	go func() {
		for sig := range signals {
			if s, ok := sig.(syscall.Signal); ok && cmd.Process != nil {
				_ = syscall.Kill(-cmd.Process.Pid, s)
			}
		}
	}()

	// stdin -> pty. rcmanager's fifo never reaches end-of-file by design, so this
	// copy lives as long as the process does.
	go func() { _, _ = io.Copy(master, os.Stdin) }()

	// pty -> stdout. This is the copy that matters on the way out: it has to
	// drain whatever the agent printed before exiting, so the supervisor's
	// capture sees the last lines.
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(os.Stdout, master)
	}()

	err = cmd.Wait()
	// The read side ends when the child closes the pty; give it that chance
	// rather than racing the process exit.
	_ = master.Close()
	<-done

	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if ok := asExitError(err, &exitErr); ok {
		if code := exitErr.ExitCode(); code >= 0 {
			return code
		}
		return 1
	}
	fmt.Fprintln(os.Stderr, "rcmanager "+Subcommand+": "+err.Error())
	return 1
}

func asExitError(err error, out **exec.ExitError) bool {
	if e, ok := err.(*exec.ExitError); ok {
		*out = e
		return true
	}
	return false
}

func envInt(name string, fallback int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil && v > 0 {
		return v
	}
	return fallback
}

type winsize struct {
	rows, cols, xpixel, ypixel uint16
}

// setWinsize tells the pty how big it is. A TUI that thinks the terminal is
// 0x0 renders nothing at all.
func setWinsize(f *os.File, rows, cols int) {
	ws := winsize{rows: uint16(rows), cols: uint16(cols)}
	_, _, _ = syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), uintptr(tiocswinsz), uintptr(unsafePointer(&ws)))
}
