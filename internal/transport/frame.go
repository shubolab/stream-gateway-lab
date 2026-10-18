package transport

import (
	"context"

	"github.com/coder/websocket"
)

func WriteFrame(ctx context.Context, conn *websocket.Conn, data []byte) error {
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return conn.Write(ctx, websocket.MessageText, data)
}
