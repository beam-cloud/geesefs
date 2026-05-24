package fuseops

import (
	"os"
	"syscall"
)

// ConvertFileMode returns an os.FileMode with the Go mode and permission bits
// set according to the Linux mode and permission bits.
func ConvertFileMode(unixMode uint32) os.FileMode {
	mode := os.FileMode(unixMode & 0777)
	switch unixMode & syscall.S_IFMT {
	case syscall.S_IFREG:
		// nothing
	case syscall.S_IFDIR:
		mode |= os.ModeDir
	case syscall.S_IFCHR:
		mode |= os.ModeCharDevice | os.ModeDevice
	case syscall.S_IFBLK:
		mode |= os.ModeDevice
	case syscall.S_IFIFO:
		mode |= os.ModeNamedPipe
	case syscall.S_IFLNK:
		mode |= os.ModeSymlink
	case syscall.S_IFSOCK:
		mode |= os.ModeSocket
	default:
		// no idea
	}
	if unixMode&syscall.S_ISUID != 0 {
		mode |= os.ModeSetuid
	}
	if unixMode&syscall.S_ISGID != 0 {
		mode |= os.ModeSetgid
	}
	if unixMode&syscall.S_ISVTX != 0 {
		mode |= os.ModeSticky
	}
	return mode
}

// ConvertGoMode returns an integer with the Linux mode and permission bits
// set according to the Go mode and permission bits.
func ConvertGoMode(inMode os.FileMode) uint32 {
	outMode := uint32(inMode) & 0777
	switch {
	default:
		outMode |= syscall.S_IFREG
	case inMode&os.ModeDir != 0:
		outMode |= syscall.S_IFDIR
	case inMode&os.ModeDevice != 0:
		if inMode&os.ModeCharDevice != 0 {
			outMode |= syscall.S_IFCHR
		} else {
			outMode |= syscall.S_IFBLK
		}
	case inMode&os.ModeNamedPipe != 0:
		outMode |= syscall.S_IFIFO
	case inMode&os.ModeSymlink != 0:
		outMode |= syscall.S_IFLNK
	case inMode&os.ModeSocket != 0:
		outMode |= syscall.S_IFSOCK
	}
	if inMode&os.ModeSetuid != 0 {
		outMode |= syscall.S_ISUID
	}
	if inMode&os.ModeSetgid != 0 {
		outMode |= syscall.S_ISGID
	}
	if inMode&os.ModeSticky != 0 {
		outMode |= syscall.S_ISVTX
	}
	return outMode
}
