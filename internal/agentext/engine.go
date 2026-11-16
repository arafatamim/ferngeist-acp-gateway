package agentext

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"
)

const (
	// deferredWorkers is how many callbacks one engine may execute at once.
	deferredWorkers = 2
	// deferredQueue bounds jobs waiting for a worker.
	deferredQueue = 32
	// engineRetainedBytes bounds one engine's queued inputs, executing inputs
	// and their outputs.
	engineRetainedBytes = 1 << 20
	// operationBytes bounds one operation's input plus its outputs.
	operationBytes = 256 << 10
	// deferredTimeout runs from admission, queue wait included.
	deferredTimeout = 2 * time.Second
)

// Budget bounds bytes retained across every engine and observer sharing it. A
// nil Budget imposes no shared limit; the per-engine limits still apply.
type Budget struct {
	mu    sync.Mutex
	limit int
	used  int
}

// NewBudget returns a shared budget of limitBytes. A limit of zero or less
// refuses every non-empty reservation.
func NewBudget(limitBytes int) *Budget {
	return &Budget{limit: limitBytes}
}

// reserve charges n bytes, and reports false without charging when the shared
// budget cannot hold them.
func (b *Budget) reserve(n int) bool {
	if b == nil {
		return true
	}
	if n < 0 {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.used+n > b.limit {
		return false
	}
	b.used += n
	return true
}

// release returns n bytes to the shared budget.
func (b *Budget) release(n int) {
	if b == nil || n <= 0 {
		return
	}
	b.mu.Lock()
	b.used -= n
	if b.used < 0 {
		b.used = 0
	}
	b.mu.Unlock()
}

// Engine executes one session's registered transformers. It is bound to one
// identity and shares no state with another session's engine.
type Engine struct {
	ctx      context.Context
	identity Identity
	registry *Registry
	budget   *Budget
	logger   *slog.Logger

	mu       sync.Mutex
	retained int
	closed   bool
	queue    chan transformJob
	workers  sync.WaitGroup
}

// Bind returns an engine for one session. A nil logger discards diagnostics.
func (r *Registry) Bind(ctx context.Context, identity Identity, budget *Budget, logger *slog.Logger) *Engine {
	if ctx == nil {
		ctx = context.Background()
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Engine{ctx: ctx, identity: identity, registry: r, budget: budget, logger: logger}
}

// Enabled reports whether any transformer matches selector and stage, so a
// caller can skip copying a frame no transformer would see.
func (e *Engine) Enabled(selector Selector, stage Stage) bool {
	chain, _ := e.resolve(TransformInput{Message: Message{Selector: selector}, Stage: stage})
	return len(chain) > 0
}

// TransformInline runs the matching inline chain on the calling goroutine. It
// returns the effective body: the input when nothing matched, the chain's
// output when it succeeded, and the pre-chain body when the policy keeps the
// original. A Reject chain reports ErrRejected; mode mismatches and a closed
// engine report their own error without running a callback.
func (e *Engine) TransformInline(ctx context.Context, in TransformInput) (Payload, error) {
	chain, mode := e.resolve(in)
	if len(chain) == 0 {
		return in.Message.Body, nil
	}
	if mode != Inline {
		return in.Message.Body, ErrExecutionMode
	}
	if e.isClosed() {
		return in.Message.Body, ErrClosed
	}
	in.Message.Identity = e.identity
	res := &reservation{engine: e}
	if err := res.add(len(in.Message.Body.body) + len(in.Message.RequestParams.body)); err != nil {
		return e.failed(chain, in.Stage, in.Message.Body, err)
	}
	defer res.release()
	return e.executeChain(ctx, chain, in, res)
}

// SubmitTransform admits the matching deferred chain and returns once it is
// queued. The completion runs exactly once: on a worker after the callback, or
// immediately for a chain that matches nothing. ErrSaturated and ErrClosed mean
// the operation was not admitted and done will not run.
func (e *Engine) SubmitTransform(ctx context.Context, in TransformInput, done func(Payload, error)) error {
	if done == nil {
		return fmt.Errorf("%w: nil completion", ErrInvalidRegistration)
	}
	chain, mode := e.resolve(in)
	if len(chain) == 0 {
		done(in.Message.Body, nil)
		return nil
	}
	if mode != Deferred {
		return ErrExecutionMode
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	in.Message.Identity = e.identity
	res := &reservation{engine: e}
	if err := res.add(len(in.Message.Body.body) + len(in.Message.RequestParams.body)); err != nil {
		return err
	}
	jobCtx, cancel := context.WithTimeout(ctx, deferredTimeout)
	job := transformJob{ctx: jobCtx, cancel: cancel, input: in, chain: chain, reserve: res, done: done}
	if err := e.enqueue(job); err != nil {
		cancel()
		res.release()
		return err
	}
	return nil
}

// Retain charges n bytes of correlated data, such as a request's params, until
// the returned release runs. It refuses before the caller copies anything when
// the operation, engine or shared budget cannot hold them.
func (e *Engine) Retain(n int) (func(), bool) {
	if n <= 0 {
		return func() {}, true
	}
	res := &reservation{engine: e}
	if err := res.add(n); err != nil {
		return nil, false
	}
	var once sync.Once
	return func() { once.Do(res.release) }, true
}

// Mode returns the execution mode of the chain matching selector and stage, and
// false when nothing matches.
func (e *Engine) Mode(selector Selector, stage Stage) (Mode, bool) {
	chain, mode := e.resolve(TransformInput{Message: Message{Selector: selector}, Stage: stage})
	if len(chain) == 0 {
		return Inline, false
	}
	return mode, true
}

// Failure returns the failure policy of the chain matching selector and stage,
// and false when nothing matches.
func (e *Engine) Failure(selector Selector, stage Stage) (Failure, bool) {
	chain, _ := e.resolve(TransformInput{Message: Message{Selector: selector}, Stage: stage})
	if len(chain) == 0 {
		return KeepOriginal, false
	}
	return chain[0].Failure, true
}

// Close stops admission, drains queued jobs without running their callbacks,
// and waits for executing callbacks. It reports ctx's error when a callback
// does not return in time; it cannot terminate that callback.
func (e *Engine) Close(ctx context.Context) error {
	if e == nil {
		return nil
	}
	e.mu.Lock()
	e.closed = true
	queue := e.queue
	e.queue = nil
	e.mu.Unlock()
	if queue != nil {
		close(queue)
	}
	done := make(chan struct{})
	go func() {
		e.workers.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// transformJob is one admitted deferred operation.
type transformJob struct {
	ctx     context.Context
	cancel  context.CancelFunc
	input   TransformInput
	chain   []Transformer
	reserve *reservation
	done    func(Payload, error)
}

// enqueue admits job under the engine's admission lock. The send never blocks:
// a full queue is saturation, not backpressure.
func (e *Engine) enqueue(job transformJob) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return ErrClosed
	}
	if e.queue == nil {
		e.queue = make(chan transformJob, deferredQueue)
		for range deferredWorkers {
			e.workers.Add(1)
			go e.worker(e.queue)
		}
	}
	select {
	case e.queue <- job:
		return nil
	default:
		return ErrSaturated
	}
}

// worker executes admitted jobs until Close closes the queue.
func (e *Engine) worker(queue chan transformJob) {
	defer e.workers.Done()
	for job := range queue {
		e.runJob(job)
	}
}

// runJob completes one admitted job exactly once. A job whose deadline elapsed
// while queued, or whose engine is closing, applies the chain's failure policy
// without running the callback.
func (e *Engine) runJob(job transformJob) {
	defer job.cancel()
	out, err := e.jobResult(job)
	// Release before completing: the completion delivers the reply, and the
	// client's next request must find this job's bytes already returned.
	job.reserve.release()
	job.done(out, err)
}

// jobResult runs an admitted job's chain, or applies its failure policy when
// the job expired or the engine closed first.
func (e *Engine) jobResult(job transformJob) (Payload, error) {
	if err := e.stopReason(job.ctx); err != nil {
		return e.failed(job.chain, job.input.Stage, job.input.Message.Body, err)
	}
	out, err := e.executeChain(job.ctx, job.chain, job.input, job.reserve)
	if err == nil {
		if ctxErr := job.ctx.Err(); ctxErr != nil {
			// The deadline or the caller won the race: discard the late result.
			return e.failed(job.chain, job.input.Stage, job.input.Message.Body, ctxErr)
		}
	}
	return out, err
}

// stopReason reports why an admitted job must not run its callback: the job
// expired while queued, or Close stopped admission.
func (e *Engine) stopReason(jobCtx context.Context) error {
	if err := jobCtx.Err(); err != nil {
		return err
	}
	if e.isClosed() {
		return ErrClosed
	}
	return nil
}

// executeChain runs chain in order over body. Any failure applies the chain's
// policy against the body from before the whole chain.
func (e *Engine) executeChain(ctx context.Context, chain []Transformer, in TransformInput, res *reservation) (Payload, error) {
	original, body := in.Message.Body, in.Message.Body
	for _, transformer := range chain {
		out, err := e.call(ctx, transformer, in, body)
		if err == nil && !out.IsZero() && !sameBody(out, body) {
			err = res.add(len(out.body))
		}
		if err == nil {
			if !out.IsZero() {
				body = out
			}
			continue
		}
		return e.failed(chain, in.Stage, original, err)
	}
	return body, nil
}

// call runs one callback with panic recovery at this boundary only, so a
// panicking extension never takes down the stdout loop.
func (e *Engine) call(ctx context.Context, t Transformer, in TransformInput, body Payload) (out Payload, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			// The panic value is not logged: it may hold payload bytes.
			e.logger.Error("extension transformer panicked",
				"extension", t.Name, "stage", t.Stage,
				"agent_id", e.identity.AgentID, "session_id", e.identity.SessionID,
				"stack", string(debug.Stack()))
			out, err = Payload{}, fmt.Errorf("%w: %s", ErrCallbackPanic, t.Name)
		}
	}()
	in.Message.Body = body
	return t.Run(ctx, in)
}

// failed applies the chain's failure policy to err and returns the pre-chain
// body. It logs the extension's identity and stage, never the body.
func (e *Engine) failed(chain []Transformer, stage Stage, original Payload, err error) (Payload, error) {
	level := slog.LevelWarn
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrClosed) {
		level = slog.LevelDebug
	}
	e.logger.Log(e.ctx, level, "extension transformer failed",
		"extension", chain[0].Name, "stage", stage,
		"agent_id", e.identity.AgentID, "session_id", e.identity.SessionID,
		"error", err)
	if chain[0].Failure == Reject {
		return original, fmt.Errorf("%w: %w", ErrRejected, err)
	}
	return original, nil
}

// resolve finds the chain for a message shape and stage.
func (e *Engine) resolve(in TransformInput) ([]Transformer, Mode) {
	if e == nil || e.registry == nil {
		return nil, Inline
	}
	chain := e.registry.chains[chainKey{selector: in.Message.Selector, stage: in.Stage}]
	if len(chain) == 0 {
		return nil, Inline
	}
	return chain, chain[0].Mode
}

// isClosed reports whether Close stopped admission.
func (e *Engine) isClosed() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.closed
}

// reservation tracks bytes charged to one operation, split between the engine
// and the shared budget, so they are released exactly once.
type reservation struct {
	engine *Engine
	bytes  int
}

// add charges n more bytes for this operation. It refuses before the caller
// retains anything when the operation, engine or shared budget would overflow.
func (r *reservation) add(n int) error {
	if n <= 0 {
		return nil
	}
	if r.bytes+n > operationBytes {
		return fmt.Errorf("%w: operation over %d bytes", ErrSaturated, operationBytes)
	}
	e := r.engine
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return ErrClosed
	}
	if e.retained+n > engineRetainedBytes {
		e.mu.Unlock()
		return fmt.Errorf("%w: engine retention over %d bytes", ErrSaturated, engineRetainedBytes)
	}
	e.retained += n
	e.mu.Unlock()
	if !e.budget.reserve(n) {
		e.release(n)
		return fmt.Errorf("%w: shared budget exhausted", ErrSaturated)
	}
	r.bytes += n
	return nil
}

// release returns every byte this operation retained.
func (r *reservation) release() {
	if r.engine == nil || r.bytes == 0 {
		return
	}
	r.engine.release(r.bytes)
	r.bytes = 0
}

// release returns n bytes to the engine and the shared budget.
func (e *Engine) release(n int) {
	e.mu.Lock()
	e.retained -= n
	if e.retained < 0 {
		e.retained = 0
	}
	e.mu.Unlock()
	e.budget.release(n)
}

// sameBody reports whether two payloads share the same owned bytes, so an
// unchanged result is not charged twice.
func sameBody(a, b Payload) bool {
	if len(a.body) != len(b.body) {
		return false
	}
	if len(a.body) == 0 {
		return true
	}
	return &a.body[0] == &b.body[0]
}
