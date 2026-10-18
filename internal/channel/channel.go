package channel

import (
	"context"
	"fmt"
	"sync"

	"github.com/rs/zerolog/log"
)

// InboundMessage represents a normalized message received from any communication channel.
type InboundMessage struct {
	ChannelName      string
	SenderID         string
	Text             string
	MessageID        int
	ReplyToMessageID int
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

// CommandSyncChannel can be implemented by channels (like Telegram) that support syncing a slash command menu.
type CommandSyncChannel interface {
	SyncCommands(ctx context.Context, commands []BotCommand) error
}

// MessageDeletableChannel can be implemented by channels (like Telegram) that support deleting messages by ID.
type MessageDeletableChannel interface {
	DeleteMessage(ctx context.Context, chatID string, messageID int) error
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

// SendStream sends chunks of text to a recipient via the specified channel.
func (r *Router) SendStream(ctx context.Context, channelName, recipient string, chunks <-chan string) error {
	r.mu.RLock()
	ch, ok := r.channels[channelName]
	r.mu.RUnlock()
	if !ok {
		return fmt.Errorf("channel %q not registered", channelName)
	}

	// Check if the channel supports streaming
	if streamer, ok := ch.(interface{ SendStream(ctx context.Context, recipient string, chunks <-chan string) error }); ok {
		return streamer.SendStream(ctx, recipient, chunks)
	}

	// Fallback: send chunks as individual messages
	for chunk := range chunks {
		if err := ch.Send(ctx, recipient, chunk); err != nil {
			return fmt.Errorf("failed to send chunk: %w", err)
		}
	}
	return nil
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

// SyncChannelCommands syncs the bot command list on channels that implement CommandSyncChannel.
func (r *Router) SyncChannelCommands(ctx context.Context, channelName string, commands []BotCommand) error {
	r.mu.RLock()
	ch, ok := r.channels[channelName]
	r.mu.RUnlock()
	if !ok {
		return fmt.Errorf("channel %q not registered", channelName)
	}
	if sc, ok := ch.(CommandSyncChannel); ok {
		return sc.SyncCommands(ctx, commands)
	}
	return fmt.Errorf("channel %q does not support command menu sync", channelName)
}

// DeleteMessage deletes a message by ID on channels that implement MessageDeletableChannel.
func (r *Router) DeleteMessage(ctx context.Context, channelName, chatID string, messageID int) error {
	r.mu.RLock()
	ch, ok := r.channels[channelName]
	r.mu.RUnlock()
	if !ok {
		return fmt.Errorf("channel %q not registered", channelName)
	}
	if dc, ok := ch.(MessageDeletableChannel); ok {
		return dc.DeleteMessage(ctx, chatID, messageID)
	}
	return fmt.Errorf("channel %q does not support message deletion", channelName)
}

