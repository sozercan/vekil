package proxy

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestDurableStateDarwinFilesystemPolicy(t *testing.T) {
	for _, tc := range []struct {
		name  string
		kind  string
		flags uint32
		want  bool
	}{
		{"local_apfs", "apfs", unix.MNT_LOCAL, true},
		{"remote_apfs", "apfs", 0, false},
		{"readonly_apfs", "apfs", unix.MNT_LOCAL | unix.MNT_RDONLY, false},
		{"hfs", "hfs", unix.MNT_LOCAL | unix.MNT_JOURNALED, false},
		{"fat", "msdos", unix.MNT_LOCAL, false},
		{"network", "smbfs", 0, false},
		{"fuse", "macfuse", unix.MNT_LOCAL, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := unix.Statfs_t{Flags: tc.flags}
			copy(fs.Fstypename[:], tc.kind)
			if got := durableStateFilesystemSupported(fs); got != tc.want {
				t.Fatalf("filesystem support = %t, want %t", got, tc.want)
			}
		})
	}
}

func TestDurableStateDarwinRejectsACLs(t *testing.T) {
	for _, target := range []string{"file", "directory"} {
		t.Run(target, func(t *testing.T) {
			s, config := newDurableStoreFixture(t, 4)
			closeDurableStoreFixture(t, s)
			before, err := os.ReadFile(config.Path)
			if err != nil {
				t.Fatal(err)
			}
			path, wantMode := config.Path, os.FileMode(0o600)
			if target == "directory" {
				path, wantMode = filepath.Dir(path), 0o700
			}
			if out, err := exec.Command("/bin/chmod", "+a", "everyone allow read", path).CombinedOutput(); err != nil {
				t.Fatalf("set test ACL: %v: %s", err, out)
			}
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != wantMode {
				t.Fatalf("ACL changed mode bits: %v, %v", info, err)
			}
			if opened, err := newDurableStateBindingStore(config); !errors.Is(err, errDurableStatePath) {
				if opened != nil {
					closeDurableStoreFixture(t, opened)
				}
				t.Fatalf("ACL-bearing %s accepted: %v", target, err)
			}
			after, err := os.ReadFile(config.Path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("rejected store was modified: %v", err)
			}
			if out, err := exec.Command("/bin/chmod", "-N", path).CombinedOutput(); err != nil {
				t.Fatalf("remove test ACL: %v: %s", err, out)
			}
			reopened, err := newDurableStateBindingStore(config)
			if err != nil {
				t.Fatalf("rejected ACL open retained lock: %v", err)
			}
			closeDurableStoreFixture(t, reopened)
		})
	}
	if err := checkDurableStatePermissions(-1); !errors.Is(err, errDurableStatePath) {
		t.Fatalf("invalid permission descriptor = %v", err)
	}
}

func TestDurableStateDarwinFullDirectorySync(t *testing.T) {
	dir, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dir.Close() }()
	if err := syncDurableStateDirectory(int(dir.Fd())); err != nil {
		t.Fatalf("directory full sync = %v", err)
	}
	if err := syncDurableStateDirectory(-1); !errors.Is(err, unix.EBADF) {
		t.Fatalf("invalid directory full sync = %v", err)
	}
}
