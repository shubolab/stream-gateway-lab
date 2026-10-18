package endpoint

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"sync"
	"time"

	"stream-gateway-lab/internal/transport"
)

// The slot survives socket and epoch changes, until the actual tool exits.
type ExecutorEndpoint struct {
	Client  *Client
	Slot    sync.Mutex
	Execute func(context.Context, string, json.RawMessage) (json.RawMessage, error)
}

func (e *ExecutorEndpoint) Receive(s *Channel, body json.RawMessage) error {
	var call transport.ToolMessage
	if json.Unmarshal(body, &call) != nil || call.Kind != "call" || call.ID == "" || call.TimeoutMS < 1 || call.TimeoutMS > transport.CallTimeout.Milliseconds() {
		return errors.New("invalid tool call")
	}
	if !e.Slot.TryLock() {
		return e.Client.Send(e.Client.Ctx, s, transport.ToolMessage{Kind: "result", ID: call.ID, Error: "executor busy"})
	}
	go func() {
		toolCtx, cancel := context.WithTimeout(s.Ctx, time.Duration(call.TimeoutMS)*time.Millisecond)
		log.Printf("executor pair=%s id=%s tool=%s started epoch=%s", e.Client.pair, call.ID, call.Tool, s.Epoch)
		stopNotice := context.AfterFunc(toolCtx, func() {
			log.Printf("executor pair=%s id=%s cancel requested: %v", e.Client.pair, call.ID, context.Cause(toolCtx))
		})
		output, err := e.Execute(toolCtx, call.Tool, call.Input)
		if toolCtx.Err() != nil {
			err = toolCtx.Err()
		}
		stopNotice()
		cancel()
		e.Slot.Unlock()
		if s.Ctx.Err() != nil {
			log.Printf("executor pair=%s id=%s tool=%s finished err=%v; old result discarded", e.Client.pair, call.ID, call.Tool, err)
			return
		}
		result := transport.ToolMessage{Kind: "result", ID: call.ID, Output: output}
		if err != nil {
			result.Output = nil
			result.Error = err.Error()
		}
		if err := e.Client.Send(e.Client.Ctx, s, result); err != nil {
			log.Printf("executor pair=%s id=%s result discarded: %v", e.Client.pair, call.ID, err)
			return
		}
		log.Printf("executor pair=%s id=%s tool=%s returned", e.Client.pair, call.ID, call.Tool)
	}()
	return nil
}

func RunExecutor(ctx context.Context, address, pair, token string) error {
	c, err := NewClient(ctx, address, pair, "executor", token)
	if err != nil {
		return err
	}
	defer c.Stop(context.Canceled)
	executorPeer := &ExecutorEndpoint{Client: c, Execute: ExecuteTool}
	c.Run(executorPeer.Receive)
	if ctx.Err() != nil {
		return nil
	}
	return context.Cause(c.Ctx)
}
