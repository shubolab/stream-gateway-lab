package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestDisconnectFailsOldCallAndRecovers(t *testing.T) {
	for _, role := range []string{"brain", "executor"} {
		t.Run(role, func(t *testing.T) { testConnectionLoss(t, role, false) })
	}
}

func TestSilentLossFailsOldCallAndRecovers(t *testing.T) {
	for _, role := range []string{"brain", "executor"} {
		t.Run(role, func(t *testing.T) { testConnectionLoss(t, role, true) })
	}
}

func testConnectionLoss(t *testing.T, disconnectedRole string, silent bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	g := testGateway(t, "A", "B")
	server := httptest.NewServer(g)
	t.Cleanup(func() { cancel(); g.close(); server.Close() })
	address := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"
	var relay *blackholeRelay
	bWaitMS, lossBudget := 700, time.Second
	if silent {
		relay = newBlackholeRelay(t, server.Listener.Addr().String())
		bWaitMS = 4500 // B remains busy across multiple heartbeats and A's loss.
		lossBudget = heartbeatInterval + heartbeatTimeout + time.Second
	}
	started := make(chan string, 4)
	canceled := make(chan error, 1)
	var aWaits, aEchoes atomic.Int32
	executors := make(map[string]*client)
	brains := make(map[string]*RemoteExecutor)
	for _, pair := range []string{"A", "B"} {
		executorURL, brainURL := address, address
		if silent && pair == "A" {
			if disconnectedRole == "executor" {
				executorURL = "ws://" + relay.address + "/ws"
			} else {
				brainURL = "ws://" + relay.address + "/ws"
			}
		}
		c, err := newClient(ctx, executorURL, pair, "executor", "test-executor-"+pair)
		if err != nil {
			t.Fatal(err)
		}
		executors[pair] = c
		e := &executorEndpoint{client: c, execute: func(toolCtx context.Context, name string, input json.RawMessage) (json.RawMessage, error) {
			if name == "wait" {
				if pair == "A" {
					aWaits.Add(1)
				}
				started <- pair
			} else if pair == "A" && name == "echo" {
				aEchoes.Add(1)
			}
			output, err := executeTool(toolCtx, name, input)
			if pair == "A" && name == "wait" && errors.Is(err, context.Canceled) {
				canceled <- err
			}
			return output, err
		}}
		go c.run(e.receive)
		brains[pair], err = newRemoteExecutor(ctx, brainURL, pair, "test-brain-"+pair)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.waitReady(ctx); err != nil {
			t.Fatal(err)
		}
	}
	a, b := brains["A"], brains["B"]
	oldA, oldExecutor := a.client.snapshot(), executors["A"].snapshot()
	oldB := b.client.snapshot()
	oldBExecutor := executors["B"].snapshot()
	oldDone, bDone := make(chan error, 1), make(chan error, 1)
	go func() {
		_, err := a.Call(ctx, "wait", json.RawMessage(`{"ms":10000}`))
		oldDone <- err
	}()
	go func() {
		input, _ := json.Marshal(map[string]int{"ms": bWaitMS})
		want, _ := json.Marshal(map[string]int{"waited_ms": bWaitMS})
		output, err := b.Call(ctx, "wait", input)
		if err == nil && string(output) != string(want) {
			err = errors.New("wrong B output")
		}
		bDone <- err
	}()
	for range 2 {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("both tools must start before the disconnect")
		}
	}
	lostAt := time.Now()
	if silent {
		// No CloseNow, EOF, TCP FIN or WS close frame is injected here.
		relay.drop.Store(true)
	} else {
		// Close only one real WS, on the gateway side. Keep endpoints alive.
		route := g.pairs["A"]
		route.mu.Lock()
		conn := route.peers[disconnectedRole].conn
		route.mu.Unlock()
		conn.CloseNow()
	}
	select {
	case err := <-oldDone:
		if err == nil || !strings.Contains(err.Error(), "channel lost") {
			t.Fatalf("old call must fail on loss, got %v", err)
		}
	case <-time.After(lossBudget):
		t.Fatal("old call did not fail within the loss detection budget")
	}
	t.Logf("silent=%t role=%s: old call failed after %s", silent, disconnectedRole, time.Since(lostAt))
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("executor did not observe cancellation")
	}
	if silent {
		lostChannel := oldA
		if disconnectedRole == "executor" {
			lostChannel = oldExecutor
		}
		if !errors.Is(context.Cause(lostChannel.ctx), errHeartbeat) {
			t.Fatalf("endpoint must detect loss via heartbeat, got %v", context.Cause(lostChannel.ctx))
		}
		for direction := range 2 {
			if relay.dropped[direction].Load() == 0 {
				t.Fatalf("no traffic blackholed in direction %d", direction)
			}
		}
		// Recovery only changes forwarding; discarded bytes are never replayed.
		relay.drop.Store(false)
	}
	before := time.Now()
	if _, err := a.Call(ctx, "echo", json.RawMessage(`"must-not-be-queued"`)); err == nil {
		t.Fatal("offline call should fail")
	}
	if time.Since(before) > time.Second {
		t.Fatal("offline call waited instead of failing immediately")
	}
	select {
	case err := <-bDone:
		if err != nil {
			t.Fatalf("B affected by A disconnect: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("B did not complete while A was offline")
	}
	if _, err := b.Call(ctx, "echo", json.RawMessage(`"B-during-A-offline"`)); err != nil {
		t.Fatalf("B cannot start a new call while A is offline: %v", err)
	}
	if err := a.client.waitReady(ctx); err != nil {
		t.Fatal(err)
	}
	// The gateway writes Executor's state first, but the two read loops
	// are scheduled independently. Observe both before inspecting them.
	if err := executors["A"].waitReady(ctx); err != nil {
		t.Fatal(err)
	}
	freshA, freshExecutor := a.client.snapshot(), executors["A"].snapshot()
	if freshA == nil || freshExecutor == nil || freshA.epoch == oldA.epoch || oldA.ctx.Err() == nil || oldExecutor.ctx.Err() == nil {
		t.Fatal("recovery must create a new epoch and leave old channels canceled")
	}
	if b.client.snapshot() != oldB || executors["B"].snapshot() != oldBExecutor {
		t.Fatal("B's channel was replaced")
	}
	if disconnectedRole == "brain" && freshExecutor.conn != oldExecutor.conn {
		t.Fatal("healthy Executor connection should remain connected")
	}
	if disconnectedRole == "executor" && freshA.conn != oldA.conn {
		t.Fatal("healthy Brain connection should remain connected")
	}
	if err := executors["A"].send(ctx, oldExecutor, toolMessage{Kind: "result", ID: "old", Output: json.RawMessage(`"late"`)}); err == nil {
		t.Fatal("old channel must not send through the recovered connection")
	}
	output, err := a.Call(ctx, "echo", json.RawMessage(`"A-new"`))
	if err != nil || string(output) != `"A-new"` {
		t.Fatalf("explicit new call failed: output=%s err=%v", output, err)
	}
	if aWaits.Load() != 1 || aEchoes.Load() != 1 {
		t.Fatalf("calls replayed or queued: waits=%d echoes=%d", aWaits.Load(), aEchoes.Load())
	}
	t.Log("old call failed; Executor canceled; offline call rejected; B unchanged; new epoch accepts only the new call")
}

func TestReconnectBudgetIsFinite(t *testing.T) {
	var connects atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		connects.Add(1)
		state, _ := json.Marshal(envelope{Type: "state", Epoch: newID()})
		_ = writeFrame(r.Context(), conn, state)
		// Every dial succeeds, then the connection ends. Success must not reset
		// the lifetime redial budget and cause an infinite reconnect loop.
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 22*time.Second)
	defer cancel()
	c, err := newClient(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), "A", "brain", "unused")
	if err != nil {
		t.Fatal(err)
	}
	c.run(func(*channel, json.RawMessage) error { return nil })
	if ctx.Err() != nil || connects.Load() != 1+maxRedials || !strings.Contains(context.Cause(c.ctx).Error(), "reconnect exhausted") {
		t.Fatalf("expected initial + 3 attempts then stop: connects=%d cause=%v", connects.Load(), context.Cause(c.ctx))
	}
	if c.snapshot() != nil || c.waitReady(ctx) == nil {
		t.Fatal("exhausted endpoint must stay unavailable")
	}
	t.Logf("successful connections=%d, stopped with %v", connects.Load(), context.Cause(c.ctx))
}
