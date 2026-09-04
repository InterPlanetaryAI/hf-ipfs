// Command hf-ipfs bridges the Hugging Face local cache and the IPFS network.
//
// It embeds an IPFS/libp2p node directly in the binary (no Kubo daemon) and
// uses filestore so shared models are never copied out of
// ~/.cache/huggingface/hub/.../blobs.
//
// Usage:
//
//	hf-ipfs daemon [--listen <ma>] [--bootstrap <ma>] [--connect <ma>]
//	               [--chunk-size N] [--no-watch] [--dht-client] [--rescan DUR]
//	hf-ipfs pull <repo_id> [--commit <hash>] [--ref main] [--repo-type model]
//	             [--peer <ma>] [--force] [--no-daemon]
//	hf-ipfs ingest <repo_id> [--commit <hash>] [--ref main] [--repo-type model]
//	hf-ipfs list | status | id | resolve <repo_id> | shutdown | version
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	logging "github.com/ipfs/go-log/v2"

	"github.com/ipai/hf-ipfs/internal/config"
	"github.com/ipai/hf-ipfs/internal/controls"
	"github.com/ipai/hf-ipfs/internal/dummy"
	"github.com/ipai/hf-ipfs/internal/hfapi"
	"github.com/ipai/hf-ipfs/internal/hfcache"
	"github.com/ipai/hf-ipfs/internal/identity"
	"github.com/ipai/hf-ipfs/internal/ingest"
	"github.com/ipai/hf-ipfs/internal/node"
	"github.com/ipai/hf-ipfs/internal/pull"
	"github.com/ipai/hf-ipfs/internal/watch"
	"github.com/ipai/hf-ipfs/internal/wire"
)

var version = "0.1.0" // overridden at release time via -ldflags -X main.version

func main() {
	if err := run(os.Args[1:]); err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(os.Stderr, "hf-ipfs: error:", err)
		}
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return errors.New("missing command")
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "daemon":
		return cmdDaemon(rest)
	case "pull":
		return cmdPull(rest)
	case "ingest":
		return cmdIngest(rest)
	case "list":
		return cmdList(rest)
	case "status":
		return cmdStatus(rest)
	case "id":
		return cmdID(rest)
	case "resolve":
		return cmdResolve(rest)
	case "shutdown":
		return cmdShutdown(rest)
	case "version", "-v", "--version":
		fmt.Println("hf-ipfs", version)
		return nil
	case "help", "-h", "--help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown command %q", cmd)
	}
}

// splitArgs moves positional arguments after flags so that
// `pull repo --commit X` and `pull --commit X repo` both parse correctly
// (Go's flag package stops at the first non-flag argument).
func splitArgs(fs *flag.FlagSet, args []string) []string {
	isBool := func(name string) bool {
		f := fs.Lookup(name)
		if f == nil {
			return false
		}
		bv, ok := f.Value.(interface{ IsBoolFlag() bool })
		return ok && bv.IsBoolFlag()
	}

	flags := make([]string, 0, len(args))
	positional := make([]string, 0, 2)

	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			positional = append(positional, a)
			continue
		}
		flags = append(flags, a)
		name := strings.TrimLeft(a, "-")
		if strings.Contains(a, "=") {
			continue
		}
		if !isBool(name) && i+1 < len(args) {
			i++
			flags = append(flags, args[i])
		}
	}
	return append(flags, positional...)
}

// commonFlags are shared by every subcommand.
type commonFlags struct {
	repo     string
	hfHub    string
	endpoint string
	hfToken  string
	logLevel string
}

func (c *commonFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&c.repo, "repo", "", "hf-ipfs state dir (default $HF_IPFS_REPO or ~/.hf-ipfs)")
	fs.StringVar(&c.hfHub, "hf-hub", "", "HF hub cache dir (default $HF_HUB_CACHE or $HF_HOME/hub)")
	fs.StringVar(&c.endpoint, "endpoint", "", "HF Hub API endpoint (default $HF_ENDPOINT)")
	fs.StringVar(&c.hfToken, "hf-token", "",
		"HF access token for gated/private repos (default $HF_TOKEN). Prefer the env var: argv is world-readable via ps")
	fs.StringVar(&c.logLevel, "log-level", "info", "debug|info|warn|error")
}

func (c *commonFlags) apply(cfg *config.Config) error {
	if c.repo != "" {
		cfg.RepoDir = c.repo
		cfg.APISocket = cfg.RepoDir + "/api.sock"
	}
	if c.hfHub != "" {
		cfg.HFHubDir = c.hfHub
	}
	if c.endpoint != "" {
		cfg.HFEndpoint = strings.TrimRight(c.endpoint, "/")
	}
	if c.hfToken != "" {
		cfg.HFToken = c.hfToken
	}
	level := c.logLevel
	if level == "" {
		level = "info"
	}
	for _, name := range []string{"hf-ipfs", "hf-ipfs/pull", "hf-ipfs/watch", "hf-ipfs/controls"} {
		if err := logging.SetLogLevel(name, level); err != nil {
			return err
		}
	}
	return nil
}

// stringList collects a repeatable string flag.
type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

func cmdDaemon(args []string) error {
	fs := flag.NewFlagSet("daemon", flag.ContinueOnError)
	var c commonFlags
	c.register(fs)
	var listen, bootstrap, connect stringList
	var chunkSize int64
	var noWatch, dhtClient, isolated bool
	var noRelay, noNatPortMap, noHolePunch, relayBulk bool
	var rescan time.Duration

	fs.Var(&listen, "listen", "libp2p listen multiaddr (repeatable)")
	fs.Var(&bootstrap, "bootstrap", "kademlia bootstrap peer multiaddr (repeatable; replaces the default list)")
	fs.Var(&connect, "connect", "peer multiaddr to dial at startup (repeatable)")
	fs.Int64Var(&chunkSize, "chunk-size", config.DefaultChunkSize, "chunk size in bytes")
	fs.BoolVar(&noWatch, "no-watch", false, "disable the HF cache file watcher")
	fs.BoolVar(&dhtClient, "dht-client", false, "run Kademlia in client mode")
	fs.BoolVar(&isolated, "isolated", false, "private DHT with no bootstrap peers (local testing only)")
	fs.BoolVar(&noRelay, "no-relay", false,
		"disable circuit v2; private nodes then have no fallback and become unreachable")
	fs.BoolVar(&noNatPortMap, "no-nat-portmap", false,
		"do not request a UPnP/NAT-PMP mapping from the gateway")
	fs.BoolVar(&noHolePunch, "no-hole-punch", false, "disable DCUtR hole punching")
	fs.BoolVar(&relayBulk, "relay-bulk", false,
		"allow bulk block streaming over circuit relays (costs the relay operator bandwidth)")
	fs.DurationVar(&rescan, "rescan", 5*time.Minute, "periodic cache rescan interval (0 disables)")

	if err := fs.Parse(splitArgs(fs, args)); err != nil {
		return err
	}

	cfg, err := config.Default()
	if err != nil {
		return err
	}
	if len(listen) > 0 {
		cfg.Listen = listen
	}
	if len(bootstrap) > 0 {
		cfg.Bootstrap = bootstrap
	}
	if len(connect) > 0 {
		cfg.Connect = connect
	}
	cfg.ChunkSize = chunkSize
	cfg.DHTServer = !dhtClient
	cfg.Isolated = isolated
	cfg.Relay = !noRelay
	cfg.NATPortMap = !noNatPortMap
	cfg.HolePunch = !noHolePunch
	cfg.RelayBulk = relayBulk
	cfg.RescanInterval = rescan
	if err := c.apply(cfg); err != nil {
		return err
	}

	ctx, stop := signalContext()
	defer stop()

	n, err := node.New(ctx, cfg)
	if err != nil {
		return err
	}
	defer n.Close()

	fmt.Printf("hf-ipfs %s daemon\n", version)
	fmt.Printf("  peer id : %s\n", n.PeerID)
	for _, a := range n.Addrs() {
		fmt.Printf("  addr    : %s\n", a)
	}
	if cfg.DHTServer && n.BoundLoopbackOnly() {
		fmt.Fprintln(os.Stderr,
			"  warn    : bound to loopback only — no other peer can dial this node.\n"+
				"            The DHT will hand out 127.0.0.1, so every transparent pull fails.\n"+
				"            Drop the --listen override to get the default /ip4/0.0.0.0/tcp/4008.")
	}
	fmt.Printf("  hub     : %s\n", cfg.HFHubDir)
	fmt.Printf("  repo    : %s\n", cfg.RepoDir)
	fmt.Printf("  hf      : endpoint=%s token=%s\n", cfg.HFEndpoint, tokenStatus(cfg.HFToken))
	bootDesc := "libp2p defaults"
	switch {
	case cfg.Isolated:
		bootDesc = "isolated (none)"
	case len(cfg.Bootstrap) > 0:
		bootDesc = fmt.Sprintf("%d custom", len(cfg.Bootstrap))
	}
	fmt.Printf("  dht     : server=%t bootstrap=%s\n", cfg.DHTServer, bootDesc)
	fmt.Printf("  nat     : portmap=%s relay=%s holepunch=%s bulk-over-relay=%s\n",
		onOff(cfg.NATPortMap && !cfg.Isolated), onOff(cfg.Relay),
		onOff(cfg.HolePunch && !cfg.Isolated), onOff(cfg.RelayBulk))

	n.DialPeers(ctx)

	// The DHT absorbs dialed peers asynchronously, so announcing before that
	// lands silently no-ops against an empty routing table.
	rt := n.WaitForRoutingTable(ctx, 15*time.Second)
	fmt.Printf("  swarm   : routing table %d peer(s)\n", rt)
	if rt == 0 && !cfg.Isolated {
		fmt.Fprintln(os.Stderr,
			"  warn    : not joined the DHT swarm; provider records cannot be announced or resolved")
	}

	// AutoNAT's first verdict typically lands well after startup, so the
	// banner shows the current state and a watcher prints the real verdict
	// the moment it arrives. Blocking the banner on the probe just made the
	// startup line wrong for tens of seconds.
	fmt.Printf("  reach   : %s\n", node.ReachabilityLine(n.Reachability(), cfg))
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case r := <-n.ReachabilityChanged():
				fmt.Printf("  reach   : %s\n", node.ReachabilityLine(r, cfg))
			}
		}
	}()

	count, err := n.ReprovideAll(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "  warn    : reprovide failed: %v\n", err)
	} else if count > 0 {
		fmt.Printf("  shared  : re-announced %d revision(s)\n", count)
	}

	srv, err := controls.Serve(ctx, n, cfg.APISocket)
	if err != nil {
		return err
	}
	defer srv.Close()
	fmt.Printf("  control : %s\n", srv.Path())
	fmt.Println("  running — Ctrl-C to stop")

	if noWatch {
		select {
		case <-ctx.Done():
		case <-srv.Shutdown:
		}
		return nil
	}

	w := watch.New(n)
	errCh := make(chan error, 1)
	go func() { errCh <- w.Run(ctx) }()

	select {
	case <-ctx.Done():
	case <-srv.Shutdown:
	case err := <-errCh:
		return err
	}
	return nil
}

func cmdPull(args []string) error {
	fs := flag.NewFlagSet("pull", flag.ContinueOnError)
	var c commonFlags
	c.register(fs)
	var peers, connect stringList
	var commit, ref, repoType string
	var force, noDaemon bool
	var from string

	fs.StringVar(&commit, "commit", "", "explicit 40-char HF commit hash (skips HF API resolution)")
	fs.StringVar(&ref, "ref", "main", "hf ref to point at the pulled commit")
	fs.StringVar(&repoType, "repo-type", "model", "model|dataset|space")
	fs.Var(&peers, "peer", "peer multiaddr to try before the DHT (repeatable)")
	fs.Var(&connect, "connect", "peer multiaddr dialed only to join the DHT (repeatable)")
	fs.BoolVar(&force, "force", false, "re-pull even if already shared locally")
	fs.BoolVar(&noDaemon, "no-daemon", false, "never proxy to a running daemon")
	fs.StringVar(&from, "from", "p2p,hf",
		"source preference: p2p (swarm only), hf (Hugging Face only), or p2p,hf (swarm first, HF fallback)")

	if err := fs.Parse(splitArgs(fs, args)); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return errors.New("usage: hf-ipfs pull <repo_id> [--commit <hash>] [--peer <multiaddr>]")
	}
	repoID := fs.Arg(0)

	cfg, err := config.Default()
	if err != nil {
		return err
	}
	if err := c.apply(cfg); err != nil {
		return err
	}

	src, err := pull.ParseSources(from)
	if err != nil {
		return fmt.Errorf("--from: %w", err)
	}

	req := wire.ControlRequest{
		Cmd:      wire.CmdPull,
		RepoID:   repoID,
		RepoType: repoType,
		Commit:   commit,
		Ref:      ref,
		Peers:    peers,
		Connect:  connect,
		Force:    force,
		From:     src.String(),
		Token:    cfg.HFToken,
	}
	if !noDaemon && controls.Alive(cfg.APISocket) {
		fmt.Fprintf(os.Stderr, "proxying to daemon at %s\n", cfg.APISocket)
		return controls.Call(cfg.APISocket, req, printEvent)
	}

	ctx, stop := signalContext()
	defer stop()

	// A standalone puller only needs to read the DHT, not serve it.
	cfg.DHTServer = false
	n, err := node.New(ctx, cfg)
	if err != nil {
		return err
	}
	defer n.Close()
	fmt.Printf("pulling %s as %s\n", repoID, n.PeerID)

	t, err := hfcache.ParseRepoType(repoType)
	if err != nil {
		return err
	}
	return pull.Run(ctx, n, pull.Options{
		RepoID:   repoID,
		RepoType: t,
		Commit:   commit,
		Ref:      ref,
		Peers:    peers,
		Connect:  connect,
		Force:    force,
		Sources:  src,
		Token:    cfg.HFToken,
	}, printEvent)
}

func cmdIngest(args []string) error {
	fs := flag.NewFlagSet("ingest", flag.ContinueOnError)
	var c commonFlags
	c.register(fs)
	var commit, ref, repoType string
	fs.StringVar(&commit, "commit", "", "explicit commit hash (default: refs/<ref>)")
	fs.StringVar(&ref, "ref", "main", "hf ref to read the commit from")
	fs.StringVar(&repoType, "repo-type", "model", "model|dataset|space")
	if err := fs.Parse(splitArgs(fs, args)); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return errors.New("usage: hf-ipfs ingest <repo_id> [--commit <hash>]")
	}
	repoID := fs.Arg(0)

	cfg, err := config.Default()
	if err != nil {
		return err
	}
	if err := c.apply(cfg); err != nil {
		return err
	}
	req := wire.ControlRequest{
		Cmd:      wire.CmdIngest,
		RepoID:   repoID,
		RepoType: repoType,
		Commit:   commit,
		Ref:      ref,
	}
	if controls.Alive(cfg.APISocket) {
		return controls.Call(cfg.APISocket, req, printEvent)
	}

	ctx, stop := signalContext()
	defer stop()
	cfg.DHTServer = false
	n, err := node.New(ctx, cfg)
	if err != nil {
		return err
	}
	defer n.Close()

	t, err := hfcache.ParseRepoType(repoType)
	if err != nil {
		return err
	}
	if commit == "" {
		paths := hfcache.NewPaths(cfg.HFHubDir, repoID, t)
		commit, err = paths.CurrentCommit(ref)
		if err != nil {
			return err
		}
	}
	res, err := ingest.Run(ctx, n, repoID, t, commit, func(m string) {
		fmt.Fprintf(os.Stderr, "  %s\n", m)
	})
	if err != nil {
		return err
	}
	fmt.Printf("shared %s@%s\n  actual_cid : %s\n  dummy_cid  : %s\n  files      : %d\n  chunks     : %d\n  size       : %s\n",
		res.RepoID, res.Commit, res.ActualCID, res.DummyCID, res.Files, res.Chunks, humanBytes(res.TotalSize))
	return nil
}

func cmdList(args []string) error {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	var c commonFlags
	c.register(fs)
	if err := fs.Parse(splitArgs(fs, args)); err != nil {
		return err
	}
	cfg, err := config.Default()
	if err != nil {
		return err
	}
	if err := c.apply(cfg); err != nil {
		return err
	}
	if controls.Alive(cfg.APISocket) {
		return controls.Call(cfg.APISocket, wire.ControlRequest{Cmd: wire.CmdList}, printEvent)
	}

	ctx, stop := signalContext()
	defer stop()
	cfg.DHTServer = false
	n, err := node.New(ctx, cfg)
	if err != nil {
		return err
	}
	defer n.Close()

	entries, err := n.Mapping.List(ctx)
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		fmt.Println("no shared revisions yet")
		return nil
	}
	for _, e := range entries {
		fmt.Printf("%-40s @ %s  ->  %s  (%s, %d files, %s)\n",
			e.RepoID, short(e.CommitHash), e.ActualCID, e.RepoType, len(e.Files), humanBytes(e.TotalSize))
	}
	return nil
}

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	var c commonFlags
	c.register(fs)
	if err := fs.Parse(splitArgs(fs, args)); err != nil {
		return err
	}
	cfg, err := config.Default()
	if err != nil {
		return err
	}
	if err := c.apply(cfg); err != nil {
		return err
	}
	if controls.Alive(cfg.APISocket) {
		fmt.Println("daemon: running")
		return controls.Call(cfg.APISocket, wire.ControlRequest{Cmd: wire.CmdStatus}, printEvent)
	}
	fmt.Println("daemon: not running")
	if _, _, err := identity.Load(cfg.KeyPath()); err == nil {
		fmt.Println("(identity present; run `hf-ipfs daemon` to share)")
	}
	return nil
}

func cmdID(args []string) error {
	fs := flag.NewFlagSet("id", flag.ContinueOnError)
	var c commonFlags
	c.register(fs)
	if err := fs.Parse(splitArgs(fs, args)); err != nil {
		return err
	}
	cfg, err := config.Default()
	if err != nil {
		return err
	}
	if err := c.apply(cfg); err != nil {
		return err
	}
	_, pid, err := identity.Load(cfg.KeyPath())
	if err != nil {
		return fmt.Errorf("no identity yet at %s (run `hf-ipfs daemon` once to create it)", cfg.KeyPath())
	}
	fmt.Println(pid)
	return nil
}

func cmdResolve(args []string) error {
	fs := flag.NewFlagSet("resolve", flag.ContinueOnError)
	var c commonFlags
	c.register(fs)
	var repoType string
	fs.StringVar(&repoType, "repo-type", "model", "model|dataset|space")
	if err := fs.Parse(splitArgs(fs, args)); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return errors.New("usage: hf-ipfs resolve <repo_id>")
	}
	cfg, err := config.Default()
	if err != nil {
		return err
	}
	if err := c.apply(cfg); err != nil {
		return err
	}
	t, err := hfcache.ParseRepoType(repoType)
	if err != nil {
		return err
	}
	ctx, stop := signalContext()
	defer stop()

	client := hfapi.NewClient(cfg.HFEndpoint)
	client.Token = cfg.HFToken
	info, err := client.RepoInfo(ctx, fs.Arg(0), t, true)
	if err != nil {
		return err
	}
	d, err := dummy.FromCommit(info.SHA)
	if err != nil {
		return err
	}
	fmt.Printf("repo       : %s\ncommit     : %s\ndummy cid  : %s\nfiles      : %d\n",
		info.ID, info.SHA, d, len(info.Siblings))
	return nil
}

func cmdShutdown(args []string) error {
	fs := flag.NewFlagSet("shutdown", flag.ContinueOnError)
	var c commonFlags
	c.register(fs)
	if err := fs.Parse(splitArgs(fs, args)); err != nil {
		return err
	}
	cfg, err := config.Default()
	if err != nil {
		return err
	}
	if err := c.apply(cfg); err != nil {
		return err
	}
	if !controls.Alive(cfg.APISocket) {
		return errors.New("no daemon running")
	}
	return controls.Call(cfg.APISocket, wire.ControlRequest{Cmd: wire.CmdShutdown}, printEvent)
}

func printEvent(e wire.ControlEvent) error {
	switch e.Type {
	case "progress":
		if e.Total > 0 {
			fmt.Fprintf(os.Stderr, "  %s (%s / %s)\n", e.Message,
				humanBytes(e.Bytes), humanBytes(e.Total))
		} else {
			fmt.Fprintf(os.Stderr, "  %s\n", e.Message)
		}
	case "done":
		fmt.Println(e.Message)
	case "error":
		return errors.New(e.Message)
	}
	return nil
}

func signalContext() (context.Context, func()) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		select {
		case <-ch:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// tokenStatus reports whether an HF token is configured without revealing it.
// Only the last four characters are shown, which is enough to tell one token
// from another and not enough to use one.
func tokenStatus(tok string) string {
	if tok == "" {
		return "unset"
	}
	if len(tok) <= 4 {
		return "set"
	}
	return "set (…" + tok[len(tok)-4:] + ")"
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

func usage() {
	fmt.Fprint(os.Stderr, `hf-ipfs — share Hugging Face models over IPFS without Kubo

Commands:
  daemon      Share the local HF cache: watch, ingest (nocopy), announce, serve
  pull        Pull a repo from peers instead of Hugging Face's servers
  ingest      Share one repo revision now (default: what refs/main points at)
  list        Show revisions this node shares
  status      Show daemon state and this node's addresses
  id          Print this node's persistent peer id
  resolve     Print a repo's latest commit and its dummy CID
  shutdown    Ask a running daemon to stop
  version     Print the version

Examples:
  hf-ipfs daemon --listen /ip4/127.0.0.1/tcp/4001
  hf-ipfs pull meta-llama/Llama-2-7b
  hf-ipfs pull google/gemma-2b --peer /ip4/10.0.0.5/tcp/4001/p2p/12D3Koo...
  hf-ipfs ingest openai-community/gpt2 --commit 607a30d783dfa663caf39e06633721c8d4cfcd7e
`)
}
