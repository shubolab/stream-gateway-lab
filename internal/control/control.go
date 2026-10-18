// Package control 管理共享执行池和 Brain 子进程；工具消息仍由 gateway 在线转发。
package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"stream-gateway-lab/internal/gateway"
	"stream-gateway-lab/internal/transport"
)

type Config struct {
	DataDir, AdminToken, BrainBinary, BrainURL, DownloadDir string
	AgentPython, AgentDir, ModelConfig                      string
}

type Executor struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Hostname     string    `json:"hostname"`
	OS           string    `json:"os"`
	Arch         string    `json:"arch"`
	RegisteredAt time.Time `json:"registered_at"`
	Online       bool      `json:"online"`
	TaskID       string    `json:"task_id,omitempty"`
}

type identity struct {
	Executor Executor `json:"executor"`
	Token    string   `json:"token"`
}

type member struct {
	identity
	route  *gateway.PairRoute
	taskID string
}

type Server struct {
	router   http.Handler
	config   Config
	ctx      context.Context
	cancel   context.CancelFunc
	mu       sync.Mutex
	modelMu  sync.Mutex
	members  map[string]*member
	joins    map[string]time.Time
	tasks    map[string]*taskRun
	sessions map[string]*Session
	closed   bool
	wg       sync.WaitGroup
}

func New(ctx context.Context, cfg Config) (*Server, error) {
	if cfg.AdminToken == "" || cfg.BrainBinary == "" || cfg.BrainURL == "" || cfg.DataDir == "" {
		return nil, errors.New("control requires admin token, data directory, Brain binary and WebSocket URL")
	}
	if err := os.MkdirAll(cfg.DataDir, 0700); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	for _, value := range []*string{&cfg.AgentPython, &cfg.AgentDir, &cfg.ModelConfig} {
		if *value != "" {
			absolute, err := filepath.Abs(*value)
			if err != nil {
				cancel()
				return nil, err
			}
			*value = absolute
		}
	}
	s := &Server{config: cfg, ctx: ctx, cancel: cancel, members: make(map[string]*member), joins: make(map[string]time.Time), tasks: make(map[string]*taskRun), sessions: make(map[string]*Session)}
	data, err := os.ReadFile(filepath.Join(cfg.DataDir, "executors.json"))
	if err != nil && !os.IsNotExist(err) {
		cancel()
		return nil, err
	}
	if err == nil {
		var saved []identity
		if err := json.Unmarshal(data, &saved); err != nil {
			cancel()
			return nil, err
		}
		for _, id := range saved {
			if id.Executor.ID == "" || id.Token == "" || s.members[id.Executor.ID] != nil {
				cancel()
				return nil, errors.New("invalid executor registry")
			}
			s.members[id.Executor.ID] = &member{identity: id, route: gateway.NewPoolRoute(id.Executor.ID, id.Token)}
		}
	}
	if err := s.loadSessions(); err != nil {
		cancel()
		return nil, err
	}
	s.router = s.newRouter()
	return s, nil
}

// 这里只保存注册身份；聊天会话和轮次由 sessions.go 保存，不用于恢复或重放工具调用。
func (s *Server) saveMembersLocked() error {
	ids := make([]identity, 0, len(s.members))
	for _, m := range s.members {
		ids = append(ids, m.identity)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].Executor.ID < ids[j].Executor.ID })
	data, err := json.MarshalIndent(ids, "", "  ")
	if err != nil {
		return err
	}
	return WritePrivateFile(filepath.Join(s.config.DataDir, "executors.json"), data)
}

func WritePrivateFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".write-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func AdminToken(dataDir string) (string, error) {
	if token := os.Getenv("LAB_ADMIN_TOKEN"); token != "" {
		return token, nil
	}
	path := filepath.Join(dataDir, "admin-token")
	data, err := os.ReadFile(path)
	if err == nil {
		if token := strings.TrimSpace(string(data)); token != "" {
			return token, nil
		}
		return "", errors.New("admin-token is empty")
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	token := transport.NewID()
	return token, WritePrivateFile(path, []byte(token+"\n"))
}

func (s *Server) Close() {
	s.mu.Lock()
	s.closed = true
	s.cancel()
	routes := make([]*gateway.PairRoute, 0, len(s.members))
	for _, m := range s.members {
		routes = append(routes, m.route)
	}
	s.mu.Unlock()
	for _, route := range routes {
		route.Close()
	}
	s.wg.Wait()
}

func (s *Server) snapshot() ([]Executor, []Task) {
	s.mu.Lock()
	defer s.mu.Unlock()
	executors := make([]Executor, 0, len(s.members))
	for _, m := range s.members {
		e := m.Executor
		e.Online, e.TaskID = m.route.ExecutorOnline(), m.taskID
		executors = append(executors, e)
	}
	sort.Slice(executors, func(i, j int) bool { return executors[i].RegisteredAt.Before(executors[j].RegisteredAt) })
	tasks := make([]Task, 0, len(s.tasks))
	for _, run := range s.tasks {
		item := run.Task
		item.Events = nil // 事件仅在选中的会话详情中返回。
		tasks = append(tasks, item)
	}
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].CreatedAt.After(tasks[j].CreatedAt) })
	return executors, tasks
}
