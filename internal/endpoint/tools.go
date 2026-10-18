package endpoint

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

func ExecuteTool(ctx context.Context, name string, input json.RawMessage) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !json.Valid(input) || len(input) > 60<<10 {
		return nil, fmt.Errorf("tool input must be valid JSON, at most 60 KiB")
	}
	switch name {
	case "echo":
		return append(json.RawMessage(nil), input...), nil
	case "wait":
		var args struct {
			MS int `json:"ms"`
		}
		if err := json.Unmarshal(input, &args); err != nil || args.MS < 0 || args.MS > 30000 {
			return nil, fmt.Errorf("wait requires ms between 0 and 30000")
		}
		timer := time.NewTimer(time.Duration(args.MS) * time.Millisecond)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timer.C:
			return json.Marshal(struct {
				WaitedMS int `json:"waited_ms"`
			}{args.MS})
		}
	case "system_info", "fs_ls", "fs_read", "fs_write", "fs_edit", "fs_glob", "fs_grep", "shell_exec":
		return executeMachineTool(ctx, name, input)
	default:
		return nil, fmt.Errorf("unknown tool: %s", name)
	}
}
