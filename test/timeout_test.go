package test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"stream-gateway-lab/internal/endpoint"
	"stream-gateway-lab/internal/transport"
)

func TestTimeoutResultArbitration(t *testing.T) {
	for _, tc := range []struct {
		name           string
		expired, ready bool
	}{
		{"result-first", false, true},
		{"timeout-first", true, false},
		{"both-ready", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deadline := time.Now().Add(5 * time.Second)
			if tc.expired {
				deadline = time.Now().Add(-time.Second)
			}
			ctx, cancel := context.WithDeadline(context.Background(), deadline)
			defer cancel()
			if tc.expired {
				<-ctx.Done()
			}
			p := &endpoint.PendingCall{Channel: &endpoint.Channel{Ctx: context.Background()}, Result: make(chan transport.ToolMessage, 1)}
			if tc.ready {
				p.Result <- transport.ToolMessage{Kind: "result", ID: "old", Output: json.RawMessage(`"success"`)}
			}
			// Isolate result arbitration; the WS test below checks disconnect,
			// execution cancellation, pending cleanup and recovery end to end.
			e := &endpoint.RemoteExecutor{Client: &endpoint.Client{}}
			output, err := e.WaitResult(ctx, p)
			if tc.expired {
				if !errors.Is(err, context.DeadlineExceeded) || output != nil {
					t.Fatalf("timeout must win over an already queued result: %s, %v", output, err)
				}
			} else if err != nil || string(output) != `"success"` {
				t.Fatalf("result before deadline should succeed: %s, %v", output, err)
			}
		})
	}
}

func TestTimeoutDiscardsLateResultAndRecovers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	g := testGateway(t, "A", "B")
	server := httptest.NewServer(g)
	t.Cleanup(func() { cancel(); g.Close(); server.Close() })
	address := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"
	oldStarted, newStarted := make(chan context.Context, 1), make(chan context.Context, 1)
	oldReturned, bStarted := make(chan struct{}, 1), make(chan struct{}, 1)
	releaseOld, releaseNew := make(chan struct{}), make(chan struct{})
	seen := make(chan transport.ToolMessage, 8)
	var aRuns atomic.Int32
	endpoints := make(map[string]*endpoint.ExecutorEndpoint)
	brains := make(map[string]*endpoint.RemoteExecutor)
	for _, pair := range []string{"A", "B"} {
		c, err := endpoint.NewClient(ctx, address, pair, "executor", "test-executor-"+pair)
		if err != nil {
			t.Fatal(err)
		}
		e := &endpoint.ExecutorEndpoint{Client: c, Execute: func(toolCtx context.Context, name string, input json.RawMessage) (json.RawMessage, error) {
			if pair == "B" {
				if name == "wait" {
					bStarted <- struct{}{}
				}
				return endpoint.ExecuteTool(toolCtx, name, input)
			}
			switch aRuns.Add(1) {
			case 1:
				oldStarted <- toolCtx
				<-toolCtx.Done()
				// Simulate success racing with cancellation during tool cleanup.
				// Only the test delays return; the test lifetime bounds this wait.
				select {
				case <-releaseOld:
				case <-ctx.Done():
				}
				oldReturned <- struct{}{}
				return json.RawMessage(`"A-late-success"`), nil
			case 2:
				newStarted <- toolCtx
				select {
				case <-releaseNew:
					return json.RawMessage(`"A-new-result"`), nil
				case <-toolCtx.Done():
					return nil, toolCtx.Err()
				}
			default:
				return nil, errors.New("unexpected replay or extra execution")
			}
		}}
		endpoints[pair] = e
		go c.Run(e.Receive)
		bc, err := endpoint.NewClient(ctx, address, pair, "brain", "test-brain-"+pair)
		if err != nil {
			t.Fatal(err)
		}
		brain := &endpoint.RemoteExecutor{Client: bc}
		brains[pair] = brain
		go bc.Run(func(s *endpoint.Channel, body json.RawMessage) error {
			if err := brain.Receive(s, body); err != nil {
				return err
			}
			if pair == "A" {
				var msg transport.ToolMessage
				_ = json.Unmarshal(body, &msg)
				select {
				case seen <- msg:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return nil
		})
		if err := bc.WaitReady(ctx); err != nil {
			t.Fatal(err)
		}
		if err := c.WaitReady(ctx); err != nil {
			t.Fatal(err)
		}
	}
	a, b, executorPeer := brains["A"], brains["B"], endpoints["A"]
	oldA, oldE := a.Client.Snapshot(), executorPeer.Client.Snapshot()
	oldB, oldBE := b.Client.Snapshot(), endpoints["B"].Client.Snapshot()
	type answer struct {
		output json.RawMessage
		err    error
	}
	oldDone, newDone, bDone := make(chan answer, 1), make(chan answer, 1), make(chan answer, 1)
	go func() {
		output, err := b.Call(ctx, "wait", json.RawMessage(`{"ms":1000}`))
		bDone <- answer{output, err}
	}()
	select {
	case <-bStarted:
	case <-ctx.Done():
		t.Fatal("B did not start")
	}
	callCtx, callCancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer callCancel()
	calledAt := time.Now()
	go func() {
		output, err := a.Call(callCtx, "wait", json.RawMessage(`{"ms":10000}`))
		oldDone <- answer{output, err}
	}()
	var toolCtx context.Context
	select {
	case toolCtx = <-oldStarted:
	case <-ctx.Done():
		t.Fatal("A did not start")
	}
	a.Mu.Lock()
	oldPending := a.Pending
	a.Mu.Unlock()
	if oldPending == nil {
		t.Fatal("A timed out before the test could capture its pending call")
	}
	select {
	case result := <-oldDone:
		if !errors.Is(result.err, context.DeadlineExceeded) || result.output != nil {
			t.Fatalf("old call must return timeout without output: %s, %v", result.output, result.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("old call did not fail promptly at its deadline")
	}
	t.Logf("500 ms call returned timeout after %s", time.Since(calledAt))
	for _, canceled := range []context.Context{oldA.Ctx, oldE.Ctx, toolCtx} {
		select {
		case <-canceled.Done():
		case <-time.After(time.Second):
			t.Fatal("timeout did not invalidate the channel and request tool cancellation")
		}
	}
	late := transport.ToolMessage{Kind: "result", ID: oldPending.ID, Output: json.RawMessage(`"A-late-success"`)}
	body, _ := json.Marshal(late)
	// A reader callback already in flight must not re-deliver after Call returns.
	if err := a.Receive(oldA, body); err != nil {
		t.Fatal(err)
	}
	a.Mu.Lock()
	pendingCleared := a.Pending == nil
	a.Mu.Unlock()
	if !pendingCleared || len(oldPending.Result) != 0 {
		t.Fatal("completed call retained a waiter or accepted a late result")
	}
	close(releaseOld)
	select {
	case <-oldReturned:
	case <-ctx.Done():
		t.Fatal("old tool did not return its late success")
	}
	free := make(chan struct{})
	go func() { executorPeer.Slot.Lock(); executorPeer.Slot.Unlock(); close(free) }()
	select {
	case <-free:
	case <-ctx.Done():
		t.Fatal("old execution slot not released")
	}
	select {
	case result := <-bDone:
		if result.err != nil || string(result.output) != `{"waited_ms":1000}` {
			t.Fatalf("A timeout affected B's in-flight call: %s, %v", result.output, result.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("B's in-flight call stalled")
	}
	output, err := b.Call(ctx, "echo", json.RawMessage(`"B-unaffected"`))
	if err != nil || string(output) != `"B-unaffected"` {
		t.Fatalf("A timeout affected B's new call: %s, %v", output, err)
	}
	if err := a.Client.WaitReady(ctx); err != nil {
		t.Fatal(err)
	}
	if err := executorPeer.Client.WaitReady(ctx); err != nil {
		t.Fatal(err)
	}
	freshA, freshE := a.Client.Snapshot(), executorPeer.Client.Snapshot()
	if freshA == nil || freshE == nil || freshA.Epoch == oldA.Epoch || freshA.Epoch != freshE.Epoch || freshE.Conn != oldE.Conn {
		t.Fatal("timeout recovery must replace the epoch and retain the healthy Executor socket")
	}
	go func() {
		output, err := a.Call(ctx, "echo", json.RawMessage(`"A-new"`))
		newDone <- answer{output, err}
	}()
	select {
	case <-newStarted:
	case <-ctx.Done():
		t.Fatal("new call did not start")
	}
	// Deliver the timed-out ID twice through the current WS/epoch, while a
	// new call is active. Observe AFTER receive to prove both were processed.
	for i := 0; i < 2; i++ {
		if err := executorPeer.Client.Send(ctx, freshE, late); err != nil {
			t.Fatal(err)
		}
		select {
		case msg := <-seen:
			if msg.ID != late.ID || string(msg.Output) != `"A-late-success"` {
				t.Fatalf("unexpected result before stale-frame barrier: %+v", msg)
			}
		case <-ctx.Done():
			t.Fatal("late result did not reach the matching check")
		}
	}
	select {
	case result := <-newDone:
		t.Fatalf("late old result ended the new call: %s, %v", result.output, result.err)
	default:
	}
	close(releaseNew)
	select {
	case result := <-newDone:
		if result.err != nil || string(result.output) != `"A-new-result"` {
			t.Fatalf("new result was overwritten: %s, %v", result.output, result.err)
		}
	case <-ctx.Done():
		t.Fatal("new call did not finish")
	}
	if aRuns.Load() != 2 || a.Client.Snapshot() != freshA || executorPeer.Client.Snapshot() != freshE || b.Client.Snapshot() != oldB || endpoints["B"].Client.Snapshot() != oldBE {
		t.Fatal("unexpected replay or channel replacement")
	}
	t.Log("one timeout return; pending cleared; cancellation observed; old success and duplicate results discarded; fresh call succeeded; B unchanged")
}
