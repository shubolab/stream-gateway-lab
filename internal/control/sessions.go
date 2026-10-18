package control

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"stream-gateway-lab/internal/transport"
)

type Session struct {
	ID           string    `json:"id"`
	Title        string    `json:"title"`
	ExecutorID   string    `json:"executor_id"`
	ExecutorName string    `json:"executor_name"`
	Workdir      string    `json:"workdir"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	Mode         string    `json:"mode"`
	ActiveTaskID string    `json:"active_task_id,omitempty"`
}

func (s *Server) sessionSnapshotLocked(session *Session) Session {
	copy := *session
	copy.Mode = "chat_only"
	if m := s.members[copy.ExecutorID]; m != nil && m.route.ExecutorOnline() {
		copy.Mode = "ready"
		if m.taskID != "" && m.taskID != session.ActiveTaskID {
			copy.Mode = "busy"
		}
	}
	if run := s.tasks[session.ActiveTaskID]; run != nil && run.ChatOnly {
		copy.Mode = "chat_only"
	}
	return copy
}

func (s *Server) sessionSnapshots() []Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]Session, 0, len(s.sessions))
	for _, session := range s.sessions {
		result = append(result, s.sessionSnapshotLocked(session))
	}
	sort.Slice(result, func(i, j int) bool { return result[i].UpdatedAt.After(result[j].UpdatedAt) })
	return result
}

func (s *Server) saveSessionsLocked() error {
	items := make([]Session, 0, len(s.sessions))
	for _, item := range s.sessions {
		copy := *item
		copy.ActiveTaskID, copy.Mode = "", ""
		items = append(items, copy)
	}
	data, err := json.MarshalIndent(items, "", "  ")
	if err != nil {
		return err
	}
	return WritePrivateFile(filepath.Join(s.config.DataDir, "sessions.json"), data)
}

func (s *Server) saveTurnLocked(task Task) error {
	data, err := json.Marshal(task)
	if err != nil {
		return err
	}
	return WritePrivateFile(filepath.Join(s.config.DataDir, "turns", task.ID+".json"), data)
}

func safeID(id string) bool {
	return id != "" && id != "." && id != ".." && filepath.Base(id) == id && !strings.ContainsAny(id, `/\`)
}

func (s *Server) loadSessions() error {
	data, err := os.ReadFile(filepath.Join(s.config.DataDir, "sessions.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var items []Session
	if err := json.Unmarshal(data, &items); err != nil {
		return err
	}
	for _, item := range items {
		if !safeID(item.ID) || s.members[item.ExecutorID] == nil {
			return errors.New("invalid saved session")
		}
		item.ActiveTaskID = ""
		s.sessions[item.ID] = &item
	}
	files, err := filepath.Glob(filepath.Join(s.config.DataDir, "turns", "*.json"))
	if err != nil {
		return err
	}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		var task Task
		if err := json.Unmarshal(data, &task); err != nil {
			return err
		}
		if !safeID(task.ID) || s.sessions[task.SessionID] == nil {
			return errors.New("invalid saved turn")
		}
		if task.Status == "running" {
			now := time.Now().UTC()
			task.Status, task.Error, task.FinishedAt = "failed", "Gateway restarted; this turn was interrupted and will not be replayed.", &now
			if err := s.saveTurnLocked(task); err != nil {
				return err
			}
		}
		s.tasks[task.ID] = &taskRun{Task: task}
	}
	return nil
}

func (s *Server) createSession(c *gin.Context) {
	var input struct {
		ExecutorID string `json:"executor_id"`
		Workdir    string `json:"workdir"`
	}
	if !readJSON(c, &input) {
		return
	}
	if input.Workdir == "" {
		input.Workdir = "/tmp"
	}
	if !filepath.IsAbs(input.Workdir) || len(input.Workdir) > 4096 {
		fail(c, 400, "工作目录必须是 Executor 上的绝对路径")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.members[input.ExecutorID]
	if s.closed || m == nil || !m.route.ExecutorOnline() {
		fail(c, 409, "创建会话前必须选择在线的 Executor")
		return
	}
	if m.taskID != "" {
		fail(c, 409, "Executor 正在执行其他会话的任务")
		return
	}
	now := time.Now().UTC()
	session := &Session{ID: transport.NewID(), Title: "新会话", ExecutorID: m.Executor.ID, ExecutorName: m.Executor.Name, Workdir: input.Workdir, CreatedAt: now, UpdatedAt: now}
	s.sessions[session.ID] = session
	if err := s.saveSessionsLocked(); err != nil {
		delete(s.sessions, session.ID)
		fail(c, 500, "无法保存会话")
		return
	}
	c.JSON(201, s.sessionSnapshotLocked(session))
}

func (s *Server) sessionDetail(c *gin.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	session := s.sessions[c.Param("id")]
	if session == nil {
		fail(c, 404, "会话不存在")
		return
	}
	turns := []Task{}
	for _, run := range s.tasks {
		if run.SessionID == session.ID {
			turns = append(turns, run.Task)
		}
	}
	sort.Slice(turns, func(i, j int) bool { return turns[i].CreatedAt.Before(turns[j].CreatedAt) })
	c.JSON(200, gin.H{"session": s.sessionSnapshotLocked(session), "turns": turns})
}

func (s *Server) sendMessage(c *gin.Context) {
	var input struct {
		Message string `json:"message"`
	}
	if !readJSON(c, &input) {
		return
	}
	task, status, err := s.createTask(TaskInput{SessionID: c.Param("id"), Message: input.Message, Kind: "agent"})
	if err != nil {
		fail(c, status, err.Error())
		return
	}
	c.JSON(status, task)
}

func (s *Server) modelReady() bool {
	if s.config.AgentPython == "" || s.config.AgentDir == "" || s.config.ModelConfig == "" {
		return false
	}
	if info, err := os.Stat(s.config.AgentPython); err != nil || info.IsDir() {
		return false
	}
	data, err := os.ReadFile(s.config.ModelConfig)
	if err != nil {
		return false
	}
	var config struct {
		BaseURL string `json:"base_url"`
		APIKey  string `json:"api_key"`
		Model   string `json:"model"`
	}
	return json.Unmarshal(data, &config) == nil && strings.TrimSpace(config.BaseURL) != "" && strings.TrimSpace(config.APIKey) != "" && strings.TrimSpace(config.Model) != ""
}
