//go:build windows

package main

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestWindowsICOFromPNG(t *testing.T) {
	png := []byte{0x89, 'P', 'N', 'G'}
	ico := windowsICOFromPNG(png)
	if len(ico) != 22+len(png) {
		t.Fatalf("len(ico) = %d, want %d", len(ico), 22+len(png))
	}
	if binary.LittleEndian.Uint16(ico[2:4]) != 1 || binary.LittleEndian.Uint16(ico[4:6]) != 1 {
		t.Fatalf("invalid ico header: %v", ico[:6])
	}
	if got := binary.LittleEndian.Uint32(ico[14:18]); got != uint32(len(png)) {
		t.Fatalf("image size = %d, want %d", got, len(png))
	}
	if got := binary.LittleEndian.Uint32(ico[18:22]); got != 22 {
		t.Fatalf("image offset = %d, want 22", got)
	}
	if !bytes.Equal(ico[22:], png) {
		t.Fatalf("ico payload = %v, want %v", ico[22:], png)
	}
}
