package test

import (
	"net"
	"sync"
	"sync/atomic"
	"testing"
)

// Test-only TCP relay. Blackholing discards bytes in both directions without
// closing either socket or saving bytes for recovery. Each leg stays open until
// its own peer closes it, so neither side can rely on the other's TCP FIN.
type blackholeRelay struct {
	address string
	drop    atomic.Bool
	dropped [2]atomic.Int64
}

func newBlackholeRelay(t *testing.T, upstream string) *blackholeRelay {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	relay := &blackholeRelay{address: listener.Addr().String()}
	var sockets []net.Conn
	var workers sync.WaitGroup
	accepted := make(chan struct{})
	go func() {
		defer close(accepted)
		for {
			down, err := listener.Accept()
			if err != nil {
				return
			}
			up, err := net.Dial("tcp", upstream)
			if err != nil {
				down.Close()
				return
			}
			sockets = append(sockets, down, up)
			for direction, source := range []net.Conn{down, up} {
				destination := up
				if direction == 1 {
					destination = down
				}
				workers.Add(1)
				go func() {
					defer workers.Done()
					buffer := make([]byte, 4096)
					for {
						n, err := source.Read(buffer)
						if n > 0 {
							if relay.drop.Load() {
								relay.dropped[direction].Add(int64(n))
							} else if _, writeErr := destination.Write(buffer[:n]); writeErr != nil {
								return
							}
						}
						if err != nil {
							return
						}
					}
				}()
			}
		}
	}()
	t.Cleanup(func() {
		listener.Close()
		<-accepted
		for _, socket := range sockets {
			socket.Close()
		}
		workers.Wait()
	})
	return relay
}
