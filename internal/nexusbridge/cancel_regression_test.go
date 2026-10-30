package nexusbridge

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	protocol "github.com/uvwt/agentdock-protocol"
	"github.com/uvwt/agentdock/internal/config"
	"github.com/uvwt/agentdock/internal/mcp"
	"github.com/uvwt/agentdock/internal/publicartifacts"
)

type cancelTestNode struct {
	NodeAPI
	canceled chan bool
}

func (n *cancelTestNode) Invoke(ctx context.Context, _ string, _ map[string]any) (map[string]any, error) {
	select {
	case <-ctx.Done():
		n.canceled <- true
	case <-time.After(80 * time.Millisecond):
		n.canceled <- false
	}
	return map[string]any{}, nil
}
func serverTestFrame(message protocol.Message) []byte {
	data, _ := json.Marshal(message)
	header := []byte{0x81}
	if len(data) < 126 {
		header = append(header, byte(len(data)))
	} else {
		header = append(header, 126, 0, 0)
		binary.BigEndian.PutUint16(header[2:], uint16(len(data)))
	}
	return append(header, data...)
}
func TestRegressionImmediateCancelIsDelivered(t *testing.T) {
	prior := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(prior)
	node := &cancelTestNode{NodeAPI: mcp.NewServer(nil, config.Config{}), canceled: make(chan bool, 1)}
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		socket, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer socket.Close()
		var hello protocol.Message
		if socket.ReadJSON(&hello) != nil {
			return
		}
		frames := serverTestFrame(protocol.Message{Type: protocol.MessageNodeReady, ProtocolVersion: protocol.ConnectionProtocolVersion, HeartbeatMS: 60000})
		frames = append(frames, serverTestFrame(protocol.Message{Type: protocol.MessageToolInvoke, RequestID: "probe", Operation: protocol.OperationToolCall, Arguments: json.RawMessage(`{"tool":"probe","arguments":{}}`)})...)
		frames = append(frames, serverTestFrame(protocol.Message{Type: protocol.MessageToolCancel, RequestID: "probe"})...)
		if _, err := socket.UnderlyingConn().Write(frames); err != nil {
			return
		}
		for {
			if _, _, err := socket.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := NewClient(Identity{Endpoint: server.URL}, node, nil, publicartifacts.Store{}, &ConnectionState{})
	done := make(chan struct{})
	go func() { client.Run(ctx); close(done) }()
	var canceled bool
	select {
	case canceled = <-node.canceled:
	case <-time.After(time.Second):
		t.Fatal("invoke did not execute")
	}
	cancel()
	<-done
	if !canceled {
		t.Fatal("cancel immediately following invoke was lost; tool ran for full 80 ms")
	}
}

// staleInvokeNode 的第一次调用在断线后继续占着 RequestID，直到测试放行。
// 第二次调用才观察 ctx 是否被 tool.cancel 取消。
type staleInvokeNode struct {
	NodeAPI
	calls         atomic.Int32
	firstStarted  chan struct{}
	releaseFirst  chan struct{}
	secondStarted chan struct{}
	canceled      chan bool
}

func (n *staleInvokeNode) Invoke(ctx context.Context, _ string, _ map[string]any) (map[string]any, error) {
	switch n.calls.Add(1) {
	case 1:
		close(n.firstStarted)
		select {
		case <-n.releaseFirst:
		case <-time.After(5 * time.Second):
			return nil, fmt.Errorf("stale invoke was not released")
		}
		return map[string]any{"generation": "stale"}, nil
	case 2:
		close(n.secondStarted)
		select {
		case <-ctx.Done():
			n.canceled <- true
		case <-time.After(3 * time.Second):
			n.canceled <- false
		}
		return map[string]any{"generation": "current"}, nil
	default:
		return nil, fmt.Errorf("unexpected extra invoke")
	}
}

func TestRegressionStaleInvokeDoesNotDropReconnectedCancel(t *testing.T) {
	node := &staleInvokeNode{
		NodeAPI:       mcp.NewServer(nil, config.Config{}),
		firstStarted:  make(chan struct{}),
		releaseFirst:  make(chan struct{}),
		secondStarted: make(chan struct{}),
		canceled:      make(chan bool, 1),
	}
	dropFirst := make(chan struct{})
	sendCancel := make(chan struct{})
	serverErr := make(chan error, 4)
	gotResult := make(chan protocol.Message, 1)
	var attempts atomic.Int32
	invoke := protocol.Message{
		Type: protocol.MessageToolInvoke, RequestID: "reused",
		Operation: protocol.OperationToolCall, Arguments: json.RawMessage(`{"tool":"probe","arguments":{}}`),
	}

	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		socket, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			serverErr <- err
			return
		}
		defer socket.Close()
		var hello protocol.Message
		if err := socket.ReadJSON(&hello); err != nil {
			serverErr <- err
			return
		}
		ready := protocol.Message{Type: protocol.MessageNodeReady, ProtocolVersion: protocol.ConnectionProtocolVersion, HeartbeatMS: 60000}
		if err := socket.WriteJSON(ready); err != nil {
			serverErr <- err
			return
		}
		if err := socket.WriteJSON(invoke); err != nil {
			serverErr <- err
			return
		}
		// 第一条连接把调用留下，再断开。第二条连接复用同一个 RequestID。
		if attempts.Add(1) == 1 {
			<-dropFirst
			return
		}
		<-sendCancel
		if err := socket.WriteJSON(protocol.Message{Type: protocol.MessageToolCancel, RequestID: "reused"}); err != nil {
			serverErr <- err
			return
		}
		var response protocol.Message
		if err := socket.ReadJSON(&response); err != nil {
			serverErr <- err
			return
		}
		gotResult <- response
		for {
			if _, _, err := socket.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	client := NewClient(Identity{Endpoint: server.URL}, node, nil, publicartifacts.Store{}, &ConnectionState{})
	done := make(chan struct{})
	go func() { client.Run(ctx); close(done) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Error("bridge did not stop")
		}
	}()

	select {
	case <-node.firstStarted:
	case err := <-serverErr:
		t.Fatal(err)
	case <-time.After(2 * time.Second):
		t.Fatal("first invoke did not start")
	}
	close(dropFirst)

	select {
	case <-node.secondStarted:
	case err := <-serverErr:
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		t.Fatal("bridge did not accept the reused RequestID after reconnect")
	}

	close(node.releaseFirst)
	// 旧调用返回后还会写结果，并在 defer 里注销取消项。
	// 等到它离开 invoke 再发 cancel，才会打中“新注册被旧 defer 删掉”的窗口。
	waitUntilStaleInvokeReturns(t)

	close(sendCancel)
	select {
	case canceled := <-node.canceled:
		if !canceled {
			t.Fatal("cancel for reused RequestID was lost after the stale invoke finished")
		}
	case err := <-serverErr:
		t.Fatal(err)
	case <-time.After(2 * time.Second):
		t.Fatal("reconnected invoke was not canceled")
	}
	select {
	case response := <-gotResult:
		if response.Type != protocol.MessageToolResult || response.RequestID != "reused" {
			t.Fatalf("response = %#v", response)
		}
	case err := <-serverErr:
		t.Fatal(err)
	case <-time.After(2 * time.Second):
		t.Fatal("reconnected invoke did not finish on the live socket")
	}
}

func waitUntilStaleInvokeReturns(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		switch clientInvokeGoroutines() {
		case 1:
			return
		case 0:
			t.Fatal("both invokes finished before cancel was sent")
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("stale invoke did not finish, invoke goroutines=%d", clientInvokeGoroutines())
}

func clientInvokeGoroutines() int {
	buf := make([]byte, 64<<10)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return strings.Count(string(buf[:n]), "(*Client).invoke(")
		}
		buf = make([]byte, len(buf)*2)
	}
}
