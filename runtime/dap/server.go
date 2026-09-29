// Package dap 提供 GLua Debug Adapter Protocol 服务的最小协议骨架。
//
// 当前包只负责 DAP 帧读写、基础握手和占位请求响应；断点命中、单步、调用栈和变量读取会在后续
// 接入 VM debug hook 后逐步补齐。
package dap

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ZingYao/go-lua-vm/bytecode"
	"github.com/ZingYao/go-lua-vm/runtime"
)

// Server 表示一个只在显式 CLI 参数开启时运行的 GLua DAP server。
//
// Address 必须是 TCP 监听地址；当前阶段调用方应只传入 loopback 地址，避免把未鉴权调试端口暴露到
// 外部网络。Ready 会在监听成功后关闭，ActualAddress 保存真实监听地址。
type Server struct {
	Address       string
	ActualAddress string
	Ready         chan struct{}

	listener net.Listener
	closed   chan struct{}
	once     sync.Once

	mu                   sync.Mutex
	breakpoints          map[string]map[int]bool
	activeSession        *connectionSession
	clientDisconnected   chan struct{}
	continueCh           chan struct{}
	configuredCh         chan struct{}
	configOnce           sync.Once
	stopped              bool
	currentStop          *stoppedLocation
	nextVariableID       int
	variableValues       map[int]runtime.Value
	variableTables       map[*runtime.Table]int
	variableTableKeys    map[int]runtime.Value
	threadIDs            map[*runtime.Thread]int
	nextThreadID         int
	sourceRoot           string
	localRoot            string
	remoteRoot           string
	stopOnEntry          bool
	stepMode             dapStepMode
	stepSource           string
	stepLine             int
	stepDepth            int
	pauseRequested       bool
	skipBreakpointSource string
	skipBreakpointLine   int
}

// NewServer 创建一个 GLua DAP server。
//
// address 为空时返回错误；调用方负责决定地址策略。返回的 Server 尚未监听，必须调用 Start。
func NewServer(address string) (*Server, error) {
	// 先校验输入，避免 net.Listen 的错误文本缺少 GLua 上下文。
	if strings.TrimSpace(address) == "" {
		// 空地址无法表达监听目标，直接返回配置错误。
		return nil, errors.New("glua DAP listen address is required")
	}
	workingDir, err := os.Getwd()
	if err != nil {
		// 工作目录不可读时仍允许 DAP 启动，只是相对源码路径会保留原样。
		workingDir = ""
	}
	return &Server{
		Address:           strings.TrimSpace(address),
		Ready:             make(chan struct{}),
		closed:            make(chan struct{}),
		breakpoints:       make(map[string]map[int]bool),
		continueCh:        make(chan struct{}),
		configuredCh:      make(chan struct{}),
		nextVariableID:    dapFirstDynamicVariablesReference,
		variableValues:    make(map[int]runtime.Value),
		variableTables:    make(map[*runtime.Table]int),
		variableTableKeys: make(map[int]runtime.Value),
		threadIDs:         make(map[*runtime.Thread]int),
		nextThreadID:      1,
		sourceRoot:        workingDir,
	}, nil
}

// Start 启动 TCP 监听并在后台处理一个或多个 DAP 客户端连接。
//
// ctx 取消或调用 Close 后服务会停止；readyWriter 非 nil 时会写出 ready 标记，供编辑器等待启动完成。
func (server *Server) Start(ctx context.Context, readyWriter io.Writer) error {
	// 先创建监听器，保证返回 nil 时端口已经可连接。
	listener, err := net.Listen("tcp", server.Address)
	if err != nil {
		// 监听失败通常是端口占用或地址非法，直接返回给 CLI 展示。
		return fmt.Errorf("start GLua DAP server on %s: %w", server.Address, err)
	}
	server.listener = listener
	server.ActualAddress = listener.Addr().String()
	close(server.Ready)
	if readyWriter != nil {
		// ready 标记使用 stderr 更适合被编辑器读取，不污染脚本 stdout。
		_, _ = fmt.Fprintf(readyWriter, "GLua DAP server listening on %s\n", server.ActualAddress)
	}
	go server.acceptLoop(ctx)
	return nil
}

// WaitForConfigurationDone 等待 DAP 客户端完成断点配置。
//
// ctx 取消、超时或 server 关闭都会返回错误；配置完成后返回 nil。readyWriter 非 nil 时输出等待状态，
// 供 IDE Debug 控制台展示启动进度，避免调试会话看起来没有任何动静。
func (server *Server) WaitForConfigurationDone(ctx context.Context, timeout time.Duration, readyWriter io.Writer) error {
	// DAP 模式必须等客户端发送 configurationDone，确保脚本执行前断点已经下发。
	if server == nil {
		// nil server 表示调用方没有启用 DAP，不需要等待。
		return nil
	}
	if timeout <= 0 {
		// 非正超时没有可等待窗口，直接返回配置错误。
		return errors.New("GLua DAP configuration wait timeout must be positive")
	}
	if readyWriter != nil {
		// 输出等待提示，帮助 IDE 控制台说明当前不是卡死。
		_, _ = fmt.Fprintln(readyWriter, "GLua DAP waiting for client configuration...")
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-server.configuredCh:
		if readyWriter != nil {
			// 配置完成后脚本即将开始执行。
			_, _ = fmt.Fprintln(readyWriter, "GLua DAP client configured; starting script.")
		}
		return nil
	case <-timer.C:
		// 没有 IDE attach/configurationDone 时不能继续跑脚本，否则断点会错过。
		return fmt.Errorf("timeout waiting for GLua DAP client configuration after %s", timeout)
	case <-ctx.Done():
		// 上层取消时保留 context 原因。
		return ctx.Err()
	case <-server.closed:
		// server 被关闭说明调试启动流程已经失效。
		return errors.New("GLua DAP server closed before client configuration")
	}
}

// Close 停止 DAP server 并释放监听端口。
//
// 该方法可重复调用；已接受连接会在读写失败或 ctx 取消后自然结束。
func (server *Server) Close() error {
	var closeErr error
	server.once.Do(func() {
		// 关闭 closed 通道用于通知 acceptLoop 退出。
		close(server.closed)
		if server.listener != nil {
			// listener.Close 会打断阻塞中的 Accept。
			closeErr = server.listener.Close()
		}
	})
	return closeErr
}

// NotifyTerminated informs the active DAP client that the debuggee finished.
func (server *Server) NotifyTerminated() {
	server.mu.Lock()
	session := server.activeSession
	server.mu.Unlock()
	if session == nil {
		return
	}
	session.write(eventMessage{Seq: session.nextSeq(), Type: "event", Event: "terminated"})
}

// WaitForClientDisconnect keeps the server alive long enough for the client to
// acknowledge termination and close the DAP session.
func (server *Server) WaitForClientDisconnect(ctx context.Context, timeout time.Duration) error {
	server.mu.Lock()
	disconnected := server.clientDisconnected
	server.mu.Unlock()
	if disconnected == nil {
		return errors.New("GLua DAP client is not connected")
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-disconnected:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return fmt.Errorf("timeout waiting for GLua DAP client disconnect after %s", timeout)
	}
}

// acceptLoop 接受 DAP 客户端连接。
//
// 当前最小实现允许多个顺序或并发连接，每个连接独立维护 response seq；后续真正调试会话可能收敛为
// 单连接模型，避免多个 IDE 同时控制同一个 VM。
func (server *Server) acceptLoop(ctx context.Context) {
	for {
		// 每轮先检查关闭信号，避免 Close 后继续 Accept。
		select {
		case <-ctx.Done():
			// 上层取消时释放监听器并退出。
			_ = server.Close()
			return
		case <-server.closed:
			// 主动关闭时直接退出。
			return
		default:
			// 未关闭时继续阻塞等待连接。
		}
		connection, err := server.listener.Accept()
		if err != nil {
			// Close/ctx 触发的 Accept 错误属于正常退出路径。
			select {
			case <-server.closed:
				return
			default:
				time.Sleep(10 * time.Millisecond)
				continue
			}
		}
		go server.handleConnection(ctx, connection)
	}
}

// stoppedLocation 保存 VM 当前暂停位置。
//
// Source 已归一化为无 @ 前缀路径；Line 使用 DAP/源码展示的一基行号；PC 使用 Proto.Code 的零基指令位置。
type stoppedLocation struct {
	Source    string
	Line      int
	PC        int
	Reason    string
	Variables []runtime.ActiveLocalSnapshot
	VM        *runtime.VM
	Depth     int
	Frames    []runtime.CallFrame
	State     *runtime.State
	Thread    *runtime.Thread
}

const (
	// dapLocalsVariablesReference 是 DAP Locals scope 的固定变量引用。
	dapLocalsVariablesReference = 1
	// dapUpvaluesVariablesReference 是 DAP Upvalues scope 的固定变量引用。
	dapUpvaluesVariablesReference = 2
	// dapFirstDynamicVariablesReference 是 table 展开节点的第一个动态变量引用。
	dapFirstDynamicVariablesReference = 3
)

// dapStepMode 表示 DAP 单步命令要求的调用深度边界。
type dapStepMode string

const (
	// dapStepModeNone 表示当前没有等待命中的单步请求。
	dapStepModeNone dapStepMode = ""
	// dapStepModeInto 表示在下一条不同源码行暂停，可进入更深调用帧。
	dapStepModeInto dapStepMode = "into"
	// dapStepModeOver 表示只在当前或更浅调用深度的下一条不同源码行暂停。
	dapStepModeOver dapStepMode = "over"
	// dapStepModeOut 表示在离开当前调用深度后暂停。
	dapStepModeOut dapStepMode = "out"
)

// protocolMessage 表示 DAP JSON 消息的通用字段。
//
// DAP 的 body/arguments 结构较多，最小实现只解析测试和握手需要的通用字段。
type protocolMessage struct {
	Seq       int             `json:"seq,omitempty"`
	Type      string          `json:"type"`
	Command   string          `json:"command,omitempty"`
	Event     string          `json:"event,omitempty"`
	Success   bool            `json:"success,omitempty"`
	Body      json.RawMessage `json:"body,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// responseMessage 表示 DAP response 消息。
//
// Body 使用 any 以便各命令返回不同结构；Message 仅在失败时填写。
type responseMessage struct {
	Seq        int    `json:"seq"`
	Type       string `json:"type"`
	RequestSeq int    `json:"request_seq"`
	Success    bool   `json:"success"`
	Command    string `json:"command"`
	Message    string `json:"message,omitempty"`
	Body       any    `json:"body,omitempty"`
}

// eventMessage 表示 DAP event 消息。
//
// Body 使用 any 以便后续补 stopped、terminated、output 等事件载荷。
type eventMessage struct {
	Seq   int    `json:"seq"`
	Type  string `json:"type"`
	Event string `json:"event"`
	Body  any    `json:"body,omitempty"`
}

// handleConnection 处理单个 DAP 客户端连接。
//
// 该函数按 DAP Content-Length 帧循环读取 request，当前只响应基础命令；未知命令返回 success=false，
// 便于 IDE 或测试看到明确能力边界。
func (server *Server) handleConnection(ctx context.Context, connection net.Conn) {
	defer connection.Close()
	reader := bufio.NewReader(connection)
	writer := bufio.NewWriter(connection)
	session := &connectionSession{connection: connection, writer: writer, server: server}
	server.setActiveSession(session)
	defer server.clearActiveSession(session)
	for {
		// 每次读取前检查 ctx，避免上层退出后连接继续阻塞。
		select {
		case <-ctx.Done():
			return
		default:
			// 未取消时继续读取下一帧。
		}
		message, err := readMessage(reader)
		if err != nil {
			// EOF 或坏帧都结束该客户端连接；后续可把坏帧写入诊断日志。
			return
		}
		if message.Type != "request" {
			// 当前 server 只处理客户端 request，其它消息直接忽略。
			continue
		}
		session.respond(message)
	}
}

// setActiveSession 记录当前可接收 stopped 事件的 DAP 连接。
//
// 当前最小实现以最后连接的客户端为活动会话；后续可改为单会话拒绝策略。
func (server *Server) setActiveSession(session *connectionSession) {
	server.mu.Lock()
	defer server.mu.Unlock()
	server.activeSession = session
	server.clientDisconnected = make(chan struct{})
}

// clearActiveSession 在连接结束时清理活动会话。
//
// 若断点正处于暂停态，连接断开会释放 VM，避免脚本永久卡住。
func (server *Server) clearActiveSession(session *connectionSession) {
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.activeSession == session {
		// 只有当前连接仍是活动会话时才清理，避免覆盖新连接。
		server.activeSession = nil
		if server.clientDisconnected != nil {
			close(server.clientDisconnected)
			server.clientDisconnected = nil
		}
	}
	if server.stopped {
		// 客户端断开时解除暂停，避免 CLI 无法退出。
		close(server.continueCh)
		server.continueCh = make(chan struct{})
		server.stopped = false
		server.currentStop = nil
		server.clearVariableReferencesLocked()
	}
}

// connectionSession 保存单个 DAP 连接上的响应序列号。
//
// DAP 要求 adapter 自己递增 seq；该结构避免多个连接共享序列导致测试不稳定。
type connectionSession struct {
	mu         sync.Mutex
	closeOnce  sync.Once
	connection net.Conn
	writer     *bufio.Writer
	server     *Server
	nextID     int
}

// respond 根据请求命令写出 DAP response，并在 initialize 后发送 initialized 事件。
//
// request 必须是 DAP request 消息；未知 command 会得到明确失败响应。
func (session *connectionSession) respond(request protocolMessage) {
	if request.Command == "setVariable" {
		// setVariable 需要把写回错误体现在 DAP success=false，方便 IDE 展示修改失败。
		body, err := session.server.setVariable(request.Arguments)
		if err != nil {
			session.write(responseMessage{
				Seq:        session.nextSeq(),
				Type:       "response",
				RequestSeq: request.Seq,
				Success:    false,
				Command:    request.Command,
				Message:    err.Error(),
				Body:       map[string]any{"error": err.Error()},
			})
			return
		}
		session.write(responseMessage{
			Seq:        session.nextSeq(),
			Type:       "response",
			RequestSeq: request.Seq,
			Success:    true,
			Command:    request.Command,
			Body:       body,
		})
		return
	}
	body, ok := session.responseBody(request)
	if !ok {
		// 未实现命令要显式失败，避免 IDE 误以为空响应。
		session.write(responseMessage{
			Seq:        session.nextSeq(),
			Type:       "response",
			RequestSeq: request.Seq,
			Success:    false,
			Command:    request.Command,
			Message:    "GLua DAP command is not implemented yet: " + request.Command,
		})
		return
	}
	session.write(responseMessage{
		Seq:        session.nextSeq(),
		Type:       "response",
		RequestSeq: request.Seq,
		Success:    true,
		Command:    request.Command,
		Body:       body,
	})
	if request.Command == "disconnect" || request.Command == "terminate" {
		session.close()
	}
	if request.Command == "initialize" {
		// initialized 事件通知客户端可以继续发送断点和 configurationDone。
		session.write(eventMessage{
			Seq:   session.nextSeq(),
			Type:  "event",
			Event: "initialized",
		})
	}
}

func (session *connectionSession) close() {
	session.closeOnce.Do(func() {
		if session.connection != nil {
			_ = session.connection.Close()
		}
	})
}

// nextSeq 返回当前 DAP 连接的下一个 adapter 序列号。
//
// DAP seq 从 1 开始即可；不需要与客户端 request_seq 对齐。
func (session *connectionSession) nextSeq() int {
	session.mu.Lock()
	defer session.mu.Unlock()
	session.nextID++
	return session.nextID
}

// write 将一个 DAP 消息编码为 Content-Length 帧。
//
// 写入失败时当前最小实现忽略错误，连接会在后续读写中自然结束。
func (session *connectionSession) write(message any) {
	payload, err := json.Marshal(message)
	if err != nil {
		// 内部响应结构不可编码时只能关闭该次写入，避免 panic 杀掉 server。
		return
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	_, _ = fmt.Fprintf(session.writer, "Content-Length: %d\r\n\r\n", len(payload))
	_, _ = session.writer.Write(payload)
	_ = session.writer.Flush()
}

// responseBody 返回已实现命令的最小响应 body。
//
// 返回 ok=false 表示命令尚未实现。当前 body 以 IDE 能完成基础握手为目标，不声称已支持真实断点。
func (session *connectionSession) responseBody(request protocolMessage) (any, bool) {
	switch request.Command {
	case "initialize":
		// 声明基础能力；断点 verified 状态会在后续 VM hook 接入后变为真实校验。
		return map[string]any{
			"supportsConfigurationDoneRequest": true,
			"supportsTerminateRequest":         true,
			"supportsEvaluateForHovers":        true,
			"supportsSetVariable":              true,
		}, true
	case "launch", "attach":
		// 当前阶段 launch/attach 都只完成协议握手，脚本执行仍由 CLI 主流程承担。
		session.server.configureClient(request.Arguments)
		return map[string]any{}, true
	case "configurationDone":
		// 客户端断点配置完成，释放 CLI 去执行脚本。
		session.server.markConfigured()
		return map[string]any{}, true
	case "setBreakpoints":
		// 解析并保存源码断点，让 VM 指令观察器可以在对应行暂停。
		return session.server.setBreakpoints(request.Arguments), true
	case "threads":
		// 主线程与已创建 coroutine 都映射为稳定 DAP threads。
		return session.server.threads(), true
	case "evaluate":
		// 仅在暂停态读取有限表达式，不执行任意 Lua 代码。
		return session.server.evaluate(request.Arguments), true
	case "stackTrace":
		// 暂停时返回从当前帧到最早帧的调用栈快照。
		return session.server.stackTrace(request.Arguments), true
	case "scopes":
		// 暂停时暴露当前帧局部变量 scope；未暂停时没有 scope。
		return session.server.scopes(), true
	case "variables":
		// 返回暂停点保存的局部变量快照。
		return session.server.variables(request.Arguments), true
	case "continue":
		// 继续请求释放 VM 暂停点。
		session.server.resumeAll(true)
		return map[string]any{"allThreadsContinued": true}, true
	case "disconnect", "terminate":
		// 断开调试会话时释放暂停点，避免 CLI 永久等待。
		session.server.resumeAll(true)
		return map[string]any{}, true
	case "next":
		// next 只在当前或更浅调用深度的下一条不同源码行暂停。
		session.server.prepareStep(dapStepModeOver)
		session.server.resumeAll(false)
		return map[string]any{}, true
	case "stepIn":
		// stepIn 允许进入被调函数，在任意深度的下一条不同源码行暂停。
		session.server.prepareStep(dapStepModeInto)
		session.server.resumeAll(false)
		return map[string]any{}, true
	case "stepOut":
		// stepOut 必须等待当前调用帧返回到更浅深度后再暂停。
		session.server.prepareStep(dapStepModeOut)
		session.server.resumeAll(false)
		return map[string]any{}, true
	case "pause":
		// pause 会在下一条可见源码行暂停。
		session.server.requestPause()
		return map[string]any{}, true
	default:
		// 未列出的命令一律按未实现处理。
		return nil, false
	}
}

func (server *Server) configureClient(arguments json.RawMessage) {
	var request struct {
		LocalRoot   string `json:"localRoot"`
		RemoteRoot  string `json:"remoteRoot"`
		StopOnEntry bool   `json:"stopOnEntry"`
	}
	if len(arguments) > 0 {
		_ = json.Unmarshal(arguments, &request)
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	server.localRoot = canonicalDAPPath(request.LocalRoot)
	server.remoteRoot = canonicalDAPPath(request.RemoteRoot)
	server.stopOnEntry = request.StopOnEntry
}

// markConfigured 标记 DAP 客户端已经完成断点配置。
func (server *Server) markConfigured() {
	// 多个客户端或重复 configurationDone 只关闭一次通道。
	server.configOnce.Do(func() {
		close(server.configuredCh)
	})
}

// threads 返回当前暂停 State 可观测的主线程与 coroutine DAP 描述。
func (server *Server) threads() map[string]any {
	// 线程列表从当前暂停 State 读取；未暂停时仍保留兼容的 main 线程占位。
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.currentStop == nil || server.currentStop.State == nil {
		// 尚未绑定 State 时维持最小 DAP threads 响应。
		return map[string]any{"threads": []map[string]any{{"id": 1, "name": "main (not stopped)"}}}
	}
	threads := server.currentStop.State.Threads()
	result := make([]map[string]any, 0, len(threads))
	for _, thread := range threads {
		// nil 项不构成 Lua coroutine，不进入 DAP 列表。
		if thread == nil {
			continue
		}
		threadID := server.threadIDLocked(thread)
		name := "coroutine " + strconv.Itoa(threadID)
		if thread.IsMain() {
			// 主线程使用固定可读名称，便于 IDE 线程面板识别。
			name = "main"
		}
		result = append(result, map[string]any{"id": threadID, "name": name + " (" + string(thread.Status()) + ")"})
	}
	return map[string]any{"threads": result}
}

// threadIDLocked 为一个 runtime thread 分配稳定的 DAP 整数标识。
//
// 调用方必须持有 server.mu；线程对象生命周期由 State 管理，因此 server 仅保留调试会话期间的弱语义索引。
func (server *Server) threadIDLocked(thread *runtime.Thread) int {
	// 已分配线程复用同一 DAP id，保证前端刷新不抖动。
	if threadID, ok := server.threadIDs[thread]; ok {
		return threadID
	}
	threadID := server.nextThreadID
	server.nextThreadID++
	server.threadIDs[thread] = threadID
	return threadID
}

// evaluate 在暂停态解析无副作用的变量路径表达式。
//
// 支持标识符、点号 table 字段和字符串/整数 table 下标；不编译或执行 Lua 代码，避免 Hover、Watch 或
// Debug Console 改变被调试程序状态。解析失败按 DAP evaluate 约定返回字符串结果而非断开连接。
func (server *Server) evaluate(arguments json.RawMessage) map[string]any {
	var request struct {
		Expression string `json:"expression"`
	}
	if err := json.Unmarshal(arguments, &request); err != nil {
		// 结构错误返回可展示结果，避免简化客户端因错误请求失去调试会话。
		return map[string]any{"result": "<invalid evaluate arguments>", "variablesReference": 0}
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	value, err := server.evaluateLocked(request.Expression)
	if err != nil {
		// 不支持的表达式以文本反馈，不执行 fallback Lua chunk。
		return map[string]any{"result": "<" + err.Error() + ">", "variablesReference": 0}
	}
	return map[string]any{"result": value.DebugString(), "type": valueTypeName(value), "variablesReference": server.valueReferenceLocked(value)}
}

// evaluateLocked 解析并读取当前暂停帧可见的变量路径。
//
// 调用方必须持有 server.mu；表达式语法有意保持只读且最小化。
func (server *Server) evaluateLocked(expression string) (runtime.Value, error) {
	// 没有暂停点时不存在安全的变量可见性边界。
	if server.currentStop == nil {
		return runtime.NilValue(), errors.New("GLua is not stopped")
	}
	parts := strings.Split(strings.TrimSpace(expression), ".")
	if len(parts) == 0 || parts[0] == "" {
		return runtime.NilValue(), errors.New("expression must be a variable path")
	}
	value, ok := server.evaluateRootLocked(parts[0])
	if !ok {
		return runtime.NilValue(), fmt.Errorf("unknown variable %q", parts[0])
	}
	for _, part := range parts[1:] {
		// 点号后必须是非空字段，且仅允许 raw table 读取。
		if part == "" || value.Kind != runtime.KindTable {
			return runtime.NilValue(), errors.New("expression is not a readable table path")
		}
		table, tableOK := value.Ref.(*runtime.Table)
		if !tableOK || table == nil {
			return runtime.NilValue(), errors.New("expression references an invalid table")
		}
		value = table.RawGetString(part)
	}
	return value, nil
}

// evaluateRootLocked 按 Locals、Upvalues、Globals 优先级读取路径根变量。
//
// 调用方必须持有 server.mu；该顺序与 Lua 词法查找可观察部分保持一致。
func (server *Server) evaluateRootLocked(name string) (runtime.Value, bool) {
	for _, local := range server.currentStop.Variables {
		// 局部变量优先覆盖同名 upvalue 和全局。
		if local.Name == name {
			return local.Value, true
		}
	}
	for _, upvalue := range server.currentStopUpvaluesLocked() {
		// 当前 closure 的具名 upvalue 是第二层查找范围。
		if upvalue.name == name {
			return upvalue.value, true
		}
	}
	if server.currentStop.State != nil && server.currentStop.State.Globals() != nil {
		// 最后使用全局表 raw 读取，避免元方法产生副作用。
		value := server.currentStop.State.Globals().RawGetString(name)
		if !value.IsNil() {
			return value, true
		}
	}
	return runtime.NilValue(), false
}

// setBreakpoints 解析 DAP setBreakpoints 参数并更新 server 断点表。
//
// 参数解析失败时返回空列表，避免坏客户端请求导致 server panic；合法断点会以 verified=true 返回。
func (server *Server) setBreakpoints(arguments json.RawMessage) map[string]any {
	var request struct {
		Source struct {
			Path string `json:"path"`
			Name string `json:"name"`
		} `json:"source"`
		Breakpoints []struct {
			Line int `json:"line"`
		} `json:"breakpoints"`
		Lines []int `json:"lines"`
	}
	if len(arguments) > 0 {
		// DAP 客户端通常发送 source.path 与 breakpoints；解析失败会走空断点响应。
		_ = json.Unmarshal(arguments, &request)
	}
	source := server.sourcePathForRuntime(request.Source.Path)
	if source == "" {
		// path 缺失时回退 name，兼容少量只传 name 的客户端。
		source = server.sourcePathForRuntime(request.Source.Name)
	}
	lineSet := make(map[int]bool)
	breakpointResults := make([]map[string]any, 0, len(request.Breakpoints)+len(request.Lines))
	for _, breakpoint := range request.Breakpoints {
		// DAP 行号是一基；非法行不写入断点表。
		if breakpoint.Line <= 0 {
			continue
		}
		lineSet[breakpoint.Line] = true
		breakpointResults = append(breakpointResults, map[string]any{"verified": true, "line": breakpoint.Line})
	}
	for _, line := range request.Lines {
		// 旧客户端可能发送 lines 数组，保守兼容。
		if line <= 0 {
			continue
		}
		lineSet[line] = true
		breakpointResults = append(breakpointResults, map[string]any{"verified": true, "line": line})
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if source != "" {
		// setBreakpoints 是替换语义，同一 source 的旧断点必须被覆盖。
		server.breakpoints[source] = lineSet
	}
	return map[string]any{"breakpoints": breakpointResults}
}

// stackTrace 返回当前暂停点的完整可用调用栈快照。
//
// Lua 帧会从 closure Proto 读取 source 和行号；Go 帧没有稳定源码位置时只返回名称，避免 IDE 跳到错误文件。
func (server *Server) stackTrace(arguments json.RawMessage) map[string]any {
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.currentStop == nil {
		// 未暂停时没有可展示帧。
		return map[string]any{"stackFrames": []any{}, "totalFrames": 0}
	}
	requestedThreadID := parseDAPThreadID(arguments)
	activeThreadID := server.threadIDLocked(server.currentStop.Thread)
	frames := server.currentStop.Frames
	stopSource := server.currentStop.Source
	stopLine := server.currentStop.Line
	if requestedThreadID > 0 && requestedThreadID != activeThreadID && server.currentStop.State != nil {
		// 选择已挂起 coroutine 时读取其 yield 保存的 traceback，而不伪造当前执行位置。
		frames = nil
		stopSource = ""
		stopLine = 0
		for _, thread := range server.currentStop.State.Threads() {
			if thread != nil && server.threadIDLocked(thread) == requestedThreadID {
				frames = thread.TracebackFrames()
				break
			}
		}
	}
	if len(frames) == 0 {
		// 顶层 chunk 在没有显式调用帧时仍需返回当前暂停位置。
		frames = []runtime.CallFrame{{Kind: runtime.CallFrameKindLua, Name: "main"}}
	}
	result := make([]map[string]any, 0, len(frames))
	for index, frame := range frames {
		// 当前顶层帧以断点位置为准，其余帧使用各自 Proto 调试信息。
		frameSource, frameLine := dapCallFrameLocation(frame)
		if index == 0 {
			frameSource = stopSource
			frameLine = stopLine
		}
		name := frame.Name
		if name == "" {
			// 缺少 debug 名称时区分 Lua/Go 帧，仍保证变量窗口可读。
			if frame.Kind == runtime.CallFrameKindGo {
				name = "[Go function]"
			} else {
				name = "[Lua function]"
			}
		}
		entry := map[string]any{"id": index + 1, "name": name, "line": frameLine, "column": 1}
		if frameSource != "" {
			// 仅在存在真实 source 时提供跳转目标。
			frameSource = mapDAPSourcePath(frameSource, server.remoteRoot, server.localRoot)
			entry["source"] = map[string]any{"name": path.Base(canonicalDAPPath(frameSource)), "path": frameSource}
		}
		result = append(result, entry)
	}
	return map[string]any{"stackFrames": result, "totalFrames": len(result)}
}

// parseDAPThreadID 解析 DAP stackTrace 请求中的可选线程标识。
//
// 参数缺失或损坏时返回 0，调用方按当前暂停线程处理，兼容未传 threadId 的简化客户端。
func parseDAPThreadID(arguments json.RawMessage) int {
	// 空参数没有指定线程。
	if len(arguments) == 0 {
		// 0 表示使用当前暂停线程。
		return 0
	}
	var request struct {
		ThreadID int `json:"threadId"`
	}
	if err := json.Unmarshal(arguments, &request); err != nil {
		// 损坏参数不应中断整个调试会话。
		return 0
	}
	return request.ThreadID
}

// dapCallFrameLocation 从 Lua closure Proto 推导 DAP 可展示的位置。
func dapCallFrameLocation(frame runtime.CallFrame) (string, int) {
	// Go closure 不含 Lua Proto，不能伪造 source 或行号。
	if frame.Function.Kind != runtime.KindLuaClosure {
		return "", 0
	}
	closure, _ := frame.Function.Ref.(*runtime.LuaClosure)
	if closure == nil || closure.Proto == nil {
		// 损坏或 stripped closure 没有可定位调试信息。
		return "", 0
	}
	source := normalizeSourcePath(closure.Proto.Source, "")
	if frame.CurrentPC < 0 || frame.CurrentPC >= len(closure.Proto.LineInfo) {
		// 缺少当前 PC 行号时仍返回 source，方便 IDE 展示调用关系。
		return source, 0
	}
	return source, closure.Proto.LineInfo[frame.CurrentPC]
}

// scopes 返回当前暂停帧的 DAP 变量作用域。
//
// 当前实现暴露局部变量、当前 Lua closure 的 upvalue 和全局环境；固定 scope 引用不会与 table 展开引用冲突。
func (server *Server) scopes() map[string]any {
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.currentStop == nil {
		// 未暂停时没有可展示 scope。
		return map[string]any{"scopes": []any{}}
	}
	scopes := make([]map[string]any, 0, 3)
	if len(server.currentStop.Variables) > 0 {
		// 仅在存在可见具名局部变量时展示 Locals scope。
		scopes = append(scopes, map[string]any{"name": "Locals", "variablesReference": dapLocalsVariablesReference, "expensive": false})
	}
	if len(server.currentStopUpvaluesLocked()) > 0 {
		// 当前 Lua closure 有捕获值时展示独立 Upvalues scope。
		scopes = append(scopes, map[string]any{"name": "Upvalues", "variablesReference": dapUpvaluesVariablesReference, "expensive": false})
	}
	if server.currentStop.State != nil && server.currentStop.State.Globals() != nil {
		// 全局环境是可读写 Lua table，复用现有 variables/table 展开和 setVariable 路径。
		globalValue := runtime.ReferenceValue(runtime.KindTable, server.currentStop.State.Globals())
		scopes = append(scopes, map[string]any{"name": "Globals", "variablesReference": server.valueReferenceLocked(globalValue), "expensive": true})
	}
	return map[string]any{"scopes": scopes}
}

// variables 返回当前暂停点保存的局部变量快照。
//
// 固定引用分别表示 Locals 和 Upvalues scope；table 值会分配独立 reference，客户端展开 table 时会用该
// reference 再次请求子项。未知 reference 返回空列表，保持 DAP 客户端请求幂等且不会 panic。
func (server *Server) variables(arguments json.RawMessage) map[string]any {
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.currentStop == nil {
		// 未暂停时没有变量。
		return map[string]any{"variables": []any{}}
	}
	reference := parseVariablesReference(arguments)
	if reference <= 0 {
		// 部分简化客户端不会携带 arguments；兼容为 Locals scope。
		reference = dapLocalsVariablesReference
	}
	if reference == dapUpvaluesVariablesReference {
		// Upvalues 使用当前顶层 Lua closure 的实时值快照。
		return map[string]any{"variables": server.upvalueVariablesLocked()}
	}
	if reference != dapLocalsVariablesReference {
		// 非 Locals reference 表示展开 table 子项。
		return map[string]any{"variables": server.tableVariablesLocked(reference)}
	}
	values := make([]map[string]any, 0, len(server.currentStop.Variables))
	for index := range server.currentStop.Variables {
		local := server.currentStop.Variables[index]
		values = append(values, map[string]any{
			"name":               local.Name,
			"value":              local.Value.DebugString(),
			"type":               valueTypeName(local.Value),
			"variablesReference": server.valueReferenceLocked(local.Value),
		})
	}
	return map[string]any{"variables": values}
}

// dapUpvalue 保存 DAP Upvalues scope 需要展示和写回的一项闭包捕获值。
type dapUpvalue struct {
	// name 是 Proto 调试信息中的 upvalue 名称，缺失时使用稳定下标名称。
	name string
	// index 是 Lua closure 中的零基 upvalue 下标。
	index int
	// value 是当前 cell 或 legacy Upvalues 切片中的实时值。
	value runtime.Value
	// closure 是提供该 upvalue 的当前 Lua closure。
	closure *runtime.LuaClosure
}

// currentStopUpvaluesLocked 从当前暂停点的顶层 Lua 调用帧提取可见 upvalue。
//
// 调用方必须持有 server.mu；Go closure 没有统一的可写 upvalue 模型，因此不会伪装成 Lua Upvalues scope。
func (server *Server) currentStopUpvaluesLocked() []dapUpvalue {
	// 没有暂停帧时不存在可读 upvalue。
	if server.currentStop == nil || len(server.currentStop.Frames) == 0 {
		// 顶层 chunk 未建立调用帧时安全返回空集合。
		return nil
	}
	function := server.currentStop.Frames[0].Function
	if function.Kind != runtime.KindLuaClosure {
		// Go 或损坏帧不提供 Lua closure upvalue。
		return nil
	}
	closure, ok := function.Ref.(*runtime.LuaClosure)
	if !ok || closure == nil {
		// 损坏 closure 引用不能进入变量面板。
		return nil
	}
	count := len(closure.Upvalues)
	if len(closure.UpvalueCells) > count {
		// 已捕获 cell 的数量可能超过旧快照切片，保留全部可见值。
		count = len(closure.UpvalueCells)
	}
	upvalues := make([]dapUpvalue, 0, count)
	for index := 0; index < count; index++ {
		// 缺少名称时使用 Lua debug 库兼容的稳定占位名称。
		name := fmt.Sprintf("upvalue_%d", index+1)
		if closure.Proto != nil && index < len(closure.Proto.Upvalues) && closure.Proto.Upvalues[index].Name != "" {
			name = closure.Proto.Upvalues[index].Name
		}
		value := runtime.NilValue()
		if index < len(closure.UpvalueCells) && closure.UpvalueCells[index] != nil {
			// 共享 cell 优先返回当前值，避免展示创建 closure 时的旧快照。
			value = closure.UpvalueCells[index].Value()
		} else if index < len(closure.Upvalues) {
			// legacy closure 没有 cell 时保留原始切片兼容。
			value = closure.Upvalues[index]
		}
		upvalues = append(upvalues, dapUpvalue{name: name, index: index, value: value, closure: closure})
	}
	return upvalues
}

// upvalueVariablesLocked 把当前 Lua closure upvalue 转换为 DAP 变量树节点。
//
// 调用方必须持有 server.mu；table upvalue 使用现有展开引用机制。
func (server *Server) upvalueVariablesLocked() []map[string]any {
	// 统一从当前暂停点读取实时 upvalue，避免长期缓存已失效 closure。
	upvalues := server.currentStopUpvaluesLocked()
	values := make([]map[string]any, 0, len(upvalues))
	for _, upvalue := range upvalues {
		// 每个捕获值沿用 Locals 的类型、调试展示和 table 展开语义。
		values = append(values, map[string]any{
			"name":               upvalue.name,
			"value":              upvalue.value.DebugString(),
			"type":               valueTypeName(upvalue.value),
			"variablesReference": server.valueReferenceLocked(upvalue.value),
		})
	}
	return values
}

// parseVariablesReference 解析 DAP variables 请求的 variablesReference。
//
// arguments 缺失或非法时返回 0，让调用方按兼容策略处理；该函数不把坏客户端请求升级为连接错误。
func parseVariablesReference(arguments json.RawMessage) int {
	// 空 arguments 兼容旧测试和简化客户端。
	if len(arguments) == 0 {
		return 0
	}
	var request struct {
		VariablesReference int `json:"variablesReference"`
	}
	if err := json.Unmarshal(arguments, &request); err != nil {
		// 无法解析时返回 0，避免 DAP 客户端一条坏请求导致调试中断。
		return 0
	}
	return request.VariablesReference
}

// tableVariablesLocked 返回指定 table reference 的子变量。
//
// 调用方必须持有 server.mu；table 内容按 runtime.RawNext 的稳定顺序展开，最多返回 200 项，避免大表在
// IDE 变量树中一次性拉取过多数据。
func (server *Server) tableVariablesLocked(reference int) []map[string]any {
	// reference 不存在或不是 table 时返回空列表，保持 DAP variables 的幂等行为。
	value, ok := server.variableValues[reference]
	if !ok || value.Kind != runtime.KindTable {
		return []map[string]any{}
	}
	table, ok := value.Ref.(*runtime.Table)
	if !ok || table == nil {
		// 损坏的 table 引用不能展开，返回空子项。
		return []map[string]any{}
	}
	values := make([]map[string]any, 0)
	key := runtime.NilValue()
	for count := 0; count < 200; count++ {
		// RawNext 使用 Lua raw 迭代语义，适合作为调试器的 table 展示基础。
		nextKey, nextValue, ok, err := table.RawNext(key)
		if err != nil || !ok {
			// 迭代结束或异常时停止展开；调试展示不应影响 VM 执行。
			break
		}
		values = append(values, map[string]any{
			"name":               dapVariableKeyName(nextKey),
			"value":              nextValue.DebugString(),
			"type":               valueTypeName(nextValue),
			"variablesReference": server.valueReferenceWithTableKeyLocked(nextValue, nextKey),
		})
		key = nextKey
	}
	return values
}

// setVariable 修改当前暂停点中的一个变量。
//
// 固定引用表示 Locals 或 Upvalues scope，name 按变量名匹配；其它 reference 表示 table 子项。
// value 只解析 nil、boolean、number 和 string 字面量；无法解析的文本按普通 string 写入。
func (server *Server) setVariable(arguments json.RawMessage) (map[string]any, error) {
	var request struct {
		VariablesReference int    `json:"variablesReference"`
		Name               string `json:"name"`
		Value              string `json:"value"`
	}
	if len(arguments) == 0 {
		// DAP setVariable 必须提供 arguments。
		return nil, errors.New("setVariable arguments are required")
	}
	if err := json.Unmarshal(arguments, &request); err != nil {
		// 参数结构错误时返回给客户端展示。
		return nil, fmt.Errorf("invalid setVariable arguments: %w", err)
	}
	value, err := parseDAPVariableValue(request.Value)
	if err != nil {
		// 当前解析器理论上不会对普通文本报错，保留错误路径便于后续扩展。
		return nil, err
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.currentStop == nil {
		// 未暂停时没有可写变量目标。
		return nil, errors.New("no stopped GLua frame is available")
	}
	reference := request.VariablesReference
	if reference <= 0 {
		// 少量客户端可能缺省 reference，按 Locals scope 兼容。
		reference = dapLocalsVariablesReference
	}
	if reference == dapLocalsVariablesReference {
		// Locals scope 写回 VM 寄存器。
		return server.setLocalVariableLocked(request.Name, value)
	}
	if reference == dapUpvaluesVariablesReference {
		// Upvalues scope 写回当前 Lua closure 的捕获值或共享 cell。
		return server.setUpvalueVariableLocked(request.Name, value)
	}
	return server.setTableVariableLocked(reference, request.Name, value)
}

// setUpvalueVariableLocked 写回当前暂停 Lua closure 的一个具名 upvalue。
//
// 调用方必须持有 server.mu；共享 cell 存在时更新 cell，否则更新 legacy Upvalues 快照。
func (server *Server) setUpvalueVariableLocked(name string, value runtime.Value) (map[string]any, error) {
	// 通过当前暂停帧重新定位 upvalue，避免变量树缓存覆盖函数调用后的 closure。
	for _, upvalue := range server.currentStopUpvaluesLocked() {
		if upvalue.name != name {
			// 名称不匹配时继续查找。
			continue
		}
		if upvalue.index < len(upvalue.closure.UpvalueCells) && upvalue.closure.UpvalueCells[upvalue.index] != nil {
			// 共享 cell 写回会同时影响引用该 cell 的其他 closure。
			upvalue.closure.UpvalueCells[upvalue.index].Set(value)
		} else {
			// legacy closure 使用独立切片保存 upvalue，必要时扩容保持索引一致。
			for len(upvalue.closure.Upvalues) <= upvalue.index {
				upvalue.closure.Upvalues = append(upvalue.closure.Upvalues, runtime.NilValue())
			}
			upvalue.closure.Upvalues[upvalue.index] = value
		}
		return server.setVariableResponseLocked(value), nil
	}
	return nil, fmt.Errorf("upvalue %q is not available", name)
}

// setLocalVariableLocked 写回当前暂停帧中的局部变量。
func (server *Server) setLocalVariableLocked(name string, value runtime.Value) (map[string]any, error) {
	if server.currentStop.VM == nil {
		// 缺少 VM 指针时只能展示快照，不能写回。
		return nil, errors.New("current GLua frame is read-only")
	}
	for index := range server.currentStop.Variables {
		local := server.currentStop.Variables[index]
		if local.Name != name {
			// 名称不匹配时继续查找。
			continue
		}
		if local.Const {
			// const local 不允许调试器绕过编译期只读约束。
			return nil, fmt.Errorf("cannot assign to const binding '%s'", name)
		}
		if err := server.currentStop.VM.SetRegister(local.Register, value); err != nil {
			// 寄存器写回失败说明调试信息或 VM 状态已失效。
			return nil, err
		}
		server.currentStop.Variables[index].Value = value
		return server.setVariableResponseLocked(value), nil
	}
	return nil, fmt.Errorf("variable %q is not available", name)
}

// setTableVariableLocked 写回 table 展开项。
func (server *Server) setTableVariableLocked(reference int, name string, value runtime.Value) (map[string]any, error) {
	tableValue, ok := server.variableValues[reference]
	if !ok || tableValue.Kind != runtime.KindTable {
		// 非 table reference 不支持写入。
		return nil, errors.New("variablesReference does not point to a GLua table")
	}
	table, ok := tableValue.Ref.(*runtime.Table)
	if !ok || table == nil {
		// 损坏 table 引用不可写。
		return nil, errors.New("variablesReference points to an invalid GLua table")
	}
	key, ok := server.variableTableKeys[reference]
	if !ok {
		parsedKey, parsedOK := parseDAPVariableKeyName(name)
		if !parsedOK {
			// 只能写回由变量树生成的 string/int key。
			return nil, fmt.Errorf("table variable %q cannot be edited", name)
		}
		key = parsedKey
	}
	if err := table.RawSet(key, value); err != nil {
		// RawSet 会保留 `_glua_const` 字段写保护和 key 合法性错误。
		return nil, err
	}
	return server.setVariableResponseLocked(value), nil
}

// setVariableResponseLocked 构造 DAP setVariable 成功响应。
func (server *Server) setVariableResponseLocked(value runtime.Value) map[string]any {
	return map[string]any{
		"value":              value.DebugString(),
		"type":               valueTypeName(value),
		"variablesReference": server.valueReferenceLocked(value),
	}
}

// valueReferenceLocked 为可展开值分配 DAP variablesReference。
//
// 调用方必须持有 server.mu；当前只有 table 可展开，同一个 table 指针会复用同一 reference，避免循环 table
// 每次展开都生成新节点。
func (server *Server) valueReferenceLocked(value runtime.Value) int {
	// 只有 table 拥有子项，其它值返回 0。
	if value.Kind != runtime.KindTable {
		return 0
	}
	table, ok := value.Ref.(*runtime.Table)
	if !ok || table == nil {
		// 损坏或 nil table 引用不可展开。
		return 0
	}
	if reference, ok := server.variableTables[table]; ok {
		// 已经注册过的 table 复用 reference，减少循环结构展开噪音。
		return reference
	}
	reference := server.nextVariableID
	server.nextVariableID++
	server.variableTables[table] = reference
	server.variableValues[reference] = value
	return reference
}

// valueReferenceWithTableKeyLocked 为 table 子项分配 reference，并记录该子项对应的父 table key。
func (server *Server) valueReferenceWithTableKeyLocked(value runtime.Value, key runtime.Value) int {
	reference := server.valueReferenceLocked(value)
	if reference > 0 {
		// table 子项可展开时记录 key，便于 IDE 对展开节点发起 setVariable。
		server.variableTableKeys[reference] = key
	}
	return reference
}

// clearVariableReferencesLocked 清空当前暂停点的 table 展开缓存。
//
// 调用方必须持有 server.mu；每次恢复或新暂停都会重建缓存，确保变量树反映最新 VM 状态。
func (server *Server) clearVariableReferencesLocked() {
	// 固定 scope 已占用 Locals/Upvalues 两个引用，因此 table 从 3 开始分配。
	server.nextVariableID = dapFirstDynamicVariablesReference
	server.variableValues = make(map[int]runtime.Value)
	server.variableTables = make(map[*runtime.Table]int)
	server.variableTableKeys = make(map[int]runtime.Value)
}

// dapVariableKeyName 把 Lua table key 转成 IDE 变量树名称。
//
// 字符串 key 用 Lua 字段访问常见形态展示；integer key 使用数组下标；其它 key 回退 DebugString。
func dapVariableKeyName(key runtime.Value) string {
	// 按 Lua key 类型生成更直观的变量名。
	switch key.Kind {
	case runtime.KindString:
		// 字符串 key 展示成 ["name"]，避免与局部变量名混淆。
		return "[" + strconv.Quote(key.String) + "]"
	case runtime.KindInteger:
		// 整数 key 展示成数组下标。
		return "[" + strconv.FormatInt(key.Integer, 10) + "]"
	default:
		// 其它合法 table key 使用稳定调试文本。
		return "[" + key.DebugString() + "]"
	}
}

// parseDAPVariableKeyName 把 IDE 变量树名称还原为 Lua table key。
func parseDAPVariableKeyName(name string) (runtime.Value, bool) {
	trimmed := strings.TrimSpace(name)
	if len(trimmed) < 3 || !strings.HasPrefix(trimmed, "[") || !strings.HasSuffix(trimmed, "]") {
		// 只接受 variables 响应中生成的 [key] 形态。
		return runtime.NilValue(), false
	}
	inner := strings.TrimSpace(trimmed[1 : len(trimmed)-1])
	if strings.HasPrefix(inner, "\"") && strings.HasSuffix(inner, "\"") {
		value, err := strconv.Unquote(inner)
		if err != nil {
			// 字符串 key 反解析失败时拒绝写入，避免误写其它字段。
			return runtime.NilValue(), false
		}
		return runtime.StringValue(value), true
	}
	integer, err := strconv.ParseInt(inner, 10, 64)
	if err == nil {
		// 整数 key 按 Lua integer 写回。
		return runtime.IntegerValue(integer), true
	}
	return runtime.NilValue(), false
}

// parseDAPVariableValue 解析 DAP setVariable 的文本值。
func parseDAPVariableValue(text string) (runtime.Value, error) {
	trimmed := strings.TrimSpace(text)
	switch trimmed {
	case "nil":
		// nil 表示删除 table 字段或把 local 置 nil。
		return runtime.NilValue(), nil
	case "true":
		// true 映射为 Lua boolean。
		return runtime.BooleanValue(true), nil
	case "false":
		// false 映射为 Lua boolean。
		return runtime.BooleanValue(false), nil
	}
	if strings.HasPrefix(trimmed, "\"") && strings.HasSuffix(trimmed, "\"") {
		value, err := strconv.Unquote(trimmed)
		if err != nil {
			// 双引号字符串必须符合 Go/Lua 常见转义边界。
			return runtime.NilValue(), err
		}
		return runtime.StringValue(value), nil
	}
	if len(trimmed) >= 2 && strings.HasPrefix(trimmed, "'") && strings.HasSuffix(trimmed, "'") {
		// 单引号文本作为普通字符串处理，暂不执行 Lua 表达式。
		return runtime.StringValue(trimmed[1 : len(trimmed)-1]), nil
	}
	if integer, err := strconv.ParseInt(trimmed, 10, 64); err == nil {
		// 十进制整数优先保留 Lua integer。
		return runtime.IntegerValue(integer), nil
	}
	if number, err := strconv.ParseFloat(trimmed, 64); err == nil {
		// 其它数字文本按 Lua number 写入。
		return runtime.NumberValue(number), nil
	}
	return runtime.StringValue(text), nil
}

// prepareStep 设置一次带调用深度边界的单步请求。
//
// 如果当前已暂停，则记录当前 source、行号和调用深度；如果未暂停，则退化为下一条可见源码行暂停。
func (server *Server) prepareStep(mode dapStepMode) {
	server.mu.Lock()
	defer server.mu.Unlock()
	server.stepMode = mode
	if server.currentStop != nil {
		// 从当前暂停位置开始，避免刚 resume 就停在同一源码行。
		server.stepSource = server.currentStop.Source
		server.stepLine = server.currentStop.Line
		server.stepDepth = server.currentStop.Depth
		return
	}
	server.stepSource = ""
	server.stepLine = 0
	server.stepDepth = 0
}

// requestPause 请求在下一条可见源码行暂停。
func (server *Server) requestPause() {
	server.mu.Lock()
	defer server.mu.Unlock()
	server.pauseRequested = true
}

// resumeAll 释放当前暂停的 VM。
//
// 没有暂停态时保持无副作用；关闭旧 channel 后立即换新，供下一次断点暂停等待。
func (server *Server) resumeAll(clearPendingStep bool) {
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.stopped && server.currentStop != nil {
		// 继续执行时先跳过当前源码行上的同一个断点，避免同一行多条 VM 指令反复暂停。
		server.skipBreakpointSource = server.currentStop.Source
		server.skipBreakpointLine = server.currentStop.Line
	}
	if clearPendingStep {
		// 用户选择普通继续或结束会话时，必须取消此前未消费的单步/暂停请求，避免恢复后又被旧 step 拉回下一行。
		server.stepMode = dapStepModeNone
		server.stepSource = ""
		server.stepLine = 0
		server.stepDepth = 0
		server.pauseRequested = false
	}
	if !server.stopped {
		// 当前没有 VM 等待 continue。
		return
	}
	close(server.continueCh)
	server.continueCh = make(chan struct{})
	server.stopped = false
	server.currentStop = nil
	server.clearVariableReferencesLocked()
}

// BeforeInstruction 在每条 Lua 指令执行前检查 DAP 断点。
//
// 命中断点且有活动 DAP 会话时会发送 stopped 事件并阻塞，直到客户端发送 continue/disconnect 或
// State context 取消。该方法实现 runtime.DebugObserver。
func (server *Server) BeforeInstruction(state *runtime.State, vm *runtime.VM, proto *bytecode.Proto, pc int) error {
	if server == nil || state == nil || proto == nil || pc < 0 || pc >= len(proto.LineInfo) {
		// 缺少上下文或 PC 无行号时不能匹配源码断点。
		return nil
	}
	line := proto.LineInfo[pc]
	if line <= 0 {
		// 非正行号不是用户可见源码行。
		return nil
	}
	source := server.sourcePathForRuntime(normalizeSourcePath(proto.Source, server.sourceRoot))
	if source == "" {
		// 缺少源码路径时不能匹配断点或步进。
		return nil
	}
	reason := ""
	if server.consumeEntryStop() {
		reason = "entry"
	} else if server.hasBreakpoint(source, line) {
		// 命中用户设置的断点。
		reason = "breakpoint"
	} else if server.consumeStepStop(source, line, state.CallDepth()) {
		// 命中 step/pause 请求。
		reason = "step"
	}
	if reason == "" {
		// 没有命中断点或步进请求。
		return nil
	}
	variables := []runtime.ActiveLocalSnapshot(nil)
	if vm != nil {
		// 暂停前复制当前活动局部变量，供 DAP variables 请求读取。
		variables = vm.ActiveLocalSnapshots()
	}
	thread, _ := state.Running()
	return server.pauseAt(state, stoppedLocation{Source: source, Line: line, PC: pc, Reason: reason, Variables: variables, VM: vm, Depth: state.CallDepth(), Frames: state.TracebackFrames(), State: state, Thread: thread})
}

func (server *Server) consumeEntryStop() bool {
	server.mu.Lock()
	defer server.mu.Unlock()
	if !server.stopOnEntry {
		return false
	}
	server.stopOnEntry = false
	return true
}

// consumeStepStop 判断当前 source:line 是否满足步进或 pause 请求。
//
// 返回 true 时会消费本次 step/pause 状态；行级 step 会跳过恢复前所在的同一源码行。
func (server *Server) consumeStepStop(source string, line int, depth int) bool {
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.pauseRequested {
		// 用户显式暂停时，下一条可见源码行立即停住。
		server.pauseRequested = false
		return true
	}
	if server.stepMode == dapStepModeNone {
		// 没有步进请求时不暂停。
		return false
	}
	if server.stepSource == source && server.stepLine == line {
		// 仍在恢复前同一行，继续执行到下一条可见行。
		return false
	}
	switch server.stepMode {
	case dapStepModeInto:
		// stepIn 在任意调用深度的下一条不同源码行停止。
	case dapStepModeOver:
		// stepOver 必须忽略更深被调函数内部的源码行。
		if depth > server.stepDepth {
			return false
		}
	case dapStepModeOut:
		// stepOut 只有在当前调用帧已经返回后才停止。
		if depth >= server.stepDepth {
			return false
		}
	default:
		// 未知模式不应静默暂停，清理状态并继续执行。
		server.stepMode = dapStepModeNone
		server.stepSource = ""
		server.stepLine = 0
		server.stepDepth = 0
		return false
	}
	server.stepMode = dapStepModeNone
	server.stepSource = ""
	server.stepLine = 0
	server.stepDepth = 0
	return true
}

// hasBreakpoint 判断 source:line 是否命中已保存断点。
//
// source 允许与断点路径精确相等，也允许在相对路径和绝对路径之间用后缀匹配兜底。
func (server *Server) hasBreakpoint(source string, line int) bool {
	server.mu.Lock()
	defer server.mu.Unlock()
	if server.skipBreakpointSource != "" {
		if server.skipBreakpointSource == source && server.skipBreakpointLine == line {
			// 刚从同一源码行断点恢复，同一行后续指令不再重复暂停。
			return false
		}
		// 已离开恢复行，重新允许后续断点命中。
		server.skipBreakpointSource = ""
		server.skipBreakpointLine = 0
	}
	for breakpointSource, lines := range server.breakpoints {
		// 精确路径或后缀路径匹配都视为同一文件，兼容 CLI 相对路径与 IDE 绝对路径差异。
		canonicalBreakpoint := canonicalDAPPath(breakpointSource)
		canonicalSource := canonicalDAPPath(source)
		if canonicalBreakpoint == canonicalSource || strings.HasSuffix(canonicalSource, "/"+canonicalBreakpoint) || strings.HasSuffix(canonicalBreakpoint, "/"+canonicalSource) {
			return lines[line]
		}
	}
	return false
}

// pauseAt 发送 stopped 事件并等待客户端继续。
//
// 如果没有活动会话，断点会被忽略；如果 State context 取消，返回对应错误以中断 VM。
func (server *Server) pauseAt(state *runtime.State, location stoppedLocation) error {
	server.mu.Lock()
	session := server.activeSession
	if session == nil {
		// 没有 IDE 连接时不暂停，避免命令行执行被断点配置卡住。
		server.mu.Unlock()
		return nil
	}
	server.currentStop = &location
	threadID := 1
	if location.Thread != nil {
		// 当前执行 coroutine 使用与 threads 请求相同的稳定 DAP id。
		threadID = server.threadIDLocked(location.Thread)
	}
	server.clearVariableReferencesLocked()
	server.stopped = true
	continueCh := server.continueCh
	session.write(eventMessage{
		Seq:   session.nextSeq(),
		Type:  "event",
		Event: "stopped",
		Body: map[string]any{
			"reason":            location.Reason,
			"threadId":          threadID,
			"allThreadsStopped": true,
		},
	})
	server.mu.Unlock()

	select {
	case <-continueCh:
		// 客户端已发送 continue/disconnect，VM 可以继续执行。
		return nil
	case <-state.Context().Done():
		// 宿主取消时停止等待，把取消错误交给 VM 统一包装。
		return state.CheckContext()
	}
}

// normalizeSourcePath 归一化 DAP 与 Lua Proto 中的源码路径。
//
// Lua chunk source 通常以 @ 开头；DAP source.path 通常为文件系统路径。baseDir 非空且 source 是相对文件时，
// 会优先解析为可供编辑器跳转的绝对路径；空路径保持空字符串。
func normalizeSourcePath(source string, baseDir string) string {
	// 先移除 Lua 文件 chunk 的 @ 前缀，并清理首尾空白。
	trimmed := strings.TrimSpace(strings.TrimPrefix(source, "@"))
	if trimmed == "" {
		// 空 source 无法匹配断点。
		return ""
	}
	if portable := canonicalDAPPath(trimmed); strings.HasPrefix(portable, "/") {
		// Unix and Android absolute paths must remain absolute even when tests or
		// editor adapters run on Windows.
		return portable
	}
	cleaned := filepath.Clean(trimmed)
	if filepath.IsAbs(cleaned) {
		// 绝对路径已经足以让 IDE 定位源码，只需要标准化分隔符与冗余路径段。
		return cleaned
	}
	if strings.TrimSpace(baseDir) == "" {
		// 没有可用基准目录时只能保留相对路径，后续断点匹配仍可用后缀兜底。
		return cleaned
	}
	absolute := filepath.Join(baseDir, cleaned)
	if _, err := os.Stat(absolute); err == nil {
		// 文件存在时返回绝对路径，VSCode 等直连 DAP 客户端才能自动打开正确文件。
		return filepath.Clean(absolute)
	}
	if absolutePath, err := filepath.Abs(absolute); err == nil {
		// 文件暂不存在时也尽量返回绝对路径，避免客户端把相对路径解析到错误目录。
		return filepath.Clean(absolutePath)
	}
	return cleaned
}

func canonicalDAPPath(value string) string {
	trimmed := strings.TrimSpace(strings.TrimPrefix(value, "@"))
	if trimmed == "" {
		return ""
	}
	return path.Clean(strings.ReplaceAll(trimmed, "\\", "/"))
}

func relativeDAPPath(value, root string) (string, bool) {
	value = canonicalDAPPath(value)
	root = strings.TrimSuffix(canonicalDAPPath(root), "/")
	if value == "" || root == "" {
		return "", false
	}
	if strings.EqualFold(value, root) {
		return "", true
	}
	prefix := root + "/"
	if len(value) > len(prefix) && strings.EqualFold(value[:len(prefix)], prefix) {
		return value[len(prefix):], true
	}
	return "", false
}

func joinDAPPath(root, relative string) string {
	root = strings.TrimSuffix(canonicalDAPPath(root), "/")
	relative = strings.TrimPrefix(canonicalDAPPath(relative), "/")
	if relative == "" || relative == "." {
		return root
	}
	if root == "" {
		return relative
	}
	return root + "/" + relative
}

func (server *Server) sourcePathForRuntime(source string) string {
	cleaned := canonicalDAPPath(source)
	server.mu.Lock()
	localRoot, remoteRoot := server.localRoot, server.remoteRoot
	server.mu.Unlock()
	if mapped := mapDAPSourcePath(cleaned, localRoot, remoteRoot); mapped != cleaned {
		return mapped
	}
	if cleaned != "" && remoteRoot != "" && !strings.Contains(cleaned, ":/") && !strings.HasPrefix(cleaned, "/") {
		return joinDAPPath(remoteRoot, cleaned)
	}
	return source
}

func (server *Server) sourcePathForClient(source string) string {
	cleaned := canonicalDAPPath(source)
	server.mu.Lock()
	localRoot, remoteRoot := server.localRoot, server.remoteRoot
	server.mu.Unlock()
	return mapDAPSourcePath(cleaned, remoteRoot, localRoot)
}

func mapDAPSourcePath(source, fromRoot, toRoot string) string {
	cleaned := canonicalDAPPath(source)
	if relative, ok := relativeDAPPath(cleaned, fromRoot); ok && canonicalDAPPath(toRoot) != "" {
		return joinDAPPath(toRoot, relative)
	}
	return source
}

// valueTypeName 返回 DAP 变量展示使用的 Lua 类型名称。
func valueTypeName(value runtime.Value) string {
	// 按运行时值类型映射到 Lua 用户可理解的类型名。
	switch value.Kind {
	case runtime.KindNil:
		// nil 类型直接展示 nil。
		return "nil"
	case runtime.KindBoolean:
		// boolean 类型展示 boolean。
		return "boolean"
	case runtime.KindInteger, runtime.KindNumber:
		// Lua 5.3 integer/float 都属于 number 类型。
		return "number"
	case runtime.KindString:
		// 字符串类型展示 string。
		return "string"
	case runtime.KindTable:
		// table 引用展示 table。
		return "table"
	case runtime.KindLuaClosure, runtime.KindGoClosure:
		// Lua 和 Go closure 对用户都表现为 function。
		return "function"
	case runtime.KindUserdata:
		// userdata 引用展示 userdata。
		return "userdata"
	case runtime.KindThread:
		// coroutine/thread 引用展示 thread。
		return "thread"
	default:
		// 未知扩展类型保守展示 value。
		return "value"
	}
}

// readMessage 从 reader 读取一个 DAP Content-Length 帧。
//
// 返回错误表示连接结束、header 缺失或 body 不合法；调用方应关闭当前客户端连接。
func readMessage(reader *bufio.Reader) (protocolMessage, error) {
	contentLength := -1
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			// 读取 header 失败通常表示客户端断开。
			return protocolMessage{}, err
		}
		trimmed := strings.TrimRight(line, "\r\n")
		if trimmed == "" {
			// 空行表示 header 结束。
			break
		}
		name, value, ok := strings.Cut(trimmed, ":")
		if !ok {
			// 非法 header 直接拒绝该帧。
			return protocolMessage{}, fmt.Errorf("invalid DAP header %q", trimmed)
		}
		if strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
			// Content-Length 是 DAP 帧 body 长度，必须是非负整数。
			length, err := strconv.Atoi(strings.TrimSpace(value))
			if err != nil || length < 0 {
				return protocolMessage{}, fmt.Errorf("invalid DAP Content-Length %q", value)
			}
			contentLength = length
		}
	}
	if contentLength < 0 {
		// 缺少 Content-Length 时无法确定 body 边界。
		return protocolMessage{}, errors.New("missing DAP Content-Length")
	}
	body := make([]byte, contentLength)
	if _, err := io.ReadFull(reader, body); err != nil {
		// body 不完整说明连接已坏。
		return protocolMessage{}, err
	}
	var message protocolMessage
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(&message); err != nil {
		// JSON 无法解析时返回坏帧错误。
		return protocolMessage{}, err
	}
	return message, nil
}
