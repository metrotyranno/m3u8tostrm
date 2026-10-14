//go:build windows && (amd64 || arm64)

package main

import (
	"testing"
	"unsafe"
)

// ShellExecuteExW rejects the call if cbSize doesn't match the C struct (112 bytes on 64-bit).
func TestShellExecuteInfoSize(t *testing.T) {
	if s := unsafe.Sizeof(shellExecuteInfo{}); s != 112 {
		t.Fatalf("SHELLEXECUTEINFOW is %d bytes, want 112", s)
	}
}
