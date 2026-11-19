package basaltplugin

import (
	"context"
	"os"
	"testing"

	"go.uber.org/goleak"
)

// The test binary doubles as a plugin: TestMain runs peerMain when the first
// argument is peerFlag, so a test talks to a real child process built from
// the SDK. The child gets an empty environment except for the socket variable
// a test sets.

const peerFlag = "--peer"

func TestMain(m *testing.M) {
	if len(os.Args) > 2 && os.Args[1] == peerFlag {
		peerMain(os.Args[2])
	}
	goleak.VerifyTestMain(m)
}

type peerPlugin struct{}

func (peerPlugin) Info() Info { return Info{Name: "sdk-peer", Version: "1.0.0"} }

// peerMain never returns. Mode notifier serves both notifier hooks, and
// notifier-connect serves them over ConnectRPC; any other mode is a plugin
// with no hook.
func peerMain(mode string) {
	var p Plugin = peerPlugin{}
	var opts []Option
	switch mode {
	case "notifier":
		p = notifierPeer{}
	case "notifier-connect":
		p = notifierPeer{}
		opts = append(opts, WithTransport(ConnectRPC))
	}
	if Serve(context.Background(), p, opts...) != nil {
		os.Exit(1)
	}
	os.Exit(0)
}
