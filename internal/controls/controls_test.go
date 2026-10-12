package controls

import (
	"net"
	"path/filepath"
	"testing"

	"github.com/ipai/hf-ipfs/internal/wire"
)

// The server terminates every command with a done or error event; an EOF
// without either means the daemon died mid-operation and must not be
// reported as success.
func TestCallEOFWithoutDoneIsError(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "api.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err == nil {
			_ = c.Close()
		}
	}()
	if err := Call(sock, wire.ControlRequest{Cmd: wire.CmdStatus}, nil); err == nil {
		t.Fatal("connection closed without done reported as success")
	}
}
