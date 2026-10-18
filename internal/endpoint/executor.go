package endpoint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"stream-gateway-lab/internal/transport"
)

type Executor interface {
	Call(ctx context.Context, tool string, input json.RawMessage) (json.RawMessage, error)
}

type LocalExecutor struct{ slot sync.Mutex }

func (e *LocalExecutor) Call(ctx context.Context, tool string, input json.RawMessage) (json.RawMessage, error) {
	if !e.slot.TryLock() {
		return nil, errors.New("executor busy")
	}
	defer e.slot.Unlock()
	ctx, cancel := context.WithTimeout(ctx, transport.CallTimeout)
	defer cancel()
	output, err := ExecuteTool(ctx, tool, input)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return output, err
}

type PendingCall struct {
	ID      string
	Channel *Channel
	Result  chan transport.ToolMessage
}

type RemoteExecutor struct {
	Client  *Client
	Mu      sync.Mutex // 保护 Pending；测试检查当前调用时也必须持锁。
	Pending *PendingCall
}

func NewRemoteExecutor(ctx context.Context, address, pair, token string) (*RemoteExecutor, error) {
	c, err := NewClient(ctx, address, pair, "brain", token)
	if err != nil {
		return nil, err
	}
	e := &RemoteExecutor{Client: c}
	go c.Run(e.Receive)
	if err := c.WaitReady(ctx); err != nil {
		c.Stop(err)
		return nil, err
	}
	return e, nil
}

func (e *RemoteExecutor) Receive(s *Channel, body json.RawMessage) error {
	var msg transport.ToolMessage
	if err := json.Unmarshal(body, &msg); err != nil || msg.Kind != "result" || msg.ID == "" || (len(msg.Output) == 0) == (msg.Error == "") {
		return errors.New("invalid tool result")
	}
	e.Mu.Lock()
	defer e.Mu.Unlock()
	if e.Pending != nil && e.Pending.Channel == s && msg.ID == e.Pending.ID {
		select {
		case e.Pending.Result <- msg:
		default: // No queue or completed-result store; ignore duplicate delivery.
		}
	}
	return nil
}

func (e *RemoteExecutor) Call(ctx context.Context, tool string, input json.RawMessage) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, transport.CallTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s := e.Client.Snapshot()
	if s == nil {
		return nil, errors.New("endpoint unavailable")
	}
	p := &PendingCall{ID: transport.NewID(), Channel: s, Result: make(chan transport.ToolMessage, 1)}
	e.Mu.Lock()
	if e.Pending != nil {
		e.Mu.Unlock()
		return nil, errors.New("executor busy")
	}
	e.Pending = p // Register before sending, even for an immediate echo response.
	e.Mu.Unlock()
	defer func() {
		e.Mu.Lock()
		e.Pending = nil
		e.Mu.Unlock()
	}()
	deadline, _ := ctx.Deadline()
	remaining := max(1, time.Until(deadline).Milliseconds())
	if err := e.Client.Send(ctx, s, transport.ToolMessage{Kind: "call", ID: p.ID, Tool: tool, Input: input, TimeoutMS: remaining}); err != nil {
		return nil, err
	}
	return e.WaitResult(ctx, p)
}

func (e *RemoteExecutor) WaitResult(ctx context.Context, p *PendingCall) (json.RawMessage, error) {
	s := p.Channel
	var result transport.ToolMessage
	select {
	case <-ctx.Done():
	case <-s.Ctx.Done():
	case result = <-p.Result:
	}
	// select may choose a result even when the deadline is already ready.
	// All wakeups pass the same terminal checks before accepting success.
	if err := ctx.Err(); err != nil {
		// No separate cancel message yet: close this channel to request cancellation.
		e.Client.Disconnect(s, err)
		return nil, err
	}
	if err := context.Cause(s.Ctx); err != nil {
		return nil, err
	}
	if result.Error != "" {
		return nil, fmt.Errorf("tool failed: %s", result.Error)
	}
	return result.Output, nil
}
