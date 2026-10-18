package test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"stream-gateway-lab/internal/app"
	"stream-gateway-lab/internal/endpoint"
	"stream-gateway-lab/internal/gateway"
	"stream-gateway-lab/internal/transport"
)

func testGateway(t *testing.T, pairs ...string) *gateway.Gateway {
	t.Helper()
	var bindings []gateway.Binding
	for _, pair := range pairs {
		b := gateway.Binding{Pair: pair, BrainTokenEnv: "TEST_BRAIN_" + pair, ExecutorTokenEnv: "TEST_EXECUTOR_" + pair}
		t.Setenv(b.BrainTokenEnv, "test-brain-"+pair)
		t.Setenv(b.ExecutorTokenEnv, "test-executor-"+pair)
		bindings = append(bindings, b)
	}
	g, err := gateway.New(bindings)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestBrainMatchesAcrossExecutors(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	g := testGateway(t, "A")
	server := httptest.NewServer(g)
	t.Cleanup(func() { cancel(); g.Close(); server.Close() })
	address := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"
	executorDone := make(chan error, 1)
	go func() { executorDone <- endpoint.RunExecutor(ctx, address, "A", "test-executor-A") }()
	remote, err := endpoint.NewRemoteExecutor(ctx, address, "A", "test-brain-A")
	if err != nil {
		t.Fatal(err)
	}
	local := &endpoint.LocalExecutor{}
	var previous []byte
	for _, item := range []struct {
		name string
		exec endpoint.Executor
	}{{"local", local}, {"remote", remote}} {
		results, err := app.RunBrain(ctx, item.exec, "hello from brain", 200)
		if err != nil {
			t.Fatalf("%s brain: %v", item.name, err)
		}
		encoded, _ := json.Marshal(results)
		if string(encoded) != `[{"tool":"echo","output":{"message":"hello from brain","wait_ms":200}},{"tool":"wait","output":{"waited_ms":200}}]` {
			t.Fatalf("%s unexpected results: %s", item.name, encoded)
		}
		if previous != nil && !bytes.Equal(previous, encoded) {
			t.Fatalf("local and remote differ: %s / %s", previous, encoded)
		}
		previous = encoded
		if _, err := item.exec.Call(ctx, "missing", json.RawMessage(`{}`)); err == nil || !strings.Contains(err.Error(), "unknown tool") {
			t.Fatalf("%s should report tool failure, got %v", item.name, err)
		}
	}
	remote.Client.Stop(context.Canceled)
	cancel() // Executor remains available after its peer leaves; stop it explicitly.
	select {
	case err := <-executorDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("executor did not stop after explicit cancellation")
	}
}

func TestGatewayRejectsMismatchedCredentials(t *testing.T) {
	g := testGateway(t, "A", "B")
	server := httptest.NewServer(g)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for _, query := range []string{"pair=A&role=executor", "pair=B&role=brain", "pair=unknown&role=brain"} {
		address := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws?" + query
		conn, response, err := websocket.Dial(ctx, address, &websocket.DialOptions{
			HTTPHeader: http.Header{"Authorization": {"Bearer test-brain-A"}},
		})
		if conn != nil {
			conn.CloseNow()
		}
		if err == nil || response == nil || response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("A brain credential must not authenticate %s: response=%v err=%v", query, response, err)
		}
	}
}

func TestPairsRouteConcurrently(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	g := testGateway(t, "A", "B")
	server := httptest.NewServer(g)
	t.Cleanup(func() { cancel(); g.Close(); server.Close() })
	address := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"
	type arrival struct {
		pair    string
		call    transport.ToolMessage
		release chan struct{}
	}
	arrivals := make(chan arrival, 2)
	brains := make(map[string]*endpoint.RemoteExecutor)
	for _, pair := range []string{"A", "B"} {
		c, err := endpoint.NewClient(ctx, address, pair, "executor", "test-executor-"+pair)
		if err != nil {
			t.Fatal(err)
		}
		// Test endpoint reports its own identity, so even a symmetric routing
		// mix-up cannot pass just because an echo eventually returns home.
		go c.Run(func(s *endpoint.Channel, body json.RawMessage) error {
			var call transport.ToolMessage
			if err := json.Unmarshal(body, &call); err != nil {
				return err
			}
			a := arrival{pair: pair, call: call, release: make(chan struct{})}
			select {
			case arrivals <- a:
			case <-ctx.Done():
				return ctx.Err()
			}
			select {
			case <-a.release:
			case <-ctx.Done():
				return ctx.Err()
			}
			output, _ := json.Marshal(struct {
				ExecutorPair string          `json:"executor_pair"`
				Input        json.RawMessage `json:"input"`
			}{pair, call.Input})
			return c.Send(ctx, s, transport.ToolMessage{Kind: "result", ID: call.ID, Output: output})
		})
		brains[pair], err = endpoint.NewRemoteExecutor(ctx, address, pair, "test-brain-"+pair)
		if err != nil {
			t.Fatal(err)
		}
	}
	// Duplicate admission must not replace the already connected A brain.
	duplicate, response, err := websocket.Dial(ctx, address+"?pair=A&role=brain", &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer test-brain-A"}},
	})
	if duplicate != nil {
		duplicate.CloseNow()
	}
	if err == nil || response == nil || response.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate identity should be rejected: response=%v err=%v", response, err)
	}
	type answer struct {
		pair   string
		output json.RawMessage
		err    error
	}
	for round := 0; round < 3; round++ {
		answers := make(chan answer, 2)
		start := make(chan struct{})
		for pair, brain := range brains {
			go func() {
				<-start
				input := json.RawMessage(fmt.Sprintf(`{"marker":"%s-%d"}`, pair, round))
				output, err := brain.Call(ctx, "echo", input)
				answers <- answer{pair, output, err}
			}()
		}
		close(start)
		var arrived []arrival
		seen := make(map[string]bool)
		for range 2 {
			select {
			case a := <-arrivals:
				want := fmt.Sprintf(`{"marker":"%s-%d"}`, a.pair, round)
				if seen[a.pair] || a.call.Kind != "call" || string(a.call.Input) != want {
					t.Fatalf("request reached wrong executor: pair=%s input=%s", a.pair, a.call.Input)
				}
				seen[a.pair] = true
				arrived = append(arrived, a)
			case <-ctx.Done():
				t.Fatal("both pairs must have an in-flight call before either completes")
			}
		}
		for _, a := range arrived {
			close(a.release)
		}
		for range 2 {
			select {
			case a := <-answers:
				want := fmt.Sprintf(`{"executor_pair":"%s","input":{"marker":"%s-%d"}}`, a.pair, a.pair, round)
				if a.err != nil || string(a.output) != want {
					t.Fatalf("result reached wrong brain: pair=%s output=%s err=%v", a.pair, a.output, a.err)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		}
	}
}

func TestGatewayRejectsAmbiguousBindings(t *testing.T) {
	t.Setenv("TEST_BRAIN", "brain-token")
	t.Setenv("TEST_EXECUTOR", "executor-token")
	a := gateway.Binding{Pair: "A", BrainTokenEnv: "TEST_BRAIN", ExecutorTokenEnv: "TEST_EXECUTOR"}
	for _, config := range [][]gateway.Binding{
		{a, a},
		{a, {Pair: "B", BrainTokenEnv: "TEST_BRAIN", ExecutorTokenEnv: "TEST_EXECUTOR"}},
	} {
		if g, err := gateway.New(config); err == nil {
			g.Close()
			t.Fatal("duplicate pair or reused credentials should fail at startup")
		}
	}
}
