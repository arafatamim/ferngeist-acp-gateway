package session

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/runtime"
	"github.com/coder/websocket"
)

func newSessionListPump(t *testing.T, decorate func(context.Context, string, string, []byte) ([]byte, error)) (*StdioPump, *websocket.Conn) {
	t.Helper()
	return newDecoratedPump(t, ReplyDecorator{Method: "session/list", FinalOnly: true, Decorate: decorate})
}

func newDecoratedPump(t *testing.T, decorators ...ReplyDecorator) (*StdioPump, *websocket.Conn) {
	t.Helper()
	serverCh := make(chan *websocket.Conn, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err == nil {
			serverCh <- conn
		}
	}))
	t.Cleanup(srv.Close)
	client, _, err := websocket.Dial(context.Background(), "ws://"+srv.Listener.Addr().String()+"/", nil)
	if err != nil {
		t.Fatalf("dial client: %v", err)
	}
	t.Cleanup(func() { _ = client.CloseNow() }) // Test teardown: no handshake to wait on.
	p := &StdioPump{
		agentID: "hermes", logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		replyDecorators: decorators,
	}
	gen := p.Attach()
	if !p.Bind(<-serverCh, gen) {
		t.Fatal("Bind should succeed")
	}
	done := p.writerDone
	t.Cleanup(func() { p.Detach(gen); <-done })
	return p, client
}

func startSessionListDrain(t *testing.T, p *StdioPump) (*io.PipeWriter, context.CancelFunc) {
	t.Helper()
	r, w := io.Pipe()
	p.proc = &runtime.Process{Stdout: r}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.StdoutDrainLoop(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		_ = w.Close() // Unblock the pipe read during teardown.
		<-done
		_ = r.Close() // The test owns both pipe ends.
	})
	return w, cancel
}

// decorateNative replaces the reply's empty session list with one native chat,
// so a test can tell the decorator's bytes from the agent's.
func decorateNative(reply []byte) []byte {
	return bytes.Replace(reply, []byte(`"sessions":[]`),
		[]byte(`"sessions":[{"sessionId":"native","cwd":"/work","title":"TUI chat"}]`), 1)
}

func TestSessionListDecorationDoesNotDelayFollowingFrames(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	p, client := newSessionListPump(t, func(ctx context.Context, _, _ string, reply []byte) ([]byte, error) {
		close(entered)
		select {
		case <-release:
			return decorateNative(reply), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	w, _ := startSessionListDrain(t, p)
	request := parseClientFrame([]byte(`{"jsonrpc":"2.0","id":"list","method":"session/list","params":{}}`))
	p.prepareOutbound(p.connGen, request)
	const notification = `{"jsonrpc":"2.0","method":"agent/progress","params":{}}`
	reply := `{"jsonrpc":"2.0","id":` + string(request.ID) + `,"result":{"sessions":[]}}`
	if _, err := w.Write([]byte(reply + "\n" + notification + "\n")); err != nil {
		t.Fatal(err)
	}
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, msg, err := client.Read(ctx)
	if err != nil {
		t.Fatalf("following frame stalled behind decoration: %v", err)
	}
	if string(msg) != notification {
		t.Fatalf("first frame = %s, want the progress notification", msg)
	}
	close(release)
	_, msg, err = client.Read(ctx)
	if err != nil {
		t.Fatalf("read decorated reply: %v", err)
	}
	got := decodeSessionList(t, string(msg))
	if len(got.Result.Sessions) != 1 || got.Result.Sessions[0].SessionID != "native" {
		t.Fatalf("sessions = %+v, want native", got.Result.Sessions)
	}
	var decoded struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(msg, &decoded); err != nil || decoded.ID != "list" {
		t.Fatalf("reply = %s, want the client's list id (error %v)", msg, err)
	}
}

func TestSessionListDecorationIsCancelledWhenThePumpStops(t *testing.T) {
	entered := make(chan context.Context, 1)
	p, client := newSessionListPump(t, func(ctx context.Context, _, _ string, _ []byte) ([]byte, error) {
		entered <- ctx
		<-ctx.Done()
		return nil, ctx.Err()
	})
	w, cancel := startSessionListDrain(t, p)
	request := parseClientFrame([]byte(`{"jsonrpc":"2.0","id":1,"method":"session/list","params":{}}`))
	p.prepareOutbound(p.connGen, request)
	if _, err := w.Write([]byte(`{"jsonrpc":"2.0","id":` + string(request.ID) + `,"result":{"sessions":[]}}` + "\n")); err != nil {
		t.Fatal(err)
	}
	readCtx := <-entered
	cancel()
	_ = w.Close() // Closing stdout wakes the drain so it can finish teardown.
	select {
	case <-readCtx.Done():
		if readCtx.Err() != context.Canceled {
			t.Fatalf("decorator error = %v, want pump cancellation", readCtx.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("decoration outlived pump shutdown")
	}
	ctx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	_, msg, err := client.Read(ctx)
	if err != nil {
		t.Fatalf("shutdown lost the agent's list reply: %v", err)
	}
	var reply struct {
		ID     int `json:"id"`
		Result *struct {
			Sessions []json.RawMessage `json:"sessions"`
		} `json:"result"`
	}
	if err := json.Unmarshal(msg, &reply); err != nil {
		t.Fatal(err)
	}
	if reply.ID != 1 || reply.Result == nil || len(reply.Result.Sessions) != 0 {
		t.Fatalf("shutdown reply = %s, want the original empty list with client id 1", msg)
	}
}

func TestSessionListHandsTheRequestToTheDecorator(t *testing.T) {
	var gotAgent, gotCwd string
	var gotReply []byte
	const decorated = `{"jsonrpc":"2.0","id":7,"result":{"sessions":[{"sessionId":"native"}]}}`
	p := &StdioPump{
		agentID: "hermes",
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		replyDecorators: []ReplyDecorator{{
			Method:    "session/list",
			FinalOnly: true,
			Decorate: func(_ context.Context, agentID, cwd string, reply []byte) ([]byte, error) {
				gotAgent, gotCwd, gotReply = agentID, cwd, reply
				return []byte(decorated), nil
			},
		}},
	}
	reply := `{"jsonrpc":"2.0","id":7,"result":{"sessions":[],"nextCursor":null}}`
	f := &frame{
		line:    reply,
		out:     []string{reply},
		isReply: true,
		origin:  pendingRequest{method: "session/list", cwd: "/work"},
	}

	p.applyDecorators(context.Background(), f, p.decoratorsFor(f))

	if gotAgent != "hermes" || gotCwd != "/work" {
		t.Errorf("decorator queried with agent=%q cwd=%q, want hermes /work", gotAgent, gotCwd)
	}
	if string(gotReply) != reply {
		t.Errorf("decorator reply = %s, want the agent's own bytes", gotReply)
	}
	if f.out[0] != decorated || f.line != decorated {
		t.Errorf("forwarded reply = %s (line %s), want the decorator's", f.out[0], f.line)
	}
}

func TestSessionListPassesThroughWhenDecorationFails(t *testing.T) {
	p := &StdioPump{
		agentID: "hermes",
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		replyDecorators: []ReplyDecorator{{
			Method:    "session/list",
			FinalOnly: true,
			Decorate: func(context.Context, string, string, []byte) ([]byte, error) {
				return nil, context.DeadlineExceeded
			},
		}},
	}
	reply := `{"jsonrpc":"2.0","id":7,"result":{"sessions":[],"nextCursor":null}}`
	f := &frame{
		line:    reply,
		out:     []string{reply},
		isReply: true,
		origin:  pendingRequest{method: "session/list"},
	}

	p.applyDecorators(context.Background(), f, p.decoratorsFor(f))

	if f.out[0] != reply {
		t.Errorf("reply after decorator failure = %s, want it untouched", f.out[0])
	}
}

func TestSessionListLeavesPagesWithACursorAlone(t *testing.T) {
	called := false
	p, _ := newSessionListPump(t, func(_ context.Context, _, _ string, reply []byte) ([]byte, error) {
		called = true
		return decorateNative(reply), nil
	})
	reply := `{"jsonrpc":"2.0","id":7,"result":{"sessions":[],"nextCursor":"next"}}`
	f := &frame{
		line:    reply,
		out:     []string{reply},
		isReply: true,
		origin:  pendingRequest{method: "session/list", gen: p.connGen},
	}

	if p.forwardDecorated(f) {
		t.Fatal("a page with a cursor was deferred for decoration")
	}
	if called {
		t.Error("the decorator ran for a page that is not the last")
	}
	if f.out[0] != reply {
		t.Errorf("paged reply = %s, want it untouched", f.out[0])
	}
}

func TestReplyDecoratorIgnoresOtherMethods(t *testing.T) {
	called := false
	p := &StdioPump{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		replyDecorators: []ReplyDecorator{{
			Method:    "session/list",
			FinalOnly: true,
			Decorate: func(context.Context, string, string, []byte) ([]byte, error) {
				called = true
				return nil, nil
			},
		}},
	}
	reply := `{"jsonrpc":"2.0","id":7,"result":{"protocolVersion":1}}`
	f := &frame{
		line:    reply,
		out:     []string{reply},
		isReply: true,
		origin:  pendingRequest{method: "session/load"},
	}

	if p.forwardDecorated(f) {
		t.Fatal("a reply to another method was deferred for decoration")
	}
	if called {
		t.Error("the session/list decorator ran for a session/load reply")
	}
}

func TestReplyDecoratorsRunInRegistrationOrder(t *testing.T) {
	reply := `{"jsonrpc":"2.0","id":7,"result":{"sessions":[],"nextCursor":null}}`
	p := &StdioPump{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		replyDecorators: []ReplyDecorator{
			{Method: "session/list", Decorate: func(_ context.Context, _, _ string, reply []byte) ([]byte, error) {
				return append(reply, " one"...), nil
			}},
			{Method: "session/list", Decorate: func(_ context.Context, _, _ string, reply []byte) ([]byte, error) {
				return append(reply, " two"...), nil
			}},
		},
	}
	f := &frame{
		line:    reply,
		out:     []string{reply},
		isReply: true,
		origin:  pendingRequest{method: "session/list"},
	}

	p.applyDecorators(context.Background(), f, p.decoratorsFor(f))

	if want := reply + " one two"; f.out[0] != want || f.line != want {
		t.Errorf("decorated reply = %s (line %s), want %s", f.out[0], f.line, want)
	}
}

func TestInlineReplyDecoratorRunsBeforeForward(t *testing.T) {
	p, client := newDecoratedPump(t, ReplyDecorator{
		Method: "session/list",
		Decorate: func(_ context.Context, _, _ string, reply []byte) ([]byte, error) {
			return decorateNative(reply), nil
		},
	})
	request := parseClientFrame([]byte(`{"jsonrpc":"2.0","id":"list","method":"session/list","params":{}}`))
	p.prepareOutbound(p.connGen, request)
	reply := `{"jsonrpc":"2.0","id":` + string(request.ID) + `,"result":{"sessions":[]}}`

	p.handleStdoutLine(reply)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, msg, err := client.Read(ctx)
	if err != nil {
		t.Fatalf("read forwarded reply: %v", err)
	}
	got := decodeSessionList(t, string(msg))
	if len(got.Result.Sessions) != 1 || got.Result.Sessions[0].SessionID != "native" {
		t.Fatalf("sessions = %+v, want native", got.Result.Sessions)
	}
}

type decodedSessionList struct {
	Result struct {
		Sessions []struct {
			SessionID string `json:"sessionId"`
			Cwd       string `json:"cwd"`
			Title     string `json:"title"`
			UpdatedAt string `json:"updatedAt"`
		} `json:"sessions"`
		NextCursor *string `json:"nextCursor"`
	} `json:"result"`
}

func decodeSessionList(t *testing.T, line string) decodedSessionList {
	t.Helper()
	var decoded decodedSessionList
	if err := json.Unmarshal([]byte(line), &decoded); err != nil {
		t.Fatalf("decode session/list reply: %v", err)
	}
	return decoded
}

// TestSessionListMergesOnTheForwardedReply runs the client request through the
// real request-id mapping and the pump's stage list, so the decorator's reply is
// what the client receives under its own request id.
func TestSessionListMergesOnTheForwardedReply(t *testing.T) {
	gotCwd := make(chan string, 1)
	pump, client := newSessionListPump(t, func(_ context.Context, _, cwd string, reply []byte) ([]byte, error) {
		gotCwd <- cwd
		return decorateNative(reply), nil
	})
	gen := pump.connGen

	request := parseClientFrame([]byte(
		`{"jsonrpc":"2.0","id":1,"method":"session/list","params":{"cwd":"/work"}}`))
	forwarded := pump.prepareOutbound(gen, request)
	var id struct {
		ID json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal(forwarded, &id); err != nil {
		t.Fatalf("decode forwarded request: %v", err)
	}
	reply := `{"jsonrpc":"2.0","id":` + string(id.ID) + `,"result":{"sessions":[],"nextCursor":null}}`
	pump.handleStdoutLine(reply)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, msg, err := client.Read(ctx)
	if err != nil {
		t.Fatalf("read forwarded reply: %v", err)
	}
	var top struct {
		ID     json.RawMessage `json:"id"`
		Result struct {
			Sessions []struct {
				SessionID string `json:"sessionId"`
			} `json:"sessions"`
		} `json:"result"`
	}
	if err := json.Unmarshal(msg, &top); err != nil {
		t.Fatalf("decode forwarded reply: %v", err)
	}
	if cwd := <-gotCwd; cwd != "/work" {
		t.Errorf("decorator queried with cwd %q, want the request's /work", cwd)
	}
	if string(top.ID) != "1" {
		t.Errorf("forwarded reply id = %s, want the client's id 1", top.ID)
	}
	if len(top.Result.Sessions) != 1 || top.Result.Sessions[0].SessionID != "native" {
		t.Errorf("forwarded sessions = %+v, want only native", top.Result.Sessions)
	}
}
