package harness

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/agentext"
)

type fakeSessionSource struct {
	sessions []Session
	err      error
	cwd      string
	calls    int
}

func (f *fakeSessionSource) Sessions(_ context.Context, cwd string) ([]Session, error) {
	f.calls++
	f.cwd = cwd
	return f.sessions, f.err
}

func TestMergeSessionListAppendsUnseenNativeChats(t *testing.T) {
	reply := []byte(`{"jsonrpc":"2.0","id":7,"result":{"sessions":[{"sessionId":"acp-1","cwd":"/work","unknown":{"big":9007199254740993},"nullable":null}],"nextCursor":null},"_meta":{"trace":"kept"}}`)
	out := mergeSessionList(reply, []Session{
		{ID: "acp-1", Cwd: "/work"},
		{ID: "tui-1", Cwd: "/work", Title: "TUI chat", UpdatedAt: "2026-11-11T10:00:00Z"},
	})
	if out == nil {
		t.Fatal("no reply was produced for a list with a new chat")
	}
	var top struct {
		Meta   map[string]string `json:"_meta"`
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
	if err := json.Unmarshal(out, &top); err != nil {
		t.Fatalf("decode merged reply: %v", err)
	}
	if len(top.Result.Sessions) != 2 {
		t.Fatalf("sessions = %+v, want the ACP chat plus one native chat", top.Result.Sessions)
	}
	if top.Result.Sessions[0].SessionID != "acp-1" || top.Result.Sessions[1].SessionID != "tui-1" {
		t.Errorf("session order = %+v, want acp-1 then tui-1", top.Result.Sessions)
	}
	native := top.Result.Sessions[1]
	if native.Cwd != "/work" || native.Title != "TUI chat" || native.UpdatedAt != "2026-11-11T10:00:00Z" {
		t.Errorf("native session = %+v, want the source's fields", native)
	}
	if top.Meta["trace"] != "kept" {
		t.Errorf("_meta = %+v, want the agent's other members preserved", top.Meta)
	}
	if top.Result.NextCursor != nil {
		t.Errorf("nextCursor = %v, want null preserved", top.Result.NextCursor)
	}
	for _, raw := range []string{`"big":9007199254740993`, `"nullable":null`} {
		if !strings.Contains(string(out), raw) {
			t.Errorf("merged reply lost the existing row's raw member %s: %s", raw, out)
		}
	}
}

func TestMergeSessionListReturnsNothingWhenEveryChatIsListed(t *testing.T) {
	reply := []byte(`{"jsonrpc":"2.0","id":7,"result":{"sessions":[{"sessionId":"acp-1"}]}}`)
	if out := mergeSessionList(reply, []Session{{ID: "acp-1"}}); out != nil {
		t.Errorf("reply = %s, want nil when no chat is new", out)
	}
	if out := mergeSessionList(reply, nil); out != nil {
		t.Errorf("reply = %s, want nil when the source lists nothing", out)
	}
}

func TestMergeSessionListLeavesAReplyWithoutAPageAlone(t *testing.T) {
	if out := mergeSessionList([]byte(`{"jsonrpc":"2.0","id":1,"result":null}`), []Session{{ID: "tui-1"}}); out != nil {
		t.Errorf("reply = %s, want nil for a reply without a result page", out)
	}
}

func TestRegistryExtensionsReturnsTheProfilesSet(t *testing.T) {
	set := hermesExtensions(&fakeSessionSource{})
	registry := New(Profile{ID: "hermes", Extensions: set})
	got := registry.Extensions("hermes")
	if len(got.Transformers) != 1 || got.Transformers[0].Name != "hermes-native-chats" {
		t.Fatalf("hermes extensions = %+v, want the native-chat transformer", got)
	}
	if plain := registry.Extensions("plain"); len(plain.Transformers) != 0 {
		t.Fatalf("plain extensions = %+v, want none", plain)
	}
}

func TestHermesListTransformAppendsNativeChats(t *testing.T) {
	source := &fakeSessionSource{sessions: []Session{{ID: "tui-1", Cwd: "/work", Title: "TUI chat"}}}
	reply := testPayload(t, `{"jsonrpc":"2.0","id":1,"result":{"sessions":[]}}`)
	params := testPayload(t, `{"cwd":"/work"}`)

	out, err := hermesListTransform(source)(context.Background(), agentext.TransformInput{
		Message: agentext.Message{Body: reply, RequestParams: params},
		Stage:   agentext.BeforeDelivery,
	})
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	if source.cwd != "/work" {
		t.Errorf("source queried with cwd %q, want /work", source.cwd)
	}
	var decoded struct {
		Result struct {
			Sessions []struct {
				SessionID string `json:"sessionId"`
			} `json:"sessions"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("decode transformed reply: %v", err)
	}
	if len(decoded.Result.Sessions) != 1 || decoded.Result.Sessions[0].SessionID != "tui-1" {
		t.Errorf("sessions = %+v, want tui-1", decoded.Result.Sessions)
	}
}

func TestHermesListTransformSkipsTheSourceOnANonFinalPage(t *testing.T) {
	source := &fakeSessionSource{sessions: []Session{{ID: "tui-1"}}}
	reply := testPayload(t, `{"jsonrpc":"2.0","id":1,"result":{"sessions":[],"nextCursor":"page-2"}}`)

	out, err := hermesListTransform(source)(context.Background(), agentext.TransformInput{
		Message: agentext.Message{Body: reply},
		Stage:   agentext.BeforeDelivery,
	})
	if err != nil {
		t.Fatalf("transform: %v", err)
	}
	if source.calls != 0 || !out.IsZero() {
		t.Fatalf("non-final page read the store %d times and changed the body (%v)", source.calls, !out.IsZero())
	}
}

func TestHermesListTransformPreservesTheReplyOnSourceFailure(t *testing.T) {
	source := &fakeSessionSource{err: errors.New("read Hermes store")}
	reply := testPayload(t, `{"jsonrpc":"2.0","id":1,"result":{"sessions":[]}}`)

	out, err := hermesListTransform(source)(context.Background(), agentext.TransformInput{
		Message: agentext.Message{Body: reply},
		Stage:   agentext.BeforeDelivery,
	})
	if err == nil {
		t.Fatal("transform hid the source error; the engine cannot log it")
	}
	if !out.IsZero() {
		t.Fatalf("failed read changed the body: %s", out.Bytes())
	}
}

// testPayload builds an owned payload for a harness transform test.
func testPayload(t *testing.T, raw string) agentext.Payload {
	t.Helper()
	payload, err := agentext.NewPayload(json.RawMessage(raw))
	if err != nil {
		t.Fatalf("NewPayload(%q): %v", raw, err)
	}
	return payload
}
