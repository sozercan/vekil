package server

import (
	"net"
	"strconv"
	"strings"
	"testing"

	"github.com/sozercan/copilot-proxy/auth"
	"github.com/sozercan/copilot-proxy/logger"
)

func TestStart_ReturnsErrorWhenPortInUse(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to reserve port: %v", err)
	}
	defer listener.Close()

	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("expected TCP address, got %T", listener.Addr())
	}

	srv := New(auth.NewTestAuthenticator("test-token"), logger.New(logger.ParseLevel("error")), "127.0.0.1", strconv.Itoa(addr.Port))
	err = srv.Start()
	if err == nil {
		t.Fatal("expected Start to fail when port is already in use")
	}
	if srv.IsRunning() {
		t.Fatal("expected server to remain stopped after listen failure")
	}
	if !strings.Contains(err.Error(), "address already in use") {
		t.Fatalf("expected address-in-use error, got %v", err)
	}
}
