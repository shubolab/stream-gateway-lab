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

	"github.com/coder/websocket"

	"stream-gateway-lab/internal/endpoint"
	"stream-gateway-lab/internal/gateway"
	"stream-gateway-lab/internal/transport"
)

func TestEpochAndLateEvents(t *testing.T) {
	for _, transition := range []string{"same-sockets", "reconnect-brain", "reconnect-executor"} {
		t.Run(transition, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			g := testGateway(t, "A", "B")
			server := httptest.NewServer(g)
			t.Cleanup(func() { cancel(); g.Close(); server.Close() })
			address := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"
			oldStarted, newStarted := make(chan context.Context, 1), make(chan context.Context, 1)
			oldFinished := make(chan error, 1)
			bStarted, releaseNew := make(chan struct{}, 1), make(chan struct{})
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
					if pair == "A" {
						switch aRuns.Add(1) {
						case 1:
							oldStarted <- toolCtx
							output, err := endpoint.ExecuteTool(toolCtx, name, input)
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
					return endpoint.ExecuteTool(toolCtx, name, input)
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
						_ = json.Unmarshal(body, &msg) // receive already validated it.
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
			route := g.Pairs["A"]
			route.Mu.Lock()
			oldPeers := map[string]*gateway.Peer{"brain": route.Peers["brain"], "executor": route.Peers["executor"]}
			route.Mu.Unlock()
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
			a.Mu.Lock()
			oldID := a.Pending.ID
			a.Mu.Unlock()
			changedAt := time.Now()
			lostRole := strings.TrimPrefix(transition, "reconnect-")
			if transition == "same-sockets" {
				// Test-only rotation: no socket close and no empty-epoch notice.
				// Keep the production ordering: Executor state before Brain state.
				route.Mu.Lock()
				route.Epoch = transport.NewID()
				state, _ := json.Marshal(transport.Envelope{Type: transport.EnvelopeTypeState, Epoch: route.Epoch})
				var stateErr error
				for _, role := range []string{"executor", "brain"} {
					if stateErr = transport.WriteFrame(ctx, route.Peers[role].Conn, state); stateErr != nil {
						break
					}
				}
				route.Mu.Unlock()
				if stateErr != nil {
					t.Fatal(stateErr)
				}
			} else {
				oldPeers[lostRole].Conn.CloseNow()
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
			go func() { executorPeer.Slot.Lock(); executorPeer.Slot.Unlock(); close(free) }()
			select {
			case <-free:
			case <-ctx.Done():
				t.Fatal("old execution slot not released")
			}
			if err := a.Client.WaitReady(ctx); err != nil {
				t.Fatal(err)
			}
			if err := executorPeer.Client.WaitReady(ctx); err != nil {
				t.Fatal(err)
			}
			freshA, freshE := a.Client.Snapshot(), executorPeer.Client.Snapshot()
			if freshA == nil || freshE == nil || freshA.Epoch == oldA.Epoch || freshE.Epoch != freshA.Epoch || oldA.Ctx.Err() == nil || oldE.Ctx.Err() == nil {
				t.Fatal("expected new channels with a shared new epoch and canceled old channels")
			}
			if transition == "same-sockets" && (freshA.Conn != oldA.Conn || freshE.Conn != oldE.Conn) {
				t.Fatal("epoch-only transition must preserve both sockets")
			}
			route.Mu.Lock()
			freshPeers := map[string]*gateway.Peer{"brain": route.Peers["brain"], "executor": route.Peers["executor"]}
			route.Mu.Unlock()
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
			a.Mu.Lock()
			newID := a.Pending.ID
			a.Mu.Unlock()

			// A late cleanup callback must not remove a replacement peer, and
			// an old channel timeout must not close even a REUSED socket.
			if transition != "same-sockets" {
				route.Remove(lostRole, oldPeers[lostRole])
			}
			a.Client.Disconnect(oldA, errors.New("late old Brain cleanup"))
			executorPeer.Client.Disconnect(oldE, errors.New("late old Executor cleanup"))
			route.Mu.Lock()
			intact := route.Epoch == freshA.Epoch && route.Peers["brain"] == freshPeers["brain"] && route.Peers["executor"] == freshPeers["executor"]
			route.Mu.Unlock()
			if !intact || a.Client.Snapshot() != freshA || executorPeer.Client.Snapshot() != freshE {
				t.Fatal("stale cleanup modified the current route/channel")
			}
			late := transport.ToolMessage{Kind: "result", ID: newID, Output: json.RawMessage(`"must-not-win"`)}
			if err := executorPeer.Client.Send(ctx, oldE, late); err == nil {
				t.Fatal("old execution channel was allowed to send")
			}
			body, _ := json.Marshal(late)
			if err := a.Receive(oldA, body); err != nil {
				t.Fatal(err)
			}

			// Raw writes deliberately bypass send's guard. Use the CURRENT
			// request ID so a missing epoch guard cannot hide behind ID checks.
			send := func(conn *websocket.Conn, epoch string, msg transport.ToolMessage) {
				t.Helper()
				body, _ := json.Marshal(msg)
				data, _ := json.Marshal(transport.Envelope{Type: transport.EnvelopeTypeData, Epoch: epoch, Body: body})
				if err := transport.WriteFrame(ctx, conn, data); err != nil {
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
			barrier := transport.ToolMessage{Kind: "result", ID: "test-gateway-barrier", Output: json.RawMessage(`null`)}
			send(freshE.Conn, oldE.Epoch, late)
			send(freshE.Conn, freshE.Epoch, barrier)
			expectSeen(barrier.ID)
			// Also test a frame already past the gateway when the epoch changed.
			barrier.ID = "test-reader-barrier"
			send(freshPeers["brain"].Conn, oldA.Epoch, late)
			send(freshPeers["brain"].Conn, freshA.Epoch, barrier)
			expectSeen(barrier.ID)
			// Same epoch but an old request ID must reach receive and be ignored.
			late.ID = oldID
			send(freshE.Conn, freshE.Epoch, late)
			expectSeen(oldID)
			select {
			case result := <-newDone:
				t.Fatalf("stale event ended the new call: %s, %v", result.output, result.err)
			default:
			}
			if freshA.Ctx.Err() != nil || freshE.Ctx.Err() != nil || newToolCtx.Err() != nil {
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
			if aRuns.Load() != 2 || a.Client.Snapshot() != freshA || executorPeer.Client.Snapshot() != freshE || b.Client.Snapshot() != oldB || endpoints["B"].Client.Snapshot() != oldBE {
				t.Fatal("unexpected replay or channel replacement")
			}
			t.Log("old call failed and execution canceled; stale cleanup/results ignored while new call was active; new result correct; B unchanged")
		})
	}
}
