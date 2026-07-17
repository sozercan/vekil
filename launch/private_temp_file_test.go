package launch

import (
	"os"
	"testing"
)

func TestWritePrivateTempFileWritesAndCleansUp(t *testing.T) {
	path, cleanup, err := writePrivateTempFile("vekil-private-test-", []byte("secret"))
	if err != nil {
		t.Fatalf("writePrivateTempFile() error = %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read private temporary file: %v", err)
	}
	if string(body) != "secret" {
		t.Fatalf("private temporary file = %q, want secret", body)
	}
	if err := cleanup(); err != nil {
		t.Fatalf("cleanup private temporary file: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("private temporary file still exists after cleanup: %v", err)
	}
	if err := cleanup(); err != nil {
		t.Fatalf("second cleanup should be idempotent: %v", err)
	}
}
