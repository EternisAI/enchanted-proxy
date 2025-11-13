package streaming

import (
	"context"
	"time"
)

// StreamSubscriber represents a single client's subscription to a stream.
//
// Each subscriber has:
//   - Unique ID for tracking and debugging
//   - Buffered channel for receiving chunks (non-blocking sends)
//   - Context for cancellation when client disconnects
//   - Join timestamp for observability
//
// Design decisions:
//   - Buffered channel (default 100 capacity): Handles burst traffic and network jitter
//   - Non-blocking sends: Slow subscribers don't block fast subscribers or upstream reading
//   - Context cancellation: Clean shutdown when client disconnects
type StreamSubscriber struct {
	// ID uniquely identifies this subscriber (typically a UUID)
	ID string

	// Ch is the channel for receiving stream chunks
	// The subscriber reads from this channel to get stream data
	Ch chan StreamChunk

	// JoinedAt is when this subscriber joined the stream
	JoinedAt time.Time

	// ctx is the subscriber's context (cancelled when client disconnects)
	ctx context.Context

	// cancel cancels the subscriber's context
	cancel context.CancelFunc

	// options are the subscriber's configuration
	options SubscriberOptions
}

// NewStreamSubscriber creates a new subscriber with the given context and options.
//
// Parameters:
//   - ctx: Context for this subscriber (typically the HTTP request context)
//   - id: Unique identifier for this subscriber
//   - opts: Configuration options (use DefaultSubscriberOptions() for defaults)
//
// Returns:
//   - *StreamSubscriber: The new subscriber
//
// The subscriber's channel is buffered to handle burst traffic.
// If the subscriber can't keep up, chunks may be dropped (non-blocking sends).
func NewStreamSubscriber(ctx context.Context, id string, opts SubscriberOptions) *StreamSubscriber {
	// Create a cancellable context derived from the provided context
	// This allows us to cancel the subscriber independently if needed
	subCtx, cancel := context.WithCancel(ctx)

	// Ensure buffer size is reasonable (min 10, max 1000)
	bufferSize := opts.BufferSize
	if bufferSize < 10 {
		bufferSize = 10
	}
	if bufferSize > 1000 {
		bufferSize = 1000
	}

	return &StreamSubscriber{
		ID:       id,
		Ch:       make(chan StreamChunk, bufferSize),
		JoinedAt: time.Now(),
		ctx:      subCtx,
		cancel:   cancel,
		options:  opts,
	}
}

// Context returns the subscriber's context.
// Useful for checking if the subscriber has been cancelled.
func (s *StreamSubscriber) Context() context.Context {
	return s.ctx
}

// Cancel cancels the subscriber's context and closes the channel.
// This should be called when the client disconnects or the stream completes.
//
// Safe to call multiple times (idempotent).
func (s *StreamSubscriber) Cancel() {
	s.cancel()
}

// Close closes the subscriber's channel.
// This should be called after all chunks have been sent.
//
// Note: Always call Cancel() before Close() to prevent sends to a closed channel.
func (s *StreamSubscriber) Close() {
	close(s.Ch)
}

// Send attempts to send a chunk to the subscriber with a timeout.
// Returns true if sent successfully, false if the subscriber is slow/disconnected.
//
// This is a non-blocking send with a timeout to prevent slow subscribers from
// blocking the broadcast loop.
//
// Parameters:
//   - chunk: The chunk to send
//   - timeout: How long to wait before giving up (typically 100ms)
//
// Returns:
//   - bool: true if sent, false if timed out or subscriber disconnected
//
// If the send times out, the subscriber is considered "slow" and this chunk is dropped.
// The subscriber will receive the next chunk, maintaining stream continuity.
func (s *StreamSubscriber) Send(chunk StreamChunk, timeout time.Duration) bool {
	select {
	case s.Ch <- chunk:
		// Sent successfully
		return true
	case <-time.After(timeout):
		// Subscriber too slow, skip this chunk
		return false
	case <-s.ctx.Done():
		// Subscriber disconnected
		return false
	}
}

// SendBlocking sends a chunk to the subscriber, blocking until sent or context cancelled.
// Used when replaying buffered chunks to late-joiners, where we want to ensure delivery.
//
// Parameters:
//   - chunk: The chunk to send
//
// Returns:
//   - bool: true if sent, false if subscriber disconnected
func (s *StreamSubscriber) SendBlocking(chunk StreamChunk) bool {
	select {
	case s.Ch <- chunk:
		return true
	case <-s.ctx.Done():
		return false
	}
}

// IsDisconnected checks if the subscriber has disconnected.
// Returns true if the context has been cancelled.
func (s *StreamSubscriber) IsDisconnected() bool {
	select {
	case <-s.ctx.Done():
		return true
	default:
		return false
	}
}

// GetOptions returns the subscriber's configuration options
func (s *StreamSubscriber) GetOptions() SubscriberOptions {
	return s.options
}
