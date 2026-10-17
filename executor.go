package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
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
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	output, err := executeTool(ctx, tool, input)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return output, err
}

type pendingCall struct {
	id      string
	channel *channel
	result  chan toolMessage
}

type RemoteExecutor struct {
	client  *client
	mu      sync.Mutex
	pending *pendingCall
}

func newRemoteExecutor(ctx context.Context, address, pair, token string) (*RemoteExecutor, error) {
	c, err := newClient(ctx, address, pair, "brain", token)
	if err != nil {
		return nil, err
	}
	e := &RemoteExecutor{client: c}
	go c.run(e.receive)
	if err := c.waitReady(ctx); err != nil {
		c.stop(err)
		return nil, err
	}
	return e, nil
}

func (e *RemoteExecutor) receive(s *channel, body json.RawMessage) error {
	var msg toolMessage
	if err := json.Unmarshal(body, &msg); err != nil || msg.Kind != "result" || msg.ID == "" || (len(msg.Output) == 0) == (msg.Error == "") {
		return errors.New("invalid tool result")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.pending != nil && e.pending.channel == s && msg.ID == e.pending.id {
		select {
		case e.pending.result <- msg:
		default: // No queue or completed-result store; ignore duplicate delivery.
		}
	}
	return nil
}

func (e *RemoteExecutor) Call(ctx context.Context, tool string, input json.RawMessage) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s := e.client.snapshot()
	if s == nil {
		return nil, errors.New("endpoint unavailable")
	}
	p := &pendingCall{id: newID(), channel: s, result: make(chan toolMessage, 1)}
	e.mu.Lock()
	if e.pending != nil {
		e.mu.Unlock()
		return nil, errors.New("executor busy")
	}
	e.pending = p // Register before sending, even for an immediate echo response.
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		e.pending = nil
		e.mu.Unlock()
	}()
	deadline, _ := ctx.Deadline()
	remaining := max(1, time.Until(deadline).Milliseconds())
	if err := e.client.send(ctx, s, toolMessage{Kind: "call", ID: p.id, Tool: tool, Input: input, TimeoutMS: remaining}); err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		// No separate cancel message yet: close this channel to request cancellation.
		e.client.disconnect(s, ctx.Err())
		return nil, ctx.Err()
	case <-s.ctx.Done():
		return nil, context.Cause(s.ctx)
	case result := <-p.result:
		if ctx.Err() != nil {
			e.client.disconnect(s, ctx.Err())
			return nil, ctx.Err()
		}
		if s.ctx.Err() != nil {
			return nil, context.Cause(s.ctx)
		}
		if result.Error != "" {
			return nil, fmt.Errorf("tool failed: %s", result.Error)
		}
		return result.Output, nil
	}
}
