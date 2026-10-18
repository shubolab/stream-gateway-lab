package gateway

import "errors"

// NewPoolRoute 给已注册的执行端保留稳定地址；Brain 凭证只在任务期间有效。
func NewPoolRoute(id, executorToken string) *PairRoute {
	return &PairRoute{pair: id, pool: true, tokens: map[string]string{"executor": executorToken}, Peers: make(map[string]*Peer)}
}

func (g *PairRoute) ExecutorOnline() bool {
	g.Mu.Lock()
	defer g.Mu.Unlock()
	p := g.Peers["executor"]
	return p != nil && p.Conn != nil
}

// ReserveBrain 必须在创建 Brain 进程之前调用；同一执行端只有一个任务名额。
func (g *PairRoute) ReserveBrain(token string) error {
	g.Mu.Lock()
	defer g.Mu.Unlock()
	if p := g.Peers["executor"]; p == nil || p.Conn == nil {
		return errors.New("executor is offline")
	}
	if g.tokens["brain"] != "" || g.Peers["brain"] != nil {
		return errors.New("executor is busy")
	}
	g.tokens["brain"] = token
	return nil
}

func (g *PairRoute) ReleaseBrain(token string) {
	g.Mu.Lock()
	if g.tokens["brain"] != token {
		g.Mu.Unlock()
		return
	}
	delete(g.tokens, "brain")
	p := g.Peers["brain"]
	if p != nil && p.Conn != nil {
		p.Conn.CloseNow()
	}
	g.Mu.Unlock()
	if p != nil {
		g.Remove("brain", p)
	}
}

func (g *PairRoute) Close() { g.close() }
