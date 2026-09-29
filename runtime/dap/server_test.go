package dap

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ZingYao/go-lua-vm/bytecode"
	"github.com/ZingYao/go-lua-vm/runtime"
)

// TestServerInitializeThreadsAndDisconnect 验证最小 DAP server 能完成 IDE 基础握手。
//
// 该测试只覆盖协议骨架，不声明断点、单步和变量已经接入 VM。
func TestServerInitializeThreadsAndDisconnect(t *testing.T) {
	// 使用 127.0.0.1:0 让系统分配空闲端口，避免测试之间端口冲突。
	server, err := NewServer("127.0.0.1:0")
	if err != nil {
		// 合法 loopback 地址不应创建失败。
		t.Fatalf("NewServer failed: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := server.Start(ctx, nil); err != nil {
		// 监听失败说明本地 TCP 环境不可用。
		t.Fatalf("Start failed: %v", err)
	}
	defer server.Close()
	connection, err := net.DialTimeout("tcp", server.ActualAddress, time.Second)
	if err != nil {
		// ready 后必须可以连接。
		t.Fatalf("dial DAP server failed: %v", err)
	}
	defer connection.Close()
	reader := bufio.NewReader(connection)
	writeRequest(t, connection, 1, "initialize")
	initializeResponse := readProtocolMessage(t, reader)
	if initializeResponse["type"] != "response" || initializeResponse["command"] != "initialize" || initializeResponse["success"] != true {
		// initialize 必须成功响应。
		t.Fatalf("initialize response = %#v", initializeResponse)
	}
	initializedEvent := readProtocolMessage(t, reader)
	if initializedEvent["type"] != "event" || initializedEvent["event"] != "initialized" {
		// initialize 后必须通知客户端可以配置断点。
		t.Fatalf("initialized event = %#v", initializedEvent)
	}
	writeRequest(t, connection, 2, "threads")
	threadsResponse := readProtocolMessage(t, reader)
	if threadsResponse["type"] != "response" || threadsResponse["command"] != "threads" || threadsResponse["success"] != true {
		// threads 必须成功响应。
		t.Fatalf("threads response = %#v", threadsResponse)
	}
	writeRequest(t, connection, 3, "disconnect")
	disconnectResponse := readProtocolMessage(t, reader)
	if disconnectResponse["type"] != "response" || disconnectResponse["command"] != "disconnect" || disconnectResponse["success"] != true {
		// disconnect 必须成功响应，便于 IDE 关闭会话。
		t.Fatalf("disconnect response = %#v", disconnectResponse)
	}
}

// TestServerBreakpointStopsAndContinues 验证 DAP 断点能暂停 VM 并通过 continue 继续。
//
// 该测试直接调用 DebugObserver 入口，锁定 DAP server 与 VM 行事件之间的最小集成契约。
func TestServerBreakpointStopsAndContinues(t *testing.T) {
	// 启动本机 DAP server 并完成 initialize 握手。
	server, connection, reader := startTestServerAndConnect(t)
	defer server.Close()
	defer connection.Close()
	writeRequest(t, connection, 1, "initialize")
	_ = readProtocolMessage(t, reader)
	_ = readProtocolMessage(t, reader)

	arguments := map[string]any{
		"source": map[string]any{"path": "main.glua"},
		"breakpoints": []map[string]any{
			{"line": 7},
		},
	}
	writeRequestWithArguments(t, connection, 2, "setBreakpoints", arguments)
	breakpointResponse := readProtocolMessage(t, reader)
	if breakpointResponse["type"] != "response" || breakpointResponse["command"] != "setBreakpoints" || breakpointResponse["success"] != true {
		// 设置断点必须成功响应。
		t.Fatalf("setBreakpoints response = %#v", breakpointResponse)
	}

	state := runtime.NewState()
	defer state.Close()
	vm := runtime.NewVM(1)
	proto := &bytecode.Proto{Source: "@main.glua", LineInfo: []int{7}, Code: []bytecode.Instruction{0}}
	stopped := make(chan error, 1)
	go func() {
		// BeforeInstruction 应命中断点并阻塞，直到 continue。
		stopped <- server.BeforeInstruction(state, vm, proto, 0)
	}()

	stoppedEvent := readProtocolMessage(t, reader)
	if stoppedEvent["type"] != "event" || stoppedEvent["event"] != "stopped" {
		// 命中断点必须发送 stopped 事件。
		t.Fatalf("stopped event = %#v", stoppedEvent)
	}
	writeRequest(t, connection, 3, "stackTrace")
	stackResponse := readProtocolMessage(t, reader)
	if stackResponse["type"] != "response" || stackResponse["command"] != "stackTrace" || stackResponse["success"] != true {
		// 暂停后必须能读取栈帧响应。
		t.Fatalf("stackTrace response = %#v", stackResponse)
	}
	writeRequest(t, connection, 4, "continue")
	continueResponse := readProtocolMessage(t, reader)
	if continueResponse["type"] != "response" || continueResponse["command"] != "continue" || continueResponse["success"] != true {
		// continue 必须释放 VM。
		t.Fatalf("continue response = %#v", continueResponse)
	}
	select {
	case err := <-stopped:
		if err != nil {
			// continue 后观察器不应返回错误。
			t.Fatalf("BeforeInstruction returned %v", err)
		}
	case <-time.After(time.Second):
		// 未释放说明 VM 会被 Debug 会话卡死。
		t.Fatalf("BeforeInstruction did not resume after continue")
	}
}

// TestServerStackTraceResolvesRelativeSourcePath 验证相对 Lua chunk source 会解析成 IDE 可跳转的绝对路径。
func TestServerStackTraceResolvesRelativeSourcePath(t *testing.T) {
	// 用临时目录模拟 VSCode 本地 launch 时 glua 子进程的工作目录。
	sourceRoot := t.TempDir()
	modulePath := filepath.Join(sourceRoot, "module.glua")
	if err := os.WriteFile(modulePath, []byte("print('module')\n"), 0o600); err != nil {
		// 测试源码文件必须创建成功，后续路径解析才有真实目标。
		t.Fatalf("write module source failed: %v", err)
	}
	server, connection, reader := startTestServerAndConnect(t)
	defer server.Close()
	defer connection.Close()
	server.sourceRoot = sourceRoot
	writeRequest(t, connection, 1, "initialize")
	_ = readProtocolMessage(t, reader)
	_ = readProtocolMessage(t, reader)
	writeRequestWithArguments(t, connection, 2, "setBreakpoints", map[string]any{
		"source":      map[string]any{"path": "module.glua"},
		"breakpoints": []map[string]any{{"line": 1}},
	})
	_ = readProtocolMessage(t, reader)

	state := runtime.NewState()
	defer state.Close()
	vm := runtime.NewVM(1)
	proto := &bytecode.Proto{Source: "@module.glua", LineInfo: []int{1}, Code: []bytecode.Instruction{0}}
	stopped := make(chan error, 1)
	go func() {
		// 第 1 行命中断点并等待 stackTrace/continue。
		stopped <- server.BeforeInstruction(state, vm, proto, 0)
	}()
	stoppedEvent := readProtocolMessage(t, reader)
	if stoppedEvent["event"] != "stopped" {
		// 相对 source 仍应与相对断点匹配并暂停。
		t.Fatalf("stopped event = %#v", stoppedEvent)
	}
	writeRequest(t, connection, 3, "stackTrace")
	stackResponse := readProtocolMessage(t, reader)
	stackBody := stackResponse["body"].(map[string]any)
	stackFrames := stackBody["stackFrames"].([]any)
	firstFrame := stackFrames[0].(map[string]any)
	source := firstFrame["source"].(map[string]any)
	if source["path"] != modulePath || source["name"] != "module.glua" {
		// VSCode 直连 DAP 依赖 source.path 打开文件，必须返回真实绝对路径。
		t.Fatalf("stack source = %#v, want path=%s", source, modulePath)
	}
	writeRequest(t, connection, 4, "continue")
	_ = readProtocolMessage(t, reader)
	select {
	case err := <-stopped:
		if err != nil {
			// continue 后暂停应正常释放。
			t.Fatalf("stop returned %v", err)
		}
	case <-time.After(time.Second):
		// 暂停必须被 continue 释放。
		t.Fatalf("continue did not resume relative source stop")
	}
}

// TestServerContinueSkipsSameLineBreakpointInstructions 验证 continue 不会在同一源码行的后续指令反复命中同一断点。
func TestServerContinueSkipsSameLineBreakpointInstructions(t *testing.T) {
	// 启动 DAP server 并设置第 7 行断点，用多条相同行号指令模拟一行表达式编译出的多个 VM 指令。
	server, connection, reader := startTestServerAndConnect(t)
	defer server.Close()
	defer connection.Close()
	writeRequest(t, connection, 1, "initialize")
	_ = readProtocolMessage(t, reader)
	_ = readProtocolMessage(t, reader)
	writeRequestWithArguments(t, connection, 2, "setBreakpoints", map[string]any{
		"source":      map[string]any{"path": "main.glua"},
		"breakpoints": []map[string]any{{"line": 7}},
	})
	_ = readProtocolMessage(t, reader)

	state := runtime.NewState()
	defer state.Close()
	vm := runtime.NewVM(1)
	proto := &bytecode.Proto{Source: "@main.glua", LineInfo: []int{7, 7, 8, 7}, Code: []bytecode.Instruction{0, 0, 0, 0}}
	firstStop := make(chan error, 1)
	go func() {
		// 第一条第 7 行指令命中断点并等待 continue。
		firstStop <- server.BeforeInstruction(state, vm, proto, 0)
	}()
	stoppedEvent := readProtocolMessage(t, reader)
	if stoppedEvent["event"] != "stopped" {
		// 初次到达断点行必须暂停。
		t.Fatalf("stopped event = %#v", stoppedEvent)
	}
	writeRequest(t, connection, 3, "continue")
	_ = readProtocolMessage(t, reader)
	select {
	case err := <-firstStop:
		if err != nil {
			// continue 后首次暂停必须释放。
			t.Fatalf("first stop returned %v", err)
		}
	case <-time.After(time.Second):
		// 未释放说明 continue 无效。
		t.Fatalf("continue did not resume first stop")
	}

	sameLine := make(chan error, 1)
	go func() {
		// 同一源码行的后续指令不应再次停在相同断点。
		sameLine <- server.BeforeInstruction(state, vm, proto, 1)
	}()
	select {
	case err := <-sameLine:
		if err != nil {
			// 跳过同一行断点时不应返回错误。
			t.Fatalf("same line returned %v", err)
		}
	case <-time.After(200 * time.Millisecond):
		// 如果阻塞，用户就会看到恢复按钮要点很多次。
		t.Fatalf("same line breakpoint was hit again")
	}

	if err := server.BeforeInstruction(state, vm, proto, 2); err != nil {
		// 离开断点行时只负责清除跳过状态，不应暂停。
		t.Fatalf("different line returned %v", err)
	}
	secondStop := make(chan error, 1)
	go func() {
		// 再次回到第 7 行代表下一次源码行执行，断点应重新生效。
		secondStop <- server.BeforeInstruction(state, vm, proto, 3)
	}()
	secondStoppedEvent := readProtocolMessage(t, reader)
	if secondStoppedEvent["event"] != "stopped" {
		// 再次到达断点行必须仍能暂停。
		t.Fatalf("second stopped event = %#v", secondStoppedEvent)
	}
	writeRequest(t, connection, 4, "continue")
	_ = readProtocolMessage(t, reader)
	select {
	case err := <-secondStop:
		if err != nil {
			// 第二次暂停也必须可恢复。
			t.Fatalf("second stop returned %v", err)
		}
	case <-time.After(time.Second):
		// 第二次暂停不能卡死。
		t.Fatalf("continue did not resume second stop")
	}
}

// TestServerVariablesAndStep 验证暂停后可读取局部变量，并且 next 会在下一条源码行再次暂停。
func TestServerVariablesAndStep(t *testing.T) {
	// 启动本机 DAP server 并完成 initialize 握手。
	server, connection, reader := startTestServerAndConnect(t)
	defer server.Close()
	defer connection.Close()
	writeRequest(t, connection, 1, "initialize")
	_ = readProtocolMessage(t, reader)
	_ = readProtocolMessage(t, reader)

	arguments := map[string]any{
		"source": map[string]any{"path": "main.glua"},
		"breakpoints": []map[string]any{
			{"line": 7},
		},
	}
	writeRequestWithArguments(t, connection, 2, "setBreakpoints", arguments)
	_ = readProtocolMessage(t, reader)

	state := runtime.NewState()
	defer state.Close()
	vm := runtime.NewVM(2)
	proto := &bytecode.Proto{
		Source:   "@main.glua",
		LineInfo: []int{7, 8},
		Code:     []bytecode.Instruction{0, 0},
		LocalVars: []bytecode.LocalVar{
			{Name: "i", Register: 0, StartPC: 0, EndPC: 2},
			{Name: "tools", Register: 1, StartPC: 0, EndPC: 2},
		},
	}
	vm.BindPrototype(proto)
	if err := vm.SetRegister(0, runtime.IntegerValue(42)); err != nil {
		// 测试 VM 只有一个寄存器，写入 0 号寄存器应成功。
		t.Fatalf("SetRegister failed: %v", err)
	}
	toolsTable := runtime.NewTable()
	toolsTable.RawSetString("name", runtime.StringValue("zing"))
	if err := vm.SetRegister(1, runtime.ReferenceValue(runtime.KindTable, toolsTable)); err != nil {
		// table local 写入 1 号寄存器应成功。
		t.Fatalf("SetRegister table failed: %v", err)
	}

	firstStop := make(chan error, 1)
	go func() {
		// 第 7 行命中断点并暂停。
		firstStop <- server.BeforeInstruction(state, vm, proto, 0)
	}()
	stoppedEvent := readProtocolMessage(t, reader)
	if stoppedEvent["event"] != "stopped" {
		// 命中断点必须发送 stopped。
		t.Fatalf("stopped event = %#v", stoppedEvent)
	}

	writeRequest(t, connection, 3, "scopes")
	scopesResponse := readProtocolMessage(t, reader)
	scopesBody := scopesResponse["body"].(map[string]any)
	scopes := scopesBody["scopes"].([]any)
	if len(scopes) != 2 || scopes[0].(map[string]any)["name"] != "Locals" || scopes[1].(map[string]any)["name"] != "Globals" {
		// 暂停帧应暴露 Locals 和可展开的 Globals scope。
		t.Fatalf("scopes response = %#v", scopesResponse)
	}
	writeRequest(t, connection, 4, "variables")
	variablesResponse := readProtocolMessage(t, reader)
	variablesBody := variablesResponse["body"].(map[string]any)
	variables := variablesBody["variables"].([]any)
	if len(variables) != 2 || variables[0].(map[string]any)["name"] != "i" {
		// 局部变量 i 和 tools 必须可见。
		t.Fatalf("variables response = %#v", variablesResponse)
	}
	var toolsReference float64
	for _, item := range variables {
		// table 变量必须带 variablesReference，供 IDE 展开子项。
		variable := item.(map[string]any)
		if variable["name"] == "tools" {
			toolsReference = variable["variablesReference"].(float64)
		}
	}
	if toolsReference <= 1 {
		// table reference 必须大于 Locals scope 的 1。
		t.Fatalf("tools variablesReference = %#v", variables)
	}
	writeRequestWithArguments(t, connection, 5, "variables", map[string]any{"variablesReference": int(toolsReference)})
	tableVariablesResponse := readProtocolMessage(t, reader)
	tableVariablesBody := tableVariablesResponse["body"].(map[string]any)
	tableVariables := tableVariablesBody["variables"].([]any)
	if len(tableVariables) != 1 || tableVariables[0].(map[string]any)["name"] != "[\"name\"]" {
		// table 子字段必须能通过二级 variables 请求展开。
		t.Fatalf("table variables response = %#v", tableVariablesResponse)
	}
	writeRequestWithArguments(t, connection, 6, "setVariable", map[string]any{"variablesReference": 1, "name": "i", "value": "53"})
	setLocalResponse := readProtocolMessage(t, reader)
	if setLocalResponse["success"] != true || setLocalResponse["body"].(map[string]any)["value"] != "integer(53)" {
		// 局部变量写回应返回新值。
		t.Fatalf("set local response = %#v", setLocalResponse)
	}
	if value := vm.RegistersSnapshot()[0]; value.Kind != runtime.KindInteger || value.Integer != 53 {
		// setVariable 必须真实写回 VM 寄存器，而不是只改 DAP 快照。
		t.Fatalf("register 0 after setVariable = %#v", value)
	}
	writeRequestWithArguments(t, connection, 7, "setVariable", map[string]any{"variablesReference": int(toolsReference), "name": "[\"name\"]", "value": "\"lua\""})
	setTableResponse := readProtocolMessage(t, reader)
	if setTableResponse["success"] != true || setTableResponse["body"].(map[string]any)["value"] != "string(\"lua\")" {
		// table 子字段写回应返回新值。
		t.Fatalf("set table response = %#v", setTableResponse)
	}
	if value := toolsTable.RawGetString("name"); value.Kind != runtime.KindString || value.String != "lua" {
		// setVariable 必须真实写回 table 字段。
		t.Fatalf("table name after setVariable = %#v", value)
	}

	clearBreakpointArguments := map[string]any{
		"source":      map[string]any{"path": "main.glua"},
		"breakpoints": []map[string]any{},
	}
	writeRequestWithArguments(t, connection, 8, "setBreakpoints", clearBreakpointArguments)
	_ = readProtocolMessage(t, reader)

	writeRequest(t, connection, 9, "next")
	_ = readProtocolMessage(t, reader)
	select {
	case err := <-firstStop:
		if err != nil {
			// next 会先释放当前暂停点。
			t.Fatalf("first stop returned %v", err)
		}
	case <-time.After(time.Second):
		// 当前暂停点必须被 next 释放。
		t.Fatalf("next did not resume first stop")
	}
	if err := server.BeforeInstruction(state, vm, proto, 0); err != nil {
		// 同一源码行不应立即再次暂停。
		t.Fatalf("same-line step returned %v", err)
	}
	secondStop := make(chan error, 1)
	go func() {
		// 下一条不同源码行应由 next 再次暂停。
		secondStop <- server.BeforeInstruction(state, vm, proto, 1)
	}()
	secondStoppedEvent := readProtocolMessage(t, reader)
	if secondStoppedEvent["event"] != "stopped" {
		// step 命中必须再次发送 stopped。
		t.Fatalf("second stopped event = %#v", secondStoppedEvent)
	}
	writeRequest(t, connection, 10, "continue")
	_ = readProtocolMessage(t, reader)
	select {
	case err := <-secondStop:
		if err != nil {
			// continue 后第二次暂停应正常释放。
			t.Fatalf("second stop returned %v", err)
		}
	case <-time.After(time.Second):
		// 第二次暂停必须被 continue 释放。
		t.Fatalf("continue did not resume second stop")
	}
}

// TestServerStepModesRespectCallDepth 验证 stepIn、next 与 stepOut 使用不同调用深度停止边界。
func TestServerStepModesRespectCallDepth(t *testing.T) {
	// 复用同一暂停位置模拟嵌套 Lua 调用；该用例只验证纯状态机，不需要启动 TCP 服务。
	server := &Server{currentStop: &stoppedLocation{Source: "main.glua", Line: 10, Depth: 3}}
	server.prepareStep(dapStepModeInto)
	if !server.consumeStepStop("child.glua", 1, 4) {
		// stepIn 必须允许进入更深调用帧后立即停在新源码行。
		t.Fatal("stepIn did not stop in child frame")
	}

	server.currentStop = &stoppedLocation{Source: "main.glua", Line: 10, Depth: 3}
	server.prepareStep(dapStepModeOver)
	if server.consumeStepStop("child.glua", 1, 4) {
		// next 必须跳过被调函数内部的行。
		t.Fatal("next stopped inside child frame")
	}
	if !server.consumeStepStop("main.glua", 11, 3) {
		// next 返回当前帧后应在下一条不同源码行停住。
		t.Fatal("next did not stop in caller frame")
	}

	server.currentStop = &stoppedLocation{Source: "main.glua", Line: 10, Depth: 3}
	server.prepareStep(dapStepModeOut)
	if server.consumeStepStop("child.glua", 1, 4) || server.consumeStepStop("main.glua", 11, 3) {
		// stepOut 在子帧和当前帧都不能提前停止。
		t.Fatal("stepOut stopped before the current frame returned")
	}
	if !server.consumeStepStop("caller.glua", 8, 2) {
		// 调用深度变浅后 stepOut 必须在下一条可见源码行停止。
		t.Fatal("stepOut did not stop after returning to caller")
	}
}

// TestServerUpvalueScopeAndWrite 验证 DAP Upvalues scope 展示当前 Lua closure 捕获值并可写回。
func TestServerUpvalueScopeAndWrite(t *testing.T) {
	// 手工构造带命名共享 cell 的 Lua closure，避免测试依赖网络或完整 DAP 暂停流程。
	cell := runtime.NewClosedUpvalueCell(runtime.StringValue("before"))
	closure := &runtime.LuaClosure{
		Proto:        &bytecode.Proto{Upvalues: []bytecode.UpvalueDesc{{Name: "captured"}}},
		Upvalues:     []runtime.Value{runtime.StringValue("stale")},
		UpvalueCells: []*runtime.UpvalueCell{cell},
	}
	server := &Server{
		currentStop:    &stoppedLocation{Frames: []runtime.CallFrame{{Kind: runtime.CallFrameKindLua, Function: runtime.ReferenceValue(runtime.KindLuaClosure, closure)}}},
		nextVariableID: dapFirstDynamicVariablesReference,
		variableValues: make(map[int]runtime.Value), variableTables: make(map[*runtime.Table]int), variableTableKeys: make(map[int]runtime.Value),
	}
	scopes := server.scopes()["scopes"].([]map[string]any)
	if len(scopes) != 1 || scopes[0]["name"] != "Upvalues" || scopes[0]["variablesReference"] != dapUpvaluesVariablesReference {
		// 无 Locals/Globals 时必须仍单独展示可读 Upvalues scope。
		t.Fatalf("scopes = %#v", scopes)
	}
	arguments, err := json.Marshal(map[string]any{"variablesReference": dapUpvaluesVariablesReference})
	if err != nil {
		// 固定测试参数必须可被 JSON 编码。
		t.Fatalf("marshal variables arguments failed: %v", err)
	}
	variables := server.variables(arguments)["variables"].([]map[string]any)
	if len(variables) != 1 || variables[0]["name"] != "captured" || variables[0]["value"] != "string(\"before\")" {
		// shared cell 当前值必须覆盖旧 Upvalues 快照。
		t.Fatalf("upvalue variables = %#v", variables)
	}
	setArguments, err := json.Marshal(map[string]any{"variablesReference": dapUpvaluesVariablesReference, "name": "captured", "value": "\"after\""})
	if err != nil {
		// 固定写回参数必须可被 JSON 编码。
		t.Fatalf("marshal setVariable arguments failed: %v", err)
	}
	response, err := server.setVariable(setArguments)
	if err != nil || response["value"] != "string(\"after\")" || cell.Value().String != "after" {
		// DAP 写回必须更新共享 cell，而不是只改显示快照。
		t.Fatalf("set upvalue response=%#v err=%v cell=%#v", response, err, cell.Value())
	}
}

// TestServerThreadsAndEvaluate 验证 DAP 协程映射与暂停态只读变量路径求值。
func TestServerThreadsAndEvaluate(t *testing.T) {
	// State 同时保留主线程和一个新建 coroutine，模拟真实调试会话可见线程集合。
	state := runtime.NewState()
	coroutine := state.NewThread(runtime.ReferenceValue(runtime.KindGoClosure, runtime.GoResultsFunction(func(args ...runtime.Value) ([]runtime.Value, error) {
		// 测试线程只需要可调用入口，不需要实际 resume。
		return nil, nil
	})))
	if coroutine == nil {
		// 正常 State 必须允许创建 coroutine。
		t.Fatal("NewThread returned nil")
	}
	globals := state.Globals()
	globals.RawSetString("global", runtime.IntegerValue(9))
	child := runtime.NewTable()
	child.RawSetString("name", runtime.StringValue("nested"))
	globals.RawSetString("tools", runtime.ReferenceValue(runtime.KindTable, child))
	closure := &runtime.LuaClosure{Proto: &bytecode.Proto{Upvalues: []bytecode.UpvalueDesc{{Name: "captured"}}}, Upvalues: []runtime.Value{runtime.StringValue("up")}}
	server := &Server{
		currentStop: &stoppedLocation{
			State: state, Thread: coroutine,
			Variables: []runtime.ActiveLocalSnapshot{{Name: "local", Value: runtime.StringValue("local")}},
			Frames:    []runtime.CallFrame{{Kind: runtime.CallFrameKindLua, Function: runtime.ReferenceValue(runtime.KindLuaClosure, closure)}},
		},
		nextVariableID: dapFirstDynamicVariablesReference,
		variableValues: make(map[int]runtime.Value), variableTables: make(map[*runtime.Table]int), variableTableKeys: make(map[int]runtime.Value),
		threadIDs: make(map[*runtime.Thread]int), nextThreadID: 1,
	}
	threadList := server.threads()["threads"].([]map[string]any)
	if len(threadList) != 2 || threadList[0]["name"] != "main (running)" {
		// 主线程与 coroutine 都必须进入 DAP threads 返回值。
		t.Fatalf("threads = %#v", threadList)
	}
	for expression, want := range map[string]string{"local": "string(\"local\")", "captured": "string(\"up\")", "global": "integer(9)", "tools.name": "string(\"nested\")"} {
		arguments, err := json.Marshal(map[string]any{"expression": expression})
		if err != nil {
			// 固定表达式参数必须可编码。
			t.Fatalf("marshal evaluate %s failed: %v", expression, err)
		}
		response := server.evaluate(arguments)
		if response["result"] != want {
			// 只读路径解析必须按 Locals、Upvalues、Globals 顺序返回值。
			t.Fatalf("evaluate %s = %#v, want %s", expression, response, want)
		}
	}
}

// TestServerSetVariableRejectsConstLocal 验证 DAP setVariable 不能覆盖 const local。
func TestServerSetVariableRejectsConstLocal(t *testing.T) {
	// 启动 DAP server 并停在包含 const local 的断点。
	server, connection, reader := startTestServerAndConnect(t)
	defer server.Close()
	defer connection.Close()
	writeRequest(t, connection, 1, "initialize")
	_ = readProtocolMessage(t, reader)
	_ = readProtocolMessage(t, reader)
	writeRequestWithArguments(t, connection, 2, "setBreakpoints", map[string]any{
		"source":      map[string]any{"path": "main.glua"},
		"breakpoints": []map[string]any{{"line": 3}},
	})
	_ = readProtocolMessage(t, reader)

	state := runtime.NewState()
	defer state.Close()
	vm := runtime.NewVM(1)
	proto := &bytecode.Proto{
		Source:   "@main.glua",
		LineInfo: []int{3},
		Code:     []bytecode.Instruction{0},
		LocalVars: []bytecode.LocalVar{
			{Name: "answer", Register: 0, Const: true, StartPC: 0, EndPC: 1},
		},
	}
	vm.BindPrototype(proto)
	if err := vm.SetRegister(0, runtime.IntegerValue(42)); err != nil {
		// const local 初始值写入测试 VM 应成功。
		t.Fatalf("SetRegister failed: %v", err)
	}
	stopped := make(chan error, 1)
	go func() {
		// 第 3 行命中断点并暂停。
		stopped <- server.BeforeInstruction(state, vm, proto, 0)
	}()
	_ = readProtocolMessage(t, reader)

	writeRequestWithArguments(t, connection, 3, "setVariable", map[string]any{"variablesReference": 1, "name": "answer", "value": "7"})
	response := readProtocolMessage(t, reader)
	body := response["body"].(map[string]any)
	if response["success"] == true || !strings.Contains(body["error"].(string), "cannot assign to const binding 'answer'") {
		// const local 写回应被拒绝。
		t.Fatalf("set const local response = %#v", response)
	}
	if value := vm.RegistersSnapshot()[0]; value.Kind != runtime.KindInteger || value.Integer != 42 {
		// 拒绝写回后 VM 寄存器必须保持原值。
		t.Fatalf("const register after setVariable = %#v", value)
	}
	writeRequest(t, connection, 4, "continue")
	_ = readProtocolMessage(t, reader)
	select {
	case err := <-stopped:
		if err != nil {
			// continue 后暂停应正常释放。
			t.Fatalf("stop returned %v", err)
		}
	case <-time.After(time.Second):
		// 暂停必须被 continue 释放。
		t.Fatalf("continue did not resume const stop")
	}
}

// TestServerContinueClearsPendingStep 验证运行态 continue 会取消尚未消费的单步请求。
func TestServerContinueClearsPendingStep(t *testing.T) {
	// 启动 DAP server 并停在第一行断点。
	server, connection, reader := startTestServerAndConnect(t)
	defer server.Close()
	defer connection.Close()
	writeRequest(t, connection, 1, "initialize")
	_ = readProtocolMessage(t, reader)
	_ = readProtocolMessage(t, reader)
	writeRequestWithArguments(t, connection, 2, "setBreakpoints", map[string]any{
		"source":      map[string]any{"path": "main.glua"},
		"breakpoints": []map[string]any{{"line": 7}},
	})
	_ = readProtocolMessage(t, reader)

	state := runtime.NewState()
	defer state.Close()
	vm := runtime.NewVM(1)
	proto := &bytecode.Proto{Source: "@main.glua", LineInfo: []int{7, 8}, Code: []bytecode.Instruction{0, 0}}
	firstStop := make(chan error, 1)
	go func() {
		// 第一条指令命中断点并等待调试客户端命令。
		firstStop <- server.BeforeInstruction(state, vm, proto, 0)
	}()
	stoppedEvent := readProtocolMessage(t, reader)
	if stoppedEvent["event"] != "stopped" {
		// 初始断点必须真实暂停。
		t.Fatalf("stopped event = %#v", stoppedEvent)
	}

	writeRequest(t, connection, 3, "next")
	_ = readProtocolMessage(t, reader)
	select {
	case err := <-firstStop:
		if err != nil {
			// next 应先释放当前暂停点。
			t.Fatalf("first stop returned %v", err)
		}
	case <-time.After(time.Second):
		// next 没有释放说明单步按钮无效。
		t.Fatalf("next did not resume first stop")
	}

	writeRequest(t, connection, 4, "continue")
	_ = readProtocolMessage(t, reader)
	nextLine := make(chan error, 1)
	go func() {
		// continue 发生在 VM 运行态时，应取消上一条 next 留下的 pending step。
		nextLine <- server.BeforeInstruction(state, vm, proto, 1)
	}()
	select {
	case err := <-nextLine:
		if err != nil {
			// 没有暂停时不应返回调试错误。
			t.Fatalf("next line returned %v", err)
		}
	case <-time.After(time.Second):
		// 如果这里阻塞，说明 continue 没有取消 pending step，会表现成恢复按钮不生效。
		t.Fatalf("continue did not clear pending step")
	}
}

// TestServerWaitForConfigurationDone 验证 CLI 可等待 IDE 完成断点配置后再执行脚本。
func TestServerWaitForConfigurationDone(t *testing.T) {
	// 启动 DAP server 并建立客户端连接。
	server, connection, reader := startTestServerAndConnect(t)
	defer server.Close()
	defer connection.Close()

	var output bytes.Buffer
	waiting := make(chan error, 1)
	go func() {
		// 等待必须阻塞到客户端发送 configurationDone。
		waiting <- server.WaitForConfigurationDone(context.Background(), time.Second, &output)
	}()

	select {
	case err := <-waiting:
		// configurationDone 前不应提前释放脚本执行。
		t.Fatalf("WaitForConfigurationDone returned before configurationDone: %v", err)
	case <-time.After(20 * time.Millisecond):
		// 短暂等待后仍阻塞，说明 CLI 不会抢跑脚本。
	}

	writeRequest(t, connection, 1, "initialize")
	_ = readProtocolMessage(t, reader)
	_ = readProtocolMessage(t, reader)
	writeRequest(t, connection, 2, "configurationDone")
	configurationDoneResponse := readProtocolMessage(t, reader)
	if configurationDoneResponse["type"] != "response" || configurationDoneResponse["command"] != "configurationDone" || configurationDoneResponse["success"] != true {
		// configurationDone 必须成功响应，才能释放 CLI。
		t.Fatalf("configurationDone response = %#v", configurationDoneResponse)
	}

	select {
	case err := <-waiting:
		if err != nil {
			// 客户端配置完成后等待应成功结束。
			t.Fatalf("WaitForConfigurationDone returned %v", err)
		}
	case <-time.After(time.Second):
		// 未释放说明 glua Debug 会一直卡在启动阶段。
		t.Fatalf("WaitForConfigurationDone did not return after configurationDone")
	}
	for _, want := range []string{"waiting for client configuration", "starting script"} {
		// 等待进度必须写入输出，便于 IDE Debug 控制台展示。
		if !strings.Contains(output.String(), want) {
			t.Fatalf("output = %q, missing %q", output.String(), want)
		}
	}
}

// TestServerMapsClientPathsAndStopsOnEntry verifies the Android-style remote
// source path mapping used by editor adapters and the optional first-line stop.
func TestServerMapsClientPathsAndStopsOnEntry(t *testing.T) {
	server, connection, reader := startTestServerAndConnect(t)
	defer server.Close()
	defer connection.Close()

	writeRequest(t, connection, 1, "initialize")
	_ = readProtocolMessage(t, reader)
	_ = readProtocolMessage(t, reader)
	writeRequestWithArguments(t, connection, 2, "launch", map[string]any{
		"localRoot": "D:\\workspace", "remoteRoot": "/data/local/tmp/project", "stopOnEntry": true,
	})
	_ = readProtocolMessage(t, reader)
	writeRequestWithArguments(t, connection, 3, "setBreakpoints", map[string]any{
		"source":      map[string]any{"path": "D:\\workspace\\main.lua"},
		"breakpoints": []map[string]any{{"line": 2}},
	})
	_ = readProtocolMessage(t, reader)

	state := runtime.NewState()
	defer state.Close()
	vm := runtime.NewVM(1)
	proto := &bytecode.Proto{
		Source:   "@/data/local/tmp/project/main.lua",
		LineInfo: []int{1, 2}, Code: []bytecode.Instruction{0, 0},
		LocalVars: []bytecode.LocalVar{{Name: "value", Register: 0, StartPC: 0, EndPC: 2}},
	}
	vm.BindPrototype(proto)
	if err := vm.SetRegister(0, runtime.IntegerValue(42)); err != nil {
		t.Fatal(err)
	}

	entryStop := make(chan error, 1)
	go func() { entryStop <- server.BeforeInstruction(state, vm, proto, 0) }()
	stopped := readProtocolMessage(t, reader)
	if stopped["event"] != "stopped" || stopped["body"].(map[string]any)["reason"] != "entry" {
		t.Fatalf("entry stopped event = %#v", stopped)
	}
	writeRequestWithArguments(t, connection, 4, "stackTrace", map[string]any{"threadId": 1})
	stack := readProtocolMessage(t, reader)
	frames := stack["body"].(map[string]any)["stackFrames"].([]any)
	source := frames[0].(map[string]any)["source"].(map[string]any)
	if source["path"] != "D:/workspace/main.lua" {
		t.Fatalf("mapped stack source = %#v", source)
	}
	writeRequest(t, connection, 5, "continue")
	_ = readProtocolMessage(t, reader)
	if err := <-entryStop; err != nil {
		t.Fatal(err)
	}

	breakpointStop := make(chan error, 1)
	go func() { breakpointStop <- server.BeforeInstruction(state, vm, proto, 1) }()
	stopped = readProtocolMessage(t, reader)
	if stopped["body"].(map[string]any)["reason"] != "breakpoint" {
		t.Fatalf("breakpoint stopped event = %#v", stopped)
	}
	writeRequest(t, connection, 6, "continue")
	_ = readProtocolMessage(t, reader)
	if err := <-breakpointStop; err != nil {
		t.Fatal(err)
	}
}

// TestServerTerminationAndDisconnect verifies the host can announce normal
// script completion and wait for the editor to close the DAP session.
func TestServerTerminationAndDisconnect(t *testing.T) {
	server, connection, reader := startTestServerAndConnect(t)
	defer server.Close()
	defer connection.Close()
	writeRequest(t, connection, 1, "initialize")
	_ = readProtocolMessage(t, reader)
	_ = readProtocolMessage(t, reader)

	server.NotifyTerminated()
	event := readProtocolMessage(t, reader)
	if event["event"] != "terminated" {
		t.Fatalf("terminated event = %#v", event)
	}
	waiting := make(chan error, 1)
	go func() { waiting <- server.WaitForClientDisconnect(context.Background(), time.Second) }()
	writeRequest(t, connection, 2, "disconnect")
	response := readProtocolMessage(t, reader)
	if response["command"] != "disconnect" || response["success"] != true {
		t.Fatalf("disconnect response = %#v", response)
	}
	if err := <-waiting; err != nil {
		t.Fatal(err)
	}
}

// startTestServerAndConnect 启动测试 DAP server 并建立 TCP 连接。
func startTestServerAndConnect(t *testing.T) (*Server, net.Conn, *bufio.Reader) {
	t.Helper()
	server, err := NewServer("127.0.0.1:0")
	if err != nil {
		// 合法 loopback 地址不应创建失败。
		t.Fatalf("NewServer failed: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := server.Start(ctx, nil); err != nil {
		// 监听失败说明本地 TCP 环境不可用。
		t.Fatalf("Start failed: %v", err)
	}
	connection, err := net.DialTimeout("tcp", server.ActualAddress, time.Second)
	if err != nil {
		// ready 后必须可以连接。
		t.Fatalf("dial DAP server failed: %v", err)
	}
	return server, connection, bufio.NewReader(connection)
}

// writeRequest 写出一个最小 DAP request。
//
// connection 必须是已连接的 TCP 连接；seq 和 command 用于构造 request。
func writeRequest(t *testing.T, connection net.Conn, seq int, command string) {
	t.Helper()
	writeRequestWithArguments(t, connection, seq, command, nil)
}

// writeRequestWithArguments 写出带 arguments 的 DAP request。
//
// arguments 为 nil 时省略该字段；测试用它覆盖 setBreakpoints 等有参数命令。
func writeRequestWithArguments(t *testing.T, connection net.Conn, seq int, command string, arguments any) {
	t.Helper()
	request := map[string]any{
		"seq":     seq,
		"type":    "request",
		"command": command,
	}
	if arguments != nil {
		// 有参数命令必须写入 arguments 字段。
		request["arguments"] = arguments
	}
	payload, err := json.Marshal(request)
	if err != nil {
		// 测试构造的 request 必须可编码。
		t.Fatalf("marshal request failed: %v", err)
	}
	if _, err := fmt.Fprintf(connection, "Content-Length: %d\r\n\r\n%s", len(payload), payload); err != nil {
		// 写入失败说明测试连接不可用。
		t.Fatalf("write request failed: %v", err)
	}
}

// readProtocolMessage 读取一个 DAP JSON 消息并转为 map。
//
// reader 必须指向 DAP Content-Length 帧流；测试只关心少量顶层字段。
func readProtocolMessage(t *testing.T, reader *bufio.Reader) map[string]any {
	t.Helper()
	message, err := readMessage(reader)
	if err != nil {
		// server 必须返回完整 DAP 帧。
		t.Fatalf("read message failed: %v", err)
	}
	payload, err := json.Marshal(message)
	if err != nil {
		// protocolMessage 必须可重新编码。
		t.Fatalf("remarshal message failed: %v", err)
	}
	var result map[string]any
	if err := json.Unmarshal(payload, &result); err != nil {
		// 编码后的消息必须可解析为通用 map。
		t.Fatalf("unmarshal message failed: %v", err)
	}
	return result
}
