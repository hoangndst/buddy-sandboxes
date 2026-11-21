package client

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// PortForwardOptions holds options for port forwarding.
type PortForwardOptions struct {
	LocalPort  int
	RemotePort int
	ReadyChan  chan struct{} // Closed when local listener is bound and listening
}

// PortForward binds a local TCP port and tunnels accepted connections to the sandbox port via the gateway.
func (c *Client) PortForward(ctx context.Context, id string, opts PortForwardOptions) error {
	addr := fmt.Sprintf("127.0.0.1:%d", opts.LocalPort)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("failed to bind local port %d: %w", opts.LocalPort, err)
	}
	defer listener.Close()

	if opts.ReadyChan != nil {
		close(opts.ReadyChan)
	}

	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()

	for {
		clientConn, err := listener.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return nil
			default:
				return fmt.Errorf("local listener accept failed: %w", err)
			}
		}

		go c.handlePortForwardConn(ctx, id, opts.RemotePort, clientConn)
	}
}

func (c *Client) handlePortForwardConn(ctx context.Context, id string, remotePort int, tcpConn net.Conn) {
	defer tcpConn.Close()

	targetURL := c.WSURL(fmt.Sprintf("/api/v1/sandboxes/%s/portforward?port=%d", url.PathEscape(id), remotePort))
	headers := http.Header{}
	if c.token != "" {
		headers.Set("Authorization", "Bearer "+c.token)
	}

	dialer := websocket.DefaultDialer
	wsConn, _, err := dialer.DialContext(ctx, targetURL, headers)
	if err != nil {
		return
	}
	defer wsConn.Close()

	sessionCtx, sessionCancel := context.WithCancel(ctx)
	defer sessionCancel()

	var wg sync.WaitGroup
	wg.Add(2)

	// Pump 1: Local TCP Socket -> Gateway WebSocket Binary
	go func() {
		defer wg.Done()
		defer sessionCancel()
		buf := make([]byte, 32768)
		for {
			n, err := tcpConn.Read(buf)
			if n > 0 {
				if err := wsConn.WriteMessage(websocket.BinaryMessage, buf[:n]); err != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	// Pump 2: Gateway WebSocket Binary -> Local TCP Socket
	go func() {
		defer wg.Done()
		defer sessionCancel()
		for {
			msgType, data, err := wsConn.ReadMessage()
			if err != nil {
				return
			}
			if msgType == websocket.BinaryMessage && len(data) > 0 {
				if _, err := tcpConn.Write(data); err != nil {
					return
				}
			}
		}
	}()

	<-sessionCtx.Done()
	_ = tcpConn.SetDeadline(time.Now())
	_ = wsConn.Close()
	wg.Wait()
}
