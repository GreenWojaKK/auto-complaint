// Package worker는 모델을 쥔 Python 프로세스를 띄우고 요청을 주고받는다.
//
// worker는 d3-ultra-INT4 한 벌을 장치에 올려 판정 요청을 받는다(backbone.py). 한 줄에 JSON 하나씩 stdin으로 보내고
// stdout으로 답을 받는다. 요청은 여러 곳에서 함께 보낼 수 있고, 답은 끝나는 순서로 요청의 id를 달고 온다.
package worker

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// source는 worker가 실행하는 프로그램이다.
//
//go:embed backbone.py
var source string

const (
	maxReply  = 64 << 20
	logLines  = 30
	endingFor = 5 * time.Second
)

// Ready는 모델을 올린 worker가 처음 알리는 것이다.
type Ready struct {
	Model        string `json:"model"`
	Device       string `json:"device"`
	PackedLinear bool   `json:"packed_linear"` // int4 층이 packed 가중치에서 바로 곱한다.
}

// Client는 처음 쓸 때 worker를 띄운다.
type Client struct {
	Python string // worker를 실행할 Python.
	Models string // d3-ultra-INT4가 든 모델 폴더.
	Device string
	// Kernels는 장치 kernel 라이브러리다. 파일이 없으면 worker는 checkpoint의 층으로 돈다.
	Kernels string

	Info Ready

	start   sync.Once
	failed  error
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	logs    *tail
	mu      sync.Mutex
	next    int
	waiting map[int]chan reply
	ended   error
}

type reply struct {
	ID    int             `json:"id"`
	Reply json.RawMessage `json:"reply"`
	Error string          `json:"error"`
}

type tail struct {
	mu    sync.Mutex
	lines []string
}

func (t *tail) add(line string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lines = append(t.lines, line)
	if len(t.lines) > logLines {
		t.lines = t.lines[len(t.lines)-logLines:]
	}
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.Join(t.lines, "\n")
}

// FromEnvironment는 환경 변수로 정한 worker다. DLLIO_PYTHON(기본 python3), DLLIO_MODELS(기본 ~/models), DLLIO_DEVICE(기본 cuda:0),
// DLLIO_KERNELS(기본은 실행 파일 옆의 libdllio_kernels.so)를 읽는다.
func FromEnvironment() *Client {
	value := func(name, fallback string) string {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			return v
		}
		return fallback
	}
	home, _ := os.UserHomeDir()
	kernels := ""
	if exe, err := os.Executable(); err == nil {
		kernels = filepath.Join(filepath.Dir(exe), "libdllio_kernels.so")
	}
	return &Client{Python: value("DLLIO_PYTHON", "python3"), Models: value("DLLIO_MODELS", filepath.Join(home, "models")),
		Device: value("DLLIO_DEVICE", "cuda:0"), Kernels: value("DLLIO_KERNELS", kernels)}
}

// Start는 첫 요청을 기다리지 않고 지금 모델을 올린다.
func (c *Client) Start() error {
	c.start.Do(func() { c.failed = c.launch() })
	return c.failed
}

func (c *Client) launch() error {
	cmd := exec.Command(c.Python, "-c", source, c.Models, c.Device, c.Kernels)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("모델 worker를 띄우지 못했다: %w", err)
	}
	c.cmd, c.stdin, c.logs, c.waiting = cmd, stdin, &tail{}, map[int]chan reply{}
	go func() {
		lines := bufio.NewScanner(stderr)
		lines.Buffer(make([]byte, 64<<10), 4<<20)
		for lines.Scan() {
			c.logs.add(lines.Text())
		}
	}()
	lines := bufio.NewScanner(stdout)
	lines.Buffer(make([]byte, 64<<10), maxReply)
	var ready struct {
		Ready
		IsReady bool `json:"ready"`
	}
	if !lines.Scan() || json.Unmarshal(lines.Bytes(), &ready) != nil || !ready.IsReady {
		cmd.Process.Kill()
		cmd.Wait()
		return fmt.Errorf("모델 worker가 준비되지 못했다:\n%s", c.logs)
	}
	c.Info = ready.Ready
	go c.receive(lines)
	return nil
}

// receive는 답을 읽어 기다리는 요청에 넘긴다. worker가 끝나면 기다리던 요청을 모두 실패로 끝낸다.
func (c *Client) receive(lines *bufio.Scanner) {
	for lines.Scan() {
		var r reply
		if json.Unmarshal(lines.Bytes(), &r) != nil {
			continue
		}
		c.mu.Lock()
		waiter, ok := c.waiting[r.ID]
		delete(c.waiting, r.ID)
		c.mu.Unlock()
		if ok {
			waiter <- r
		}
	}
	c.mu.Lock()
	c.ended = fmt.Errorf("모델 worker가 끝났다:\n%s", c.logs)
	for id, waiter := range c.waiting {
		waiter <- reply{ID: id, Error: c.ended.Error()}
		delete(c.waiting, id)
	}
	c.mu.Unlock()
}

// Call은 요청 하나를 보내고 그 답을 기다린다. op는 decide이고, body는 그 요청의 JSON 본문이다.
// 답은 같은 요청을 HTTP로 보냈을 때의 응답 본문과 같은 JSON이다.
func (c *Client) Call(ctx context.Context, op string, body []byte) ([]byte, error) {
	if err := c.Start(); err != nil {
		return nil, err
	}
	waiter := make(chan reply, 1)
	c.mu.Lock()
	if c.ended != nil {
		c.mu.Unlock()
		return nil, c.ended
	}
	c.next++
	id := c.next
	line, err := json.Marshal(map[string]any{"id": id, "op": op, "request": json.RawMessage(body)})
	if err == nil {
		c.waiting[id] = waiter
		_, err = c.stdin.Write(append(line, '\n'))
		if err != nil {
			delete(c.waiting, id)
		}
	}
	c.mu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("모델 worker에 쓰지 못했다: %w\n%s", err, c.logs)
	}
	select {
	case r := <-waiter:
		if r.Error != "" {
			return nil, errors.New(r.Error)
		}
		return r.Reply, nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.waiting, id)
		c.mu.Unlock()
		return nil, ctx.Err()
	}
}

// Close는 worker를 끝낸다. worker는 stdin이 닫히면 멈춘다.
func (c *Client) Close() {
	if c.cmd == nil {
		return
	}
	c.stdin.Close()
	done := make(chan struct{})
	go func() {
		c.cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(endingFor):
		c.cmd.Process.Kill()
		<-done
	}
	c.cmd = nil
}
