package nexusbridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	protocol "github.com/uvwt/agentdock-protocol"
	"github.com/uvwt/agentdock/internal/app"
	"github.com/uvwt/agentdock/internal/buildinfo"
	"github.com/uvwt/agentdock/internal/observability"
	"github.com/uvwt/agentdock/internal/publicartifacts"
	"github.com/uvwt/agentdock/internal/runtimeapi"
)

const (
	maxMessageBytes           = 8 << 20
	invokeDrainTimeout        = 5 * time.Second
	stableConnectionDuration  = 30 * time.Second
	officialNexusEndpoint     = "https://mcp.nexusdock.co"
	officialWebSocketEndpoint = "https://ws.nexusdock.co"
)

type connectionOutcome struct {
	err             error
	connectedFor    time.Duration
	fallbackAllowed bool
}

// NodeAPI 由 Nexus Bridge 消费方定义，只包含握手和远程 operation 真正需要的节点能力。
type NodeAPI interface {
	ToolNames() []string
	ToolDescriptors() []map[string]any
	UIResources() []protocol.UIResourceCapability
	ToolContractHash() string
	AgentDockLocalContext(context.Context) (map[string]any, error)
	Invoke(context.Context, string, map[string]any) (map[string]any, error)
	ReadAppResource(string) (map[string]any, error)
}

type Client struct {
	identity         Identity
	node             NodeAPI
	runtime          runtimeapi.Runtime
	artifacts        publicartifacts.Store
	state            *ConnectionState
	invokeWG         sync.WaitGroup
	writeMu          sync.Mutex
	handshakeTimeout time.Duration
	readTimeout      time.Duration
}

// invocationCancels 保存一条 WebSocket 上尚未结束的调用取消函数。
//
// RequestID 只在一条连接内唯一。连接断开时父 context 会被取消，但工具调用
// 不一定马上退出；Run 随后会拨出新连接，服务端可以复用同一个 RequestID。
// 取消表如果挂在 Client 上，旧调用结束时的 delete 会把新连接刚注册的取消函数一起删掉，
// 之后服务端再发 tool.cancel，新调用就收不到。所以每条连接一张表，清理时只删自己的。
type invocationCancels struct {
	mu      sync.Mutex
	entries map[string]context.CancelFunc
}

func newInvocationCancels() *invocationCancels {
	return &invocationCancels{entries: make(map[string]context.CancelFunc)}
}

func (r *invocationCancels) register(requestID string, cancel context.CancelFunc) {
	r.mu.Lock()
	r.entries[requestID] = cancel
	r.mu.Unlock()
}

func (r *invocationCancels) unregister(requestID string) {
	r.mu.Lock()
	delete(r.entries, requestID)
	r.mu.Unlock()
}

func (r *invocationCancels) cancel(requestID string) {
	r.mu.Lock()
	cancel := r.entries[requestID]
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func NewClient(identity Identity, node NodeAPI, runtime runtimeapi.Runtime, artifacts publicartifacts.Store, state *ConnectionState) *Client {
	return &Client{
		identity: identity, node: node, runtime: runtime, artifacts: artifacts, state: state,
		handshakeTimeout: 15 * time.Second, readTimeout: 90 * time.Second,
	}
}

func (c *Client) Run(ctx context.Context) {
	defer c.drainInvocations()
	backoff := time.Second
	for ctx.Err() == nil {
		endpoints := c.connectionEndpoints()
		endpoint := endpoints[0]
		outcome := c.connect(ctx, endpoint)
		if ctx.Err() != nil {
			return
		}
		backoff = resetBackoffAfterStableConnection(backoff, outcome.connectedFor)
		if len(endpoints) > 1 && outcome.fallbackAllowed {
			slog.Warn("NexusDock preferred WebSocket endpoint unavailable; trying paired endpoint", "endpoint", endpoint, "error", outcome.err)
			endpoint = endpoints[1]
			outcome = c.connect(ctx, endpoint)
			if ctx.Err() != nil {
				return
			}
			backoff = resetBackoffAfterStableConnection(backoff, outcome.connectedFor)
		}
		slog.Warn("NexusDock connection lost", "endpoint", endpoint, "error", outcome.err, "retry_in", backoff)
		timer := time.NewTimer(backoff + time.Duration(rand.IntN(500))*time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (c *Client) connectionEndpoints() []string {
	preferred := strings.TrimSpace(c.identity.WebSocketEndpoint)
	fallback := strings.TrimSpace(c.identity.Endpoint)
	// 老版本 identity 没有 websocket_endpoint。只为官方 SaaS 补默认值，
	// 自建 NexusDock 仍严格使用配对时保存的 endpoint；服务端显式下发值始终优先。
	if preferred == "" && strings.EqualFold(fallback, officialNexusEndpoint) {
		preferred = officialWebSocketEndpoint
	}
	if preferred == "" || preferred == fallback {
		return []string{fallback}
	}
	return []string{preferred, fallback}
}

func resetBackoffAfterStableConnection(current, connectedFor time.Duration) time.Duration {
	if connectedFor >= stableConnectionDuration {
		return time.Second
	}
	return current
}

func fallbackAllowedForHTTPStatus(status int) bool {
	return status == http.StatusRequestTimeout ||
		status == http.StatusTooEarly ||
		status == http.StatusTooManyRequests ||
		status >= http.StatusInternalServerError
}

func fallbackAllowedForSocketError(err error) bool {
	if err == nil {
		return false
	}
	var closeErr *websocket.CloseError
	if !errors.As(err, &closeErr) {
		return true
	}
	switch closeErr.Code {
	case websocket.CloseProtocolError,
		websocket.CloseUnsupportedData,
		websocket.CloseInvalidFramePayloadData,
		websocket.ClosePolicyViolation,
		websocket.CloseMessageTooBig,
		websocket.CloseMandatoryExtension:
		return false
	default:
		return true
	}
}

func (c *Client) drainInvocations() {
	done := make(chan struct{})
	go func() {
		c.invokeWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(invokeDrainTimeout):
		slog.Warn("NexusDock bridge in-flight invocations drain timeout")
	}
}

func (c *Client) connect(ctx context.Context, rawEndpoint string) connectionOutcome {
	c.state.SetConnected(false)
	endpoint, err := url.Parse(rawEndpoint)
	if err != nil {
		return connectionOutcome{err: err}
	}
	if endpoint.Scheme == "https" {
		endpoint.Scheme = "wss"
	} else {
		endpoint.Scheme = "ws"
	}
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + protocol.ConnectPath
	header := http.Header{"Authorization": []string{"Bearer " + c.identity.DeviceToken}}
	socket, response, err := websocket.DefaultDialer.DialContext(ctx, endpoint.String(), header)
	if err != nil {
		if response != nil {
			return connectionOutcome{
				err:             fmt.Errorf("连接 NexusDock（HTTP %d）: %w", response.StatusCode, err),
				fallbackAllowed: fallbackAllowedForHTTPStatus(response.StatusCode),
			}
		}
		return connectionOutcome{err: fmt.Errorf("连接 NexusDock: %w", err), fallbackAllowed: true}
	}
	defer socket.Close()
	// gorilla/websocket 的阻塞读取不会自动观察 context；父 Context 取消时主动关闭连接，
	// 让 ReadJSON 立即返回，确保 Bridge 能在 Runtime 关闭前退出。
	stopContextClose := context.AfterFunc(ctx, func() { _ = socket.Close() })
	defer stopContextClose()
	socket.SetReadLimit(maxMessageBytes)

	tools := c.node.ToolNames()
	descriptors, err := bridgeToolDescriptors(c.node.ToolDescriptors())
	if err != nil {
		return connectionOutcome{err: err}
	}
	if err := c.write(socket, protocol.Message{
		Type: protocol.MessageNodeHello, ProtocolVersion: protocol.ConnectionProtocolVersion,
		Hello: bridgeHello(c.identity, tools, descriptors, c.node.UIResources(), c.node.ToolContractHash()),
	}); err != nil {
		return connectionOutcome{err: err, fallbackAllowed: true}
	}
	var ready protocol.Message
	_ = socket.SetReadDeadline(time.Now().Add(c.handshakeTimeout))
	if err := socket.ReadJSON(&ready); err != nil {
		return connectionOutcome{err: fmt.Errorf("读取 NexusDock 握手响应: %w", err), fallbackAllowed: fallbackAllowedForSocketError(err)}
	}
	if ready.Type != protocol.MessageNodeReady || ready.ProtocolVersion != protocol.ConnectionProtocolVersion {
		return connectionOutcome{err: errors.New("NexusDock 返回了不兼容的节点协议")}
	}
	if oauthRuntime, ok := c.runtime.(runtimeapi.NexusOAuthCallbackRuntime); ok {
		if err := oauthRuntime.SetNexusOAuthCallback(ready.PublicURL, c.identity.NodeID); err != nil {
			// Nexus 公网 URL 只影响“通过 Nexus 授权”这个可选路径，不能让 Recall、
			// Runtime 管理等整条节点 Bridge 因一项回调配置失效。
			slog.Warn("NexusDock OAuth callback unavailable", "error", err)
		}
		defer func() {
			if err := oauthRuntime.SetNexusOAuthCallback("", c.identity.NodeID); err != nil {
				slog.Warn("clear NexusDock OAuth callback failed", "error", err)
			}
		}()
	}
	// 业务心跳写入成功不代表对端仍在响应。用标准 WebSocket Ping/Pong
	// 检测半开连接；超时后 ReadJSON 返回，由 Run 负责重新连接。
	_ = socket.SetReadDeadline(time.Now().Add(c.readTimeout))
	socket.SetPongHandler(func(string) error {
		return socket.SetReadDeadline(time.Now().Add(c.readTimeout))
	})
	c.state.SetConnected(true)
	defer c.state.SetConnected(false)
	connectedAt := time.Now()
	slog.Info("NexusDock node connected", "node_id", c.identity.NodeID, "endpoint", rawEndpoint)
	heartbeat := time.Duration(ready.HeartbeatMS) * time.Millisecond
	if heartbeat <= 0 {
		heartbeat = 30 * time.Second
	}
	connectionCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go c.heartbeat(connectionCtx, socket, heartbeat)
	// 取消表跟这条连接走，不挂在 Client 上。见 invocationCancels。
	cancels := newInvocationCancels()
	for {
		var incoming protocol.Message
		if err := socket.ReadJSON(&incoming); err != nil {
			return connectionOutcome{
				err:             err,
				connectedFor:    time.Since(connectedAt),
				fallbackAllowed: fallbackAllowedForSocketError(err),
			}
		}
		_ = socket.SetReadDeadline(time.Now().Add(c.readTimeout))
		switch incoming.Type {
		case protocol.MessageToolInvoke:
			// 接收循环先注册取消，再交给 goroutine 执行，确保下一帧 cancel 不会丢失。
			invokeCtx, invokeCancel := context.WithCancel(connectionCtx)
			cancels.register(incoming.RequestID, invokeCancel)
			c.invokeWG.Add(1)
			go func() {
				defer c.invokeWG.Done()
				defer invokeCancel()
				c.invoke(invokeCtx, socket, incoming, cancels)
			}()
		case protocol.MessageToolCancel:
			cancels.cancel(incoming.RequestID)
		case protocol.MessageNodeHeartbeat:
		}
	}
}

func bridgeHello(identity Identity, tools []string, descriptors []protocol.ToolDescriptor, uiResources []protocol.UIResourceCapability, toolContractHash string) *protocol.Hello {
	return &protocol.Hello{
		DeviceID:        identity.DeviceID,
		Version:         buildinfo.Version,
		ProtocolVersion: protocol.ConnectionProtocolVersion,
		OS:              runtime.GOOS,
		Arch:            runtime.GOARCH,
		Capabilities:    append([]string(nil), tools...),
		BridgeCapabilities: []string{
			protocol.CapabilitiesNegotiationCapability,
			protocol.ContextLocalCapability,
			protocol.RuntimeRequestCapability,
			protocol.ResourceReadCapability,
			protocol.ArtifactReadCapability,
		},
		ToolContractHash: toolContractHash,
		Tools:            descriptors,
		UIResources:      uiResources,
	}
}

func (c *Client) invoke(parent context.Context, socket *websocket.Conn, incoming protocol.Message, cancels *invocationCancels) {
	ctx := extractBridgeTraceContext(parent, &incoming)
	defer func() {
		recovered := recover()
		// 旧调用可能在重连之后才退出。这里只能注销本连接的项。
		cancels.unregister(incoming.RequestID)
		if recovered != nil {
			// Nexus Bridge 是远程 RPC 边界。单次工具 panic 只结束当前请求，
			// 避免把整个 AgentDock 守护进程和其他本地会话一并带崩。
			slog.Error("NexusDock node operation panicked", "request_id", incoming.RequestID, "operation", incoming.Operation, "panic", recovered, "stack", string(debug.Stack()))
			_ = c.write(socket, protocol.Message{
				Type:      protocol.MessageToolError,
				RequestID: incoming.RequestID,
				Error:     &protocol.RemoteError{Code: "NODE_OPERATION_FAILED", Message: "AgentDock node operation failed", Category: "internal"},
			})
		}
	}()

	var result map[string]any
	var err error
	switch incoming.Operation {
	case protocol.OperationRuntimeRequest:
		var request runtimeapi.Request
		if decodeErr := json.Unmarshal(incoming.Arguments, &request); decodeErr != nil {
			err = fmt.Errorf("解析 Runtime 请求: %w", decodeErr)
		} else {
			result, err = c.dispatchRuntimeRequest(ctx, request)
		}
	case protocol.OperationContextLocal:
		result, err = c.node.AgentDockLocalContext(ctx)
	case protocol.OperationToolCall:
		var request struct {
			Tool      string         `json:"tool"`
			Arguments map[string]any `json:"arguments"`
		}
		if decodeErr := json.Unmarshal(incoming.Arguments, &request); decodeErr != nil {
			err = fmt.Errorf("解析工具请求: %w", decodeErr)
		} else {
			toolCtx := observability.WithSource(ctx, observability.SourceNexus)
			result, err = c.node.Invoke(toolCtx, request.Tool, request.Arguments)
		}
	case protocol.OperationResourceRead:
		var request struct {
			URI string `json:"uri"`
		}
		if decodeErr := json.Unmarshal(incoming.Arguments, &request); decodeErr != nil {
			err = fmt.Errorf("解析 MCP App resource 请求: %w", decodeErr)
		} else {
			result, err = c.node.ReadAppResource(request.URI)
		}
	case protocol.OperationArtifactRead:
		var request struct {
			ArtifactID string `json:"artifact_id"`
			Offset     int64  `json:"offset"`
			MaxBytes   int    `json:"max_bytes"`
		}
		if decodeErr := json.Unmarshal(incoming.Arguments, &request); decodeErr != nil {
			err = fmt.Errorf("解析 Artifact 读取请求: %w", decodeErr)
		} else {
			result, err = c.readArtifactChunk(request.ArtifactID, request.Offset, request.MaxBytes)
		}
	default:
		err = fmt.Errorf("不支持的 NexusDock 节点操作: %s", incoming.Operation)
	}
	if err != nil {
		if incoming.Operation == protocol.OperationRuntimeRequest {
			var request runtimeapi.Request
			if json.Unmarshal(incoming.Arguments, &request) == nil && request.Path == runtimeapi.ResourceLibraryPath {
				remote := bridgeError(err)
				// No remote message: upstream/transport errors can embed credentials or user paths.
				slog.Warn("Resource library remote operation failed", "error_code", remote.Code, "error_category", remote.Category)
			}
		}
		_ = c.write(socket, protocol.Message{Type: protocol.MessageToolError, RequestID: incoming.RequestID, Error: bridgeError(err)})
		return
	}
	encoded, encodeErr := json.Marshal(result)
	if encodeErr != nil {
		_ = c.write(socket, protocol.Message{Type: protocol.MessageToolError, RequestID: incoming.RequestID, Error: bridgeError(fmt.Errorf("编码节点结果: %w", encodeErr))})
		return
	}
	_ = c.write(socket, protocol.Message{Type: protocol.MessageToolResult, RequestID: incoming.RequestID, Result: encoded})
}

func (c *Client) dispatchRuntimeRequest(ctx context.Context, request runtimeapi.Request) (map[string]any, error) {
	// Nexus Runtime operation 沿用原有 64 KiB Bridge 请求上限；HTTP 各路由仍保留自己的错误语义。
	if len(request.Body) > 64*1024 {
		return nil, &app.ToolError{Code: "INVALID_ARGUMENT", Message: "runtime request body is too large", Category: "validation"}
	}
	return runtimeapi.Dispatch(ctx, c.runtime, request)
}

func bridgeToolDescriptors(descriptors []map[string]any) ([]protocol.ToolDescriptor, error) {
	encoded, err := json.Marshal(descriptors)
	if err != nil {
		return nil, fmt.Errorf("编码 Nexus Bridge 工具契约: %w", err)
	}
	var tools []protocol.ToolDescriptor
	if err := json.Unmarshal(encoded, &tools); err != nil {
		return nil, fmt.Errorf("解析 Nexus Bridge 工具契约: %w", err)
	}
	return tools, nil
}

func (c *Client) heartbeat(ctx context.Context, socket *websocket.Conn, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	pingTicker := time.NewTicker(c.readTimeout / 3)
	defer pingTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := c.write(socket, protocol.Message{Type: protocol.MessageNodeHeartbeat}); err != nil {
				_ = socket.Close()
				return
			}
		case <-pingTicker.C:
			// WriteControl 可与数据帧写入并发，避免等待工具响应占用的写锁。
			if err := socket.WriteControl(websocket.PingMessage, nil, time.Now().Add(15*time.Second)); err != nil {
				_ = socket.Close()
				return
			}
		}
	}
}

func (c *Client) write(socket *websocket.Conn, outgoing protocol.Message) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_ = socket.SetWriteDeadline(time.Now().Add(15 * time.Second))
	return socket.WriteJSON(outgoing)
}

func bridgeError(err error) *protocol.RemoteError {
	converted := &protocol.RemoteError{Code: "NODE_OPERATION_FAILED", Message: err.Error()}
	var toolErr *app.ToolError
	if errors.As(err, &toolErr) {
		converted.Code = toolErr.Code
		converted.Message = toolErr.Message
		converted.Category = toolErr.Category
		converted.Retryable = toolErr.Retryable
		converted.Details = toolErr.Details
	}
	return converted
}
