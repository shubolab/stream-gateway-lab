package main

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
)

const (
	maxRedials = 3
	retryDelay = 5 * time.Second
	peerWait   = 30 * time.Second
)

var errPeerWait = errors.New("peer unavailable for 30 seconds; endpoint stopped")

// A channel belongs to one socket AND one pair epoch. Never reuse it after loss.
type channel struct {
	conn  *websocket.Conn
	epoch string
	ctx   context.Context
	stop  context.CancelCauseFunc
}

type client struct {
	ctx                 context.Context
	stop                context.CancelCauseFunc
	address, pair, role string
	token               string
	mu                  sync.Mutex
	conn                *websocket.Conn
	current             *channel
	changed             chan struct{}
}

func newClient(ctx context.Context, address, pair, role, token string) (*client, error) {
	u, err := url.Parse(address)
	if err != nil {
		return nil, err
	}
	q := u.Query()
	q.Set("pair", pair)
	q.Set("role", role)
	u.RawQuery = q.Encode()
	ctx, stop := context.WithCancelCause(ctx)
	return &client{ctx: ctx, stop: stop, address: u.String(), pair: pair, role: role, token: token, changed: make(chan struct{})}, nil
}

// Only transport connections are retried. There is no outbound business queue.
func (c *client) run(handle func(*channel, json.RawMessage) error) {
	for attempt := 0; attempt <= maxRedials; attempt++ {
		if attempt > 0 {
			log.Printf("%s pair=%s redial=%d/%d in=%s", c.role, c.pair, attempt, maxRedials, retryDelay)
			timer := time.NewTimer(retryDelay)
			select {
			case <-c.ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
		if c.ctx.Err() != nil {
			return
		}
		dialCtx, cancel := context.WithTimeout(c.ctx, 5*time.Second)
		conn, response, err := websocket.Dial(dialCtx, c.address, &websocket.DialOptions{
			HTTPHeader: http.Header{"Authorization": {"Bearer " + c.token}},
		})
		cancel()
		if err == nil {
			conn.SetReadLimit(messageLimit)
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
		if c.ctx.Err() != nil {
			return
		}
		log.Printf("%s pair=%s disconnected: %v", c.role, c.pair, err)
		if errors.Is(err, errPeerWait) || (response != nil && (response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden)) {
			c.stop(err)
			return
		}
		if attempt == maxRedials {
			c.stop(fmt.Errorf("reconnect exhausted after %d redials: %w", maxRedials, err))
			return
		}
	}
}

// Read uses the connection lifetime, never a tool deadline. A healthy socket
// stays connected while its peer is offline, but only waits a bounded time.
func (c *client) readConnection(conn *websocket.Conn, handle func(*channel, json.RawMessage) error) error {
	ctx, cancel := heartbeatContext(c.ctx, conn)
	defer cancel(context.Canceled)
	timer := time.AfterFunc(peerWait, func() { cancel(errPeerWait) })
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
		var msg envelope
		if typ != websocket.MessageText || json.Unmarshal(data, &msg) != nil {
			return errors.New("invalid gateway message")
		}
		switch msg.Type {
		case "state":
			c.mu.Lock()
			if c.current != nil && c.current.epoch != msg.Epoch {
				c.invalidateLocked(errors.New("channel lost: peer disconnected or epoch changed"))
			}
			if msg.Epoch != "" && c.current == nil {
				channelCtx, stop := context.WithCancelCause(c.ctx)
				c.current = &channel{conn: conn, epoch: msg.Epoch, ctx: channelCtx, stop: stop}
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
		case "data":
			s := c.snapshot()
			if s != nil && s.conn == conn && s.epoch == msg.Epoch {
				if err := handle(s, msg.Body); err != nil {
					return err
				}
			}
		default:
			return errors.New("unknown gateway message")
		}
	}
}

func (c *client) notifyLocked() {
	close(c.changed)
	c.changed = make(chan struct{})
}

func (c *client) invalidateLocked(cause error) {
	if c.current != nil {
		log.Printf("%s pair=%s channel lost epoch=%s", c.role, c.pair, c.current.epoch)
		c.current.stop(cause)
		c.current = nil
		c.notifyLocked()
	}
}

func (c *client) snapshot() *channel {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.current == nil || c.current.ctx.Err() != nil {
		return nil
	}
	return c.current
}

func (c *client) waitReady(ctx context.Context) error {
	for {
		c.mu.Lock()
		s, changed := c.current, c.changed
		c.mu.Unlock()
		if c.ctx.Err() != nil {
			return context.Cause(c.ctx)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if s != nil && s.ctx.Err() == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.ctx.Done():
			return context.Cause(c.ctx)
		case <-changed:
		}
	}
}

func (c *client) disconnect(s *channel, cause error) {
	c.mu.Lock()
	current := c.current == s && c.conn == s.conn
	if current {
		c.invalidateLocked(cause)
	}
	c.mu.Unlock()
	if current {
		s.conn.CloseNow()
	}
}

// The caller must supply its original channel, including for a late result.
func (c *client) send(ctx context.Context, s *channel, body toolMessage) error {
	c.mu.Lock()
	valid := s != nil && c.current == s && c.conn == s.conn && s.ctx.Err() == nil
	c.mu.Unlock()
	if !valid {
		return errors.New("endpoint unavailable or channel expired")
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	data, err := json.Marshal(envelope{Type: "data", Epoch: s.epoch, Body: payload})
	if err != nil {
		return err
	}
	if len(data) > messageLimit {
		return errors.New("message exceeds 64 KiB")
	}
	if err := writeFrame(ctx, s.conn, data); err != nil {
		c.disconnect(s, err)
		return err
	}
	return nil
}
