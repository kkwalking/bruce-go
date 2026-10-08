package llm

import (
	"context"
	"errors"
)

// ErrNotConfigured is returned by every chat attempt made before a provider is
// set up. It is a sentinel so callers can recognize the state without matching
// on message text.
var ErrNotConfigured = errors.New("no LLM provider is configured; run /provider add to set one up")

// UnconfiguredClient is the stand-in used when setting.json has no usable
// provider, so that the rest of the program can run far enough to let the user
// configure one.
//
// Every capability reports zero, which is the value the runtime already treats
// as "unknown": automatic compaction is skipped, overflow detection is skipped,
// and the status line prints no context usage. Chat is the only method that
// fails, because it is the only one that needs a real provider.
//
// The client is immutable and therefore safe to share.
type UnconfiguredClient struct{}

// NewUnconfiguredClient returns the stand-in client.
func NewUnconfiguredClient() *UnconfiguredClient { return &UnconfiguredClient{} }

// Chat always fails: there is no endpoint to send the request to. The message
// names the command that fixes it, and it is phrased so that the "Network
// error: " prefix the agent adds still reads as a sentence.
func (c *UnconfiguredClient) Chat(context.Context, []Message, []ToolDefinition, StreamOptions) (ChatResponse, error) {
	return ChatResponse{}, ErrNotConfigured
}

// ProviderName and ModelName report a recognizable placeholder rather than an
// empty string, so a status line never renders as "/" with nothing around it.
func (c *UnconfiguredClient) ProviderName() string { return "unconfigured" }
func (c *UnconfiguredClient) ModelName() string    { return "none" }

func (c *UnconfiguredClient) MaxContextWindow() int       { return 0 }
func (c *UnconfiguredClient) MaxOutputTokens() int        { return 0 }
func (c *UnconfiguredClient) SupportsTools() bool         { return false }
func (c *UnconfiguredClient) SupportsImages() bool        { return false }
func (c *UnconfiguredClient) SupportsPromptCaching() bool { return false }

// IsNotConfigured reports whether a chat failure means "no provider is set up",
// as opposed to a transport or API error.
func IsNotConfigured(err error) bool {
	return errors.Is(err, ErrNotConfigured)
}
