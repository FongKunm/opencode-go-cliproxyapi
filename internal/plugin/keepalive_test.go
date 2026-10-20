package plugin

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
)

// withKeepaliveInterval shrinks streamKeepaliveInterval for one test.
func withKeepaliveInterval(t *testing.T, d time.Duration) {
	t.Helper()
	old := streamKeepaliveInterval
	streamKeepaliveInterval = d
	t.Cleanup(func() { streamKeepaliveInterval = old })
}

// emittedFrames decodes every downstream emission recorded so far, in order.
func emittedFrames(t *testing.T, f *fakeCaller) []string {
	t.Helper()
	var out []string
	for _, c := range f.callsOf(pluginabi.MethodHostStreamEmit) {
		out = append(out, string(wireBody(t, decodePayload(t, c), "payload")))
	}
	return out
}

func countFrames(frames []string, frame []byte) int {
	n := 0
	for _, f := range frames {
		if f == string(frame) {
			n++
		}
	}
	return n
}

// gatedStream scripts an upstream stream whose reads are released one at
// a time: read i returns frames[i] once gates[i] is closed (a nil gate
// returns immediately); reads past the script report a clean end.
type gatedStream struct {
	mu     sync.Mutex
	reads  int
	frames []string
	gates  []chan struct{}
	once   []sync.Once
}

func newGatedStream(frames ...string) *gatedStream {
	return &gatedStream{frames: frames, gates: make([]chan struct{}, len(frames)), once: make([]sync.Once, len(frames))}
}

// gate makes read i block until release(i).
func (g *gatedStream) gate(i int) {
	g.gates[i] = make(chan struct{})
}

func (g *gatedStream) release(i int) {
	if g.gates[i] != nil {
		g.once[i].Do(func() { close(g.gates[i]) })
	}
}

func (g *gatedStream) releaseAll() {
	for i := range g.gates {
		g.release(i)
	}
}

func (g *gatedStream) respond(method string, _ []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodHostHTTPDoStream:
		return hostOK(hostStreamStartResp{
			StatusCode: http.StatusOK,
			Headers:    http.Header{"Content-Type": []string{"text/event-stream"}},
			StreamID:   "up-ka",
		}), nil
	case pluginabi.MethodHostHTTPStreamRead:
		g.mu.Lock()
		i := g.reads
		g.reads++
		g.mu.Unlock()
		if i >= len(g.frames) {
			return hostOK(hostStreamReadResp{Done: true}), nil
		}
		if g.gates[i] != nil {
			<-g.gates[i]
		}
		return hostOK(hostStreamReadResp{Payload: []byte(g.frames[i])}), nil
	default:
		return hostOK(map[string]any{}), nil
	}
}

func newGatedManager(t *testing.T, g *gatedStream, emitResponder func([]byte) ([]byte, error)) (*Manager, *fakeCaller) {
	t.Helper()
	f := &fakeCaller{}
	m := NewManager(NewHostBridge(f.call))
	t.Cleanup(func() { _, _ = m.HandleCall("plugin.shutdown", nil) })
	t.Cleanup(g.releaseAll)
	f.responder = wrapWithCatalog(multiRouteCatalog, func(method string, payload []byte) ([]byte, error) {
		if method == pluginabi.MethodHostStreamEmit && emitResponder != nil {
			return emitResponder(payload)
		}
		return g.respond(method, payload)
	})
	if _, err := m.HandleCall("plugin.register", lifecycleRequestBody(testValidYAML)); err != nil {
		t.Fatalf("register: %v", err)
	}
	return m, f
}

func startStream(t *testing.T, m *Manager, format string) {
	t.Helper()
	resp, err := m.HandleCall("executor.execute_stream",
		execStreamReqBody("opencode-go/glm-5.3", format, []byte(ccRequestBody), "down-ka"))
	if err != nil {
		t.Fatalf("execute_stream: %v", err)
	}
	if env := decodeEnv(t, resp); !env.OK {
		t.Fatalf("execute_stream envelope = %+v", env.Error)
	}
}

const (
	kaChunkText = `data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"}}]}` + "\n\n"
	kaChunkMore = `data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":" there"}}]}` + "\n\n"
	kaChunkStop = `data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
		"data: [DONE]\n\n"
)

// Claude: while the first upstream chunk is outstanding the client gets
// Anthropic ping events, then the normal message stream follows.
func TestKeepaliveClaudePingsWhileWaitingForFirstChunk(t *testing.T) {
	withKeepaliveInterval(t, 20*time.Millisecond)
	g := newGatedStream(kaChunkText, kaChunkStop)
	g.gate(0)
	m, f := newGatedManager(t, g, nil)
	startStream(t, m, "claude")

	waitFor(t, "two pings before the first chunk", func() bool {
		return countFrames(emittedFrames(t, f), claudePingFrame) >= 2
	})
	g.release(0)
	m.bridge.WaitForInFlight(5 * time.Second)

	frames := emittedFrames(t, f)
	firstEvent := -1
	for i, fr := range frames {
		if fr != string(claudePingFrame) {
			firstEvent = i
			break
		}
	}
	if firstEvent < 2 || !strings.HasPrefix(frames[firstEvent], "event: message_start\n") {
		t.Fatalf("pings must lead while waiting, then message_start; frames = %q", frames)
	}
	if !strings.HasPrefix(frames[len(frames)-1], "event: message_stop\n") {
		t.Fatalf("stream must still end with message_stop: %q", frames[len(frames)-1])
	}
	downCloses := f.callsOf(pluginabi.MethodHostStreamClose)
	if len(downCloses) != 1 || strings.Contains(string(downCloses[0].payload), `"error"`) {
		t.Fatalf("downstream must close cleanly: %v", downCloses)
	}
}

// Claude: a mid-stream stall gets pings too; no frame follows the close.
func TestKeepaliveClaudePingsDuringMidStreamStallAndStopsAfterClose(t *testing.T) {
	withKeepaliveInterval(t, 20*time.Millisecond)
	g := newGatedStream(kaChunkText, kaChunkMore, kaChunkStop)
	g.gate(1)
	m, f := newGatedManager(t, g, nil)
	startStream(t, m, "claude")

	waitFor(t, "a ping after message_start", func() bool {
		frames := emittedFrames(t, f)
		for i, fr := range frames {
			if strings.HasPrefix(fr, "event: message_start\n") {
				return countFrames(frames[i+1:], claudePingFrame) >= 1
			}
		}
		return false
	})
	g.release(1)
	m.bridge.WaitForInFlight(5 * time.Second)
	settled := len(emittedFrames(t, f))
	time.Sleep(100 * time.Millisecond) // five intervals: a leaked loop would ping
	if got := len(emittedFrames(t, f)); got != settled {
		t.Fatalf("frames emitted after the stream closed: %d -> %d", settled, got)
	}
}

// Responses: no keepalive before the first event (CPA would hold it and
// it would close the bootstrap window), SSE comment heartbeats after it.
func TestKeepaliveResponsesCommentOnlyAfterFirstEvent(t *testing.T) {
	withKeepaliveInterval(t, 20*time.Millisecond)
	g := newGatedStream(kaChunkText, kaChunkMore, kaChunkStop)
	g.gate(0)
	g.gate(1)
	m, f := newGatedManager(t, g, nil)
	startStream(t, m, "openai-response")

	time.Sleep(120 * time.Millisecond) // six intervals before the first chunk
	if frames := emittedFrames(t, f); len(frames) != 0 {
		t.Fatalf("nothing may be emitted before the first event: %q", frames)
	}
	g.release(0)
	waitFor(t, "a keepalive comment after response.created", func() bool {
		return countFrames(emittedFrames(t, f), sseCommentKeepalive) >= 1
	})
	g.release(1)
	m.bridge.WaitForInFlight(5 * time.Second)
	frames := emittedFrames(t, f)
	if !strings.HasPrefix(frames[0], "event: response.created\n") {
		t.Fatalf("first frame must be response.created: %q", frames)
	}
	if !strings.HasPrefix(frames[len(frames)-1], "event: response.completed\n") {
		t.Fatalf("stream must still end with response.completed: %q", frames[len(frames)-1])
	}
}

// Chat Completions passthrough gets no keepalive: CPA frames this format.
func TestKeepaliveNoneForChatCompletions(t *testing.T) {
	withKeepaliveInterval(t, 20*time.Millisecond)
	g := newGatedStream(kaChunkText, kaChunkMore, kaChunkStop)
	g.gate(1)
	m, f := newGatedManager(t, g, nil)
	startStream(t, m, "openai")

	waitFor(t, "the first chunk", func() bool { return len(emittedFrames(t, f)) >= 1 })
	time.Sleep(120 * time.Millisecond)
	g.release(1)
	m.bridge.WaitForInFlight(5 * time.Second)
	for _, fr := range emittedFrames(t, f) {
		if strings.Contains(fr, "ping") || strings.HasPrefix(fr, ":") {
			t.Fatalf("openai stream got a keepalive frame: %q", fr)
		}
	}
}

// A keepalive write that fails means the downstream is gone: both streams
// close with the error, like a failed event write.
func TestKeepaliveEmitFailureFailsStream(t *testing.T) {
	withKeepaliveInterval(t, 20*time.Millisecond)
	g := newGatedStream(kaChunkText, kaChunkStop)
	g.gate(0)
	m, f := newGatedManager(t, g, func(payload []byte) ([]byte, error) {
		var wire struct {
			Payload []byte `json:"payload"`
		}
		_ = json.Unmarshal(payload, &wire)
		if string(wire.Payload) == string(claudePingFrame) {
			return nil, errors.New("downstream reset")
		}
		return hostOK(map[string]any{}), nil
	})
	startStream(t, m, "claude")

	waitFor(t, "the downstream close", func() bool {
		return len(f.callsOf(pluginabi.MethodHostStreamClose)) >= 1
	})
	g.releaseAll()
	m.bridge.WaitForInFlight(5 * time.Second)
	downCloses := f.callsOf(pluginabi.MethodHostStreamClose)
	if len(downCloses) != 1 || !strings.Contains(string(downCloses[0].payload), "downstream reset") {
		t.Fatalf("downstream must close once with the write error: %v", downCloses)
	}
	upCloses := f.callsOf(pluginabi.MethodHostHTTPStreamClose)
	if len(upCloses) == 0 || !strings.Contains(string(upCloses[0].payload), `"up-ka"`) {
		t.Fatalf("upstream must be closed: %v", upCloses)
	}
	for _, fr := range emittedFrames(t, f) {
		if strings.HasPrefix(fr, "event: message_start") {
			t.Fatalf("no event may be written after the failed keepalive closed the stream: %q", fr)
		}
	}
}

// ---- downstreamWriter unit behavior ----

func newWriterUnderTest(t *testing.T) (*downstreamWriter, *fakeCaller) {
	t.Helper()
	f := &fakeCaller{}
	m := NewManager(NewHostBridge(f.call))
	return newDownstreamWriter(m, "down-unit"), f
}

func TestDownstreamWriterKeepaliveOnlyAfterSilence(t *testing.T) {
	w, f := newWriterUnderTest(t)
	if err := w.emit([][]byte{[]byte("event: a\ndata: {}\n\n")}); err != nil {
		t.Fatalf("emit: %v", err)
	}
	wait, err := w.keepalive(claudePingFrame, true, time.Hour)
	if err != nil || wait <= 0 || wait > time.Hour {
		t.Fatalf("fresh stream: wait=%v err=%v, want the remaining interval", wait, err)
	}
	if got := len(f.callsOf(pluginabi.MethodHostStreamEmit)); got != 1 {
		t.Fatalf("no keepalive may be written while traffic is fresh: %d emits", got)
	}
	w.last = time.Now().Add(-2 * time.Hour)
	if wait, err = w.keepalive(claudePingFrame, true, time.Hour); err != nil || wait != time.Hour {
		t.Fatalf("silent stream: wait=%v err=%v", wait, err)
	}
	frames := emittedFrames(t, f)
	if len(frames) != 2 || frames[1] != string(claudePingFrame) {
		t.Fatalf("silent stream must get one ping: %q", frames)
	}
	if time.Since(w.last) > time.Minute {
		t.Fatal("a keepalive write must reset the silence clock")
	}
}

func TestDownstreamWriterKeepaliveWaitsForFirstEventWhenRequired(t *testing.T) {
	w, f := newWriterUnderTest(t)
	w.last = time.Now().Add(-2 * time.Hour)
	if wait, err := w.keepalive(sseCommentKeepalive, false, time.Hour); err != nil || wait != time.Hour {
		t.Fatalf("wait=%v err=%v", wait, err)
	}
	if got := len(f.callsOf(pluginabi.MethodHostStreamEmit)); got != 0 {
		t.Fatalf("no comment may precede the first event: %d emits", got)
	}
}

func TestDownstreamWriterStopBlocksEveryWrite(t *testing.T) {
	w, f := newWriterUnderTest(t)
	w.stop()
	w.last = time.Now().Add(-2 * time.Hour)
	if wait, err := w.keepalive(claudePingFrame, true, time.Hour); err != nil || wait != 0 {
		t.Fatalf("stopped keepalive: wait=%v err=%v, want 0 (loop exit)", wait, err)
	}
	if err := w.emit([][]byte{[]byte("x")}); !errors.Is(err, errDownstreamClosed) {
		t.Fatalf("emit after stop = %v, want errDownstreamClosed", err)
	}
	if got := len(f.callsOf(pluginabi.MethodHostStreamEmit)); got != 0 {
		t.Fatalf("stopped writer emitted %d frames", got)
	}
}

func TestKeepaliveFramePerFormat(t *testing.T) {
	if frame, before := keepaliveFrame("claude"); string(frame) != "event: ping\ndata: {\"type\":\"ping\"}\n\n" || !before {
		t.Fatalf("claude keepalive = %q before=%v", frame, before)
	}
	if frame, before := keepaliveFrame("openai-response"); string(frame) != ": keep-alive\n\n" || before {
		t.Fatalf("responses keepalive = %q before=%v", frame, before)
	}
	if frame, _ := keepaliveFrame("openai"); frame != nil {
		t.Fatalf("openai must get no keepalive, got %q", frame)
	}
	if stop := (&downstreamWriter{}).startKeepalive("openai", func(error) {}); stop == nil {
		t.Fatal("startKeepalive must always return a stop func")
	} else {
		stop()
	}
}
