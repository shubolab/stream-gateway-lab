package test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"stream-gateway-lab/internal/control"
	"stream-gateway-lab/internal/endpoint"
	"stream-gateway-lab/internal/gateway"
)

type poolState struct {
	Executors []control.Executor `json:"executors"`
	Tasks     []control.Task     `json:"tasks"`
}

// 公网 RTT 超过旧的 2 秒上限时，池路由仍应保持连接。
func TestPoolDelayedPong(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	route := gateway.NewPoolRoute("delayed", "executor-token")
	server := httptest.NewServer(route)
	defer server.Close()
	defer route.Close()
	pongDelayed := make(chan struct{})
	var once sync.Once
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/ws?pair=delayed&role=executor", &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer executor-token"}},
		OnPingReceived: func(ctx context.Context, _ []byte) bool {
			select {
			case <-time.After(3 * time.Second):
				once.Do(func() { close(pongDelayed) })
				return true
			case <-ctx.Done():
				return false
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	go func() {
		for {
			if _, _, err := conn.Read(ctx); err != nil {
				return
			}
		}
	}()
	select {
	case <-pongDelayed:
	case <-ctx.Done():
		t.Fatal("did not observe delayed pong")
	}
	pingCtx, stop := context.WithTimeout(ctx, 2*time.Second)
	defer stop()
	if err := conn.Ping(pingCtx); err != nil {
		t.Fatalf("pool disconnected on a 3-second pong: %v", err)
	}
	if !route.ExecutorOnline() {
		t.Fatal("executor must remain online")
	}
}

func TestControlDownloads(t *testing.T) {
	dir := t.TempDir()
	s, err := control.New(context.Background(), control.Config{AdminToken: "admin", DataDir: t.TempDir(), BrainBinary: "unused", BrainURL: "ws://unused", DownloadDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	server := httptest.NewServer(s)
	defer server.Close()
	builds := map[string]string{"linux/amd64": control.LinuxBinary, "darwin/arm64": control.DarwinBinary}
	for platform, binary := range builds {
		poolRequest(t, server.URL, "/downloads/"+binary, "", "GET", nil, 503, nil)
		content := []byte("platform-specific payload: " + platform)
		if err := os.WriteFile(filepath.Join(dir, binary), content, 0600); err != nil {
			t.Fatal(err)
		}
		compressed := []byte("compressed " + platform)
		if err := os.WriteFile(filepath.Join(dir, binary+".gz"), compressed, 0600); err != nil {
			t.Fatal(err)
		}
		if got := poolRequest(t, server.URL, "/downloads/"+binary+".gz", "", "GET", nil, 200, nil); !bytes.Equal(got, compressed) {
			t.Fatalf("wrong compressed download for %s", platform)
		}
		got := poolRequest(t, server.URL, "/downloads/"+binary, "", "GET", nil, 200, nil)
		if !bytes.Equal(got, content) {
			t.Fatalf("wrong download for %s", platform)
		}
		got = poolRequest(t, server.URL, "/downloads/"+binary+".sha256", "", "GET", nil, 200, nil)
		if string(got) != fmt.Sprintf("%x  %s\n", sha256.Sum256(content), binary) {
			t.Fatalf("wrong checksum for %s: %s", platform, got)
		}
	}
	var state struct {
		Downloads map[string]bool `json:"downloads"`
	}
	poolRequest(t, server.URL, "/api/state", "admin", "GET", nil, 200, &state)
	for platform := range builds {
		if !state.Downloads[platform] {
			t.Errorf("missing ready build: %s", platform)
		}
	}
	poolRequest(t, server.URL, "/downloads/executor.json", "", "GET", nil, 404, nil)
}

func poolRequest(t *testing.T, address, path, token, method string, input any, want int, output any) []byte {
	t.Helper()
	var body io.Reader
	if input != nil {
		data, err := json.Marshal(input)
		if err != nil {
			t.Fatal(err)
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, address+path, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	response, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != want {
		t.Fatalf("%s %s: status=%d want=%d body=%s", method, path, response.StatusCode, want, data)
	}
	if output != nil {
		if err := json.Unmarshal(data, output); err != nil {
			t.Fatal(err)
		}
	}
	return data
}

func poolEventually(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("pool condition did not become true")
}

func TestControlPool(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "lab")
	build := exec.Command("go", "build", "-o", binary, ".")
	build.Dir = ".."
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build Brain: %v\n%s", err, output)
	}
	dataDir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	httpServer := httptest.NewUnstartedServer(nil)
	address := "http://" + httpServer.Listener.Addr().String()
	wsURL := "ws" + strings.TrimPrefix(address, "http") + "/ws"
	s, err := control.New(ctx, control.Config{AdminToken: "pool-test-admin", DataDir: dataDir, BrainBinary: binary, BrainURL: wsURL})
	if err != nil {
		t.Fatal(err)
	}
	httpServer.Config.Handler = s
	httpServer.Start()
	t.Cleanup(func() { cancel(); s.Close(); httpServer.Close() })
	request := func(path, method string, input any, status int, output any) []byte {
		return poolRequest(t, address, path, "pool-test-admin", method, input, status, output)
	}
	state := func() poolState { var result poolState; request("/api/state", "GET", nil, 200, &result); return result }
	poolRequest(t, address, "/api/state", "wrong", "GET", nil, 401, nil)
	poolRequest(t, address, "/api/enrollments", "wrong", "POST", nil, 401, nil)
	request("/api/tasks", "POST", map[string]any{"unknown_field": 1}, 400, nil)
	request("/api/tasks", "POST", control.TaskInput{ExecutorID: "missing"}, 404, nil)
	registrations := make(map[string]struct{ ID, Token string })
	for _, name := range []string{"A", "B"} {
		var join struct{ Token string }
		request("/api/enrollments", "POST", nil, 201, &join)
		var saved struct{ ID, Token string }
		input := map[string]string{"name": name, "hostname": "machine-" + name, "os": "linux", "arch": "amd64"}
		poolRequest(t, address, "/api/register", join.Token, "POST", input, 201, &saved)
		poolRequest(t, address, "/api/register", join.Token, "POST", input, 401, nil)
		registrations[name] = saved
		request("/api/tasks", "POST", control.TaskInput{ExecutorID: saved.ID}, 409, nil)
	}
	data := request("/api/state", "GET", nil, 200, nil)
	for _, saved := range registrations {
		if bytes.Contains(data, []byte(saved.Token)) {
			t.Fatal("state leaked executor token")
		}
	}
	info, err := os.Stat(filepath.Join(dataDir, "executors.json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("registry must be private: %v %v", info, err)
	}
	arrivals := make(chan string, 16)
	started := make(chan string, 4)
	canceled := make(chan string, 4)
	releaseB := make(chan struct{})
	clients := make(map[string]*endpoint.Client)
	for _, name := range []string{"A", "B"} {
		id := registrations[name]
		client, err := endpoint.NewPoolClient(ctx, wsURL, id.ID, id.Token)
		if err != nil {
			t.Fatal(err)
		}
		clients[name] = client
		e := &endpoint.ExecutorEndpoint{Client: client, Execute: func(toolCtx context.Context, tool string, input json.RawMessage) (json.RawMessage, error) {
			if tool == "echo" {
				arrivals <- name + ":" + string(input)
			}
			if tool == "wait" {
				started <- name
				select {
				case <-toolCtx.Done():
					canceled <- name
					return nil, toolCtx.Err()
				case <-releaseB:
				}
			}
			return endpoint.ExecuteTool(toolCtx, tool, input)
		}}
		go client.Run(e.Receive)
	}
	poolEventually(t, func() bool {
		all := state()
		return len(all.Executors) == 2 && all.Executors[0].Online && all.Executors[1].Online
	})
	// 执行端凭证不能冒充 Brain，也不能访问其他注册身份。
	for _, target := range []struct{ ID, Role string }{{registrations["A"].ID, "brain"}, {registrations["B"].ID, "executor"}} {
		conn, response, err := websocket.Dial(ctx, wsURL+"?pair="+target.ID+"&role="+target.Role, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + registrations["A"].Token}}})
		if conn != nil {
			conn.CloseNow()
		}
		if err == nil || response == nil || response.StatusCode != 401 {
			t.Fatalf("identity crossover accepted: %v", err)
		}
	}
	// 两次并发抢占同一执行端，只能创建一个真实 Brain 进程。
	var winner control.Task
	var winnerMu sync.Mutex
	var wg sync.WaitGroup
	statuses := make(chan int, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			input, _ := json.Marshal(control.TaskInput{ExecutorID: registrations["A"].ID, Message: "only-A"})
			req, _ := http.NewRequest("POST", address+"/api/tasks", bytes.NewReader(input))
			req.Header.Set("Authorization", "Bearer pool-test-admin")
			response, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Error(err)
				return
			}
			defer response.Body.Close()
			statuses <- response.StatusCode
			if response.StatusCode == 201 {
				winnerMu.Lock()
				json.NewDecoder(response.Body).Decode(&winner)
				winnerMu.Unlock()
			}
		}()
	}
	wg.Wait()
	close(statuses)
	counts := make(map[int]int)
	for status := range statuses {
		counts[status]++
	}
	if counts[201] != 1 || counts[409] != 1 || winner.PID == 0 || winner.PID == os.Getpid() {
		t.Fatalf("invalid process reservation: %v %+v", counts, winner)
	}
	var taskB control.Task
	request("/api/tasks", "POST", control.TaskInput{ExecutorID: registrations["B"].ID, Message: "only-B"}, 201, &taskB)
	if taskB.PID == winner.PID {
		t.Fatal("two tasks reused one Brain process")
	}
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(10 * time.Second):
			t.Fatal("tools did not start")
		}
	}
	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		select {
		case arrival := <-arrivals:
			if strings.HasPrefix(arrival, "A:") && strings.Contains(arrival, "only-A") {
				seen["A"] = true
			}
			if strings.HasPrefix(arrival, "B:") && strings.Contains(arrival, "only-B") {
				seen["B"] = true
			}
		case <-time.After(time.Second):
			t.Fatal("missing executor arrival")
		}
	}
	if len(seen) != 2 {
		t.Fatal("task reached the wrong executor")
	}
	request("/api/tasks/"+winner.ID+"/cancel", "POST", nil, 202, nil)
	select {
	case name := <-canceled:
		if name != "A" {
			t.Fatal("canceled wrong executor")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor did not observe Brain cancellation")
	}
	close(releaseB)
	poolEventually(t, func() bool {
		all := state()
		done := 0
		for _, task := range all.Tasks {
			if task.ID == winner.ID && task.Status == "canceled" {
				done++
			}
			if task.ID == taskB.ID && task.Status == "succeeded" && strings.Contains(string(task.Results), "only-B") {
				done++
			}
		}
		return done == 2
	})
	// Executor 可以在没有 Brain 的情况下等待超过旧的 30 秒对端期限。
	t.Log("waiting beyond the legacy peer timeout to verify an idle pool member stays online")
	time.Sleep(31 * time.Second)
	var fresh control.Task
	request("/api/tasks", "POST", control.TaskInput{ExecutorID: registrations["A"].ID, Message: "A-after-idle"}, 201, &fresh)
	poolEventually(t, func() bool {
		for _, task := range state().Tasks {
			if task.ID == fresh.ID {
				return task.Status == "succeeded"
			}
		}
		return false
	})
	// 已知离线的执行端拒绝新任务。
	clients["A"].Stop(context.Canceled)
	poolEventually(t, func() bool {
		for _, e := range state().Executors {
			if e.ID == registrations["A"].ID {
				return !e.Online
			}
		}
		return false
	})
	request("/api/tasks", "POST", control.TaskInput{ExecutorID: registrations["A"].ID}, 409, nil)
	// 重建控制层会保留注册身份，但不会复活旧任务或在线状态。
	s.Close()
	restored, err := control.New(ctx, control.Config{AdminToken: "pool-test-admin", DataDir: dataDir, BrainBinary: "/does-not-exist/brain", BrainURL: wsURL})
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	record := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/state", nil)
	req.Header.Set("Authorization", "Bearer pool-test-admin")
	restored.ServeHTTP(record, req)
	var recovered poolState
	if err := json.Unmarshal(record.Body.Bytes(), &recovered); err != nil {
		t.Fatal(err)
	}
	if len(recovered.Executors) != 2 || len(recovered.Tasks) != 0 {
		t.Fatalf("invalid restart state: %+v", recovered)
	}
	for _, e := range recovered.Executors {
		if e.Online || e.TaskID != "" {
			t.Fatal("restart restored live runtime state")
		}
	}
	restoredHTTP := httptest.NewServer(restored)
	defer restoredHTTP.Close()
	restoredClient, err := endpoint.NewPoolClient(ctx, "ws"+strings.TrimPrefix(restoredHTTP.URL, "http")+"/ws", registrations["A"].ID, registrations["A"].Token)
	if err != nil {
		t.Fatal(err)
	}
	defer restoredClient.Stop(context.Canceled)
	restoredEndpoint := &endpoint.ExecutorEndpoint{Client: restoredClient, Execute: endpoint.ExecuteTool}
	go restoredClient.Run(restoredEndpoint.Receive)
	poolEventually(t, func() bool {
		var all poolState
		poolRequest(t, restoredHTTP.URL, "/api/state", "pool-test-admin", "GET", nil, 200, &all)
		for _, e := range all.Executors {
			if e.ID == registrations["A"].ID {
				return e.Online
			}
		}
		return false
	})
	// 进程启动失败释放预占；连续两次均应报启动错误，而非遗留 busy。
	for i := 0; i < 2; i++ {
		poolRequest(t, restoredHTTP.URL, "/api/tasks", "pool-test-admin", "POST", control.TaskInput{ExecutorID: registrations["A"].ID}, 500, nil)
	}
}
