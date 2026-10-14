//go:build linux

package ptywrap

import (
	"os"
	"strconv"
	"syscall"
	"unsafe"
)

// Linux's posix_openpt: unlock the pair, then ask for its number.
const (
	tiocsptlck = 0x40045431
	tiocgptn   = 0x80045430
	tiocswinsz = 0x5414
)

func openPty() (*os.File, string, error) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, "", err
	}
	unlock := int32(0)
	if err := ioctl(master.Fd(), tiocsptlck, uintptr(unsafe.Pointer(&unlock))); err != nil {
		_ = master.Close()
		return nil, "", err
	}
	var n uint32
	if err := ioctl(master.Fd(), tiocgptn, uintptr(unsafe.Pointer(&n))); err != nil {
		_ = master.Close()
		return nil, "", err
	}
	return master, "/dev/pts/" + strconv.Itoa(int(n)), nil
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
