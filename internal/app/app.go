// Package app 负责三种角色的命令行入口和实验流程装配。
package app

import (
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

	"stream-gateway-lab/internal/endpoint"
	"stream-gateway-lab/internal/gateway"
)

func run(ctx context.Context, args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "gateway":
			return runGatewayCommand(ctx, args[1:])
		case "brain":
			return runBrainCommand(ctx, args[1:])
		case "executor":
			return runExecutorCommand(ctx, args[1:])
		}
	}
	return errors.New("usage: stream-gateway-lab gateway|brain|executor [flags]")
}

// Gateway：读取全部绑定，创建路由，启动 HTTP 服务。
func runGatewayCommand(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("gateway", flag.ContinueOnError)
	config := flags.String("config", "bindings.example.json", "static bindings configuration")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("invalid arguments")
	}
	log.Printf("role=gateway pid=%d", os.Getpid())

	bindings, err := loadBindings(*config)
	if err != nil {
		return err
	}
	g, err := gateway.New(bindings)
	if err != nil {
		return err
	}
	defer g.Close()

	listener, err := net.Listen("tcp", "127.0.0.1:8787")
	if err != nil {
		return err
	}
	server := &http.Server{Handler: g, ReadHeaderTimeout: 5 * time.Second}
	stop := context.AfterFunc(ctx, func() { server.Close() })
	defer stop()
	log.Printf("gateway listening on %s bindings=%d", listener.Addr(), len(g.Pairs))
	err = server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Brain：选择本地或远程 Executor，再运行一次请求或读取标准输入。
func runBrainCommand(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("brain", flag.ContinueOnError)
	config := flags.String("config", "bindings.example.json", "static bindings configuration")
	mode := flags.String("mode", "remote", "local or remote")
	address := flags.String("url", "ws://127.0.0.1:8787/ws", "gateway WebSocket URL")
	pair := flags.String("pair", "A", "binding to use")
	message := flags.String("message", "hello from brain", "echo marker for this experiment")
	waitMS := flags.Int("wait-ms", 200, "mock wait duration, 0 to 30000 ms")
	stdin := flags.Bool("stdin", false, "read explicit brain requests as JSON lines; no automatic call retry")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || (*mode != "local" && *mode != "remote") || *waitMS < 0 || *waitMS > 30000 {
		return errors.New("invalid arguments")
	}
	log.Printf("role=brain pid=%d mode=%s pair=%s", os.Getpid(), *mode, *pair)

	// 本地模式直接使用工具；只有远程模式需要配置、凭证和连接。
	var executor endpoint.Executor = &endpoint.LocalExecutor{}
	inputCtx := ctx
	if *mode == "remote" {
		token, err := loadBindingToken(*config, *pair, "brain")
		if err != nil {
			return err
		}
		remote, err := endpoint.NewRemoteExecutor(ctx, *address, *pair, token)
		if err != nil {
			return err
		}
		defer remote.Client.Stop(context.Canceled)
		executor = remote
		inputCtx = remote.Client.Ctx
	}

	if *stdin {
		return runBrainInput(inputCtx, executor)
	}
	results, err := RunBrain(ctx, executor, *message, *waitMS)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(results)
}

// Executor：读取本组执行端凭证，连接网关并处理工具调用。
func runExecutorCommand(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("executor", flag.ContinueOnError)
	config := flags.String("config", "bindings.example.json", "static bindings configuration")
	address := flags.String("url", "ws://127.0.0.1:8787/ws", "gateway WebSocket URL")
	pair := flags.String("pair", "A", "binding to use")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("invalid arguments")
	}
	log.Printf("role=executor pid=%d mode=remote pair=%s", os.Getpid(), *pair)

	token, err := loadBindingToken(*config, *pair, "executor")
	if err != nil {
		return err
	}
	return endpoint.RunExecutor(ctx, *address, *pair, token)
}

func loadBindings(path string) ([]gateway.Binding, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var bindings []gateway.Binding
	if err := json.Unmarshal(data, &bindings); err != nil || len(bindings) == 0 {
		return nil, errors.New("configuration must be a nonempty array of bindings")
	}
	return bindings, nil
}

func loadBindingToken(path, pair, role string) (string, error) {
	bindings, err := loadBindings(path)
	if err != nil {
		return "", err
	}
	var selected *gateway.Binding
	for i := range bindings {
		if bindings[i].Pair == pair {
			if selected != nil {
				return "", fmt.Errorf("duplicate pair: %q", pair)
			}
			selected = &bindings[i]
		}
	}
	if selected == nil {
		return "", fmt.Errorf("unknown pair: %q", pair)
	}
	return selected.Token(role)
}

// Main 是三个角色共用的 CLI 入口，负责退出信号和最终错误处理。
func Main() {
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
