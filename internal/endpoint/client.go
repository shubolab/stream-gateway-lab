// Package endpoint 负责两端的连接生命周期、工具调用和工具执行。
package endpoint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/coder/websocket"

	"stream-gateway-lab/internal/transport"
)

const (
	MaxRedials = 3
	retryDelay = 5 * time.Second
	peerWait   = 30 * time.Second
)

var errPeerWait = errors.New("peer unavailable for 30 seconds; endpoint stopped")

// A channel belongs to one socket AND one pair epoch. Never reuse it after loss.
type Channel struct {
	Conn  *websocket.Conn
	Epoch string
	Ctx   context.Context
	stop  context.CancelCauseFunc
}

type Client struct {
	Ctx                 context.Context
	Stop                context.CancelCauseFunc
	address, pair, role string
	token               string
	keepIdle            bool // 池中的执行端可以长期在线等待下一次任务。
	mu                  sync.Mutex
	conn                *websocket.Conn
	current             *Channel
	changed             chan struct{}
}

func NewClient(ctx context.Context, address, pair, role, token string) (*Client, error) {
	u, err := url.Parse(address)
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("pair", pair)
	q.Set("role", role)
	u.RawQuery = q.Encode()
	ctx, stop := context.WithCancelCause(ctx)
	return &Client{Ctx: ctx, Stop: stop, address: u.String(), pair: pair, role: role, token: token, changed: make(chan struct{})}, nil
}

func NewPoolClient(ctx context.Context, address, id, token string) (*Client, error) {
	c, err := NewClient(ctx, address, id, "executor", token)
	if err == nil {
		c.keepIdle = true
	}
	return c, err
}

// Only transport connections are retried. There is no outbound business queue.
func (c *Client) Run(handle func(*Channel, json.RawMessage) error) {
	for attempt := 0; attempt <= MaxRedials; attempt++ {
		if attempt > 0 {
			log.Printf("%s pair=%s redial=%d/%d in=%s", c.role, c.pair, attempt, MaxRedials, retryDelay)
			timer := time.NewTimer(retryDelay)
			select {
			case <-c.Ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
		if c.Ctx.Err() != nil {
			return
		}
		dialCtx, cancel := context.WithTimeout(c.Ctx, 5*time.Second)
		conn, response, err := websocket.Dial(dialCtx, c.address, &websocket.DialOptions{
			HTTPHeader: http.Header{"Authorization": {"Bearer " + c.token}},
		})
		cancel()
		if err == nil {
			conn.SetReadLimit(transport.MessageLimit)
			c.mu.Lock()
			c.conn = conn
			c.mu.Unlock()
			err = c.readConnection(conn, handle)
			c.mu.Lock()
			if c.conn == conn {
				c.invalidateLocked(fmt.Errorf("channel lost: %w", err))
				c.conn = nil
			}
			c.mu.Unlock()
			conn.CloseNow()
		}
		if c.Ctx.Err() != nil {
			return
		}
		log.Printf("%s pair=%s disconnected: %v", c.role, c.pair, err)
		if errors.Is(err, errPeerWait) || (response != nil && (response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden)) {
			c.Stop(err)
			return
		}
		if attempt == MaxRedials {
			c.Stop(fmt.Errorf("reconnect exhausted after %d redials: %w", MaxRedials, err))
			return
		}
	}
}

// Read uses the connection lifetime, never a tool deadline. A healthy socket
// stays connected while its peer is offline, but only waits a bounded time.
func (c *Client) readConnection(conn *websocket.Conn, handle func(*Channel, json.RawMessage) error) error {
	heartbeat := transport.HeartbeatContext
	if c.keepIdle {
		heartbeat = transport.PoolHeartbeatContext
	}
	ctx, cancel := heartbeat(c.Ctx, conn)
	defer cancel(context.Canceled)
	timer := time.AfterFunc(peerWait, func() {
		if !c.keepIdle {
			cancel(errPeerWait)
		}
	})
	defer timer.Stop()
	waiting := true
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			if cause := context.Cause(ctx); cause != nil {
				return cause
			}
			return err
		}
		var msg transport.Envelope
		if typ != websocket.MessageText || json.Unmarshal(data, &msg) != nil {
			return errors.New("invalid gateway message")
		}
		switch msg.Type {
		case transport.EnvelopeTypeState:
			c.mu.Lock()
			if c.current != nil && c.current.Epoch != msg.Epoch {
				c.invalidateLocked(errors.New("channel lost: peer disconnected or epoch changed"))
			}
			if msg.Epoch != "" && c.current == nil {
				channelCtx, stop := context.WithCancelCause(c.Ctx)
				c.current = &Channel{Conn: conn, Epoch: msg.Epoch, Ctx: channelCtx, stop: stop}
				c.notifyLocked()
				log.Printf("%s pair=%s ready epoch=%s", c.role, c.pair, msg.Epoch)
			}
			c.mu.Unlock()
			if msg.Epoch != "" {
				timer.Stop()
				waiting = false
			} else if !waiting {
				timer.Reset(peerWait)
				waiting = true
			}
		case transport.EnvelopeTypeData:
			s := c.Snapshot()
			if s != nil && s.Conn == conn && s.Epoch == msg.Epoch {
				if err := handle(s, msg.Body); err != nil {
					return err
				}
			}
		default:
			return errors.New("unknown gateway message")
		}
	}
}

func (c *Client) notifyLocked() {
	close(c.changed)
	c.changed = make(chan struct{})
}

func (c *Client) invalidateLocked(cause error) {
	if c.current != nil {
		log.Printf("%s pair=%s channel lost epoch=%s", c.role, c.pair, c.current.Epoch)
		c.current.stop(cause)
		c.current = nil
		c.notifyLocked()
	}
}

func (c *Client) Snapshot() *Channel {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.current == nil || c.current.Ctx.Err() != nil {
		return nil
	}
	return c.current
}

func (c *Client) WaitReady(ctx context.Context) error {
	for {
		c.mu.Lock()
		s, changed := c.current, c.changed
		c.mu.Unlock()
		if c.Ctx.Err() != nil {
			return context.Cause(c.Ctx)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if s != nil && s.Ctx.Err() == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.Ctx.Done():
			return context.Cause(c.Ctx)
		case <-changed:
		}
	}
}

func (c *Client) Disconnect(s *Channel, cause error) {
	c.mu.Lock()
	current := c.current == s && c.conn == s.Conn
	if current {
		c.invalidateLocked(cause)
	}
	c.mu.Unlock()
	if current {
		s.Conn.CloseNow()
	}
}

// The caller must supply its original channel, including for a late result.
func (c *Client) Send(ctx context.Context, s *Channel, body transport.ToolMessage) error {
	c.mu.Lock()
	valid := s != nil && c.current == s && c.conn == s.Conn && s.Ctx.Err() == nil
	c.mu.Unlock()
	if !valid {
		return errors.New("endpoint unavailable or channel expired")
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	data, err := json.Marshal(transport.Envelope{Type: transport.EnvelopeTypeData, Epoch: s.Epoch, Body: payload})
	if err != nil {
		return err
	}
	if len(data) > transport.MessageLimit {
		return errors.New("message exceeds 64 KiB")
	}
	if err := transport.WriteFrame(ctx, s.Conn, data); err != nil {
		c.Disconnect(s, err)
		return err
	}
	return nil
}
