package server

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
)

// Only listener handoff is paused. Successful binds and close operations still
// cross the real TCP boundary; the production Start/Stop transitions are used.
type startupListenBarrier struct {
	entered    chan struct{}
	release    chan struct{}
	once       sync.Once
	calls      atomic.Int32
	beforeBind bool
	ctx        context.Context
	addr       string
	listenErr  error
}

func newStartupListenBarrier(t *testing.T, beforeBind bool) *startupListenBarrier {
	t.Helper()
	b := &startupListenBarrier{entered: make(chan struct{}), release: make(chan struct{}), beforeBind: beforeBind}
	t.Cleanup(b.unblock)
	return b
}

func (b *startupListenBarrier) unblock() { b.once.Do(func() { close(b.release) }) }

func (b *startupListenBarrier) listen(ctx context.Context, network, addr string) (net.Listener, error) {
	call := b.calls.Add(1)
	if call == 1 && b.beforeBind {
		b.ctx = ctx
		close(b.entered)
		<-b.release
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, network, addr)
	if call != 1 {
		// A broken owner transition must fail an assertion, not panic later
		// by having two Serve goroutines close the same public Done channel.
		if ln != nil {
			_ = ln.Close()
		}
		return nil, errors.Join(errors.New("unexpected second listener attempt"), err)
	}
	b.listenErr = err
	if ln != nil {
		b.addr = ln.Addr().String()
	}
	if !b.beforeBind {
		b.ctx = ctx
		close(b.entered)
		<-b.release
	}
	return ln, err
}

func awaitStartupResult(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("startup operation did not return")
		return nil
	}
}

func TestStartConcurrentTransition(t *testing.T) {
	for _, mode := range []string{"dynamic", "fixed"} {
		t.Run(mode, func(t *testing.T) {
			port := "0"
			if mode == "fixed" {
				reservation, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				port = strconv.Itoa(reservation.Addr().(*net.TCPAddr).Port)
				_ = reservation.Close()
			}
			srv, err := New(auth.NewTestAuthenticator("synthetic-token"), logger.NewWithWriter(logger.LevelError, io.Discard), "127.0.0.1", port)
			if err != nil {
				t.Fatal(err)
			}
			barrier := newStartupListenBarrier(t, false)
			srv.listen = barrier.listen
			t.Cleanup(func() {
				barrier.unblock()
				_ = srv.Stop(context.Background())
			})
			first := make(chan error, 1)
			go func() { first <- srv.Start() }()
			select {
			case <-barrier.entered:
			case <-time.After(2 * time.Second):
				t.Fatal("first Start did not reach real listener handoff")
			}
			second := make(chan error, 1)
			go func() { second <- srv.Start() }()
			if err := awaitStartupResult(t, second); err == nil {
				t.Error("overlapping Start was not rejected")
			}
			if got := barrier.calls.Load(); got != 1 {
				t.Errorf("listener attempts = %d, want one owning Start", got)
			}
			if srv.proxyHandler.ShuttingDown() {
				t.Error("losing Start shut down the owning generation")
			}
			barrier.unblock()
			if err := awaitStartupResult(t, first); err != nil {
				t.Fatal(err)
			}
			client := &http.Client{Timeout: time.Second}
			resp, err := client.Get("http://" + srv.Addr() + "/healthz")
			if err != nil {
				t.Fatalf("owning listener is not usable: %v", err)
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("health = %d", resp.StatusCode)
			}
			if err := srv.Stop(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := awaitStartupResult(t, srv.Done()); err != nil {
				t.Fatal(err)
			}
			select {
			case _, ok := <-srv.Done():
				if ok {
					t.Fatal("Done published more than one Serve result")
				}
			case <-time.After(time.Second):
				t.Fatal("Done was not closed after its single result")
			}
		})
	}
}
