package harness

import (
	"context"
	"encoding/json"

	"github.com/arafatamim/ferngeist-acp-gateway/internal/agentext"
)

// hermesListSelector matches the agent's reply to a session/list request.
var hermesListSelector = agentext.Selector{
	Direction: agentext.ToClient,
	Kind:      agentext.Reply,
	Method:    "session/list",
}

// hermesExtensions registers Hermes's native-chat presentation. It is deferred
// and explicitly independent of other replies: appending chats may run after
// later frames, so the client correlates the reply by its request id.
func hermesExtensions(source SessionSource) agentext.Set {
	return agentext.Set{Transformers: []agentext.Transformer{{
		Name:             "hermes-native-chats",
		Selector:         hermesListSelector,
		Stage:            agentext.BeforeDelivery,
		Mode:             agentext.Deferred,
		Failure:          agentext.KeepOriginal,
		IndependentReply: true,
		Run:              hermesListTransform(source),
	}}}
}

// hermesListTransform appends the chats the Hermes adapter omits to a final
// session/list page. A non-final page is left untouched without reading the
// session store, and missing or unreadable state preserves the agent's reply.
func hermesListTransform(source SessionSource) agentext.TransformFunc {
	return func(ctx context.Context, in agentext.TransformInput) (agentext.Payload, error) {
		reply := in.Message.Body.Bytes()
		if !finalSessionListPage(reply) {
			return agentext.Payload{}, nil
		}
		sessions, err := source.Sessions(ctx, hermesListCwd(in.Message.RequestParams.Bytes()))
		if err != nil {
			return agentext.Payload{}, err // KeepOriginal delivers the agent's reply.
		}
		if len(sessions) == 0 {
			return agentext.Payload{}, nil
		}
		merged := mergeSessionList(reply, sessions)
		if merged == nil {
			return agentext.Payload{}, nil
		}
		return agentext.NewPayload(merged)
	}
}

// finalSessionListPage reports whether a session/list reply has no cursor. Only
// that page is extended, so a paged listing never repeats an added chat.
func finalSessionListPage(reply []byte) bool {
	var top struct {
		Result struct {
			NextCursor *string `json:"nextCursor"`
		} `json:"result"`
	}
	if json.Unmarshal(reply, &top) != nil {
		return false
	}
	return top.Result.NextCursor == nil
}

// hermesListCwd reads the folder filter the client asked for. No filter lists
// every folder.
func hermesListCwd(params []byte) string {
	var request struct {
		Cwd string `json:"cwd"`
	}
	_ = json.Unmarshal(params, &request) // A malformed filter means every folder.
	return request.Cwd
}
