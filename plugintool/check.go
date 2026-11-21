package plugintool

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/basaltdbhq/basalt-plugin-go/protocol"
)

// SocketEnv is the one environment variable a local socket plugin is started
// with: the path of the Unix socket it listens on.
const SocketEnv = "BASALT_PLUGIN_SOCKET"

// Transport names in a manifest's endpoint.
const (
	transportStdio   = "jsonrpc-stdio"
	transportUnix    = "jsonrpc-unix"
	transportConnect = "connectrpc"
)

const (
	defaultCallTimeout = 5 * time.Second
	defaultStartWait   = 30 * time.Second
	socketPoll         = 10 * time.Millisecond
	stopWait           = 2 * time.Second
	// maxSocketDir keeps a socket's path under the shortest sun_path the
	// supported systems have (104 bytes on macOS).
	maxSocketPath = 100
	dirMode       = 0o700
)

//go:embed samples/*.json
var sampleFS embed.FS

// The contract's fixed samples. The engine's check sends the same bytes,
// with the times of the notice set to now.
const (
	sampleTime      = "2026-01-01T00:00:01Z"
	sampleExpiresAt = "2026-01-02T00:00:01Z"
)

// CheckOptions tune Check. The zero value is what a person wants.
type CheckOptions struct {
	// Stderr receives the plugin's stderr; nil discards it.
	Stderr io.Writer
	// CallTimeout is the deadline of each call; 5 seconds when zero.
	CallTimeout time.Duration
	// StartWait bounds the start and, for a socket plugin, the wait for its
	// socket; 30 seconds when zero.
	StartWait time.Duration
	// Say receives one line per step as it passes; nil prints nothing.
	Say func(format string, args ...any)
}

// CheckedHook is one hook that answered its sample.
type CheckedHook struct {
	Hook    string
	Ms      int64
	LastSeq *int64
}

// CheckResult is what a passing check measured.
type CheckResult struct {
	Name         string
	Version      string
	InitializeMs int64
	HealthStatus string
	HealthMs     int64
	Hooks        []CheckedHook
}

// Check starts the plugin in folder dir the way the engine starts a local
// plugin (its own process group, the folder as working directory, no
// environment but SocketEnv for a socket transport), runs initialize and
// health, sends each declared notifier hook the contract's fixed sample, and
// stops it. It needs no engine, no signature, no key and no database, and it
// runs the program as the caller's own user, which is what the author does
// when they run it by hand. It refuses to run as root, and a remote plugin
// (nothing to start here). A manifest whose files do not match the folder is
// refused with the command that fixes it.
func Check(ctx context.Context, dir string, opt CheckOptions) (CheckResult, error) {
	var res CheckResult
	if opt.CallTimeout <= 0 {
		opt.CallTimeout = defaultCallTimeout
	}
	if opt.StartWait <= 0 {
		opt.StartWait = defaultStartWait
	}
	say := opt.Say
	if say == nil {
		say = func(string, ...any) {}
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return res, err
	}
	b, head, err := readManifest(dir)
	if err != nil {
		return res, err
	}
	local, err := localEndpoint(head)
	if err != nil {
		return res, err
	}
	if isRoot() {
		return res, errors.New("a local plugin is never started with root's privileges; run the check as your own user")
	}
	if err := checkFilesMatch(dir, head); err != nil {
		return res, err
	}
	hooks := sortedHooks(head)
	var meta struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(b, &meta); err != nil {
		return res, errors.New("the manifest is not valid JSON")
	}

	startCtx, cancel := context.WithTimeout(ctx, opt.StartWait)
	defer cancel()
	c, err := startPlugin(startCtx, dir, local.Command, local.Args, local.Transport, opt.Stderr)
	if err != nil {
		return res, err
	}
	defer c.close()

	cctx, ccancel := context.WithTimeout(ctx, opt.CallTimeout)
	start := time.Now()
	init, err := c.initialize(cctx, protocol.InitializeRequest{
		Protocols: []string{protocol.Version1}, Hooks: hooks,
		Limits: protocol.Limits{MaxMessageBytes: protocol.MaxMessageBytes, MaxInFlight: protocol.MaxInFlight, TimeoutMs: int(opt.CallTimeout / time.Millisecond)},
	})
	ccancel()
	if err != nil {
		return res, fmt.Errorf("initialize failed: %w", err)
	}
	res.InitializeMs = wholeMs(time.Since(start))
	if err := checkIdentity(init, meta.Name, meta.Version, hooks); err != nil {
		stopPlugin(ctx, c, opt.CallTimeout)
		return res, err
	}
	res.Name, res.Version = meta.Name, meta.Version

	cctx, ccancel = context.WithTimeout(ctx, opt.CallTimeout)
	start = time.Now()
	h, err := c.health(cctx)
	ccancel()
	if err != nil {
		stopPlugin(ctx, c, opt.CallTimeout)
		return res, fmt.Errorf("health failed: %w", err)
	}
	res.HealthStatus, res.HealthMs = h.Status, wholeMs(time.Since(start))
	say("initialize %dms, health %s %dms\n", res.InitializeMs, h.Status, res.HealthMs)
	if h.Status != protocol.HealthReady {
		stopPlugin(ctx, c, opt.CallTimeout)
		return res, fmt.Errorf("the plugin answered health %s, not %s", h.Status, protocol.HealthReady)
	}
	res.Hooks, err = checkHooks(ctx, c, hooks, opt.CallTimeout, say)
	stopPlugin(ctx, c, opt.CallTimeout)
	return res, err
}

func wholeMs(d time.Duration) int64 { return d.Round(time.Millisecond).Milliseconds() }

type localEndpointInfo struct {
	Transport string
	Command   string
	Args      []string
}

func localEndpoint(h manifestHead) (localEndpointInfo, error) {
	if len(h.Endpoint.Remote) > 0 {
		return localEndpointInfo{}, errors.New("this is a remote plugin: there is no program in the folder to start")
	}
	l := h.Endpoint.Local
	if l == nil {
		return localEndpointInfo{}, errors.New("the manifest has no endpoint.local")
	}
	if !validRelativePath(l.Command) {
		return localEndpointInfo{}, errors.New("endpoint.local.command is not a relative path inside the folder")
	}
	switch l.Transport {
	case transportStdio, transportUnix, transportConnect:
	default:
		return localEndpointInfo{}, fmt.Errorf("transport %q is not one of %s, %s, %s", l.Transport, transportStdio, transportUnix, transportConnect)
	}
	return localEndpointInfo{Transport: l.Transport, Command: l.Command, Args: l.Args}, nil
}

// checkFilesMatch refuses a folder the engine would refuse for its files: a
// hash that is not the manifest's, a file the manifest does not list, a listed
// file that is missing. The fix is always the manifest command.
func checkFilesMatch(dir string, h manifestHead) error {
	if len(h.Files) == 0 {
		return errors.New("the manifest lists no files; run basalt-plugin manifest first")
	}
	have, err := folderFiles(dir)
	if err != nil {
		return err
	}
	for _, rel := range have {
		if _, ok := h.Files[rel]; !ok {
			return fmt.Errorf("%s is not in the manifest's files; run basalt-plugin manifest", filepath.Join(dir, filepath.FromSlash(rel)))
		}
	}
	for rel, want := range h.Files {
		got, err := hashFile(filepath.Join(dir, filepath.FromSlash(rel)))
		if err != nil {
			return fmt.Errorf("%w; the manifest lists it", err)
		}
		if got != want {
			return fmt.Errorf("%s changed since the manifest was written; run basalt-plugin manifest again", filepath.Join(dir, filepath.FromSlash(rel)))
		}
	}
	return nil
}

func sortedHooks(h manifestHead) []string {
	out := make([]string, 0, len(h.Hooks))
	for name := range h.Hooks {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// checkIdentity: the plugin says what the manifest says. The engine refuses
// an entry whose name, version or hooks differ.
func checkIdentity(r protocol.InitializeResponse, name, version string, hooks []string) error {
	if r.Protocol != protocol.Version1 {
		return fmt.Errorf("the plugin speaks %q, not %q", r.Protocol, protocol.Version1)
	}
	if r.Name != name || r.Version != version {
		return fmt.Errorf("the plugin says it is %s %s; the manifest says %s %s", r.Name, r.Version, name, version)
	}
	got := make([]string, 0, len(r.Hooks))
	for h := range r.Hooks {
		got = append(got, h)
	}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(hooks, ",") {
		return fmt.Errorf("the plugin declares the hooks [%s]; the manifest declares [%s]", strings.Join(got, " "), strings.Join(hooks, " "))
	}
	return nil
}

func checkHooks(ctx context.Context, c caller, hooks []string, timeout time.Duration, say func(string, ...any)) ([]CheckedHook, error) {
	done := []CheckedHook{}
	for _, hook := range hooks {
		switch hook {
		case protocol.MethodApprovalDeliver:
			req, err := deliverSample(time.Now())
			if err != nil {
				return done, err
			}
			cctx, cancel := context.WithTimeout(ctx, timeout)
			start := time.Now()
			err = c.approvalDeliver(cctx, req)
			cancel()
			if err != nil {
				return done, fmt.Errorf("%s failed: %w", hook, err)
			}
			ms := wholeMs(time.Since(start))
			say("%s %dms\n", hook, ms)
			done = append(done, CheckedHook{Hook: hook, Ms: ms})
		case protocol.MethodFeedObserve:
			req, want, err := feedSample()
			if err != nil {
				return done, err
			}
			cctx, cancel := context.WithTimeout(ctx, timeout)
			start := time.Now()
			last, err := c.feedObserve(cctx, req)
			cancel()
			if err != nil {
				return done, fmt.Errorf("%s failed: %w", hook, err)
			}
			if last != want {
				return done, fmt.Errorf("%s failed: the plugin answered lastSeq %d; the sample's events end at seq %d, and a plugin that handles them answers it", hook, last, want)
			}
			ms := wholeMs(time.Since(start))
			say("%s lastSeq %d %dms\n", hook, last, ms)
			done = append(done, CheckedHook{Hook: hook, Ms: ms, LastSeq: &last})
		}
		// The other hooks have no fixed sample yet.
	}
	return done, nil
}

// feedSample is the feed sample and the lastSeq a plugin that handles it
// answers: the highest data.seq in it.
func feedSample() (protocol.FeedObserveRequest, int64, error) {
	b, err := sampleFS.ReadFile("samples/feed-check-sample.json")
	if err != nil {
		return protocol.FeedObserveRequest{}, 0, err
	}
	var req protocol.FeedObserveRequest
	if err := json.Unmarshal(b, &req); err != nil {
		return req, 0, err
	}
	var events []struct {
		Data struct {
			Seq int64 `json:"seq"`
		} `json:"data"`
	}
	if err := json.Unmarshal(req.Events, &events); err != nil {
		return req, 0, err
	}
	var last int64
	for _, e := range events {
		last = max(last, e.Data.Seq)
	}
	return req, last, nil
}

// deliverSample is the notice sample, made at now and expiring an hour later.
func deliverSample(now time.Time) (protocol.ApprovalDeliverRequest, error) {
	b, err := sampleFS.ReadFile("samples/deliver-check-sample.json")
	if err != nil {
		return protocol.ApprovalDeliverRequest{}, err
	}
	now = now.UTC().Truncate(time.Millisecond)
	b = bytes.Replace(b, []byte(`"time": "`+sampleTime+`"`), []byte(`"time": "`+now.Format(time.RFC3339Nano)+`"`), 1)
	b = bytes.Replace(b, []byte(`"expiresAt": "`+sampleExpiresAt+`"`), []byte(`"expiresAt": "`+now.Add(time.Hour).Format(time.RFC3339Nano)+`"`), 1)
	var req protocol.ApprovalDeliverRequest
	err = json.Unmarshal(b, &req)
	return req, err
}

// stopPlugin asks the plugin to shut down, on a context of its own so a
// call that used up ctx does not skip it, then close stops the process.
func stopPlugin(ctx context.Context, c caller, timeout time.Duration) {
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	_ = c.shutdown(sctx)
}

// startPlugin starts the program and returns a connection to it. Closing it
// stops the process group.
func startPlugin(ctx context.Context, dir, command string, args []string, transport string, stderr io.Writer) (caller, error) {
	path := filepath.Join(dir, filepath.FromSlash(command))
	cmd := exec.Command(path, args...)
	cmd.Dir = dir
	cmd.Env = []string{}
	cmd.Stderr = stderr
	ownProcessGroup(cmd)
	if transport == transportStdio {
		return startStdio(cmd)
	}
	sockDir, err := socketDir()
	if err != nil {
		return nil, err
	}
	socket := filepath.Join(sockDir, "p.sock")
	cmd.Env = []string{SocketEnv + "=" + socket}
	if err := cmd.Start(); err != nil {
		_ = os.RemoveAll(sockDir)
		return nil, errors.New("the plugin's command could not be started (is it built for this machine and executable?)")
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	stop := func() {
		killGroup(cmd)
		<-exited
		_ = os.RemoveAll(sockDir)
	}
	if err := waitSocket(ctx, socket, exited); err != nil {
		stop()
		return nil, err
	}
	if transport == transportConnect {
		return newConnectClient(socket, stop), nil
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
	if err != nil {
		stop()
		return nil, errors.New("the plugin's socket could not be connected to")
	}
	return newRPCClient(conn, conn, func() { _ = conn.Close(); stop() }), nil
}

func startStdio(cmd *exec.Cmd) (caller, error) {
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, errors.New("the plugin's command could not be started (is it built for this machine and executable?)")
	}
	stop := func() {
		_ = in.Close()
		killGroup(cmd)
		_ = cmd.Wait()
	}
	return newRPCClient(out, in, stop), nil
}

// socketDir makes a directory only this user can reach, short enough for a
// socket path.
func socketDir() (string, error) {
	dir, err := os.MkdirTemp("", "bp")
	if err == nil && len(dir)+len("/p.sock") > maxSocketPath {
		_ = os.RemoveAll(dir)
		dir, err = os.MkdirTemp("/tmp", "bp")
	}
	if err != nil {
		return "", errors.New("a directory for the plugin's socket cannot be made")
	}
	if err := os.Chmod(dir, dirMode); err != nil {
		_ = os.RemoveAll(dir)
		return "", errors.New("a directory for the plugin's socket cannot be made")
	}
	return dir, nil
}

// waitSocket waits for the plugin to listen on its socket, or for ctx, or for
// the process to exit.
func waitSocket(ctx context.Context, socket string, exited <-chan struct{}) error {
	tick := time.NewTicker(socketPoll)
	defer tick.Stop()
	for {
		if fi, err := os.Lstat(socket); err == nil && fi.Mode()&os.ModeSocket != 0 {
			return nil
		}
		select {
		case <-exited:
			return errors.New("the plugin exited before it listened on its socket (does it read " + SocketEnv + "?)")
		case <-ctx.Done():
			return errors.New("the plugin did not listen on its socket in time")
		case <-tick.C:
		}
	}
}
