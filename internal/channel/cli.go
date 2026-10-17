package channel

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
)

// CLIChannel provides an interactive terminal channel for local testing and administration.
type CLIChannel struct {
	in  io.Reader
	out io.Writer
}

func NewCLIChannel() *CLIChannel {
	return &CLIChannel{
		in:  os.Stdin,
		out: os.Stdout,
	}
}

func (c *CLIChannel) Name() string { return "cli" }

func (c *CLIChannel) Start(ctx context.Context, inbound chan<- InboundMessage) error {
	scanner := bufio.NewScanner(c.in)
	fmt.Fprintln(c.out, "\n[Kawa Interactive CLI Ready — Type /help for commands, or chat directly]")
	for {
		fmt.Fprint(c.out, "You > ")
		if !scanner.Scan() {
			return scanner.Err()
		}
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		inbound <- InboundMessage{
			ChannelName: c.Name(),
			SenderID:    "local-admin",
			Text:        line,
		}
	}
}

func (c *CLIChannel) Send(_ context.Context, _ string, text string) error {
	fmt.Fprintf(c.out, "\nKawa >\n%s\n\n", text)
	return nil
}
