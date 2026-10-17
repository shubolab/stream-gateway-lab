package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/coder/websocket"
)

const (
	heartbeatInterval = time.Second
	heartbeatTimeout  = 2 * time.Second
)

var errHeartbeat = errors.New("heartbeat failed")

// Watch exactly one socket, including while waiting for its paired endpoint.
// The caller must keep reading: websocket.Read handles Ping/Pong control frames.
func heartbeatContext(parent context.Context, conn *websocket.Conn) (context.Context, context.CancelCauseFunc) {
	ctx, cancel := context.WithCancelCause(parent)
	go func() {
		ticker := time.NewTicker(heartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			pingCtx, stop := context.WithTimeout(ctx, heartbeatTimeout)
			err := conn.Ping(pingCtx)
			stop()
			if err != nil {
				if ctx.Err() == nil {
					cancel(fmt.Errorf("%w: %v", errHeartbeat, err))
					// A Pong timeout alone need not close the socket. End the read
					// loop so existing channel invalidation/reconnect handles loss.
					conn.CloseNow()
				}
				return
			}
		}
	}()
	return ctx, cancel
}
