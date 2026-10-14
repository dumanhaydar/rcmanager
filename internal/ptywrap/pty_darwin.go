//go:build darwin

package ptywrap

import (
	"os"
	"syscall"
	"unsafe"
)

// Darwin's posix_openpt in three ioctls: grant, unlock, then ask the kernel
// for the slave's name.
const (
	tiocptygrant = 0x20007454
	tiocptyunlk  = 0x20007452
	tiocptygname = 0x40807453
	tiocswinsz   = 0x80087467
)

func openPty() (*os.File, string, error) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, "", err
	}
	if err := ioctl(master.Fd(), tiocptygrant, 0); err != nil {
		_ = master.Close()
		return nil, "", err
	}
	if err := ioctl(master.Fd(), tiocptyunlk, 0); err != nil {
		_ = master.Close()
		return nil, "", err
	}

	var name [128]byte
	if err := ioctl(master.Fd(), tiocptygname, uintptr(unsafe.Pointer(&name[0]))); err != nil {
		_ = master.Close()
		return nil, "", err
	}
	end := 0
	for end < len(name) && name[end] != 0 {
		end++
	}
	return master, string(name[:end]), nil
}

func ioctl(fd, request, arg uintptr) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, request, arg); errno != 0 {
		return errno
	}
	return nil
}

func unsafePointer(v any) uintptr {
	if ws, ok := v.(*winsize); ok {
		return uintptr(unsafe.Pointer(ws))
	}
	return 0
}
