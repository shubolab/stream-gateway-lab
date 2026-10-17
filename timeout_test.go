package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
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
			p := &pendingCall{channel: &channel{ctx: context.Background()}, result: make(chan toolMessage, 1)}
			if tc.ready {
				p.result <- toolMessage{Kind: "result", ID: "old", Output: json.RawMessage(`"success"`)}
			}
			// Isolate result arbitration; the WS test below checks disconnect,
			// execution cancellation, pending cleanup and recovery end to end.
			e := &RemoteExecutor{client: &client{}}
			output, err := e.waitResult(ctx, p)
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
	t.Cleanup(func() { cancel(); g.close(); server.Close() })
	address := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"
	oldStarted, newStarted := make(chan context.Context, 1), make(chan context.Context, 1)
	oldReturned, bStarted := make(chan struct{}, 1), make(chan struct{}, 1)
	releaseOld, releaseNew := make(chan struct{}), make(chan struct{})
	seen := make(chan toolMessage, 8)
	var aRuns atomic.Int32
	endpoints := make(map[string]*executorEndpoint)
	brains := make(map[string]*RemoteExecutor)
	for _, pair := range []string{"A", "B"} {
		c, err := newClient(ctx, address, pair, "executor", "test-executor-"+pair)
		if err != nil {
			t.Fatal(err)
		}
		e := &executorEndpoint{client: c, execute: func(toolCtx context.Context, name string, input json.RawMessage) (json.RawMessage, error) {
			if pair == "B" {
				if name == "wait" {
					bStarted <- struct{}{}
				}
				return executeTool(toolCtx, name, input)
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
		go c.run(e.receive)
		bc, err := newClient(ctx, address, pair, "brain", "test-brain-"+pair)
		if err != nil {
			t.Fatal(err)
		}
		brain := &RemoteExecutor{client: bc}
		brains[pair] = brain
		go bc.run(func(s *channel, body json.RawMessage) error {
			if err := brain.receive(s, body); err != nil {
				return err
			}
			if pair == "A" {
				var msg toolMessage
				_ = json.Unmarshal(body, &msg)
				select {
				case seen <- msg:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return nil
		})
		if err := bc.waitReady(ctx); err != nil {
			t.Fatal(err)
		}
		if err := c.waitReady(ctx); err != nil {
			t.Fatal(err)
		}
	}
	a, b, endpoint := brains["A"], brains["B"], endpoints["A"]
	oldA, oldE := a.client.snapshot(), endpoint.client.snapshot()
	oldB, oldBE := b.client.snapshot(), endpoints["B"].client.snapshot()
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
	a.mu.Lock()
	oldPending := a.pending
	a.mu.Unlock()
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
	for _, canceled := range []context.Context{oldA.ctx, oldE.ctx, toolCtx} {
		select {
		case <-canceled.Done():
		case <-time.After(time.Second):
			t.Fatal("timeout did not invalidate the channel and request tool cancellation")
		}
	}
	late := toolMessage{Kind: "result", ID: oldPending.id, Output: json.RawMessage(`"A-late-success"`)}
	body, _ := json.Marshal(late)
	// A reader callback already in flight must not re-deliver after Call returns.
	if err := a.receive(oldA, body); err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	pendingCleared := a.pending == nil
	a.mu.Unlock()
	if !pendingCleared || len(oldPending.result) != 0 {
		t.Fatal("completed call retained a waiter or accepted a late result")
	}
	close(releaseOld)
	select {
	case <-oldReturned:
	case <-ctx.Done():
		t.Fatal("old tool did not return its late success")
	}
	free := make(chan struct{})
	go func() { endpoint.slot.Lock(); endpoint.slot.Unlock(); close(free) }()
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
	if err := a.client.waitReady(ctx); err != nil {
		t.Fatal(err)
	}
	if err := endpoint.client.waitReady(ctx); err != nil {
		t.Fatal(err)
	}
	freshA, freshE := a.client.snapshot(), endpoint.client.snapshot()
	if freshA == nil || freshE == nil || freshA.epoch == oldA.epoch || freshA.epoch != freshE.epoch || freshE.conn != oldE.conn {
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
		if err := endpoint.client.send(ctx, freshE, late); err != nil {
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
	if aRuns.Load() != 2 || a.client.snapshot() != freshA || endpoint.client.snapshot() != freshE || b.client.snapshot() != oldB || endpoints["B"].client.snapshot() != oldBE {
		t.Fatal("unexpected replay or channel replacement")
	}
	t.Log("one timeout return; pending cleared; cancellation observed; old success and duplicate results discarded; fresh call succeeded; B unchanged")
}
