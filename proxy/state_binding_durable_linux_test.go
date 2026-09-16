//go:build linux

package proxy

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

func TestDurableStateFilesystemUsesDescriptor(t *testing.T) {
	s, config := newDurableStoreFixture(t, 4)
	closeDurableStoreFixture(t, s)
	file, err := os.Open(config.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	if err := checkDurableStateFilesystem(int(file.Fd())); err != nil {
		t.Fatalf("local file filesystem = %v", err)
	}
	// A real unsupported regular-file descriptor, without mounting anything or
	// requiring privileged access. Integration below substitutes only the
	// primitive's result to model a bind-mounted file under an allowed directory.
	proc, err := os.Open("/proc/self/stat")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = proc.Close() }()
	if err := checkDurableStateFilesystem(int(proc.Fd())); !errors.Is(err, errDurableStatePlatform) {
		t.Fatalf("proc file filesystem = %v", err)
	}
	if err := checkDurableStateFilesystem(-1); !errors.Is(err, errDurableStatePath) {
		t.Fatalf("failed filesystem query = %v", err)
	}
}

func TestDurableStateFilesystemChecksBothFileOpens(t *testing.T) {
	for _, failAt := range []int{1, 2} {
		for _, failure := range []error{errDurableStatePlatform, errDurableStatePath} {
			t.Run(fmt.Sprintf("file-open=%d/%v", failAt, failure), func(t *testing.T) {
				s, config := newDurableStoreFixture(t, 4)
				closeDurableStoreFixture(t, s)
				before, err := os.ReadFile(config.Path)
				if err != nil {
					t.Fatal(err)
				}
				fileChecks, rejectedFD := 0, -1
				check := func(fd int) error {
					var stat unix.Stat_t
					if err := unix.Fstat(fd, &stat); err != nil {
						t.Fatal(err)
					}
					if stat.Mode&unix.S_IFMT == unix.S_IFREG {
						fileChecks++
						if fileChecks == failAt {
							rejectedFD = fd
							return failure
						}
					}
					return checkDurableStateFilesystem(fd)
				}
				db, _, syncDirectory, openErr := openDurableStateDatabaseWithFilesystemCheck(config.Path, false, check)
				if db != nil {
					_ = syncDirectory()
					_ = db.Close()
				}
				if fileChecks != failAt || !errors.Is(openErr, failure) {
					t.Fatalf("file checks=%d want=%d err=%v want=%v", fileChecks, failAt, openErr, failure)
				}
				var stat unix.Stat_t
				if err := unix.Fstat(rejectedFD, &stat); !errors.Is(err, unix.EBADF) {
					t.Fatalf("rejected file descriptor leaked: %v", err)
				}
				after, err := os.ReadFile(config.Path)
				if err != nil || !bytes.Equal(before, after) {
					t.Fatalf("rejected store was modified: %v", err)
				}
				reopened, err := newDurableStateBindingStore(config)
				if err != nil {
					t.Fatalf("rejected open retained lock: %v", err)
				}
				defer closeDurableStoreFixture(t, reopened)
			})
		}
	}
}
