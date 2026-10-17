package main

import (
	"crypto/rand"
	"encoding/json"
	"time"
)

const (
	messageLimit = 64 << 10
	callTimeout  = 15 * time.Second
	writeTimeout = 2 * time.Second
)

// The gateway reads only this envelope; Body belongs to the two endpoints.
type envelope struct {
	Type  string          `json:"type"`
	Epoch string          `json:"epoch"`
	Body  json.RawMessage `json:"body,omitempty"`
}

type toolMessage struct {
	Kind      string          `json:"kind"`
	ID        string          `json:"id"`
	Tool      string          `json:"tool,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	TimeoutMS int64           `json:"timeout_ms,omitempty"`
	Output    json.RawMessage `json:"output,omitempty"`
	Error     string          `json:"error,omitempty"`
}

func newID() string { return rand.Text() }
