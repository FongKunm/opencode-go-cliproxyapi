package plugin

// Downstream keepalive for streaming responses.
//
// A reasoning model can stay silent for a long time — DeepSeek may queue
// before the first token (its own ": keep-alive" SSE comments carry no data
// and are dropped by the converter), and a long tool argument can stall the
// stream — and an idle connection may be cut by an intermediary. When
// nothing has reached the client for streamKeepaliveInterval, the pump
// emits the client protocol's standard keepalive frame:
//
//   - claude: Anthropic's own `event: ping` / {"type":"ping"}, the frame
//     real Messages streams carry. SDKs skip it by event name, so it is
//     sent from the start of the stream, while still waiting for the first
//     upstream chunk.
//   - openai-response: an SSE comment line (": keep-alive"), the same
//     heartbeat CPA itself writes to Codex clients; the Responses event
//     vocabulary has no ping event. It is sent only after the first event:
//     CPA's Responses handler holds comment-only chunks until the first data
//     frame anyway, so an earlier comment would reach nobody while closing
//     CPA's bootstrap-retry window.
//   - openai (Chat Completions): none. CPA wraps every chunk of this format
//     in a data: line itself, so the plugin has no way to send a comment.

import (
	"errors"
	"sync"
	"time"
)

// streamKeepaliveInterval is how long a downstream stream may stay silent
// before a keepalive frame is emitted. Package var so tests can shrink it;
// zero or negative disables keepalives.
var streamKeepaliveInterval = 15 * time.Second

// claudePingFrame is Anthropic's standard keepalive event.
var claudePingFrame = []byte("event: ping\ndata: {\"type\":\"ping\"}\n\n")

// sseCommentKeepalive is the SSE comment heartbeat for Responses clients.
var sseCommentKeepalive = []byte(": keep-alive\n\n")

// keepaliveFrame returns the keepalive frame for a client protocol and
// whether it may precede the first converted event; a nil frame means the
// protocol gets no keepalive.
func keepaliveFrame(sourceFormat string) (frame []byte, beforeFirstEvent bool) {
	switch sourceFormat {
	case "claude":
		return claudePingFrame, true
	case "openai-response":
		return sseCommentKeepalive, false
	}
	return nil, false
}

// errDownstreamClosed reports a write attempted after the downstream
// stream began closing.
var errDownstreamClosed = errors.New("downstream stream closed")

// downstreamWriter serializes every write to one downstream stream, so a
// keepalive frame never interleaves with converter output, and remembers
// when the client last received bytes.
type downstreamWriter struct {
	m        *Manager
	streamID string

	mu      sync.Mutex
	last    time.Time // last successful write, or the stream start
	emitted bool      // a converted event already reached the client
	stopped bool      // the downstream is closing: no further writes
}

func newDownstreamWriter(m *Manager, streamID string) *downstreamWriter {
	return &downstreamWriter{m: m, streamID: streamID, last: time.Now()}
}

// emit forwards converted events in order.
func (w *downstreamWriter) emit(events [][]byte) error {
	if len(events) == 0 {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped {
		return errDownstreamClosed
	}
	if err := w.m.emitAll(w.streamID, events); err != nil {
		return err
	}
	w.last, w.emitted = time.Now(), true
	return nil
}

// stop forbids further writes; it waits for a write in progress, so no
// frame can follow the downstream close that comes after it.
func (w *downstreamWriter) stop() {
	w.mu.Lock()
	w.stopped = true
	w.mu.Unlock()
}

// startKeepalive runs the keepalive loop for sourceFormat until the
// returned stop function is called; stop waits for the loop to exit, so
// once it returns no keepalive write is in flight. A failed keepalive write
// is reported through fail exactly like a failed event write — the
// downstream is gone — and ends the loop. Formats without a keepalive
// frame start nothing.
func (w *downstreamWriter) startKeepalive(sourceFormat string, fail func(error)) (stop func()) {
	frame, beforeFirst := keepaliveFrame(sourceFormat)
	interval := streamKeepaliveInterval
	if frame == nil || interval <= 0 {
		return func() {}
	}
	done := make(chan struct{})
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		defer func() {
			if r := recover(); r != nil && w.m.bridge != nil {
				_ = w.m.bridge.Log("error", "stream keepalive panicked", nil)
			}
		}()
		timer := time.NewTimer(interval)
		defer timer.Stop()
		for {
			select {
			case <-done:
				return
			case <-timer.C:
			}
			wait, err := w.keepalive(frame, beforeFirst, interval)
			if err != nil {
				fail(err)
				return
			}
			if wait <= 0 {
				return // stopped
			}
			timer.Reset(wait)
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			close(done)
			<-exited
		})
	}
}

// keepalive writes frame when the stream has been silent for interval and
// returns how long to wait before the next check (0 once stopped). Before
// the first converted event, formats that must not lead with a keepalive
// just keep waiting.
func (w *downstreamWriter) keepalive(frame []byte, beforeFirst bool, interval time.Duration) (time.Duration, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped {
		return 0, nil
	}
	if idle := time.Since(w.last); idle < interval {
		return interval - idle, nil
	}
	if !beforeFirst && !w.emitted {
		return interval, nil
	}
	if err := w.m.emitAll(w.streamID, [][]byte{frame}); err != nil {
		return 0, err
	}
	w.last = time.Now()
	return interval, nil
}
