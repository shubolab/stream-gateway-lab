package transport

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/coder/websocket"
)

const (
	HeartbeatInterval = time.Second
	HeartbeatTimeout  = 2 * time.Second
)

var ErrHeartbeat = errors.New("heartbeat failed")

// Watch exactly one socket, including while waiting for its paired endpoint.
// The caller must keep reading: websocket.Read handles Ping/Pong control frames.
func HeartbeatContext(parent context.Context, conn *websocket.Conn) (context.Context, context.CancelCauseFunc) {
	ctx, cancel := context.WithCancelCause(parent)
	go func() {
		ticker := time.NewTicker(HeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			pingCtx, stop := context.WithTimeout(ctx, HeartbeatTimeout)
			err := conn.Ping(pingCtx)
			stop()
			if err != nil {
				if ctx.Err() == nil {
					cancel(fmt.Errorf("%w: %v", ErrHeartbeat, err))
					// 心跳失败后不再使用这条连接，直接将其关闭。
					// 读取连接的代码会因此返回错误；客户端随后清理旧通道，并按原有规则尝试重连。
					conn.CloseNow()
				}
				return
			}
		}
	}()
	return ctx, cancel
}
