package hgctl

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"
)

type codexRPCLine struct {
	content []byte
	err     error
}

type codexRPC struct {
	ctx      context.Context
	cancel   context.CancelFunc
	stdin    io.WriteCloser
	lines    chan codexRPCLine
	wait     chan error
	process  *os.Process
	closeOne sync.Once
}

func startCodexRPC(parent context.Context, codexHome string) (*codexRPC, error) {
	ctx, cancel := context.WithTimeout(parent, codexRPCSessionLimit)
	cmd := exec.CommandContext(ctx, "codex", "app-server", "--stdio")
	cmd.Env = append(os.Environ(), "CODEX_HOME="+codexHome)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, err
	}
	rpc := &codexRPC{
		ctx: ctx, cancel: cancel, stdin: stdin, lines: make(chan codexRPCLine, 8),
		wait: make(chan error, 1), process: cmd.Process,
	}
	go func() {
		_, _ = io.Copy(io.Discard, stderr)
	}()
	go func() {
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 64*1024), codexRPCLineLimit+2)
		for scanner.Scan() {
			if len(scanner.Bytes()) > codexRPCLineLimit {
				select {
				case rpc.lines <- codexRPCLine{err: errors.New("Codex app-server line exceeds the limit")}:
				case <-ctx.Done():
				}
				return
			}
			line := append([]byte(nil), scanner.Bytes()...)
			select {
			case rpc.lines <- codexRPCLine{content: line}:
			case <-ctx.Done():
				return
			}
		}
		err := scanner.Err()
		if err == nil {
			err = io.EOF
		}
		select {
		case rpc.lines <- codexRPCLine{err: err}:
		case <-ctx.Done():
		}
	}()
	go func() { rpc.wait <- cmd.Wait() }()
	return rpc, nil
}

func (rpc *codexRPC) Notify(message any) error {
	return rpc.send(message)
}

func (rpc *codexRPC) Call(id int, message any) (json.RawMessage, error) {
	if err := rpc.send(message); err != nil {
		return nil, err
	}
	timer := time.NewTimer(codexRPCResponseWait)
	defer timer.Stop()
	for {
		select {
		case line := <-rpc.lines:
			if line.err != nil {
				return nil, fmt.Errorf("Codex app-server stdout: %w", line.err)
			}
			result, matched, err := decodeCodexRPCResponse(line.content, id)
			if err != nil {
				return nil, err
			}
			if matched {
				return result, nil
			}
		case <-timer.C:
			return nil, fmt.Errorf("Codex app-server response %d timed out", id)
		case <-rpc.ctx.Done():
			return nil, fmt.Errorf("Codex app-server stopped: %w", rpc.ctx.Err())
		}
	}
}

func (rpc *codexRPC) send(message any) error {
	content, err := json.Marshal(message)
	if err != nil {
		return err
	}
	if len(content) > codexRPCLineLimit {
		return errors.New("Codex app-server request exceeds the line limit")
	}
	content = append(content, '\n')
	_, err = rpc.stdin.Write(content)
	return err
}

func decodeCodexRPCResponse(content []byte, wantID int) (json.RawMessage, bool, error) {
	var message struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := decodeExternalJSON(content, &message); err != nil {
		return nil, false, fmt.Errorf("malformed Codex app-server message: %w", err)
	}
	if len(message.ID) == 0 {
		if message.Method == "" {
			return nil, false, errors.New("Codex app-server emitted a message without id or method")
		}
		return nil, false, nil
	}
	var gotID int
	if err := json.Unmarshal(message.ID, &gotID); err != nil || gotID != wantID {
		return nil, false, errors.New("Codex app-server returned an unexpected response id")
	}
	if len(message.Error) != 0 && !bytes.Equal(message.Error, []byte("null")) {
		return nil, false, errors.New("Codex app-server returned an error")
	}
	if len(message.Result) == 0 || bytes.Equal(message.Result, []byte("null")) {
		return nil, false, errors.New("Codex app-server response has no result")
	}
	return message.Result, true, nil
}

func decodeExternalJSON(content []byte, dst any) error {
	decoder := json.NewDecoder(bytes.NewReader(content))
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func (rpc *codexRPC) Close() {
	rpc.closeOne.Do(func() {
		_ = rpc.stdin.Close()
		rpc.cancel()
		select {
		case <-rpc.wait:
		case <-time.After(5 * time.Second):
			if rpc.process != nil {
				_ = rpc.process.Kill()
			}
			select {
			case <-rpc.wait:
			case <-time.After(5 * time.Second):
			}
		}
	})
}
