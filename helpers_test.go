package basaltplugin

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const wait = 5 * time.Second

// shortDir returns a directory with a path short enough for a Unix socket.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "sdk")
	require.NoError(t, err)
	if len(dir) > 50 {
		_ = os.RemoveAll(dir)
		dir, err = os.MkdirTemp("/tmp", "sdk")
		require.NoError(t, err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func socketPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(shortDir(t), "p.sock")
}

// served is a Serve running in the background.
type served struct {
	cancel context.CancelFunc
	done   chan error
}

func (s *served) stop(t *testing.T) error {
	t.Helper()
	s.cancel()
	return s.await(t)
}

func (s *served) await(t *testing.T) error {
	t.Helper()
	select {
	case err := <-s.done:
		return err
	case <-time.After(wait):
		t.Fatal("Serve did not return")
		return nil
	}
}

func serveBackground(p Plugin, opts ...Option) *served {
	ctx, cancel := context.WithCancel(context.Background())
	s := &served{cancel: cancel, done: make(chan error, 1)}
	go func() { s.done <- Serve(ctx, p, opts...) }()
	return s
}

// serveSocket serves p on a fresh socket path and waits for it to exist.
func serveSocket(t *testing.T, p Plugin, opts ...Option) (*served, string) {
	t.Helper()
	path := socketPath(t)
	env := withEnv(func(k string) string {
		if k == SocketEnv {
			return path
		}
		return ""
	})
	s := serveBackground(p, append([]Option{env}, opts...)...)
	t.Cleanup(s.cancel)
	// The socket file appears at bind, before listen: dial, so a test never
	// meets a socket that refuses its first connection.
	waitSocket(t, path)
	return s, path
}

// stdioClient is the engine's side of a stdio plugin.
type stdioClient struct {
	t    *testing.T
	w    io.WriteCloser
	r    *bufio.Reader
	next int
}

type reply struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *rpcBody        `json:"error"`
}

func serveStdio(t *testing.T, p Plugin, opts ...Option) (*served, *stdioClient) {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	s := serveBackground(p, append([]Option{withStdio(inR, outW)}, opts...)...)
	t.Cleanup(func() {
		s.cancel()
		_ = inW.Close()
		_ = outR.Close()
	})
	return s, &stdioClient{t: t, w: inW, r: bufio.NewReaderSize(outR, MaxMessageBytes+4096)}
}

func (c *stdioClient) send(line string) {
	c.t.Helper()
	_, err := io.WriteString(c.w, line+"\n")
	require.NoError(c.t, err)
}

func (c *stdioClient) read() reply {
	c.t.Helper()
	type res struct {
		r   reply
		err error
	}
	ch := make(chan res, 1)
	go func() {
		line, err := c.r.ReadBytes('\n')
		var r reply
		if err == nil {
			err = json.Unmarshal(line, &r)
		}
		ch <- res{r, err}
	}()
	select {
	case got := <-ch:
		require.NoError(c.t, got.err)
		return got.r
	case <-time.After(wait):
		c.t.Fatal("no reply")
		return reply{}
	}
}

// call sends one request and returns its reply.
func (c *stdioClient) call(method, params string) reply {
	c.t.Helper()
	c.next++
	c.send(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":%q,"params":%s}`, c.next, method, params))
	r := c.read()
	require.Equal(c.t, fmt.Sprint(c.next), string(r.ID))
	return r
}

const initParams = `{"protocols":["basalt-plugin/1"],"hooks":[],"config":{},"limits":{"maxMessageBytes":4194304,"maxInFlight":4,"timeoutMs":2000}}`

// peerProc starts the test binary as a plugin with env and returns a function
// that stops it.
func peerProc(t *testing.T, mode string, env ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], peerFlag, mode)
	cmd.Env = env
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd
}

func waitSocket(t *testing.T, path string) {
	t.Helper()
	require.Eventually(t, func() bool {
		c, err := net.Dial("unix", path)
		if err != nil {
			return false
		}
		_ = c.Close()
		return true
	}, wait, 5*time.Millisecond)
}
