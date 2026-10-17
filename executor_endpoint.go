package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"sync"
	"time"
)

// The slot survives socket and epoch changes, until the actual tool exits.
type executorEndpoint struct {
	client  *client
	slot    sync.Mutex
	execute func(context.Context, string, json.RawMessage) (json.RawMessage, error)
}

func (e *executorEndpoint) receive(s *channel, body json.RawMessage) error {
	var call toolMessage
	if json.Unmarshal(body, &call) != nil || call.Kind != "call" || call.ID == "" || call.TimeoutMS < 1 || call.TimeoutMS > callTimeout.Milliseconds() {
		return errors.New("invalid tool call")
	}
	if !e.slot.TryLock() {
		return e.client.send(e.client.ctx, s, toolMessage{Kind: "result", ID: call.ID, Error: "executor busy"})
	}
	go func() {
		toolCtx, cancel := context.WithTimeout(s.ctx, time.Duration(call.TimeoutMS)*time.Millisecond)
		log.Printf("executor pair=%s id=%s tool=%s started epoch=%s", e.client.pair, call.ID, call.Tool, s.epoch)
		stopNotice := context.AfterFunc(toolCtx, func() {
			log.Printf("executor pair=%s id=%s cancel requested: %v", e.client.pair, call.ID, context.Cause(toolCtx))
		})
		output, err := e.execute(toolCtx, call.Tool, call.Input)
		if toolCtx.Err() != nil {
			err = toolCtx.Err()
		}
		stopNotice()
		cancel()
		e.slot.Unlock()
		if s.ctx.Err() != nil {
			log.Printf("executor pair=%s id=%s tool=%s finished err=%v; old result discarded", e.client.pair, call.ID, call.Tool, err)
			return
		}
		result := toolMessage{Kind: "result", ID: call.ID, Output: output}
		if err != nil {
			result.Output = nil
			result.Error = err.Error()
		}
		if err := e.client.send(e.client.ctx, s, result); err != nil {
			log.Printf("executor pair=%s id=%s result discarded: %v", e.client.pair, call.ID, err)
			return
		}
		log.Printf("executor pair=%s id=%s tool=%s returned", e.client.pair, call.ID, call.Tool)
	}()
	return nil
}

func runExecutor(ctx context.Context, address, pair, token string) error {
	c, err := newClient(ctx, address, pair, "executor", token)
	if err != nil {
		return err
	}
	defer c.stop(context.Canceled)
	endpoint := &executorEndpoint{client: c, execute: executeTool}
	c.run(endpoint.receive)
	if ctx.Err() != nil {
		return nil
	}
	return context.Cause(c.ctx)
}
