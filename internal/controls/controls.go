// Package controls implements the daemon's local Unix-domain control API.
//
// bbolt admits a single writer process, so the daemon owns the repo for as long
// as it runs. `hf-ipfs pull` therefore proxies to the daemon when one is
// listening, and only builds its own embedded node when none is.
package controls

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	logging "github.com/ipfs/go-log/v2"

	"github.com/ipai/hf-ipfs/internal/hfcache"
	"github.com/ipai/hf-ipfs/internal/ingest"
	"github.com/ipai/hf-ipfs/internal/node"
	"github.com/ipai/hf-ipfs/internal/protoio"
	"github.com/ipai/hf-ipfs/internal/pull"
	"github.com/ipai/hf-ipfs/internal/wire"
)

var log = logging.Logger("hf-ipfs/controls")

// Server accepts control requests on a Unix socket.
type Server struct {
	n    *node.Node
	ln   net.Listener
	path string

	// Shutdown is closed when a client asks the daemon to stop.
	Shutdown chan struct{}
}

// Alive reports whether a hf-ipfs daemon is listening on socketPath.
func Alive(socketPath string) bool { return probe(socketPath) }

// Call sends one control request to the daemon and streams events through fn.
// It returns the first error reported by the daemon.
func Call(socketPath string, req wire.ControlRequest, fn func(wire.ControlEvent) error) error {
	c, err := net.Dial("unix", socketPath)
	if err != nil {
		return fmt.Errorf("dial daemon at %s: %w", socketPath, err)
	}
	defer c.Close()

	if err := protoio.WriteJSON(c, req); err != nil {
		return fmt.Errorf("send control request: %w", err)
	}
	for {
		var ev wire.ControlEvent
		if err := protoio.ReadJSON(c, &ev); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("read control event: %w", err)
		}
		if fn != nil {
			if err := fn(ev); err != nil {
				return err
			}
		}
		if ev.Type == "done" {
			return nil
		}
		if ev.Type == "error" {
			return errors.New(ev.Message)
		}
	}
}

// Serve starts the control server on socketPath.
func Serve(ctx context.Context, n *node.Node, socketPath string) (*Server, error) {
	// A stale socket left by a crashed daemon would block bind forever.
	if st, err := os.Stat(socketPath); err == nil && st.Mode()&os.ModeSocket != 0 {
		if !probe(socketPath) {
			_ = os.Remove(socketPath)
		}
	}
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("listen %s: %w", socketPath, err)
	}
	s := &Server{n: n, ln: ln, path: socketPath, Shutdown: make(chan struct{})}
	go s.acceptLoop(ctx)
	return s, nil
}

// Path reports the socket path.
func (s *Server) Path() string { return s.path }

// Close stops the listener and removes the socket file.
func (s *Server) Close() error {
	err := s.ln.Close()
	_ = os.Remove(s.path)
	return err
}

func (s *Server) acceptLoop(ctx context.Context) {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			log.Warnf("accept: %s", err)
			return
		}
		go s.handle(ctx, conn)
	}
}

func (s *Server) handle(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Hour))

	var req wire.ControlRequest
	if err := protoio.ReadJSON(conn, &req); err != nil {
		writeEvent(conn, wire.ControlEvent{Type: "error", Message: err.Error()})
		return
	}

	ev := func(e wire.ControlEvent) error { writeEvent(conn, e); return nil }

	switch req.Cmd {
	case wire.CmdPull:
		t, err := hfcache.ParseRepoType(req.RepoType)
		if err != nil {
			ev(wire.ControlEvent{Type: "error", Message: err.Error()})
			return
		}
		err = pull.Run(ctx, s.n, pull.Options{
			RepoID:   req.RepoID,
			RepoType: t,
			Commit:   req.Commit,
			Ref:      req.Ref,
			Peers:    req.Peers,
			Connect:  req.Connect,
			Force:    req.Force,
		}, ev)
		if err != nil {
			ev(wire.ControlEvent{Type: "error", Message: err.Error()})
		}

	case wire.CmdIngest:
		t, err := hfcache.ParseRepoType(req.RepoType)
		if err != nil {
			ev(wire.ControlEvent{Type: "error", Message: err.Error()})
			return
		}
		commit := req.Commit
		if commit == "" {
			paths := hfcache.NewPaths(s.n.Cfg.HFHubDir, req.RepoID, t)
			commit, err = paths.CurrentCommit(req.Ref)
			if err != nil {
				ev(wire.ControlEvent{Type: "error", Message: err.Error()})
				return
			}
		}
		res, err := ingest.Run(ctx, s.n, req.RepoID, t, commit, func(m string) {
			ev(wire.ControlEvent{Type: "progress", Message: m})
		})
		if err != nil {
			ev(wire.ControlEvent{Type: "error", Message: err.Error()})
			return
		}
		ev(wire.ControlEvent{Type: "done",
			Message: fmt.Sprintf("shared %s@%s: %s (%s)", res.RepoID, short(res.Commit),
				res.ActualCID, humanBytes(res.TotalSize))})

	case wire.CmdStatus:
		ev(wire.ControlEvent{Type: "done", Message: statusLine(s.n)})

	case wire.CmdList:
		entries, err := s.n.Mapping.List(ctx)
		if err != nil {
			ev(wire.ControlEvent{Type: "error", Message: err.Error()})
			return
		}
		for _, e := range entries {
			ev(wire.ControlEvent{Type: "progress",
				Message: fmt.Sprintf("%s@%s -> %s (%s, %d files)",
					e.RepoID, short(e.CommitHash), e.ActualCID, humanBytes(e.TotalSize), len(e.Files))})
		}
		ev(wire.ControlEvent{Type: "done", Message: fmt.Sprintf("%d shared revision(s)", len(entries))})

	case wire.CmdShutdown:
		ev(wire.ControlEvent{Type: "done", Message: "shutting down"})
		select {
		case <-s.Shutdown:
		default:
			close(s.Shutdown)
		}

	default:
		ev(wire.ControlEvent{Type: "error", Message: "unknown command: " + req.Cmd})
	}
}

func writeEvent(conn net.Conn, e wire.ControlEvent) {
	if err := protoio.WriteJSON(conn, e); err != nil {
		log.Debugf("write control event: %s", err)
	}
}

func probe(socketPath string) bool {
	c, err := net.DialTimeout("unix", socketPath, 500*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

func statusLine(n *node.Node) string {
	return fmt.Sprintf("peer %s\n%s", n.PeerID, strings.Join(n.Addrs(), "\n"))
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

func humanBytes(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(b)/float64(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(b)/float64(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(b)/float64(1<<10))
	default:
		return fmt.Sprintf("%d B", b)
	}
}
