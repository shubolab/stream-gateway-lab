package endpoint

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

const fileLimit = 1 << 20

type machineArgs struct {
	Workdir    string `json:"workdir"`
	Path       string `json:"path"`
	Content    string `json:"content"`
	Old        string `json:"old_string"`
	New        string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all"`
	Offset     int    `json:"offset"`
	Limit      int    `json:"limit"`
	Pattern    string `json:"pattern"`
	Glob       string `json:"glob"`
	Command    string `json:"command"`
	TimeoutMS  int    `json:"timeout_ms"`
}

// 机器工具只在 Executor 进程内执行。workdir 来自本任务的 Brain 适配器，
// 文件和 shell 允许访问 Executor 用户可访问的全部路径，workdir 只决定相对路径和 cwd。
func executeMachineTool(ctx context.Context, name string, input json.RawMessage) (json.RawMessage, error) {
	var a machineArgs
	decoder := json.NewDecoder(bytes.NewReader(input))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&a); err != nil {
		return nil, errors.New("invalid machine tool arguments")
	}
	if !filepath.IsAbs(a.Workdir) || len(a.Workdir) > 4096 {
		return nil, errors.New("workdir must be an absolute directory on the Executor")
	}
	info, err := os.Stat(a.Workdir)
	if err != nil {
		return nil, fmt.Errorf("open workdir: %w", err)
	}
	if !info.IsDir() {
		return nil, errors.New("workdir must be a directory")
	}
	if a.Path == "" {
		a.Path = "."
	}
	if len(a.Path) > 4096 {
		return nil, errors.New("file path exceeds 4096 bytes")
	}
	if !filepath.IsAbs(a.Path) {
		a.Path = filepath.Join(a.Workdir, a.Path)
	}
	a.Path = filepath.Clean(a.Path)
	var result any
	switch name {
	case "system_info":
		host, _ := os.Hostname()
		result = map[string]any{"hostname": host, "os": runtime.GOOS, "arch": runtime.GOARCH, "workdir": a.Workdir, "tools_version": 1}
	case "fs_ls":
		var f *os.File
		f, err = os.Open(a.Path)
		if err == nil {
			defer f.Close()
			var entries []os.DirEntry
			entries, err = f.ReadDir(101)
			if errors.Is(err, io.EOF) {
				err = nil
			}
			items := make([]map[string]any, 0, min(len(entries), 100))
			for _, entry := range entries[:min(len(entries), 100)] {
				items = append(items, map[string]any{"path": path.Join(a.Path, entry.Name()), "is_dir": entry.IsDir()})
			}
			result = map[string]any{"entries": items, "truncated": len(entries) > 100}
		}
	case "fs_read":
		var content string
		content, err = readText(a.Path)
		if err == nil {
			if a.Offset < 0 || a.Limit < 0 {
				return nil, errors.New("offset and limit must be nonnegative")
			}
			if a.Limit == 0 || a.Limit > 200 {
				a.Limit = 200
			}
			lines := strings.SplitAfter(content, "\n")
			if lines[len(lines)-1] == "" {
				lines = lines[:len(lines)-1]
			}
			start, end, size := min(a.Offset, len(lines)), min(a.Offset, len(lines)), 0
			for end < len(lines) && end-start < a.Limit && size+len(lines[end]) <= 8192 {
				size += len(lines[end])
				end++
			}
			if start < len(lines) && end == start {
				return nil, errors.New("line exceeds 8 KiB read window; inspect with a bounded shell command")
			}
			result = map[string]any{"content": strings.Join(lines[start:end], ""), "offset": start, "next_offset": end, "total_lines": len(lines)}
		}
	case "fs_write":
		if len(a.Content) > 32<<10 {
			return nil, errors.New("write content exceeds 32 KiB")
		}
		err = writeText(a.Path, a.Content)
		result = map[string]any{"path": a.Path}
	case "fs_edit":
		var content string
		content, err = readText(a.Path)
		if err == nil {
			count := strings.Count(content, a.Old)
			if a.Old == "" || count == 0 || (!a.ReplaceAll && count != 1) {
				return nil, errors.New("old_string must be nonempty and match exactly once, or use replace_all")
			}
			content = strings.ReplaceAll(content, a.Old, a.New)
			if len(content) > fileLimit {
				return nil, errors.New("edited file exceeds 1 MiB")
			}
			err = writeText(a.Path, content)
			result = map[string]any{"path": a.Path, "occurrences": count}
		}
	case "fs_glob", "fs_grep":
		result, err = searchFiles(ctx, name, a)
	case "shell_exec":
		result, err = executeCommand(ctx, a)
	default:
		return nil, fmt.Errorf("unknown tool: %s", name)
	}
	if err != nil {
		return nil, err
	}
	data, err := json.Marshal(result)
	if len(data) > 60<<10 {
		return nil, errors.New("tool result exceeds 60 KiB; narrow the request")
	}
	return data, err
}

func readText(name string) (string, error) {
	f, err := os.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() > fileLimit {
		return "", errors.New("file must be a regular text file of at most 1 MiB")
	}
	data, err := io.ReadAll(io.LimitReader(f, fileLimit+1))
	if len(data) > fileLimit || bytes.IndexByte(data, 0) >= 0 {
		return "", errors.New("file is too large or contains binary data")
	}
	return string(data), err
}

func writeText(name, content string) error {
	if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|syscall.O_NONBLOCK, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("target must be a regular file")
	}
	if err := f.Truncate(0); err != nil {
		return err
	}
	if _, err := io.WriteString(f, content); err != nil {
		return err
	}
	return f.Sync()
}

// ** 匹配任意层目录，其余片段使用 Go 标准 glob 规则。
func globMatch(pattern, name string) bool {
	p, n := strings.Split(pattern, "/"), strings.Split(name, "/")
	var match func([]string, []string) bool
	match = func(p, n []string) bool {
		if len(p) == 0 {
			return len(n) == 0
		}
		if p[0] == "**" {
			return match(p[1:], n) || (len(n) > 0 && match(p, n[1:]))
		}
		if len(n) == 0 {
			return false
		}
		ok, _ := path.Match(p[0], n[0])
		return ok && match(p[1:], n[1:])
	}
	return match(p, n)
}

func searchFiles(ctx context.Context, name string, a machineArgs) (any, error) {
	if len(a.Pattern) > 512 || len(a.Glob) > 512 || (strings.Count(a.Pattern, "**") > 4 || strings.Count(a.Glob, "**") > 4) {
		return nil, errors.New("search pattern too complex")
	}
	items := []map[string]any{}
	visited, size, truncated := 0, 0, false
	err := filepath.WalkDir(a.Path, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		visited++
		if visited > 2000 || len(items) >= 100 || size > 8192 {
			truncated = true
			return fs.SkipAll
		}
		if name == "fs_glob" {
			rel := strings.TrimPrefix(strings.TrimPrefix(p, strings.TrimSuffix(a.Path, "/")+"/"), "./")
			if globMatch(strings.TrimPrefix(a.Pattern, "/"), rel) {
				items = append(items, map[string]any{"path": p, "is_dir": d.IsDir()})
				size += len(p)
			}
			return nil
		}
		if !d.Type().IsRegular() || (a.Glob != "" && !globMatch(a.Glob, p) && !globMatch(a.Glob, path.Base(p))) {
			return nil
		}
		text, readErr := readText(p)
		if readErr != nil {
			return nil // 搜索跳过过大、非文本或不可读文件。
		}
		scanner := bufio.NewScanner(strings.NewReader(text))
		scanner.Buffer(make([]byte, 4096), fileLimit+1)
		for line := 1; scanner.Scan(); line++ {
			t := scanner.Text()
			if strings.Contains(t, a.Pattern) {
				if len(t) > 2048 {
					t = t[:2048]
					truncated = true
				}
				items = append(items, map[string]any{"path": p, "line": line, "text": t})
				size += len(p) + len(t)
				if len(items) >= 100 || size > 8192 {
					truncated = true
					return fs.SkipAll
				}
			}
		}
		return scanner.Err()
	})
	return map[string]any{"items": items, "truncated": truncated}, err
}

type cappedBuffer struct {
	buffer    bytes.Buffer
	truncated bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if len(p) > 4096-b.buffer.Len() {
		p, b.truncated = p[:4096-b.buffer.Len()], true
	}
	_, _ = b.buffer.Write(p)
	return n, nil
}

func (b *cappedBuffer) String() string { return b.buffer.String() }

// 单次 shell 最长 12 秒，给 15 秒 RPC 留出清理余量。取消整个进程组，
// 包括 shell 创建的子进程；即使命令退出，也清理它留下的后台进程。
func executeCommand(ctx context.Context, a machineArgs) (any, error) {
	if a.Command == "" || len(a.Command) > 8192 || a.TimeoutMS < 0 || a.TimeoutMS > 12000 {
		return nil, errors.New("command required (up to 8 KiB); timeout_ms must be 0..12000")
	}
	if a.TimeoutMS == 0 {
		a.TimeoutMS = 10000
	}
	commandCtx, cancel := context.WithTimeout(ctx, time.Duration(a.TimeoutMS)*time.Millisecond)
	defer cancel()
	cmd := exec.CommandContext(commandCtx, "/bin/sh", "-c", a.Command)
	cmd.Dir = a.Workdir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 500 * time.Millisecond
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	stdout, stderr := &cappedBuffer{}, &cappedBuffer{}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	started := time.Now()
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	err := cmd.Wait()
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) && !errors.Is(err, exec.ErrWaitDelay) {
			return nil, err
		}
	}
	host, _ := os.Hostname()
	return map[string]any{
		"stdout": stdout.String(), "stderr": stderr.String(), "exit_code": cmd.ProcessState.ExitCode(),
		"timed_out": commandCtx.Err() != nil, "truncated": stdout.truncated || stderr.truncated || errors.Is(err, exec.ErrWaitDelay),
		"duration_ms": time.Since(started).Milliseconds(), "hostname": host, "workdir": a.Workdir,
	}, nil
}
