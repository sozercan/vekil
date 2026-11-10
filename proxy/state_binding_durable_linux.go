package proxy

import "golang.org/x/sys/unix"

func checkDurableStateFilesystem(fd int) error {
	var fs unix.Statfs_t
	if unix.Fstatfs(fd, &fs) != nil {
		return errDurableStatePath
	}
	switch fs.Type {
	case unix.EXT4_SUPER_MAGIC, unix.XFS_SUPER_MAGIC, unix.BTRFS_SUPER_MAGIC, unix.TMPFS_MAGIC, unix.OVERLAYFS_SUPER_MAGIC:
		return nil
	default:
		return errDurableStatePlatform
	}
}

// Linux's ACL mask is reflected in the group mode bits. The shared 0700/0600
// checks therefore exclude access by named ACL users and groups as well.
func checkDurableStatePermissions(int) error { return nil }

func syncDurableStateDirectory(fd int) error {
	for {
		err := unix.Fsync(fd)
		if err != unix.EINTR {
			return err
		}
	}
}
