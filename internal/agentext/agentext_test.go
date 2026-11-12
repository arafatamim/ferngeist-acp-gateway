package agentext

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testPayload(t *testing.T, raw string) Payload {
	t.Helper()
	p, err := NewPayload(json.RawMessage(raw))
	if err != nil {
		t.Fatalf("NewPayload(%q): %v", raw, err)
	}
	return p
}

func testRegistry(t *testing.T, set Set) *Registry {
	t.Helper()
	reg, err := New(set)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return reg
}

func testEngine(t *testing.T, set Set, budget *Budget) *Engine {
	t.Helper()
	return testEngineWithLogger(t, set, budget, nil)
}

func testEngineWithLogger(t *testing.T, set Set, budget *Budget, logger *slog.Logger) *Engine {
	t.Helper()
	engine := testRegistry(t, set).Bind(context.Background(),
		Identity{AgentID: "hermes", RuntimeID: "rt", SessionID: "sess"}, budget, logger)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := engine.Close(ctx); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return engine
}

// inlineInspect is one BeforeState transformer for an independent request.
func inlineInspect(name string, failure Failure, run TransformFunc) Transformer {
	return Transformer{
		Name:     name,
		Selector: Selector{Direction: ToAgent, Kind: Request, Method: "example/inspect"},
		Stage:    BeforeState, Mode: Inline, Failure: failure, Run: run,
	}
}

// deferredList is one deferred BeforeDelivery transformer for a list reply.
func deferredList(name string, failure Failure, run TransformFunc) Transformer {
	return Transformer{
		Name:     name,
		Selector: Selector{Direction: ToClient, Kind: Reply, Method: "session/list"},
		Stage:    BeforeDelivery, Mode: Deferred, Failure: failure,
		IndependentReply: true, Run: run,
	}
}

func inspectInput(p Payload) TransformInput {
	return TransformInput{
		Message: Message{Selector: Selector{Direction: ToAgent, Kind: Request, Method: "example/inspect"}, Body: p},
		Stage:   BeforeState,
	}
}

func listInput(p Payload) TransformInput {
	return TransformInput{
		Message: Message{Selector: Selector{Direction: ToClient, Kind: Reply, Method: "session/list"}, Body: p},
		Stage:   BeforeDelivery,
	}
}

func TestPayloadDoesNotShareCallerBuffers(t *testing.T) {
	const original = `{"count":9007199254740993,"nested":{"keep":null}}`
	raw := json.RawMessage(original)
	p, err := NewPayload(raw)
	if err != nil {
		t.Fatalf("NewPayload: %v", err)
	}
	raw[2] = 'X'
	if got := string(p.Bytes()); got != original {
		t.Fatalf("payload changed with the source buffer: %s", got)
	}
	got := p.Bytes()
	got[2] = 'Y'
	if again := string(p.Bytes()); again != original {
		t.Fatalf("payload changed through a Bytes result: %s", again)
	}
}

func TestNewPayloadRejectsInvalidJSON(t *testing.T) {
	for _, raw := range []string{"", "  ", "{", `1 2`, `"unterminated`} {
		if _, err := NewPayload(json.RawMessage(raw)); !errors.Is(err, ErrInvalidPayload) {
			t.Errorf("NewPayload(%q) error = %v, want ErrInvalidPayload", raw, err)
		}
	}
}

func TestTransformerChainRollsBackOnFailure(t *testing.T) {
	for _, failure := range []Failure{KeepOriginal, Reject} {
		t.Run(strings.TrimSpace(failureName(failure)), func(t *testing.T) {
			var order []string
			var count atomic.Int64
			set := Set{Transformers: []Transformer{
				inlineInspect("first", failure, func(context.Context, TransformInput) (Payload, error) {
					order = append(order, "first")
					return testPayload(t, `{"stage":"first"}`), nil
				}),
				inlineInspect("second", failure, func(context.Context, TransformInput) (Payload, error) {
					order = append(order, "second")
					count.Add(1)
					return Payload{}, errors.New("refused")
				}),
			}}
			engine := testEngine(t, set, nil)
			original := testPayload(t, `{"stage":"original"}`)
			out, err := engine.TransformInline(context.Background(), inspectInput(original))
			if !bytes.Equal(out.Bytes(), original.Bytes()) {
				t.Fatalf("rollback body = %s, want %s", out.Bytes(), original.Bytes())
			}
			if strings.Join(order, ",") != "first,second" || count.Load() != 1 {
				t.Fatalf("chain ran %v (second count %d), want first then second once", order, count.Load())
			}
			if failure == Reject && !errors.Is(err, ErrRejected) {
				t.Fatalf("Reject chain error = %v, want ErrRejected", err)
			}
			if failure == KeepOriginal && err != nil {
				t.Fatalf("KeepOriginal chain error = %v, want nil", err)
			}
		})
	}
}

func failureName(f Failure) string {
	if f == Reject {
		return "Reject"
	}
	return "KeepOriginal"
}

func TestTransformUsesBoundIdentityAndOrder(t *testing.T) {
	var seen Identity
	set := Set{Transformers: []Transformer{
		inlineInspect("first", KeepOriginal, func(_ context.Context, in TransformInput) (Payload, error) {
			seen = in.Message.Identity
			raw := in.Message.Body.Bytes()
			raw[0] = '['
			return testPayload(t, `{"first":true}`), nil
		}),
		inlineInspect("second", KeepOriginal, func(_ context.Context, in TransformInput) (Payload, error) {
			if !bytes.Equal(in.Message.Body.Bytes(), []byte(`{"first":true}`)) {
				t.Errorf("second body = %s, want first's output", in.Message.Body.Bytes())
			}
			return Payload{}, nil
		}),
	}}
	engine := testEngine(t, set, nil)
	in := inspectInput(testPayload(t, `{"original":true}`))
	in.Message.Identity = Identity{AgentID: "spoofed"}
	out, err := engine.TransformInline(context.Background(), in)
	if err != nil {
		t.Fatalf("TransformInline: %v", err)
	}
	if want := (Identity{AgentID: "hermes", RuntimeID: "rt", SessionID: "sess"}); seen != want {
		t.Fatalf("identity = %+v, want %+v", seen, want)
	}
	if !bytes.Equal(out.Bytes(), []byte(`{"first":true}`)) {
		t.Fatalf("output = %s, want the first transformer's body", out.Bytes())
	}
}

func TestDeferredModeCannotRunInline(t *testing.T) {
	var called atomic.Int64
	engine := testEngine(t, Set{Transformers: []Transformer{
		deferredList("list", KeepOriginal, func(context.Context, TransformInput) (Payload, error) {
			called.Add(1)
			return Payload{}, nil
		}),
	}}, nil)
	body := testPayload(t, `{}`)
	out, err := engine.TransformInline(context.Background(), listInput(body))
	if !errors.Is(err, ErrExecutionMode) {
		t.Fatalf("error = %v, want ErrExecutionMode", err)
	}
	if called.Load() != 0 {
		t.Fatalf("callback ran %d times on an inline call", called.Load())
	}
	if !bytes.Equal(out.Bytes(), body.Bytes()) {
		t.Fatalf("body = %s, want input", out.Bytes())
	}
}

func TestInlineModeCannotBeSubmitted(t *testing.T) {
	called := false
	engine := testEngine(t, Set{Transformers: []Transformer{
		inlineInspect("inspect", KeepOriginal, func(context.Context, TransformInput) (Payload, error) {
			called = true
			return Payload{}, nil
		}),
	}}, nil)
	err := engine.SubmitTransform(context.Background(), inspectInput(testPayload(t, `{}`)), func(Payload, error) {
		t.Error("completion ran for an inline chain")
	})
	if !errors.Is(err, ErrExecutionMode) || called {
		t.Fatalf("SubmitTransform = %v, called %v; want ErrExecutionMode and no callback", err, called)
	}
}

func TestTransformPanicUsesFailurePolicyAndHidesPayload(t *testing.T) {
	for _, failure := range []Failure{KeepOriginal, Reject} {
		t.Run(failureName(failure), func(t *testing.T) {
			var logs bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&logs, nil))
			engine := testEngineWithLogger(t, Set{Transformers: []Transformer{
				inlineInspect("boom", failure, func(context.Context, TransformInput) (Payload, error) {
					panic("secret payload text")
				}),
			}}, nil, logger)
			original := testPayload(t, `{"secret":"payload-body"}`)
			out, err := engine.TransformInline(context.Background(), inspectInput(original))
			if !bytes.Equal(out.Bytes(), original.Bytes()) {
				t.Fatalf("rollback body = %s, want original", out.Bytes())
			}
			if failure == Reject {
				if !errors.Is(err, ErrRejected) || !errors.Is(err, ErrCallbackPanic) {
					t.Fatalf("error = %v, want ErrRejected and ErrCallbackPanic", err)
				}
			} else if err != nil {
				t.Fatalf("KeepOriginal error = %v, want nil", err)
			}
			if strings.Contains(logs.String(), "secret") {
				t.Fatalf("log leaked callback data: %s", logs.String())
			}
		})
	}
}

func TestNewRejectsInvalidRegistrations(t *testing.T) {
	valid := inlineInspect("valid", KeepOriginal, func(context.Context, TransformInput) (Payload, error) {
		return Payload{}, nil
	})
	with := func(mutate func(*Transformer)) Set {
		transformer := valid
		mutate(&transformer)
		return Set{Transformers: []Transformer{transformer}}
	}
	mixedMode := deferredList("deferred", KeepOriginal, func(context.Context, TransformInput) (Payload, error) {
		return Payload{}, nil
	})
	mixedMode.Selector = valid.Selector
	mixedMode.Stage = valid.Stage

	cases := map[string]Set{
		"empty name":    with(func(t *Transformer) { t.Name = "" }),
		"nil callback":  with(func(t *Transformer) { t.Run = nil }),
		"empty method":  with(func(t *Transformer) { t.Selector.Method = "" }),
		"bad direction": with(func(t *Transformer) { t.Selector.Direction = 9 }),
		"bad kind":      with(func(t *Transformer) { t.Selector.Kind = 9 }),
		"bad stage":     with(func(t *Transformer) { t.Stage = 9 }),
		"bad mode":      with(func(t *Transformer) { t.Mode = 9 }),
		"bad failure":   with(func(t *Transformer) { t.Failure = 9 }),
		"before-state deferred": with(func(t *Transformer) {
			t.Stage, t.Mode, t.IndependentReply = BeforeState, Deferred, true
		}),
		"deferred without independent reply": with(func(t *Transformer) {
			t.Stage, t.Mode, t.IndependentReply = BeforeDelivery, Deferred, false
		}),
		"duplicate name": {Transformers: []Transformer{valid, valid}},
		"mixed modes":    {Transformers: []Transformer{valid, mixedMode}},
		"mixed failure":  {Transformers: []Transformer{valid, inlineInspect("reject", Reject, valid.Run)}},
	}
	for name, set := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := New(set); !errors.Is(err, ErrInvalidRegistration) {
				t.Fatalf("New error = %v, want ErrInvalidRegistration", err)
			}
		})
	}
}

func TestUnknownExtensionMethodTransforms(t *testing.T) {
	engine := testEngine(t, Set{Transformers: []Transformer{{
		Name:     "vendor",
		Selector: Selector{Direction: ToAgent, Kind: Notification, Method: "x/vendor/thing"},
		Stage:    BeforeState, Mode: Inline, Failure: KeepOriginal,
		Run: func(_ context.Context, in TransformInput) (Payload, error) {
			return testPayload(t, `{"rewritten":true}`), nil
		},
	}}}, nil)
	in := TransformInput{
		Message: Message{Selector: Selector{Direction: ToAgent, Kind: Notification, Method: "x/vendor/thing"}, Body: testPayload(t, `{}`)},
		Stage:   BeforeState,
	}
	out, err := engine.TransformInline(context.Background(), in)
	if err != nil || string(out.Bytes()) != `{"rewritten":true}` {
		t.Fatalf("unknown method = %s, %v; want rewritten", out.Bytes(), err)
	}
}

func TestDeferredAdmissionIsBounded(t *testing.T) {
	entered := make(chan struct{}, deferredWorkers)
	release := make(chan struct{})
	var completed atomic.Int64
	engine := testEngine(t, Set{Transformers: []Transformer{
		deferredList("list", KeepOriginal, func(context.Context, TransformInput) (Payload, error) {
			select {
			case entered <- struct{}{}:
			default:
			}
			<-release
			return Payload{}, nil
		}),
	}}, NewBudget(16<<20))
	done := func(Payload, error) { completed.Add(1) }
	body := testPayload(t, `{}`)
	for range deferredWorkers {
		if err := engine.SubmitTransform(context.Background(), listInput(body), done); err != nil {
			t.Fatalf("submit worker job: %v", err)
		}
	}
	for range deferredWorkers {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("workers never started")
		}
	}
	for i := 0; i < deferredQueue; i++ {
		if err := engine.SubmitTransform(context.Background(), listInput(body), done); err != nil {
			t.Fatalf("submit queued job %d: %v", i, err)
		}
	}
	if err := engine.SubmitTransform(context.Background(), listInput(body), done); !errors.Is(err, ErrSaturated) {
		t.Fatalf("submit past the queue = %v, want ErrSaturated", err)
	}
	close(release)
	waitFor(t, func() bool { return completed.Load() == deferredWorkers+deferredQueue })
}

func TestSharedBudgetSpansEngines(t *testing.T) {
	body := testPayload(t, `{"v":"`+strings.Repeat("a", 30)+`"}`)
	budget := NewBudget(2 * len(body.body))
	entered := make(chan struct{}, deferredWorkers)
	release := make(chan struct{})
	var firstCalls, secondCalls atomic.Int64
	newSet := func(calls *atomic.Int64) Set {
		return Set{Transformers: []Transformer{
			deferredList("list", KeepOriginal, func(context.Context, TransformInput) (Payload, error) {
				calls.Add(1)
				entered <- struct{}{}
				<-release
				return Payload{}, nil
			}),
		}}
	}
	first := testEngine(t, newSet(&firstCalls), budget)
	for range deferredWorkers {
		if err := first.SubmitTransform(context.Background(), listInput(body), func(Payload, error) {}); err != nil {
			t.Fatalf("first engine submit: %v", err)
		}
	}
	for range deferredWorkers {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("first engine workers never started")
		}
	}
	second := testEngine(t, newSet(&secondCalls), budget)
	err := second.SubmitTransform(context.Background(), listInput(body), func(Payload, error) {
		t.Error("second engine completion ran despite a full shared budget")
	})
	if !errors.Is(err, ErrSaturated) {
		t.Fatalf("second engine submit = %v, want ErrSaturated from the shared budget", err)
	}
	if firstCalls.Load() != deferredWorkers || secondCalls.Load() != 0 {
		t.Fatalf("callbacks = %d/%d, want the second engine skipped", firstCalls.Load(), secondCalls.Load())
	}
	close(release)
}

func TestEngineRetentionIsBounded(t *testing.T) {
	entered := make(chan struct{}, deferredWorkers)
	release := make(chan struct{})
	engine := testEngine(t, Set{Transformers: []Transformer{
		deferredList("list", KeepOriginal, func(context.Context, TransformInput) (Payload, error) {
			select {
			case entered <- struct{}{}:
			default:
			}
			<-release
			return Payload{}, nil
		}),
	}}, NewBudget(1<<30))
	big := testPayload(t, `"`+strings.Repeat("a", 200<<10)+`"`)
	for range deferredWorkers {
		if err := engine.SubmitTransform(context.Background(), listInput(big), func(Payload, error) {}); err != nil {
			t.Fatalf("submit worker job: %v", err)
		}
	}
	for range deferredWorkers {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("workers never started")
		}
	}
	// 2 executing + 3 queued fit the 1 MiB engine cap; the sixth does not.
	for i := 0; i < 3; i++ {
		if err := engine.SubmitTransform(context.Background(), listInput(big), func(Payload, error) {}); err != nil {
			t.Fatalf("submit job %d: %v", i, err)
		}
	}
	if err := engine.SubmitTransform(context.Background(), listInput(big), func(Payload, error) {}); !errors.Is(err, ErrSaturated) {
		t.Fatalf("submit past the engine cap = %v, want ErrSaturated", err)
	}
	close(release)
}

func TestOperationSizeIsBounded(t *testing.T) {
	huge := testPayload(t, `"`+strings.Repeat("a", operationBytes)+`"`)
	for _, failure := range []Failure{KeepOriginal, Reject} {
		t.Run(failureName(failure), func(t *testing.T) {
			called := false
			engine := testEngine(t, Set{Transformers: []Transformer{
				inlineInspect("inspect", failure, func(context.Context, TransformInput) (Payload, error) {
					called = true
					return Payload{}, nil
				}),
			}}, nil)
			out, err := engine.TransformInline(context.Background(), inspectInput(huge))
			if called {
				t.Fatal("callback ran on an oversized operation")
			}
			if !bytes.Equal(out.Bytes(), huge.Bytes()) {
				t.Fatal("oversized operation did not keep the original body")
			}
			if failure == Reject && !errors.Is(err, ErrRejected) {
				t.Fatalf("error = %v, want ErrRejected", err)
			}
			if failure == KeepOriginal && err != nil {
				t.Fatalf("error = %v, want nil", err)
			}
		})
	}
}

func TestDeferredDeadlineCompletesOnce(t *testing.T) {
	engine := testEngine(t, Set{Transformers: []Transformer{
		deferredList("list", KeepOriginal, func(ctx context.Context, in TransformInput) (Payload, error) {
			<-ctx.Done()
			return testPayload(t, `{"late":true}`), nil
		}),
	}}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	type outcome struct {
		body string
		err  error
	}
	results := make(chan outcome, 1)
	if err := engine.SubmitTransform(ctx, listInput(testPayload(t, `{"original":true}`)), func(p Payload, err error) {
		results <- outcome{body: string(p.Bytes()), err: err}
	}); err != nil {
		t.Fatalf("SubmitTransform: %v", err)
	}
	select {
	case got := <-results:
		if got.err != nil {
			t.Fatalf("completion error = %v, want KeepOriginal's nil", got.err)
		}
		if got.body != `{"original":true}` {
			t.Fatalf("late result = %s, want the original body", got.body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("completion never ran")
	}
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer closeCancel()
	if err := engine.Close(closeCtx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-results:
		t.Fatal("completion ran twice")
	default:
	}
}

func TestCloseStopsAdmissionAndReportsUncooperativeCallback(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	engine := testEngine(t, Set{Transformers: []Transformer{
		deferredList("list", KeepOriginal, func(context.Context, TransformInput) (Payload, error) {
			close(entered)
			<-release
			return Payload{}, nil
		}),
	}}, nil)
	if err := engine.SubmitTransform(context.Background(), listInput(testPayload(t, `{}`)), func(Payload, error) {}); err != nil {
		t.Fatalf("SubmitTransform: %v", err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("worker never started")
	}
	shortCtx, shortCancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer shortCancel()
	if err := engine.Close(shortCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close = %v, want context.DeadlineExceeded", err)
	}
	if err := engine.SubmitTransform(context.Background(), listInput(testPayload(t, `{}`)), func(Payload, error) {}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Submit after Close = %v, want ErrClosed", err)
	}
	close(release)
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer waitCancel()
	if err := engine.Close(waitCtx); err != nil {
		t.Fatalf("second Close = %v", err)
	}
}

// waitFor polls cond until it holds or the test times out.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition never held")
		}
		time.Sleep(time.Millisecond)
	}
}
