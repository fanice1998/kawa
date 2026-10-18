package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/fanice1998/kawa/internal/channel"
)

type richFakeChannel struct {
	recipient string
	text      string
	plain     bool
}

func (f *richFakeChannel) Name() string { return "telegram" }
func (f *richFakeChannel) Start(context.Context, chan<- channel.InboundMessage) error {
	return nil
}
func (f *richFakeChannel) Send(context.Context, string, string) error {
	f.plain = true
	return nil
}
func (f *richFakeChannel) SendRich(_ context.Context, recipient, text string) error {
	f.recipient = recipient
	f.text = text
	return nil
}

func TestSendRichMessageUsesChannelNameNotUserID(t *testing.T) {
	fake := &richFakeChannel{}
	router := channel.NewRouter()
	router.Register(fake)
	tool := NewSendRichMessageTool(&CustomToolDeps{Router: router})

	ctx := WithDeliveryTarget(context.Background(), "telegram", "795566183")
	out, err := tool.Execute(ctx, `{"text":"*hi*"}`)
	if err != nil {
		t.Fatal(err)
	}
	if fake.plain {
		t.Fatal("rich send must not fall back to plain Router.Send")
	}
	if fake.recipient != "795566183" || fake.text != "*hi*" {
		t.Fatalf("SendRich got recipient=%q text=%q", fake.recipient, fake.text)
	}
	if !strings.Contains(out, "795566183") {
		t.Fatalf("unexpected result %q", out)
	}
}

func TestSendRichMessageMissingChannelNamesTheChannel(t *testing.T) {
	tool := NewSendRichMessageTool(&CustomToolDeps{Router: channel.NewRouter()})
	_, err := tool.Execute(context.Background(), `{"recipient":"795566183","text":"hi"}`)
	if err == nil || !strings.Contains(err.Error(), `channel "telegram" not registered`) {
		t.Fatalf("expected the telegram channel to be missing, got %v", err)
	}
}
