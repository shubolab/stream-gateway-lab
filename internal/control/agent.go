package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"stream-gateway-lab/internal/transport"
)

// exec waits for this JSONL writer before Wait returns, avoiding a scanner/pipe-close race.
type agentEvents struct {
	server *Server
	id     string
	buffer []byte
	err    error
	final  bool
}

func (w *agentEvents) Write(p []byte) (int, error) {
	n := len(p)
	w.buffer = append(w.buffer, p...)
	for {
		line, rest, ok := bytes.Cut(w.buffer, []byte("\n"))
		if !ok {
			break
		}
		if len(line) != 0 {
			w.accept(line)
		}
		w.buffer = rest
	}
	if len(w.buffer) > 128<<10 {
		w.err = errors.New("Brain event exceeds 128 KiB")
		return 0, w.err
	}
	return n, nil
}

func (w *agentEvents) accept(data []byte) {
	var event struct {
		Type     string `json:"type"`
		Message  string `json:"message"`
		Answer   string `json:"answer"`
		ChatOnly bool   `json:"chat_only"`
	}
	if json.Unmarshal(data, &event) != nil || event.Type == "" {
		w.err = errors.New("invalid Brain event")
		return
	}
	s := w.server
	s.mu.Lock()
	defer s.mu.Unlock()
	run := s.tasks[w.id]
	if run == nil {
		return
	}
	if len(data) > 32<<10 {
		data, _ = json.Marshal(map[string]any{"type": event.Type, "message": "内容过长，事件已截断"})
		run.EventsTruncated = true
	}
	run.Events = append(run.Events, append(json.RawMessage(nil), data...))
	if len(run.Events) > 200 {
		run.Events = run.Events[len(run.Events)-200:]
		run.EventsTruncated = true
	}
	switch event.Type {
	case "executor_offline":
		run.ChatOnly = true
	case "error":
		run.Error = event.Message
	case "final":
		w.final = true
		run.ChatOnly = event.ChatOnly
		run.Results, _ = json.Marshal(map[string]any{"answer": event.Answer, "chat_only": event.ChatOnly})
	}
}

func (s *Server) createAgentTurn(input TaskInput) (Task, int, error) {
	if strings.TrimSpace(input.Message) == "" || len(input.Message) > 32<<10 {
		return Task{}, 400, errors.New("消息不能为空且不能超过 32 KiB")
	}
	if !s.modelReady() {
		return Task{}, 503, errors.New("模型尚未配置，请先填写服务端 Responses API 配置")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return Task{}, 503, errors.New("gateway is stopping")
	}
	session := s.sessions[input.SessionID]
	if session == nil {
		return Task{}, 404, errors.New("会话不存在，请先绑定在线 Executor 创建会话")
	}
	if session.ActiveTaskID != "" {
		return Task{}, 409, errors.New("当前会话正在回复，请等待或停止本轮")
	}
	m := s.members[session.ExecutorID]
	if m == nil {
		return Task{}, 409, errors.New("bound Executor is missing")
	}
	chatOnly := !m.route.ExecutorOnline()
	if !chatOnly && m.taskID != "" {
		return Task{}, 409, errors.New("Executor 正在执行其他会话的任务")
	}
	token := transport.NewID()
	if !chatOnly {
		if err := m.route.ReserveBrain(token); err != nil {
			if !m.route.ExecutorOnline() {
				chatOnly = true
			} else {
				return Task{}, 409, err
			}
		}
	}
	ctx, cancel := context.WithTimeout(s.ctx, 10*time.Minute)
	now := time.Now().UTC()
	task := Task{ID: transport.NewID(), SessionID: session.ID, Kind: "agent", ExecutorID: session.ExecutorID, ExecutorName: session.ExecutorName, Workdir: session.Workdir, Message: input.Message, Status: "running", CreatedAt: now, ChatOnly: chatOnly}
	history, _ := filepath.Abs(filepath.Join(s.config.DataDir, "histories", session.ID+".json"))
	args := []string{"-m", "gateway_brain", "--url", s.config.BrainURL, "--executor-id", session.ExecutorID, "--workdir", session.Workdir, "--model-config", s.config.ModelConfig, "--history", history}
	if chatOnly {
		args = append(args, "--chat-only")
	}
	cmd := exec.CommandContext(ctx, s.config.AgentPython, args...)
	cmd.Dir = s.config.AgentDir
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "PYTHONUNBUFFERED=1", "PYTHONUTF8=1", "LAB_TASK_TOKEN=" + token}
	request, _ := json.Marshal(map[string]string{"message": input.Message})
	cmd.Stdin = bytes.NewReader(request)
	events := &agentEvents{server: s, id: task.ID}
	stderr := &limitedOutput{}
	cmd.Stdout, cmd.Stderr = events, stderr
	if err := cmd.Start(); err != nil {
		cancel()
		if !chatOnly {
			m.route.ReleaseBrain(token)
		}
		return Task{}, 500, fmt.Errorf("start Python Brain: %w", err)
	}
	task.PID = cmd.Process.Pid
	run := &taskRun{Task: task, cancel: cancel}
	s.tasks[task.ID] = run
	session.ActiveTaskID, session.UpdatedAt = task.ID, now
	if session.Title == "新会话" {
		title := []rune(strings.TrimSpace(input.Message))
		session.Title = string(title[:min(40, len(title))])
	}
	if !chatOnly {
		m.taskID = task.ID
	}
	if err := s.saveTurnLocked(task); err != nil {
		cancel()
		log.Printf("save turn: %v", err)
	}
	if err := s.saveSessionsLocked(); err != nil {
		cancel()
		log.Printf("save session: %v", err)
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		err := cmd.Wait()
		cause := ctx.Err()
		cancel()
		if !chatOnly {
			m.route.ReleaseBrain(token)
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		now := time.Now().UTC()
		run.FinishedAt = &now
		// Raw stderr may contain provider response bodies; only structured errors reach chat.
		run.Status = "succeeded"
		switch {
		case cause != nil:
			run.Status, run.Error = "canceled", "本轮已停止"
			if errors.Is(cause, context.DeadlineExceeded) {
				run.Status, run.Error = "failed", "本轮达到 10 分钟时限"
			}
		case err != nil || events.err != nil || !events.final:
			run.Status = "failed"
			if run.Error == "" {
				run.Error = "Brain 未正常完成，请检查模型配置和运行环境"
			}
		}
		if m.taskID == task.ID {
			m.taskID = ""
		}
		session.ActiveTaskID, session.UpdatedAt = "", now
		if err := s.saveTurnLocked(run.Task); err != nil {
			log.Printf("save turn: %v", err)
		}
		if err := s.saveSessionsLocked(); err != nil {
			log.Printf("save session: %v", err)
		}
	}()
	return task, 201, nil
}
