// Package transport 定义共用消息格式、心跳检测和 WebSocket 写入辅助函数。
package transport

import (
	"crypto/rand"
	"encoding/json"
	"time"
)

const (
	MessageLimit = 64 << 10
	CallTimeout  = 15 * time.Second
	writeTimeout = 2 * time.Second
)

const (
	EnvelopeTypeState = "state"
	EnvelopeTypeData  = "data"
)

// 网关通过 Type 判断消息类型，通过 Epoch 校验消息是否属于当前会话。
// Body 装的是工具调用或执行结果，由 Brain 和 Executor 处理；网关只转发，不解析其中的业务内容。
type Envelope struct {
	Type  string          `json:"type"` // 消息类型：state 表示配对状态，data 表示工具请求或结果。
	Epoch string          `json:"epoch"`
	Body  json.RawMessage `json:"body,omitempty"`
}

type ToolMessage struct {
	Kind      string          `json:"kind"`
	ID        string          `json:"id"`
	Tool      string          `json:"tool,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	TimeoutMS int64           `json:"timeout_ms,omitempty"`
	Output    json.RawMessage `json:"output,omitempty"`
	Error     string          `json:"error,omitempty"`
}

func NewID() string { return rand.Text() }
