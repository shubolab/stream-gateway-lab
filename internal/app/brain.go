package app

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"

	"stream-gateway-lab/internal/endpoint"
)

type Observation struct {
	Tool   string          `json:"tool"`
	Output json.RawMessage `json:"output"`
}

type brainInput struct {
	Message string `json:"message"`
	WaitMS  int    `json:"wait_ms"`
}

// The entire mock brain: no transport, addressing or connection logic here.
func RunBrain(ctx context.Context, executor endpoint.Executor, message string, waitMS int) ([]Observation, error) {
	input, _ := json.Marshal(brainInput{message, waitMS})
	echo, err := executor.Call(ctx, "echo", input)
	if err != nil {
		return nil, err
	}
	// Make the second decision from the first tool's result in both modes.
	var decision struct {
		WaitMS int `json:"wait_ms"`
	}
	if err := json.Unmarshal(echo, &decision); err != nil {
		return nil, err
	}
	input, _ = json.Marshal(map[string]int{"ms": decision.WaitMS})
	wait, err := executor.Call(ctx, "wait", input)
	if err != nil {
		return nil, err
	}
	return []Observation{{"echo", echo}, {"wait", wait}}, nil
}

// Each input line is an explicit new request from the experiment operator.
// A failed line is reported once; it is never retried after reconnection.
func runBrainInput(ctx context.Context, executor endpoint.Executor) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 4096), 60<<10)
	// Closing inherited blocking stdin may not interrupt Scan. This CLI owns
	// one reader goroutine; process exit releases any still-blocked stdin read.
	lines := make(chan string)
	go func() {
		defer close(lines)
		for scanner.Scan() {
			select {
			case lines <- scanner.Text(): // Copy before Scan reuses its buffer.
			case <-ctx.Done():
				return
			}
		}
	}()
	encoder := json.NewEncoder(os.Stdout)
	for {
		var line string
		var ok bool
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case line, ok = <-lines:
		}
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		if !ok {
			return scanner.Err()
		}
		var input brainInput
		err := json.Unmarshal([]byte(line), &input)
		var result struct {
			Results []Observation `json:"results,omitempty"`
			Error   string        `json:"error,omitempty"`
		}
		if err == nil && (input.WaitMS < 0 || input.WaitMS > 30000) {
			err = errors.New("wait_ms must be between 0 and 30000")
		}
		if err == nil {
			result.Results, err = RunBrain(ctx, executor, input.Message, input.WaitMS)
		}
		if err != nil {
			result.Error = err.Error()
		}
		if err := encoder.Encode(result); err != nil {
			return err
		}
	}
}
