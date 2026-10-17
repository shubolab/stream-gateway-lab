package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"

	"github.com/coder/websocket"
)

type peer struct{ conn *websocket.Conn }

// The route table is built once at startup and never mutated while serving.
type gateway struct {
	pairs map[string]*pairRoute
}

// Each pair owns its connections, epoch and lock; no task or result state.
type pairRoute struct {
	mu     sync.Mutex
	pair   string
	tokens map[string]string
	peers  map[string]*peer
	epoch  string
}

func newGateway(bindings []binding) (*gateway, error) {
	g := &gateway{pairs: make(map[string]*pairRoute)}
	usedTokens := make(map[string]bool)
	for _, b := range bindings {
		if b.Pair == "" || g.pairs[b.Pair] != nil {
			return nil, fmt.Errorf("empty or duplicate pair: %q", b.Pair)
		}
		route := &pairRoute{pair: b.Pair, tokens: make(map[string]string), peers: make(map[string]*peer)}
		for _, role := range []string{"brain", "executor"} {
			token, err := b.token(role)
			if err != nil {
				return nil, err
			}
			if usedTokens[token] {
				return nil, fmt.Errorf("credentials must differ for every pair and role")
			}
			usedTokens[token] = true
			route.tokens[role] = token
		}
		g.pairs[b.Pair] = route
	}
	if len(g.pairs) == 0 {
		return nil, fmt.Errorf("at least one binding is required")
	}
	return g, nil
}

func (g *gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	route := g.pairs[r.URL.Query().Get("pair")]
	if route == nil {
		http.Error(w, "invalid endpoint credentials", http.StatusUnauthorized)
		return
	}
	route.ServeHTTP(w, r)
}

func (g *gateway) close() {
	for _, route := range g.pairs {
		route.close()
	}
}

func opposite(role string) string {
	if role == "brain" {
		return "executor"
	}
	return "brain"
}

func writeFrame(ctx context.Context, conn *websocket.Conn, data []byte) error {
	ctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return conn.Write(ctx, websocket.MessageText, data)
}

// ServeHTTP 接收 Brain（发起工具调用的一端）或 Executor（执行工具的一端）的连接，
// 并在同一组绑定的两端之间转发 WebSocket 消息，实现 http.Handler 接口。
//
// 客户端通过 /ws?pair=<绑定名称>&role=brain 或 role=executor 接入，
// 并在 Authorization 请求头中携带对应角色的 Bearer 凭证。
// w 用于返回 HTTP 错误或完成 WebSocket 握手，r 提供请求参数、凭证和上下文。
// 升级成功后，本函数会一直读取该连接，直到读取、协议校验或写入出错才返回；
// 因此，一次 ServeHTTP 调用管理的是一条连接的生命周期，而不是一次工具调用。
//
// 处理流程：
//  1. 校验路径、绑定名称、角色和凭证；不匹配时返回 HTTP 401。
//  2. 为角色预留连接位置，阻止同一角色重复接入；重复时返回 HTTP 409。
//  3. 将 HTTP 连接升级为 WebSocket，并限制单条入站消息的大小。
//  4. 只有一端连接时，发送 epoch 为空的 state 消息，表示仍在等待对端；
//     两端都连接后，生成本次会话的 epoch，依次向 Executor、Brain 发送就绪状态。
//  5. 持续读取 data 消息，确认它属于当前会话后，将原始消息转发给对端。
//  6. 函数退出时关闭当前连接并移除角色；作废 epoch，通知仍在线的对端等待恢复。
//
// epoch 是一次配对会话的标识：两端发送 data 时必须带上网关下发的当前值，
// 这样旧会话的消息就不会被转发到当前会话。网关只检查 envelope 外层字段，
// 不解释 Body 中的工具名称、参数或结果，也不缓存、排队或重放消息。
//
// 每组绑定使用独立的 pairRoute。本函数只处理这一组的端点，g.mu 保护其 peers、
// peer.conn 和 epoch。当前实现也在持锁期间写消息，避免转发途中配对状态被修改；
// 写入通过 writeFrame 限时，读取等待消息时则不持锁，让另一端可以接入或转发。
func (g *pairRoute) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 1. 按角色取出预设凭证。known 为 false 表示请求使用了未知角色。
	// 任意一项校验失败都返回同一个 401；凭证使用 ConstantTimeCompare 比较。
	role := r.URL.Query().Get("role")
	token, known := g.tokens[role]
	if r.URL.Path != "/ws" || r.URL.Query().Get("pair") != g.pair || !known ||
		subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) != 1 {
		http.Error(w, "invalid endpoint credentials", http.StatusUnauthorized)
		return
	}
	// 2. 先占位再握手：p.conn 暂时为空，但该角色已经被占用。
	// 检查和占位必须在同一把锁内完成，否则并发请求可能同时通过重复连接检查。
	p := &peer{}
	g.mu.Lock()
	if g.peers[role] != nil {
		g.mu.Unlock()
		http.Error(w, "endpoint already connected", http.StatusConflict)
		return
	}
	g.peers[role] = p
	g.mu.Unlock()
	// 从占位成功起就安排清理，即使后续 WebSocket 握手失败，也会释放占位。
	// remove 会核对 p 是否仍是该角色的当前记录，避免误删其他连接。
	defer g.remove(role, p)

	// 3. 完成 WebSocket 握手；Accept 失败时会自行向客户端写入 HTTP 错误响应。
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	// defer 按后注册、先执行的顺序运行：退出时先关闭连接，再执行上面的 remove。
	defer conn.CloseNow()
	// messageLimit 当前为 64 KiB；超过限制会使后续读取失败，进而退出本函数。
	conn.SetReadLimit(messageLimit)
	ctx, cancel := heartbeatContext(r.Context(), conn)
	defer cancel(context.Canceled)

	// 4. 保存握手成功的连接，再检查对端是否也已完成握手。
	g.mu.Lock()
	p.conn = conn
	log.Printf("gateway pair=%s role=%s connected", g.pair, role)
	other := g.peers[opposite(role)]
	// other 非空仅表示对端已占位；other.conn 非空才表示对端可以收发消息。
	if other != nil && other.conn != nil {
		g.epoch = newID()
		// 必须先给 Executor 发状态，再通知 Brain；Brain 收到非空 epoch 后就能发起调用。
		// 结合下方持锁转发，这保证 Executor 的连接先收到状态消息，再收到调用消息。
		for _, target := range []string{"executor", "brain"} {
			state, _ := json.Marshal(envelope{Type: "state", Epoch: g.epoch})
			if err = writeFrame(ctx, g.peers[target].conn, state); err != nil {
				// 任一端收不到就绪状态，就关闭整对连接并清空 epoch。
				// closeLocked 要求调用者已经持有 g.mu。
				g.closeLocked()
				break
			}
		}
		if err == nil {
			log.Printf("gateway pair=%s ready", g.pair)
		}
	} else {
		// 对端尚未就绪：只通知当前端等待，不生成 epoch。
		// 对端以后接入时，会由它的 ServeHTTP 调用向双方发送就绪状态。
		state, _ := json.Marshal(envelope{Type: "state"})
		err = writeFrame(ctx, conn, state)
	}
	g.mu.Unlock()
	if err != nil {
		return
	}

	// 5. 每个端点各自运行一个读取循环：Brain 的消息发给 Executor，反向亦然。
	for {
		// Read 会等待下一条消息；连接关闭、上下文取消或读取失败时退出并触发清理。
		typ, data, err := conn.Read(ctx)
		if err != nil {
			if cause := context.Cause(ctx); cause != nil {
				log.Printf("gateway pair=%s role=%s disconnected: %v", g.pair, role, cause)
			}
			return
		}
		var msg envelope
		// 客户端只能发送文本格式的 JSON data 消息，且必须带有 Body 字段。
		// 这里只检查 Body 的原始 JSON 字节是否为空，不校验内部工具协议。
		// 格式或类型不合法时直接结束连接，不发送业务层错误消息。
		if typ != websocket.MessageText || json.Unmarshal(data, &msg) != nil || msg.Type != "data" || len(msg.Body) == 0 {
			return
		}
		g.mu.Lock()
		other = g.peers[opposite(role)]
		// 转发需同时满足：自己仍是当前连接、会话已就绪、消息 epoch 匹配、对端已连接。
		// 格式合法但不满足这些条件的消息会被直接丢弃，循环继续等待下一条消息。
		if g.peers[role] == p && g.epoch != "" && msg.Epoch == g.epoch && other != nil && other.conn != nil {
			// 原样发送 data 字节，不重新组装消息。writeFrame 的单次写入超时当前为 2 秒。
			err = writeFrame(ctx, other.conn, data)
			if err != nil {
				// 对端写入失败意味着本次会话无法继续，关闭双方连接。
				g.closeLocked()
			} else {
				log.Printf("gateway pair=%s forward %s->%s bytes=%d", g.pair, role, opposite(role), len(data))
			}
		}
		g.mu.Unlock()
		if err != nil {
			return
		}
	}
}

func (g *pairRoute) remove(role string, p *peer) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.peers[role] != p {
		return
	}
	delete(g.peers, role)
	if g.epoch != "" {
		g.epoch = ""
		if other := g.peers[opposite(role)]; other != nil && other.conn != nil {
			state, _ := json.Marshal(envelope{Type: "state"})
			if err := writeFrame(context.Background(), other.conn, state); err != nil {
				other.conn.CloseNow()
			}
		}
		log.Printf("gateway pair=%s role=%s offline; epoch invalidated", g.pair, role)
	}
}

func (g *pairRoute) closeLocked() {
	g.epoch = ""
	for _, p := range g.peers {
		if p.conn != nil {
			p.conn.CloseNow()
		}
	}
}

func (g *pairRoute) close() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.closeLocked()
}
