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

	"github.com/coder/websocket"
)

func TestEpochAndLateEvents(t *testing.T) {
	for _, transition := range []string{"same-sockets", "reconnect-brain", "reconnect-executor"} {
		t.Run(transition, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			g := testGateway(t, "A", "B")
			server := httptest.NewServer(g)
			t.Cleanup(func() { cancel(); g.close(); server.Close() })
			address := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"
			oldStarted, newStarted := make(chan context.Context, 1), make(chan context.Context, 1)
			oldFinished := make(chan error, 1)
			bStarted, releaseNew := make(chan struct{}, 1), make(chan struct{})
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
					if pair == "A" {
						switch aRuns.Add(1) {
						case 1:
							oldStarted <- toolCtx
							output, err := executeTool(toolCtx, name, input)
							oldFinished <- err
							return output, err
						case 2:
							newStarted <- toolCtx
							// Keep the NEW call active until all stale events have
							// passed their checks. Context still bounds this wait.
							select {
							case <-releaseNew:
								return json.RawMessage(`"A-new-result"`), nil
							case <-toolCtx.Done():
								return nil, toolCtx.Err()
							}
						default:
							return nil, errors.New("unexpected replay or extra execution")
						}
					}
					if name == "wait" {
						bStarted <- struct{}{}
					}
					return executeTool(toolCtx, name, input)
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
						_ = json.Unmarshal(body, &msg) // receive already validated it.
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
			route := g.pairs["A"]
			route.mu.Lock()
			oldPeers := map[string]*peer{"brain": route.peers["brain"], "executor": route.peers["executor"]}
			route.mu.Unlock()
			oldDone, bDone := make(chan error, 1), make(chan error, 1)
			go func() { _, err := a.Call(ctx, "wait", json.RawMessage(`{"ms":10000}`)); oldDone <- err }()
			go func() {
				output, err := b.Call(ctx, "wait", json.RawMessage(`{"ms":700}`))
				if err == nil && string(output) != `{"waited_ms":700}` {
					err = errors.New("wrong B output")
				}
				bDone <- err
			}()
			var oldToolCtx context.Context
			select {
			case oldToolCtx = <-oldStarted:
			case <-ctx.Done():
				t.Fatal("old A call did not start")
			}
			select {
			case <-bStarted:
			case <-ctx.Done():
				t.Fatal("B call did not start")
			}
			a.mu.Lock()
			oldID := a.pending.id
			a.mu.Unlock()
			changedAt := time.Now()
			lostRole := strings.TrimPrefix(transition, "reconnect-")
			if transition == "same-sockets" {
				// Test-only rotation: no socket close and no empty-epoch notice.
				// Keep the production ordering: Executor state before Brain state.
				route.mu.Lock()
				route.epoch = newID()
				state, _ := json.Marshal(envelope{Type: "state", Epoch: route.epoch})
				var stateErr error
				for _, role := range []string{"executor", "brain"} {
					if stateErr = writeFrame(ctx, route.peers[role].conn, state); stateErr != nil {
						break
					}
				}
				route.mu.Unlock()
				if stateErr != nil {
					t.Fatal(stateErr)
				}
			} else {
				oldPeers[lostRole].conn.CloseNow()
			}
			select {
			case err := <-oldDone:
				if err == nil || !strings.Contains(err.Error(), "channel lost") {
					t.Fatalf("old call should fail on epoch change, got %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("old call did not fail promptly")
			}
			t.Logf("old call failed after %s", time.Since(changedAt))
			select {
			case err := <-oldFinished:
				if !errors.Is(err, context.Canceled) || oldToolCtx.Err() == nil {
					t.Fatalf("old execution did not observe cancellation: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("old execution was not canceled")
			}
			// The tool's return precedes slot release by a few instructions.
			free := make(chan struct{})
			go func() { endpoint.slot.Lock(); endpoint.slot.Unlock(); close(free) }()
			select {
			case <-free:
			case <-ctx.Done():
				t.Fatal("old execution slot not released")
			}
			if err := a.client.waitReady(ctx); err != nil {
				t.Fatal(err)
			}
			if err := endpoint.client.waitReady(ctx); err != nil {
				t.Fatal(err)
			}
			freshA, freshE := a.client.snapshot(), endpoint.client.snapshot()
			if freshA == nil || freshE == nil || freshA.epoch == oldA.epoch || freshE.epoch != freshA.epoch || oldA.ctx.Err() == nil || oldE.ctx.Err() == nil {
				t.Fatal("expected new channels with a shared new epoch and canceled old channels")
			}
			if transition == "same-sockets" && (freshA.conn != oldA.conn || freshE.conn != oldE.conn) {
				t.Fatal("epoch-only transition must preserve both sockets")
			}
			route.mu.Lock()
			freshPeers := map[string]*peer{"brain": route.peers["brain"], "executor": route.peers["executor"]}
			route.mu.Unlock()
			if transition != "same-sockets" && freshPeers[lostRole] == oldPeers[lostRole] {
				t.Fatal("reconnect did not install a new peer")
			}

			type answer struct {
				output json.RawMessage
				err    error
			}
			newDone := make(chan answer, 1)
			go func() {
				output, err := a.Call(ctx, "wait", json.RawMessage(`{"ms":10000}`))
				newDone <- answer{output, err}
			}()
			var newToolCtx context.Context
			select {
			case newToolCtx = <-newStarted:
			case <-ctx.Done():
				t.Fatal("new call did not start")
			}
			a.mu.Lock()
			newID := a.pending.id
			a.mu.Unlock()

			// A late cleanup callback must not remove a replacement peer, and
			// an old channel timeout must not close even a REUSED socket.
			if transition != "same-sockets" {
				route.remove(lostRole, oldPeers[lostRole])
			}
			a.client.disconnect(oldA, errors.New("late old Brain cleanup"))
			endpoint.client.disconnect(oldE, errors.New("late old Executor cleanup"))
			route.mu.Lock()
			intact := route.epoch == freshA.epoch && route.peers["brain"] == freshPeers["brain"] && route.peers["executor"] == freshPeers["executor"]
			route.mu.Unlock()
			if !intact || a.client.snapshot() != freshA || endpoint.client.snapshot() != freshE {
				t.Fatal("stale cleanup modified the current route/channel")
			}
			late := toolMessage{Kind: "result", ID: newID, Output: json.RawMessage(`"must-not-win"`)}
			if err := endpoint.client.send(ctx, oldE, late); err == nil {
				t.Fatal("old execution channel was allowed to send")
			}
			body, _ := json.Marshal(late)
			if err := a.receive(oldA, body); err != nil {
				t.Fatal(err)
			}

			// Raw writes deliberately bypass send's guard. Use the CURRENT
			// request ID so a missing epoch guard cannot hide behind ID checks.
			send := func(conn *websocket.Conn, epoch string, msg toolMessage) {
				t.Helper()
				body, _ := json.Marshal(msg)
				data, _ := json.Marshal(envelope{Type: "data", Epoch: epoch, Body: body})
				if err := writeFrame(ctx, conn, data); err != nil {
					t.Fatal(err)
				}
			}
			expectSeen := func(id string) {
				t.Helper()
				select {
				case msg := <-seen:
					if msg.ID != id {
						t.Fatalf("unexpected result passed epoch filtering: %+v", msg)
					}
				case <-ctx.Done():
					t.Fatal("result barrier did not arrive")
				}
			}
			barrier := toolMessage{Kind: "result", ID: "test-gateway-barrier", Output: json.RawMessage(`null`)}
			send(freshE.conn, oldE.epoch, late)
			send(freshE.conn, freshE.epoch, barrier)
			expectSeen(barrier.ID)
			// Also test a frame already past the gateway when the epoch changed.
			barrier.ID = "test-reader-barrier"
			send(freshPeers["brain"].conn, oldA.epoch, late)
			send(freshPeers["brain"].conn, freshA.epoch, barrier)
			expectSeen(barrier.ID)
			// Same epoch but an old request ID must reach receive and be ignored.
			late.ID = oldID
			send(freshE.conn, freshE.epoch, late)
			expectSeen(oldID)
			select {
			case result := <-newDone:
				t.Fatalf("stale event ended the new call: %s, %v", result.output, result.err)
			default:
			}
			if freshA.ctx.Err() != nil || freshE.ctx.Err() != nil || newToolCtx.Err() != nil {
				t.Fatal("stale event canceled the new channel/execution")
			}
			select {
			case err := <-bDone:
				if err != nil {
					t.Fatalf("B's in-flight call failed: %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("B's in-flight call stalled")
			}
			output, err := b.Call(ctx, "echo", json.RawMessage(`"B-unaffected"`))
			if err != nil || string(output) != `"B-unaffected"` {
				t.Fatalf("B's new call failed: %s, %v", output, err)
			}
			close(releaseNew)
			select {
			case result := <-newDone:
				if result.err != nil || string(result.output) != `"A-new-result"` {
					t.Fatalf("new call was overwritten or canceled: %s, %v", result.output, result.err)
				}
			case <-time.After(time.Second):
				t.Fatal("new call did not finish after release")
			}
			expectSeen(newID)
			if aRuns.Load() != 2 || a.client.snapshot() != freshA || endpoint.client.snapshot() != freshE || b.client.snapshot() != oldB || endpoints["B"].client.snapshot() != oldBE {
				t.Fatal("unexpected replay or channel replacement")
			}
			t.Log("old call failed and execution canceled; stale cleanup/results ignored while new call was active; new result correct; B unchanged")
		})
	}
}
