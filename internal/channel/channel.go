package channel

import (
	"context"
	"fmt"
	"sync"

	"github.com/rs/zerolog/log"
)

// InboundMessage represents a normalized message received from any communication channel.
type InboundMessage struct {
	ChannelName string
	SenderID    string
	Text        string
}

// Channel is the extensible interface for all communication integrations (Telegram, iMessage, Discord, Slack, etc.).
type Channel interface {
	Name() string
	Start(ctx context.Context, inbound chan<- InboundMessage) error
	Send(ctx context.Context, recipient string, text string) error
}

// TypingChannel can be implemented by channels (like Telegram) that support a continuous "typing..." indicator.
type TypingChannel interface {
	StartTyping(ctx context.Context, recipient string) func()
}

// Router manages multiple active communication channels and routes messages between them and the Agent.
type Router struct {
	mu       sync.RWMutex
	channels map[string]Channel
	inbound  chan InboundMessage
}

func NewRouter() *Router {
	return &Router{
		channels: make(map[string]Channel),
		inbound:  make(chan InboundMessage, 64),
	}
}

func (r *Router) Register(ch Channel) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.channels[ch.Name()] = ch
}

func (r *Router) Inbound() <-chan InboundMessage {
	return r.inbound
}

func (r *Router) StartAll(ctx context.Context) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	for _, ch := range r.channels {
		c := ch
		go func() {
			log.Info().Str("channel", c.Name()).Msg("Starting communication channel")
			if err := c.Start(ctx, r.inbound); err != nil && ctx.Err() == nil {
				log.Error().Err(err).Str("channel", c.Name()).Msg("Channel stopped with error")
			}
		}()
	}
}

func (r *Router) Send(ctx context.Context, channelName, recipient, text string) error {
	r.mu.RLock()
	ch, ok := r.channels[channelName]
	r.mu.RUnlock()
	if !ok {
		return fmt.Errorf("channel %q not registered", channelName)
	}
	return ch.Send(ctx, recipient, text)
}

// StartTyping starts a continuous "typing..." indicator on channels that support it and returns a stop function.
func (r *Router) StartTyping(ctx context.Context, channelName, recipient string) func() {
	r.mu.RLock()
	ch, ok := r.channels[channelName]
	r.mu.RUnlock()
	if !ok {
		return func() {}
	}
	if tc, ok := ch.(TypingChannel); ok {
		return tc.StartTyping(ctx, recipient)
	}
	return func() {}
}
