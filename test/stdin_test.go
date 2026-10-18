package test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"stream-gateway-lab/internal/app"
	"stream-gateway-lab/internal/endpoint"
	"stream-gateway-lab/internal/gateway"
)

// Run the real CLI/signal handler in a child, with an inherited blocking pipe.
func TestBrainStdinProcess(t *testing.T) {
	if os.Getenv("LAB_TEST_STDIN_CHILD") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			os.Args = append(os.Args[:1], os.Args[i+1:]...)
			app.Main()
			os.Exit(0)
		}
	}
	t.Fatal("missing child CLI arguments")
}

func TestBrainStdinStops(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("subprocess SIGINT check requires Unix")
	}
	for _, mode := range []string{"local", "remote"} {
		for _, ending := range []string{"idle", "partial", "eof"} {
			t.Run(mode+"/"+ending, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				args := []string{"-test.run=^TestBrainStdinProcess$", "--", "brain", "-mode", mode, "-stdin"}
				var executorPeer *endpoint.Client
				var address string
				if mode == "remote" {
					g := testGateway(t, "A")
					server := httptest.NewServer(g)
					t.Cleanup(func() { cancel(); g.Close(); server.Close() })
					address = "ws" + strings.TrimPrefix(server.URL, "http") + "/ws"
					config := filepath.Join(t.TempDir(), "bindings.json")
					data, _ := json.Marshal([]gateway.Binding{{Pair: "A", BrainTokenEnv: "TEST_BRAIN_A", ExecutorTokenEnv: "TEST_EXECUTOR_A"}})
					if err := os.WriteFile(config, data, 0600); err != nil {
						t.Fatal(err)
					}
					args = append(args, "-url", address, "-config", config)
					var err error
					executorPeer, err = endpoint.NewClient(ctx, address, "A", "executor", "test-executor-A")
					if err != nil {
						t.Fatal(err)
					}
					e := &endpoint.ExecutorEndpoint{Client: executorPeer, Execute: endpoint.ExecuteTool}
					go executorPeer.Run(e.Receive)
				}
				cmd := exec.Command(os.Args[0], args...)
				cmd.Env = append(os.Environ(), "LAB_TEST_STDIN_CHILD=1")
				cmd.Stderr = os.Stderr
				stdin, err := cmd.StdinPipe()
				if err != nil {
					t.Fatal(err)
				}
				defer stdin.Close()
				stdout, err := cmd.StdoutPipe()
				if err != nil {
					t.Fatal(err)
				}
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
				done := make(chan struct{})
				var waitErr error
				go func() { waitErr = cmd.Wait(); close(done) }()
				t.Cleanup(func() { _ = cmd.Process.Kill(); <-done })
				// A successful request proves the signal handler is installed and
				// the Brain has entered -stdin, rather than still starting up.
				if _, err := fmt.Fprintln(stdin, `{"message":"before-stop","wait_ms":0}`); err != nil {
					t.Fatal(err)
				}
				result := make(chan error, 1)
				go func() {
					var response struct {
						Results []app.Observation `json:"results"`
					}
					err := json.NewDecoder(stdout).Decode(&response)
					if err == nil && (len(response.Results) != 2 || string(response.Results[1].Output) != `{"waited_ms":0}`) {
						err = fmt.Errorf("unexpected response: %+v", response)
					}
					result <- err
				}()
				select {
				case err := <-result:
					if err != nil {
						t.Fatal(err)
					}
				case <-ctx.Done():
					t.Fatal("Brain did not finish the initial request")
				}
				var old *endpoint.Channel
				if executorPeer != nil {
					old = executorPeer.Snapshot()
					if old == nil {
						t.Fatal("Executor must still be connected")
					}
				}
				if ending == "partial" {
					if _, err := fmt.Fprint(stdin, `{"message":`); err != nil {
						t.Fatal(err)
					}
				}
				// Let Scan block with the writer still open. No EOF helps SIGINT.
				time.Sleep(50 * time.Millisecond)
				stoppedAt := time.Now()
				if ending == "eof" {
					err = stdin.Close()
				} else {
					err = cmd.Process.Signal(os.Interrupt)
				}
				if err != nil {
					t.Fatal(err)
				}
				select {
				case <-done:
					if waitErr != nil {
						t.Fatalf("Brain did not exit cleanly: %v", waitErr)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("Brain did not exit within 2 seconds; pipe writer is still open")
				}
				t.Logf("%s: exit code 0 after %s (race builds include runtime exit delay)", ending, time.Since(stoppedAt))
				if executorPeer != nil {
					select {
					case <-old.Ctx.Done():
					case <-time.After(time.Second):
						t.Fatal("gateway did not invalidate the old channel")
					}
					// Reuse the same Brain identity immediately. This also proves
					// gateway cleanup released its slot and the Executor stayed up.
					freshCtx, stop := context.WithTimeout(ctx, time.Second)
					defer stop()
					fresh, err := endpoint.NewRemoteExecutor(freshCtx, address, "A", "test-brain-A")
					if err != nil {
						t.Fatalf("Brain identity not reusable: %v", err)
					}
					defer fresh.Client.Stop(context.Canceled)
					if err := executorPeer.WaitReady(freshCtx); err != nil {
						t.Fatal(err)
					}
					if executorPeer.Snapshot().Conn != old.Conn {
						t.Fatal("healthy Executor socket was replaced")
					}
					output, err := fresh.Call(freshCtx, "echo", json.RawMessage(`"after-stop"`))
					if err != nil || string(output) != `"after-stop"` {
						t.Fatalf("replacement Brain call failed: %s, %v", output, err)
					}
				}
			})
		}
	}
}
