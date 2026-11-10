package proxy

import (
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

func checkDurableStateFilesystem(fd int) error {
	var fs unix.Statfs_t
	if unix.Fstatfs(fd, &fs) != nil {
		return errDurableStatePath
	}
	if !durableStateFilesystemSupported(fs) {
		return errDurableStatePlatform
	}
	// bbolt calls os.File.Sync for both data and metadata commits. On Darwin,
	// Go uses F_FULLFSYNC but may fall back to fsync on unsupported storage.
	// Verify full-sync support on each actual descriptor before bbolt opens it.
	if err := syncDurableStateDirectory(fd); err != nil {
		if err == unix.ENOTSUP || err == unix.EINVAL {
			return errDurableStatePlatform
		}
		return errDurableStateIO
	}
	return nil
}

func durableStateFilesystemSupported(fs unix.Statfs_t) bool {
	return fs.Flags&unix.MNT_LOCAL != 0 && fs.Flags&unix.MNT_RDONLY == 0 && unix.ByteSliceToString(fs.Fstypename[:]) == "apfs"
}

func syncDurableStateDirectory(fd int) error {
	for {
		_, err := unix.FcntlInt(uintptr(fd), unix.F_FULLFSYNC, 0)
		if err != unix.EINTR {
			return err
		}
	}
}

func checkDurableStatePermissions(fd int) error {
	// Darwin ACLs can grant access independently of 0700/0600 mode bits. Require
	// an absent ACL on both the private directory and the database descriptor.
	// x/sys has no fgetattrlist wrapper; the stable Darwin syscall avoids a
	// path lookup and keeps CGO-disabled builds supported.
	attrs := unix.Attrlist{Bitmapcount: unix.ATTR_BIT_MAP_COUNT, Commonattr: unix.ATTR_CMN_EXTENDED_SECURITY}
	// An absent ACL returns only this attribute-reference header. REPORT_FULLSIZE
	// makes a nonempty ACL exceed it even though we need not read its entries.
	var result struct {
		length uint32
		offset int32
		size   uint32
	}
	for {
		//nolint:staticcheck // SA1019: x/sys has no fgetattrlist wrapper for CGO-free descriptor ACL checks.
		_, _, errno := unix.Syscall6(unix.SYS_FGETATTRLIST, uintptr(fd), uintptr(unsafe.Pointer(&attrs)), uintptr(unsafe.Pointer(&result)), unsafe.Sizeof(result), unix.FSOPT_REPORT_FULLSIZE, 0)
		runtime.KeepAlive(&attrs)
		runtime.KeepAlive(&result)
		if errno == unix.EINTR {
			continue
		}
		if errno != 0 || result.length != uint32(unsafe.Sizeof(result)) || result.size != 0 {
			return errDurableStatePath
		}
		return nil
	}
}
