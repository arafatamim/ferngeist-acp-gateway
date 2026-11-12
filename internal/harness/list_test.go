package harness

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

type fakeSessionSource struct {
	sessions []Session
	err      error
	cwd      string
}

func (f *fakeSessionSource) Sessions(_ context.Context, cwd string) ([]Session, error) {
	f.cwd = cwd
	return f.sessions, f.err
}

func TestMergeSessionListAppendsUnseenNativeChats(t *testing.T) {
	reply := []byte(`{"jsonrpc":"2.0","id":7,"result":{"sessions":[{"sessionId":"acp-1","cwd":"/work"}],"nextCursor":null},"_meta":{"trace":"kept"}}`)
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

func TestRegistryDecorateListMergesTheProfilesChats(t *testing.T) {
	source := &fakeSessionSource{sessions: []Session{{ID: "tui-1", Cwd: "/work"}}}
	reply := []byte(`{"jsonrpc":"2.0","id":1,"result":{"sessions":[]}}`)

	out, err := New(Profile{ID: "hermes", Sessions: source}).DecorateList(context.Background(), "hermes", "/work", reply)
	if err != nil {
		t.Fatalf("DecorateList: %v", err)
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
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatalf("decode decorated reply: %v", err)
	}
	if len(decoded.Result.Sessions) != 1 || decoded.Result.Sessions[0].SessionID != "tui-1" {
		t.Errorf("sessions = %+v, want tui-1", decoded.Result.Sessions)
	}
}

func TestRegistryDecorateListReturnsTheSourceError(t *testing.T) {
	failure := errors.New("read Hermes store")
	source := &fakeSessionSource{err: failure}
	registry := New(Profile{ID: "hermes", Sessions: source})

	if _, err := registry.DecorateList(context.Background(), "hermes", "", []byte(`{"result":{"sessions":[]}}`)); !errors.Is(err, failure) {
		t.Errorf("error = %v, want the source's", err)
	}
	if out, err := registry.DecorateList(context.Background(), "plain", "", []byte(`{"result":{"sessions":[]}}`)); out != nil || err != nil {
		t.Errorf("agent without a source = (%s, %v), want (nil, nil)", out, err)
	}
}
