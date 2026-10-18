package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"stream-gateway-lab/internal/transport"
)

type Task struct {
	SessionID       string            `json:"session_id,omitempty"`
	Kind            string            `json:"kind,omitempty"`
	Workdir         string            `json:"workdir,omitempty"`
	ChatOnly        bool              `json:"chat_only"`
	Events          []json.RawMessage `json:"events,omitempty"`
	EventsTruncated bool              `json:"events_truncated,omitempty"`
	ID              string            `json:"id"`
	ExecutorID      string            `json:"executor_id"`
	ExecutorName    string            `json:"executor_name"`
	Message         string            `json:"message"`
	WaitMS          int               `json:"wait_ms"`
	Status          string            `json:"status"`
	PID             int               `json:"pid"`
	CreatedAt       time.Time         `json:"created_at"`
	FinishedAt      *time.Time        `json:"finished_at,omitempty"`
	Results         json.RawMessage   `json:"results,omitempty"`
	Error           string            `json:"error,omitempty"`
	Logs            string            `json:"logs,omitempty"`
}

type taskRun struct {
	Task
	cancel context.CancelFunc
}

type TaskInput struct {
	SessionID  string `json:"session_id,omitempty"`
	Kind       string `json:"kind,omitempty"`
	ExecutorID string `json:"executor_id"`
	Message    string `json:"message"`
	WaitMS     int    `json:"wait_ms"`
}

// limitedOutput 只保留进程诊断所需的有限输出，绝不用于补发或恢复任务。
type limitedOutput struct{ data []byte }

func (b *limitedOutput) Write(p []byte) (int, error) {
	n := len(p)
	if remaining := (64 << 10) - len(b.data); remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		b.data = append(b.data, p...)
	}
	return n, nil
}

func (s *Server) createTask(input TaskInput) (Task, int, error) {
	if input.Kind == "agent" || input.SessionID != "" {
		return s.createAgentTurn(input)
	}
	if input.Kind != "" && input.Kind != "echo" {
		return Task{}, 400, errors.New("invalid task kind")
	}
	if input.ExecutorID == "" || len(input.Message) > 8<<10 || input.WaitMS < 0 || input.WaitMS > 30000 {
		return Task{}, 400, errors.New("choose an executor; message must be at most 8 KiB and wait_ms between 0 and 30000")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return Task{}, 503, errors.New("gateway is stopping")
	}
	m := s.members[input.ExecutorID]
	if m == nil {
		return Task{}, 404, errors.New("executor not found")
	}
	if m.taskID != "" {
		return Task{}, 409, errors.New("executor is busy")
	}
	token := transport.NewID()
	if err := m.route.ReserveBrain(token); err != nil {
		return Task{}, 409, err
	}
	ctx, cancel := context.WithTimeout(s.ctx, 40*time.Second)
	task := Task{ID: transport.NewID(), ExecutorID: m.Executor.ID, ExecutorName: m.Executor.Name, Message: input.Message, WaitMS: input.WaitMS, Status: "running", CreatedAt: time.Now().UTC()}
	cmd := exec.CommandContext(ctx, s.config.BrainBinary, "brain", "-url", s.config.BrainURL, "-pair", m.Executor.ID, "-token-env", "LAB_TASK_TOKEN", "-message", input.Message, "-wait-ms", strconv.Itoa(input.WaitMS))
	// 子进程只收到本任务的临时凭证，不继承平台管理密钥或其他执行端身份。
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "LAB_TASK_TOKEN=" + token}
	stdout, stderr := &limitedOutput{}, &limitedOutput{}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Start(); err != nil {
		cancel()
		m.route.ReleaseBrain(token)
		return Task{}, 500, fmt.Errorf("start Brain: %w", err)
	}
	task.PID = cmd.Process.Pid
	run := &taskRun{Task: task, cancel: cancel}
	s.tasks[task.ID], m.taskID = run, task.ID
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		err := cmd.Wait()
		cause := ctx.Err()
		cancel()
		// 撤销临时凭证并完成旧 Brain 的路由清理后，才允许下一个任务占用执行端。
		m.route.ReleaseBrain(token)
		s.mu.Lock()
		defer s.mu.Unlock()
		now := time.Now().UTC()
		run.FinishedAt, run.Logs = &now, string(stderr.data)
		run.Status = "succeeded"
		switch {
		case cause != nil:
			run.Status, run.Error = "canceled", cause.Error()
			if errors.Is(cause, context.DeadlineExceeded) {
				run.Status = "failed"
			}
		case err != nil:
			run.Status, run.Error = "failed", err.Error()
		case !json.Valid(stdout.data):
			run.Status, run.Error = "failed", "Brain returned invalid JSON"
		default:
			run.Results = append(json.RawMessage(nil), stdout.data...)
		}
		if run.Status == "failed" && cause == nil {
			for _, line := range strings.Split(run.Logs, "\n") {
				if _, detail, ok := strings.Cut(line, "error: "); ok {
					run.Error = detail
				}
			}
		}
		m.taskID = ""
		s.trimTasksLocked()
	}()
	return task, 201, nil
}

func (s *Server) trimTasksLocked() {
	// 最近 100 个已完成任务加上全部运行中任务；控制台不会无限保留输出。
	for {
		count := 0
		var oldest *taskRun
		for _, run := range s.tasks {
			if run.FinishedAt == nil || run.SessionID != "" {
				continue
			}
			count++
			if oldest == nil || run.CreatedAt.Before(oldest.CreatedAt) {
				oldest = run
			}
		}
		if count <= 100 {
			return
		}
		delete(s.tasks, oldest.ID)
	}
}
