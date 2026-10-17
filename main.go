package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

type binding struct {
	Pair             string `json:"pair"`
	BrainTokenEnv    string `json:"brain_token_env"`
	ExecutorTokenEnv string `json:"executor_token_env"`
}

func (b binding) token(role string) (string, error) {
	key := b.BrainTokenEnv
	if role == "executor" {
		key = b.ExecutorTokenEnv
	}
	token := os.Getenv(key)
	if token == "" {
		return "", fmt.Errorf("missing credential environment variable: %s", key)
	}
	return token, nil
}

type observation struct {
	Tool   string          `json:"tool"`
	Output json.RawMessage `json:"output"`
}

type brainInput struct {
	Message string `json:"message"`
	WaitMS  int    `json:"wait_ms"`
}

// The entire mock brain: no transport, addressing or connection logic here.
func runBrain(ctx context.Context, executor Executor, message string, waitMS int) ([]observation, error) {
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
	return []observation{{"echo", echo}, {"wait", wait}}, nil
}

// Each input line is an explicit new request from the experiment operator.
// A failed line is reported once; it is never retried after reconnection.
func runBrainInput(ctx context.Context, executor Executor) error {
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
			Results []observation `json:"results,omitempty"`
			Error   string        `json:"error,omitempty"`
		}
		if err == nil && (input.WaitMS < 0 || input.WaitMS > 30000) {
			err = errors.New("wait_ms must be between 0 and 30000")
		}
		if err == nil {
			result.Results, err = runBrain(ctx, executor, input.Message, input.WaitMS)
		}
		if err != nil {
			result.Error = err.Error()
		}
		if err := encoder.Encode(result); err != nil {
			return err
		}
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 || (args[0] != "gateway" && args[0] != "brain" && args[0] != "executor") {
		return errors.New("usage: stream-gateway-lab gateway|brain|executor [flags]")
	}
	role := args[0]
	flags := flag.NewFlagSet(role, flag.ContinueOnError)
	config := flags.String("config", "bindings.example.json", "static bindings configuration")
	mode, address := "remote", "ws://127.0.0.1:8787/ws"
	pair, message, waitMS := "A", "hello from brain", 200
	stdin := false
	if role == "brain" {
		flags.StringVar(&mode, "mode", mode, "local or remote")
		flags.StringVar(&message, "message", message, "echo marker for this experiment")
		flags.IntVar(&waitMS, "wait-ms", waitMS, "mock wait duration, 0 to 30000 ms")
		flags.BoolVar(&stdin, "stdin", false, "read explicit brain requests as JSON lines; no automatic call retry")
	}
	if role != "gateway" {
		flags.StringVar(&address, "url", address, "gateway WebSocket URL")
		flags.StringVar(&pair, "pair", pair, "binding to use")
	}
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 || (mode != "local" && mode != "remote") || waitMS < 0 || waitMS > 30000 {
		return errors.New("invalid arguments")
	}
	if role == "gateway" {
		log.Printf("role=gateway pid=%d", os.Getpid())
	} else {
		log.Printf("role=%s pid=%d mode=%s pair=%s", role, os.Getpid(), mode, pair)
	}
	var executor Executor = &LocalExecutor{}
	inputCtx := ctx
	if role != "brain" || mode != "local" {
		data, err := os.ReadFile(*config)
		if err != nil {
			return err
		}
		var bindings []binding
		if err := json.Unmarshal(data, &bindings); err != nil || len(bindings) == 0 {
			return errors.New("configuration must be a nonempty array of bindings")
		}
		if role == "gateway" {
			g, err := newGateway(bindings)
			if err != nil {
				return err
			}
			defer g.close()
			listener, err := net.Listen("tcp", "127.0.0.1:8787")
			if err != nil {
				return err
			}
			server := &http.Server{Handler: g, ReadHeaderTimeout: 5 * time.Second}
			stop := context.AfterFunc(ctx, func() { server.Close() })
			defer stop()
			log.Printf("gateway listening on %s bindings=%d", listener.Addr(), len(g.pairs))
			err = server.Serve(listener)
			if errors.Is(err, http.ErrServerClosed) {
				return nil
			}
			return err
		}
		var selected *binding
		for i := range bindings {
			if bindings[i].Pair == pair {
				if selected != nil {
					return fmt.Errorf("duplicate pair: %q", pair)
				}
				selected = &bindings[i]
			}
		}
		if selected == nil {
			return fmt.Errorf("unknown pair: %q", pair)
		}
		token, err := selected.token(role)
		if err != nil {
			return err
		}
		if role == "executor" {
			return runExecutor(ctx, address, pair, token)
		}
		remote, err := newRemoteExecutor(ctx, address, pair, token)
		if err != nil {
			return err
		}
		defer remote.client.stop(context.Canceled)
		executor = remote
		inputCtx = remote.client.ctx
	}
	if stdin {
		return runBrainInput(inputCtx, executor)
	}
	results, err := runBrain(ctx, executor, message, waitMS)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(results)
}

func main() {
	log.SetFlags(log.Ltime | log.Lmicroseconds)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		if ctx.Err() != nil && errors.Is(err, context.Canceled) {
			return
		}
		log.Printf("error: %v", err)
		os.Exit(1)
	}
}
