package test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"stream-gateway-lab/internal/endpoint"
	"stream-gateway-lab/internal/transport"
)

func TestUncancellableToolKeepsSlotAcrossReconnect(t *testing.T) {
	for _, lostRole := range []string{"brain", "executor"} {
		t.Run(lostRole, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			g := testGateway(t, "A", "B")
			server := httptest.NewServer(g)
			t.Cleanup(func() { cancel(); g.Close(); server.Close() })
			address := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"

			// Only this test tool ignores cancellation. Release it after the
			// busy check, with a finite upper bound even if the check fails.
			release := make(chan struct{})
			var releaseOnce sync.Once
			finish := func() { releaseOnce.Do(func() { close(release) }) }
			t.Cleanup(finish)
			started := make(chan context.Context, 1)
			returned := make(chan error, 1)
			bStarted := make(chan struct{}, 1)
			var aWaits, aEchoes atomic.Int32
			// Observe results on Brain A's current channel before matching IDs.
			aResults := make(chan transport.ToolMessage, 8)
			nextResult := func() transport.ToolMessage {
				t.Helper()
				select {
				case msg := <-aResults:
					return msg
				case <-ctx.Done():
					t.Fatal("missing WS result")
					return transport.ToolMessage{}
				}
			}
			endpoints := make(map[string]*endpoint.ExecutorEndpoint)
			brains := make(map[string]*endpoint.RemoteExecutor)
			for _, pair := range []string{"A", "B"} {
				c, err := endpoint.NewClient(ctx, address, pair, "executor", "test-executor-"+pair)
				if err != nil {
					t.Fatal(err)
				}
				e := &endpoint.ExecutorEndpoint{Client: c, Execute: func(toolCtx context.Context, name string, input json.RawMessage) (json.RawMessage, error) {
					if pair == "A" && name == "wait" {
						if aWaits.Add(1) != 1 {
							return nil, errors.New("old wait unexpectedly replayed")
						}
						started <- toolCtx
						timer := time.NewTimer(12 * time.Second)
						defer timer.Stop()
						select {
						case <-release:
						case <-timer.C:
						}
						returned <- toolCtx.Err()
						return json.RawMessage(`"A-old-result"`), nil
					}
					if pair == "A" && name == "echo" {
						aEchoes.Add(1)
					}
					if pair == "B" && name == "wait" {
						bStarted <- struct{}{}
					}
					return endpoint.ExecuteTool(toolCtx, name, input)
				}}
				endpoints[pair] = e
				go c.Run(e.Receive)
				brainClient, err := endpoint.NewClient(ctx, address, pair, "brain", "test-brain-"+pair)
				if err != nil {
					t.Fatal(err)
				}
				brain := &endpoint.RemoteExecutor{Client: brainClient}
				brains[pair] = brain
				go brainClient.Run(func(s *endpoint.Channel, body json.RawMessage) error {
					if pair == "A" {
						var msg transport.ToolMessage
						if err := json.Unmarshal(body, &msg); err != nil {
							return err
						}
						select {
						case aResults <- msg:
						case <-ctx.Done():
							return ctx.Err()
						}
					}
					return brain.Receive(s, body)
				})
				if err := brainClient.WaitReady(ctx); err != nil {
					t.Fatal(err)
				}
				if err := c.WaitReady(ctx); err != nil {
					t.Fatal(err)
				}
			}

			a, b := brains["A"], brains["B"]
			aEndpoint := endpoints["A"]
			oldA, oldExecutor := a.Client.Snapshot(), aEndpoint.Client.Snapshot()
			oldB, oldBExecutor := b.Client.Snapshot(), endpoints["B"].Client.Snapshot()
			oldDone, bDone := make(chan error, 1), make(chan error, 1)
			go func() {
				_, err := a.Call(ctx, "wait", json.RawMessage(`{"ms":12000}`))
				oldDone <- err
			}()
			go func() {
				output, err := b.Call(ctx, "wait", json.RawMessage(`{"ms":700}`))
				if err == nil && string(output) != `{"waited_ms":700}` {
					err = errors.New("wrong B output")
				}
				bDone <- err
			}()
			var toolCtx context.Context
			select {
			case toolCtx = <-started:
			case <-ctx.Done():
				t.Fatal("A tool did not start")
			}
			select {
			case <-bStarted:
			case <-ctx.Done():
				t.Fatal("B tool did not start")
			}
			g.Pairs["A"].Mu.Lock()
			conn := g.Pairs["A"].Peers[lostRole].Conn
			g.Pairs["A"].Mu.Unlock()
			lostAt := time.Now()
			conn.CloseNow()
			select {
			case err := <-oldDone:
				if err == nil || !strings.Contains(err.Error(), "channel lost") {
					t.Fatalf("old call must fail immediately, got %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("old call waited for the uncancellable tool")
			}
			t.Logf("old call failed after %s", time.Since(lostAt))
			select {
			case <-toolCtx.Done():
			case <-time.After(time.Second):
				t.Fatal("Executor did not request cancellation")
			}
			select {
			case <-returned:
				t.Fatal("old tool stopped on cancellation")
			default:
			}
			select {
			case err := <-bDone:
				if err != nil {
					t.Fatalf("B affected by A disconnect: %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("B did not finish while A was disconnected")
			}
			if err := a.Client.WaitReady(ctx); err != nil {
				t.Fatal(err)
			}
			if err := aEndpoint.Client.WaitReady(ctx); err != nil {
				t.Fatal(err)
			}
			freshA, freshExecutor := a.Client.Snapshot(), aEndpoint.Client.Snapshot()
			if freshA == nil || freshExecutor == nil || freshA.Epoch == oldA.Epoch || freshExecutor.Epoch != freshA.Epoch || oldA.Ctx.Err() == nil || oldExecutor.Ctx.Err() == nil {
				t.Fatal("reconnection must invalidate the old channel and create a new epoch")
			}
			if (lostRole == "brain" && freshExecutor.Conn != oldExecutor.Conn) || (lostRole == "executor" && freshA.Conn != oldA.Conn) {
				t.Fatal("healthy A socket was replaced")
			}
			select {
			case <-returned:
				t.Fatal("old tool exited before the recovered-channel busy check")
			default:
			}
			busyAt := time.Now()
			if _, err := a.Call(ctx, "echo", json.RawMessage(`"must-not-run"`)); err == nil || err.Error() != "tool failed: executor busy" {
				t.Fatalf("new call must reach the recovered Executor and return busy: %v", err)
			}
			if time.Since(busyAt) > time.Second || aEchoes.Load() != 0 {
				t.Fatal("busy call waited or started a second tool")
			}
			busyResult := nextResult() // A real WS response, not RemoteExecutor's local busy check.
			if busyResult.Error != "executor busy" || busyResult.Kind != "result" {
				t.Fatalf("unexpected busy result: %+v", busyResult)
			}
			t.Logf("recovered after %s; old tool still running; new call returned busy in %s", time.Since(lostAt), time.Since(busyAt))
			output, err := b.Call(ctx, "echo", json.RawMessage(`"B-while-A-busy"`))
			if err != nil || string(output) != `"B-while-A-busy"` {
				t.Fatalf("B affected by A's occupied slot: %s, %v", output, err)
			}

			finish()
			select {
			case err := <-returned:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("old tool must return its late output despite canceled context: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("test tool did not exit after release")
			}
			// Wait for actual execution-slot release, not just Call returning.
			free := make(chan struct{})
			go func() { aEndpoint.Slot.Lock(); aEndpoint.Slot.Unlock(); close(free) }()
			select {
			case <-free:
			case <-time.After(time.Second):
				t.Fatal("execution slot not released after tool exit")
			}
			output, err = a.Call(ctx, "echo", json.RawMessage(`"A-after-old-exit"`))
			if err != nil || string(output) != `"A-after-old-exit"` {
				t.Fatalf("slot did not recover or old output polluted new call: %s, %v", output, err)
			}
			newResult := nextResult()
			if newResult.ID == busyResult.ID || string(newResult.Output) != `"A-after-old-exit"` {
				t.Fatalf("unexpected fresh result: %+v", newResult)
			}
			select {
			case msg := <-aResults:
				t.Fatalf("unexpected late/duplicate result: %+v", msg)
			case <-time.After(300 * time.Millisecond):
			}
			if aWaits.Load() != 1 || aEchoes.Load() != 1 {
				t.Fatalf("old call replayed or busy call queued: waits=%d echoes=%d", aWaits.Load(), aEchoes.Load())
			}
			if b.Client.Snapshot() != oldB || endpoints["B"].Client.Snapshot() != oldBExecutor {
				t.Fatal("B's channels changed")
			}
			t.Log("old success discarded; busy request never executed; fresh request succeeded; B unchanged")
		})
	}
}
