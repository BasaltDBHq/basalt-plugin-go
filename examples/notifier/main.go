// Command notifier is a complete Basalt notifier in about twenty lines: it
// writes one line to stderr for every write that waits for approval and for
// every approval notice. The README walks through it.
package main

import (
	"context"
	"fmt"
	"os"

	basaltplugin "github.com/BasaltDBHq/basalt-plugin-go"
)

type notifier struct{}

func (notifier) Info() basaltplugin.Info {
	return basaltplugin.Info{Name: "hello-notify", Version: "0.1.0"}
}

func (notifier) ObserveFeed(_ context.Context, b basaltplugin.FeedBatch) (int64, error) {
	for _, ev := range b.Events {
		held, ok := ev.Detail.(basaltplugin.WriteHeld)
		if !ok || ev.IsCheckSample() {
			continue
		}
		if _, err := fmt.Fprintf(os.Stderr, "%s: %d rows wait for approval\n", b.Agent, held.Rows); err != nil {
			return 0, err // the engine sends this batch again later
		}
	}
	return b.LastSeq(), nil
}

func (notifier) DeliverApproval(_ context.Context, n basaltplugin.Notice) error {
	if n.IsCheckSample() {
		return nil
	}
	_, err := fmt.Fprintln(os.Stderr, n.Headline, n.Instruction, n.Link)
	return err
}

func main() {
	if err := basaltplugin.Serve(context.Background(), notifier{}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
