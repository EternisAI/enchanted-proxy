package streaming

import (
	"context"
	"testing"
	"time"
)

func TestNewStreamSubscriber(t *testing.T) {
	ctx := context.Background()
	opts := DefaultSubscriberOptions()

	sub := NewStreamSubscriber(ctx, "test-subscriber-1", opts)

	if sub == nil {
		t.Fatal("NewStreamSubscriber returned nil")
	}
	if sub.ID != "test-subscriber-1" {
		t.Errorf("expected ID 'test-subscriber-1', got %s", sub.ID)
	}
	if sub.Ch == nil {
		t.Error("subscriber channel is nil")
	}
	if sub.JoinedAt.IsZero() {
		t.Error("JoinedAt timestamp is zero")
	}
	if cap(sub.Ch) != opts.BufferSize {
		t.Errorf("expected channel capacity %d, got %d", opts.BufferSize, cap(sub.Ch))
	}
}

func TestSubscriberSend(t *testing.T) {
	ctx := context.Background()
	opts := SubscriberOptions{
		ReplayFromStart: false,
		BufferSize:      10,
	}

	sub := NewStreamSubscriber(ctx, "test-sub", opts)
	defer sub.Close()

	chunk := StreamChunk{
		Index:     0,
		Line:      "data: test",
		Timestamp: time.Now(),
		IsFinal:   false,
		IsError:   false,
	}

	// Test successful send
	sent := sub.Send(chunk, 100*time.Millisecond)
	if !sent {
		t.Error("failed to send chunk to subscriber")
	}

	// Verify chunk was received
	select {
	case received := <-sub.Ch:
		if received.Line != chunk.Line {
			t.Errorf("expected line %s, got %s", chunk.Line, received.Line)
		}
	case <-time.After(100 * time.Millisecond):
		t.Error("timeout waiting for chunk")
	}
}

func TestSubscriberSendBlocking(t *testing.T) {
	ctx := context.Background()
	opts := SubscriberOptions{
		ReplayFromStart: false,
		BufferSize:      2,
	}

	sub := NewStreamSubscriber(ctx, "test-sub", opts)
	defer sub.Close()

	chunk1 := StreamChunk{Index: 0, Line: "data: chunk1", Timestamp: time.Now()}
	chunk2 := StreamChunk{Index: 1, Line: "data: chunk2", Timestamp: time.Now()}

	// Send two chunks (fill buffer)
	if !sub.SendBlocking(chunk1) {
		t.Error("failed to send first chunk")
	}
	if !sub.SendBlocking(chunk2) {
		t.Error("failed to send second chunk")
	}

	// Verify both chunks received
	received1 := <-sub.Ch
	if received1.Line != chunk1.Line {
		t.Errorf("expected %s, got %s", chunk1.Line, received1.Line)
	}

	received2 := <-sub.Ch
	if received2.Line != chunk2.Line {
		t.Errorf("expected %s, got %s", chunk2.Line, received2.Line)
	}
}

func TestSubscriberCancel(t *testing.T) {
	ctx := context.Background()
	opts := DefaultSubscriberOptions()

	sub := NewStreamSubscriber(ctx, "test-sub", opts)

	// Cancel the subscriber
	sub.Cancel()

	// Context should be done
	select {
	case <-sub.Context().Done():
		// Expected
	case <-time.After(100 * time.Millisecond):
		t.Error("context not cancelled after Cancel()")
	}
}

func TestSubscriberIsDisconnected(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	opts := DefaultSubscriberOptions()

	sub := NewStreamSubscriber(ctx, "test-sub", opts)

	// Should not be disconnected initially
	if sub.IsDisconnected() {
		t.Error("subscriber should not be disconnected initially")
	}

	// Cancel parent context
	cancel()

	// Should be disconnected now
	time.Sleep(10 * time.Millisecond) // Give time for context cancellation to propagate
	if !sub.IsDisconnected() {
		t.Error("subscriber should be disconnected after context cancellation")
	}
}

func TestSubscriberSendTimeout(t *testing.T) {
	ctx := context.Background()
	opts := SubscriberOptions{
		ReplayFromStart: false,
		BufferSize:      10, // Minimum enforced buffer size
	}

	sub := NewStreamSubscriber(ctx, "test-sub", opts)
	defer sub.Close()

	// Fill buffer completely (minimum is 10 after adjustment)
	for i := 0; i < 10; i++ {
		chunk := StreamChunk{Index: i, Line: "data: chunk", Timestamp: time.Now()}
		if !sub.Send(chunk, 100*time.Millisecond) {
			t.Fatalf("failed to send chunk %d", i)
		}
	}

	// Try to send another chunk (should timeout because buffer is full)
	extraChunk := StreamChunk{Index: 10, Line: "data: chunk10", Timestamp: time.Now()}
	sent := sub.Send(extraChunk, 10*time.Millisecond)
	if sent {
		t.Error("send should have timed out with full buffer")
	}
}

func TestSubscriberBufferSizeLimits(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name         string
		inputSize    int
		expectedSize int
	}{
		{"too small", 5, 10}, // Minimum is 10
		{"just right", 50, 50},
		{"too large", 2000, 1000}, // Maximum is 1000
		{"zero", 0, 10},           // Default to minimum
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := SubscriberOptions{
				BufferSize: tt.inputSize,
			}
			sub := NewStreamSubscriber(ctx, "test-sub", opts)
			defer sub.Close()

			if cap(sub.Ch) != tt.expectedSize {
				t.Errorf("expected buffer size %d, got %d", tt.expectedSize, cap(sub.Ch))
			}
		})
	}
}

func TestSubscriberGetOptions(t *testing.T) {
	ctx := context.Background()
	opts := SubscriberOptions{
		ReplayFromStart: true,
		BufferSize:      150,
	}

	sub := NewStreamSubscriber(ctx, "test-sub", opts)
	defer sub.Close()

	retrievedOpts := sub.GetOptions()
	if retrievedOpts.ReplayFromStart != opts.ReplayFromStart {
		t.Errorf("expected ReplayFromStart %v, got %v", opts.ReplayFromStart, retrievedOpts.ReplayFromStart)
	}
	// Note: BufferSize may be adjusted by limits, so we check the actual channel capacity
	if cap(sub.Ch) != 150 {
		t.Errorf("expected actual buffer size 150, got %d", cap(sub.Ch))
	}
}

func TestDefaultSubscriberOptions(t *testing.T) {
	opts := DefaultSubscriberOptions()

	if opts.ReplayFromStart {
		t.Error("default ReplayFromStart should be false")
	}
	if opts.BufferSize != 100 {
		t.Errorf("default BufferSize should be 100, got %d", opts.BufferSize)
	}
}
