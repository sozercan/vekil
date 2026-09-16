package proxy

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Install a one-way syscall restriction only in an owned disposable child.
// This exercises real bbolt page-write/fdatasync errors without touching mounts,
// a shared filesystem's free space, production processes, or store sync options.
func durableFixtureFailSyscall(number uint32) error {
	filter := []unix.SockFilter{
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 4},
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: unix.AUDIT_ARCH_X86_64, Jt: 1},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_KILL_PROCESS},
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 0},
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: number, Jf: 1},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ERRNO | uint32(unix.EIO)},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ALLOW},
	}
	program := unix.SockFprog{Len: uint16(len(filter)), Filter: &filter[0]}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return err
	}
	_, _, errno := unix.Syscall(unix.SYS_SECCOMP, unix.SECCOMP_SET_MODE_FILTER, unix.SECCOMP_FILTER_FLAG_TSYNC, uintptr(unsafe.Pointer(&program)))
	runtime.KeepAlive(filter)
	if errno != 0 {
		return errno
	}
	return nil
}

func TestDurableStateSyscallFaultChild(t *testing.T) {
	kind := os.Getenv("VEKIL_TEST_STATE_SYSCALL_FAULT")
	if kind == "" {
		return
	}
	s, err := newDurableStateBindingStore(DurableStateBindingsConfig{Path: os.Getenv("VEKIL_TEST_STATE_FILE"), MaxEntries: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer closeDurableStoreFixture(t, s)
	syscallNumber := uint32(unix.SYS_FDATASYNC)
	if kind == "close" {
		syscallNumber = unix.SYS_CLOSE
	} else if kind == "write" {
		syscallNumber = unix.SYS_PWRITE64
	} else if kind != "sync" {
		t.Fatal("unknown fault")
	}
	if err := durableFixtureFailSyscall(syscallNumber); err != nil {
		t.Fatalf("cannot install child-only fault: %v", err)
	}
	if kind == "close" {
		if err := s.close(); !errors.Is(err, errDurableStateIO) {
			t.Fatalf("real close fault = %v", err)
		}
		if r := s.lookup(stateBindingTypeResponseID, "prior-committed"); !errors.Is(r.err, errDurableStateClosed) {
			t.Fatal("close failure left store usable")
		}
		return
	}
	if r := s.bindAll([]stateBindingToken{{stateBindingTypeResponseID, "failed-transaction"}}, durableFixtureOwner()); !errors.Is(r.err, errDurableStateIO) {
		t.Fatalf("real syscall fault = %+v", r)
	}
	if r := s.lookup(stateBindingTypeResponseID, "prior-committed"); !errors.Is(r.err, errDurableStateIO) {
		t.Fatal("uncertain store did not freeze")
	}
}

func TestDurableStateRealStorageSyscallFailures(t *testing.T) {
	for _, kind := range []string{"write", "sync", "close"} {
		t.Run(kind, func(t *testing.T) {
			s, config := newDurableStoreFixture(t, 4)
			if r := s.bindAll([]stateBindingToken{{stateBindingTypeResponseID, "prior-committed"}}, durableFixtureOwner()); r.err != nil {
				t.Fatal(r.err)
			}
			if err := s.close(); err != nil {
				t.Fatal(err)
			}
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, exe, "-test.run=^TestDurableStateSyscallFaultChild$", "-test.timeout=8s")
			cmd.Env = []string{"GOMAXPROCS=2", "VEKIL_TEST_STATE_SYSCALL_FAULT=" + kind, "VEKIL_TEST_STATE_FILE=" + config.Path}
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("isolated syscall fault: %v\n%s", err, out)
			}
			reopened, err := newDurableStateBindingStore(config)
			if err != nil {
				t.Fatal(err)
			}
			defer closeDurableStoreFixture(t, reopened)
			if r := reopened.lookup(stateBindingTypeResponseID, "prior-committed"); r.err != nil || r.outcome != stateBindingLookupKnown {
				t.Fatal("storage failure lost prior proof")
			}
			if r := reopened.lookup(stateBindingTypeResponseID, "failed-transaction"); r.err != nil || r.outcome != stateBindingLookupUnknown {
				t.Fatal("failed transaction published new proof")
			}
		})
	}
}
