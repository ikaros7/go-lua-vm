// Package lua 提供 go-lua-vm 的对外嵌入 API 边界。
//
// 当前阶段的对外接口以 run-time 与编译阶段已存在能力为准：
// - State 生命周期与受保护调用边界。
// - 栈和寄存器边界的基础读写。
// - pcall/xpcall 的统一返回语义。
//
// 该文件仅做 API 重导出，不改 runtime 内部实现。
package lua

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ZingYao/go-lua-vm/bytecode"
	"github.com/ZingYao/go-lua-vm/compiler/codegen"
	"github.com/ZingYao/go-lua-vm/compiler/lexer"
	"github.com/ZingYao/go-lua-vm/compiler/parser"
	"github.com/ZingYao/go-lua-vm/extensions"
	"github.com/ZingYao/go-lua-vm/runtime"
	baselib "github.com/ZingYao/go-lua-vm/stdlib/base"
	debuglib "github.com/ZingYao/go-lua-vm/stdlib/debug"
	iolib "github.com/ZingYao/go-lua-vm/stdlib/io"
	mathlib "github.com/ZingYao/go-lua-vm/stdlib/math"
	oslib "github.com/ZingYao/go-lua-vm/stdlib/os"
	packagelib "github.com/ZingYao/go-lua-vm/stdlib/package"
	stringlib "github.com/ZingYao/go-lua-vm/stdlib/string"
	tablelib "github.com/ZingYao/go-lua-vm/stdlib/table"
	utf8lib "github.com/ZingYao/go-lua-vm/stdlib/utf8"
)

// SyntaxErrorDetails 保存 Lua 源码语法错误的扩展诊断信息。
//
// 字段用于命令行之外的 Go 宿主展示更丰富的错误详情；Error() 只返回 Message，
// 这些字段不会自动拼入主错误文本。
type SyntaxErrorDetails struct {
	// SourceName 保存原始 chunk name，例如 `@test.glua` 或 `=(string)`。
	SourceName string
	// SourceID 保存用户可见 source 标识，例如 `test.glua` 或 `(string)`。
	SourceID string
	// Line 保存错误所在行号，起始值为 1。
	Line int
	// Column 保存错误所在列号，起始值为 1；EOF 位于行尾后一列。
	Column int
	// Near 保存 Lua 风格 near token 文本，例如 `<eof>` 或 `'end'`。
	Near string
	// Expected 保存 parser 原始期望或错误说明。
	Expected string
	// Hint 保存面向用户的修复提示或更明确的期望说明。
	Hint string
	// LineText 保存错误行源码文本，便于宿主构造 SQL 风格指针展示。
	LineText string
}

// SyntaxError 表示 Lua 源码加载阶段的结构化语法错误。
//
// Message 是 Error() 返回的紧凑主错误；Details 保存可扩展诊断字段；Cause 保留底层
// parser 错误，调用方仍可通过 errors.As 取得 parser.ParseError。
type SyntaxError struct {
	// Message 保存可直接展示的主错误文本。
	Message string
	// Details 保存错误位置、near token 和源码行等扩展诊断信息。
	Details SyntaxErrorDetails
	// Cause 保存底层 parser 错误。
	Cause error
}

// Error 返回语法错误的主消息。
//
// 该文本面向 CLI 和日志，保持 `source:line:column: syntax error near token` 形态。
func (err *SyntaxError) Error() string {
	if err == nil {
		// nil 接收者没有可展示错误。
		return ""
	}
	return err.Message
}

// Unwrap 返回底层 parser 错误。
//
// 通过 Unwrap 保留 parser.IsSyntaxError、errors.As 和既有错误分类能力。
func (err *SyntaxError) Unwrap() error {
	if err == nil {
		// nil 接收者没有底层错误。
		return nil
	}
	return err.Cause
}

// State 表示 Lua 运行状态。
//
// 类型本身与 runtime.State 共享同一内存布局，调用方可以直接使用 state.Close、
// state.StackTop 等 runtime 方法；此层仅承担包级 API 入口。
//
// 对齐 lua_State 公开语义时，后续可在该类型上继续补充包装方法。
type State = runtime.State

// luaCoroutineContinuation 保存 Lua coroutine 在 coroutine.yield 之后继续执行所需的最小现场。
//
// 当前 continuation 仅覆盖同步 VM 执行循环内的 Lua closure：保存 VM、调用帧、调用请求和 hook
// 行号状态；下一次 resume 会把实参写成 coroutine.yield 的返回值，然后从 CALL 后一条指令继续。
type luaCoroutineContinuation struct {
	thread             *runtime.Thread
	function           Value
	closure            *runtime.LuaClosure
	proto              *bytecode.Proto
	vm                 *runtime.VM
	frame              runtime.CallFrame
	outerFrames        []runtime.CallFrame
	callRequest        *runtime.CallRequest
	goResume           func(args []Value, callErr error) ([]Value, error)
	resumeRegister     int
	resumeInstruction  bytecode.Instruction
	snapshotLevel      int
	nextPC             int
	previousPC         int
	previousPreviousPC int
	lastHookLine       int64
	parent             *luaCoroutineContinuation
}

var (
	// luaContinuationMu 保护协程 continuation 表，避免宿主并发 resume/debug 造成 map 竞争。
	luaContinuationMu sync.Mutex
	// luaContinuations 按线程保存最近一次 yield 的 VM continuation。
	luaContinuations = make(map[*runtime.Thread]*luaCoroutineContinuation)
	// luaLastContinuationThreads 按 State 记录最近一次保存 continuation 的线程，避免旧协程残留污染 pcall 恢复。
	luaLastContinuationThreads = make(map[*runtime.State]*runtime.Thread)
)

// saveLuaContinuation 保存线程的 Lua VM continuation。
//
// continuation 必须非 nil 且绑定 thread；保存后 runtime.Thread 会被标记为 continuationPending。
func saveLuaContinuation(continuation *luaCoroutineContinuation) {
	if continuation == nil || continuation.thread == nil {
		// 缺少线程时没有可恢复入口，直接忽略。
		return
	}

	// 写入全局 continuation 表，并通知 runtime 下一次 Resume 走续执行路径。
	luaContinuationMu.Lock()
	if current := luaContinuations[continuation.thread]; current != nil {
		// 已有内层 continuation 时，把新现场挂到链尾；下一次 resume 先恢复最内层，再回到外层 VM。
		tail := current
		for tail.parent != nil {
			tail = tail.parent
		}
		tail.parent = continuation
	} else {
		luaContinuations[continuation.thread] = continuation
	}
	luaContinuationMu.Unlock()
	if state := continuation.thread.State(); state != nil {
		// 记录同一 State 最近挂起的线程，供 yield 后 runningThread 已恢复父线程的 pcall/xpcall 定位。
		luaContinuationMu.Lock()
		luaLastContinuationThreads[state] = continuation.thread
		luaContinuationMu.Unlock()
	}
	continuation.thread.MarkContinuationPending(true)
}

// takeLuaContinuation 取出线程的 Lua VM continuation。
//
// 取出后会清理 continuationPending 标记；若执行过程中再次 yield，会重新保存新的 continuation。
func takeLuaContinuation(thread *runtime.Thread) *luaCoroutineContinuation {
	if thread == nil {
		// nil 线程没有 continuation。
		return nil
	}

	// 从表中原子取出 continuation，避免同一现场被重复恢复。
	luaContinuationMu.Lock()
	continuation := luaContinuations[thread]
	delete(luaContinuations, thread)
	if state := thread.State(); state != nil && luaLastContinuationThreads[state] == thread {
		// 当前线程的 continuation 被取出后清理最近挂起记录；再次 yield 会重新写入。
		delete(luaLastContinuationThreads, state)
	}
	luaContinuationMu.Unlock()
	thread.MarkContinuationPending(false)
	return continuation
}

// appendContinuationParent 把已取出的父 continuation 续接到重新 yield 后的新 continuation 链尾。
//
// thread 必须是当前恢复中的协程；parent 是尚未执行的外层 continuation。内层恢复过程中再次
// coroutine.yield 时，新的内层现场会重新写入全局表，本函数负责保留外层 pcall/xpcall 或 Lua
// 调用现场，避免下一次 resume 只恢复最内层而丢失 protected-call 返回包装。
func appendContinuationParent(thread *runtime.Thread, parent *luaCoroutineContinuation) {
	if thread == nil || parent == nil {
		// 缺少线程或父现场时没有可续接内容。
		return
	}
	luaContinuationMu.Lock()
	current := luaContinuations[thread]
	if current == nil {
		// 理论上 ErrCoroutineYield 已保存新现场；缺失时直接以父现场兜底，避免永久丢链。
		luaContinuations[thread] = parent
		if state := thread.State(); state != nil {
			// 兜底写入时也刷新最近挂起线程。
			luaLastContinuationThreads[state] = thread
		}
		luaContinuationMu.Unlock()
		thread.MarkContinuationPending(true)
		return
	}
	tail := current
	for tail.parent != nil {
		// 找到新保存现场链尾，再挂回尚未执行的父 continuation。
		tail = tail.parent
	}
	tail.parent = parent
	if state := thread.State(); state != nil {
		// 重新挂父链后保持最近挂起线程指向当前协程。
		luaLastContinuationThreads[state] = thread
	}
	luaContinuationMu.Unlock()
	thread.MarkContinuationPending(true)
}

// Value 表示 Lua 运行时值。
//
// 直接复用 runtime.Value，避免调用方跨包误用内置实现细节。
type Value = runtime.Value

// ValueKind 表示 Lua 类型标签。
//
// 该枚举对齐 runtime.ValueKind 并保持后续 API 稳定性。
type ValueKind = runtime.ValueKind

// Options 表示 State 配置。
//
// 约束资源上限并通过 State 创建注入，后续与 lauxlib 风格 API 对齐。
type Options = runtime.Options

// SyntaxSet 表示一组可选语法扩展。
//
// 嵌入方可通过 WithSyntaxExtensions 或 WithoutSyntaxExtensions 写入 Options。
type SyntaxSet = extensions.SyntaxSet

// GoFunction 表示 Go 回调函数。
//
// 入参是 Lua 调用参数列表，返回单值和错误。错误会进入 protected call。
type GoFunction = runtime.GoFunction

// GoResultsFunction 表示可返回多值的 Go 回调。
//
// 与标准库及元方法分发的返回约定一致，返回 []Value 供 Lua 值栈接收。
type GoResultsFunction = runtime.GoResultsFunction

// Function 表示 lua 包推荐的 Go 回调签名。
//
// 入参 args 是 Lua 调用传入的实参快照；返回值按顺序写回 Lua 多返回值列表。返回 error 时，
// ProtectedCall 或后续 bridge 层会把错误转换为 Lua error object。
type Function = GoResultsFunction

// ProtectedCallFunc 表示 protected call 边界内的执行函数。
//
// 需要处理保护边界与 pcall/xpcall 语义时使用该签名。
type ProtectedCallFunc = runtime.ProtectedCallFunc

// ErrorHandler 表示 xpcall 的错误处理函数。
//
// 入参为 Lua 错误对象，返回处理后的 Lua 对象；返回 error 时转为 xpcall 结果。
type ErrorHandler = runtime.ErrorHandler

// RuntimeError 表示可传播到 Lua 层的运行时错误。
//
// 嵌入方可通过 errors.As 识别该类型，并通过 ErrorObject 取回 Lua 侧 error object。
type RuntimeError = runtime.RuntimeError

// ErrorClass 表示 runtime 层可识别的错误分类。
//
// 该类型用于 CLI、嵌入 API 和后续 Debug traceback 对错误做稳定分流。
type ErrorClass = runtime.ErrorClass

// ResourceLimitKind 表示触发的资源限制类型。
//
// 嵌入方可结合 ResourceLimitError.Kind 判断栈、调用深度或分配预算限制。
type ResourceLimitKind = runtime.ResourceLimitKind

// ResourceLimitError 表示 Lua VM 运行中触发资源限制。
//
// 该错误支持 errors.As，字段包含限制类型、上限、实际值和错误消息。
type ResourceLimitError = runtime.ResourceLimitError

// 与 runtime 对齐的类型常量与默认参数。
const (
	// KindNil 对齐 Lua 5.3 nil 类型标签。
	KindNil = runtime.KindNil
	// KindBoolean 对齐 Lua 5.3 boolean 类型标签。
	KindBoolean = runtime.KindBoolean
	// KindInteger 对齐 Lua 5.3 integer 类型标签。
	KindInteger = runtime.KindInteger
	// KindNumber 对齐 Lua 5.3 number 类型标签。
	KindNumber = runtime.KindNumber
	// KindString 对齐 Lua 5.3 string 类型标签。
	KindString = runtime.KindString
	// KindTable 对齐 Lua 5.3 table 类型标签。
	KindTable = runtime.KindTable
	// KindLuaClosure 对齐 Lua 5.3 lua function 类型标签。
	KindLuaClosure = runtime.KindLuaClosure
	// KindGoClosure 对齐 Go callable closure 类型标签。
	KindGoClosure = runtime.KindGoClosure
	// KindUserdata 对齐 Lua userdata 类型标签。
	KindUserdata = runtime.KindUserdata
	// KindThread 对齐 Lua thread/coroutine 类型标签。
	KindThread = runtime.KindThread

	// RegistryPseudoIndex 对齐 runtime.RegistryPseudoIndex。
	RegistryPseudoIndex = runtime.RegistryPseudoIndex
	// RegistryIndexMainThread 对齐 runtime.RegistryIndexMainThread。
	RegistryIndexMainThread = runtime.RegistryIndexMainThread
	// RegistryIndexGlobals 对齐 runtime.RegistryIndexGlobals。
	RegistryIndexGlobals = runtime.RegistryIndexGlobals

	// DefaultMaxStackDepth 对齐 runtime.DefaultMaxStackDepth。
	DefaultMaxStackDepth = runtime.DefaultMaxStackDepth
	// DefaultMaxCallDepth 对齐 runtime.DefaultMaxCallDepth。
	DefaultMaxCallDepth = runtime.DefaultMaxCallDepth

	// ErrorClassRuntime 表示 Lua 运行期错误分类。
	ErrorClassRuntime = runtime.ErrorClassRuntime
	// ErrorClassResourceLimit 表示资源限制错误分类。
	ErrorClassResourceLimit = runtime.ErrorClassResourceLimit
	// ErrorClassOther 表示当前 runtime 包不能识别的错误分类。
	ErrorClassOther = runtime.ErrorClassOther

	// ResourceLimitStack 表示 Lua 栈深度超过限制。
	ResourceLimitStack = runtime.ResourceLimitStack
	// ResourceLimitCall 表示调用深度超过限制。
	ResourceLimitCall = runtime.ResourceLimitCall
	// ResourceLimitAllocation 表示分配预算超过限制。
	ResourceLimitAllocation = runtime.ResourceLimitAllocation

	// SyntaxContinue 表示 continue 语句语法糖扩展。
	SyntaxContinue = extensions.SyntaxContinue
	// SyntaxSwitch 表示 switch/case/default 语句语法糖扩展。
	SyntaxSwitch = extensions.SyntaxSwitch
	// SyntaxConst 表示 const 只读局部变量语法糖扩展。
	SyntaxConst = extensions.SyntaxConst

	// luaContextCheckInstructionInterval 表示普通 VM 热路径两次 context 检查之间允许跳过的指令数。
	//
	// debug hook、协程和 continuation 路径仍逐指令检查，以保持调试和挂起恢复语义；普通路径
	// 使用固定窗口降低 tight loop 中 atomic/context 查询的固定成本。
	luaContextCheckInstructionInterval = 128
)

// 与 runtime 对齐的错误对象。
var (
	// ErrClosedState 表示 State 已关闭。
	ErrClosedState = runtime.ErrClosedState
	// ErrStackUnderflow 表示栈读取空洞。
	ErrStackUnderflow = runtime.ErrStackUnderflow
	// ErrCallFrameUnderflow 表示调用帧栈读取空洞。
	ErrCallFrameUnderflow = runtime.ErrCallFrameUnderflow
	// ErrNilProtectedCall 表示 protected call 为空。
	ErrNilProtectedCall = runtime.ErrNilProtectedCall
	// ErrNilState 表示收到空 state 指针。
	ErrNilState = runtime.ErrNilState
	// ErrNilContext 表示设置 context 时传入 nil。
	ErrNilContext = runtime.ErrNilContext
	// ErrLuaError 表示 Lua `error` 主动抛出的运行时错误原因。
	ErrLuaError = runtime.ErrLuaError
	// ErrExpectedCallable 表示调用语义遇到了不可调用的值。
	ErrExpectedCallable = runtime.ErrExpectedCallable
	// ErrExecutionUnavailable 表示脚本已经完成加载，但当前阶段尚未接入完整 Proto 执行器。
	ErrExecutionUnavailable = errors.New("lua execution is not wired yet")
)

// debugNameCacheKey 表示 Lua closure 调试名称缓存键。
//
// state 区分不同全局环境；closure 使用 LuaClosure 指针 identity，避免跨闭包复用错误名称。
type debugNameCacheKey struct {
	// state 保存当前 Lua 状态指针。
	state *State
	// closure 保存被调 Lua closure 指针。
	closure *runtime.LuaClosure
}

// debugNameCacheEntry 表示一次 debug 名称推断缓存。
//
// globalsVersion 是写入缓存时 `_G` 的 raw 写入版本；版本变化后缓存必须失效并重新扫描。
type debugNameCacheEntry struct {
	// name 保存推断出的函数名，空字符串也可表示一次负缓存。
	name string
	// nameWhat 保存函数名来源，例如 global。
	nameWhat string
	// globalsVersion 保存缓存依赖的全局表版本。
	globalsVersion uint64
}

// luaDebugNameCache 缓存 Lua closure 到全局函数名的推断结果。
//
// 该缓存是性能优化：官方 constructs.lua 会大量调用本地 Lua closure，负缓存能避免每次都
// 遍历并排序 `_G`；全局表版本变化时会自动重扫。
var luaDebugNameCache sync.Map

// NewState 创建默认配置的 Lua State。
//
// 以 runtime.NewState 作为实现源，返回生命周期可控的最小入口。
func NewState() *State {
	state := runtime.NewState()
	registerLuaMetamethodRunner(state)
	return state
}

// NewStateWithOptions 创建带自定义选项的 Lua State。
//
// options 经过 runtime.NormalizeOptions 处理，零值字段会替换为默认上限。
func NewStateWithOptions(options Options) *State {
	// State 创建逻辑集中在 runtime，lua 包只暴露稳定入口。
	state := runtime.NewStateWithOptions(options)
	registerLuaMetamethodRunner(state)
	return state
}

// NewStateWithContext 创建带上下文和自定义选项的 Lua State。
//
// ctx 必须非 nil；options 会通过 NormalizeOptions 填充默认资源限制。返回的 State 会立即绑定
// ctx，后续 Call、ProtectedCall 和 VM 检查点可通过 CheckContext 观察取消或超时。
func NewStateWithContext(ctx context.Context, options Options) (*State, error) {
	// 先拒绝 nil context，避免创建出无法提供取消语义的 State。
	if ctx == nil {
		// nil context 没有取消信号来源，返回明确错误便于宿主修正调用方式。
		return nil, ErrNilContext
	}
	state := NewStateWithOptions(options)
	if err := SetContext(state, ctx); err != nil {
		// 理论上新建 State 不应设置失败；若失败则关闭 State，避免泄漏半初始化对象。
		state.Close()
		return nil, err
	}
	return state, nil
}

// DefaultOptions 返回 Lua State 的默认资源限制配置。
//
// 返回值包含默认栈深度、默认调用深度和不限制分配预算，调用方可在此基础上修改局部字段后传入
// NewStateWithOptions 或 NewStateWithContext。
func DefaultOptions() Options {
	// 零值 Options 经 NormalizeOptions 后会填充项目默认资源限制。
	return runtime.NormalizeOptions(runtime.Options{})
}

// NormalizeOptions 规范化 Lua State 资源限制配置。
//
// options 允许零值；返回值会填充默认栈深度、默认调用深度，并把负分配预算归一为 0。
func NormalizeOptions(options Options) Options {
	// 直接复用 runtime 的规范化逻辑，保证 lua 包与 VM 实际限制一致。
	return runtime.NormalizeOptions(options)
}

// WithSyntaxExtensions 返回启用指定语法集合后的 Options 副本。
//
// syntax 为 0 时表示 Lua 5.3 兼容模式；未编译进当前二进制的扩展会被自动裁剪。
func WithSyntaxExtensions(options Options, syntax SyntaxSet) Options {
	// 委托 runtime.Options 保持 lua 包和 runtime 实际配置一致。
	return options.WithSyntaxExtensions(syntax)
}

// WithoutSyntaxExtensions 返回关闭指定语法扩展后的 Options 副本。
//
// options 未显式配置语法集合时会先使用默认扩展集合，再移除 disabled 指定项。
func WithoutSyntaxExtensions(options Options, disabled SyntaxSet) Options {
	// 委托 runtime.Options 统一处理默认集合与裁剪规则。
	return options.WithoutSyntaxExtensions(disabled)
}

// WithGluaEvents 返回配置 glua 自定义事件运行能力后的 Options 副本。
//
// enabled 为 true 时仍受当前 build tag 裁剪；未编译事件能力的二进制会在 NormalizeOptions 中保持关闭。
func WithGluaEvents(options Options, enabled bool) Options {
	// 委托 runtime.Options 统一处理默认值与 build tag 裁剪。
	return options.WithGluaEvents(enabled)
}

// DefaultSyntaxExtensions 返回当前构建产物默认启用的语法扩展集合。
func DefaultSyntaxExtensions() SyntaxSet {
	// 默认集合等于当前二进制已经编译的扩展集合。
	return extensions.Default()
}

// Close 关闭 Lua State。
//
// state 允许为 nil；nil 时返回 ErrNilState。非 nil State 会调用 runtime.Close，释放 registry、
// globals、栈、调用帧和 userdata finalizer；重复关闭保持幂等。
func Close(state *State) error {
	if state == nil {
		// nil State 没有可释放资源，返回明确错误便于宿主定位生命周期问题。
		return ErrNilState
	}
	state.Close()
	return nil
}

// OpenLibs 注册当前已实现的 Lua 5.3 标准库。
//
// state 必须非 nil 且未关闭；注册顺序先 base/package，再注册 table、string、math、io、os、
// utf8 和 debug，避免 package.loaded 初始化晚于其他库。注册成功的库会写入 package.loaded，
// 使 require("string") 等标准模块加载路径与 Lua 5.3 CLI 保持一致。
func OpenLibs(state *State) error {
	if state == nil {
		// nil State 没有全局环境，无法注册标准库。
		return ErrNilState
	}
	registerLuaMetamethodRunner(state)
	registerTableFinalizerRunner(state)
	if err := validateGluaNamespace(state.Globals()); err != nil {
		// 宿主占用 glua 为非 table 时不能静默跳过全部扩展注册。
		return err
	}
	if err := baselib.Open((*runtime.State)(state)); err != nil {
		// base 库注册失败时没有稳定全局环境，直接返回。
		return err
	}
	registerProtectedCallGlobals(state)
	registerGluaSerializationGlobals(state)
	registerGluaUtilityGlobals(state)
	if state.Options().GluaEventsEnabled {
		// glua event 是运行期扩展能力，只有当前 State 显式开启且构建产物包含时才注册全局 API。
		registerGluaEventGlobals(state)
	}
	if err := openPackageWithStateCaller(state); err != nil {
		// package 库负责 require 和 package.loaded，失败时后续库无法登记模块缓存。
		return err
	}
	if err := registerLoadedLibrary(state, "_G"); err != nil {
		// _G 是基础环境模块，登记失败说明 package.loaded 不可用。
		return err
	}
	if err := registerLoadedLibrary(state, "package"); err != nil {
		// package 自身也应可被 require 命中。
		return err
	}

	libraries := []struct {
		// name 是标准库全局名，同时也是 package.loaded 的模块名。
		name string
		// opener 把对应标准库表写入全局环境。
		opener func(*runtime.State) error
	}{
		{name: "coroutine", opener: openCoroutineLibrary},
		{name: "table", opener: tablelib.Open},
		{name: "string", opener: stringlib.Open},
		{name: "math", opener: mathlib.Open},
		{name: "io", opener: iolib.Open},
		{name: "os", opener: oslib.Open},
		{name: "utf8", opener: utf8lib.Open},
		{name: "debug", opener: debuglib.Open},
	}
	for _, library := range libraries {
		// 任一标准库注册失败都立即返回，避免产生半初始化后继续执行的假象。
		if err := library.opener((*runtime.State)(state)); err != nil {
			return err
		}
		if err := registerLoadedLibrary(state, library.name); err != nil {
			// 标准库已打开但未登记到 package.loaded 会破坏 require("lib") 兼容性。
			return err
		}
	}
	return nil
}

// registerProtectedCallGlobals 注册支持 coroutine continuation 的 pcall/xpcall。
//
// state 必须已打开 base 库；本函数覆盖 base.Open 注册的 pcall/xpcall，使完整 Lua 环境中的
// protected call 通过 lua 包主执行器运行 Lua closure，并在 coroutine.yield 后恢复返回布局。
func registerProtectedCallGlobals(state *State) {
	if state == nil || state.Globals() == nil {
		// 无效 State 没有全局表，保持 base.Open 的错误语义由调用方处理。
		return
	}
	globals := state.Globals()
	globals.RawSetString("pcall", runtime.ReferenceValue(runtime.KindGoClosure, runtime.GoResultsFunction(func(args ...runtime.Value) ([]runtime.Value, error) {
		// pcall 需要当前 State 和 continuation 表，因此在 lua 包内实现完整路径。
		return pCallWithContinuation(state, args...)
	})))
	globals.RawSetString("xpcall", runtime.ReferenceValue(runtime.KindGoClosure, runtime.GoResultsFunction(func(args ...runtime.Value) ([]runtime.Value, error) {
		// xpcall 的被调函数或 handler 都可能 yield，统一由 continuation-aware 入口处理。
		return xPCallWithContinuation(state, args...)
	})))
}

// pCallWithContinuation 实现支持 coroutine.yield 的 Lua 5.3 pcall。
//
// 第一个参数必须可调用；被调函数成功时返回 true 加原始返回值，失败时返回 false 加 Lua error
// object。若被调函数 yield，则保存 pcall continuation，恢复后再根据内层完成结果生成返回布局。
func pCallWithContinuation(state *State, args ...runtime.Value) ([]runtime.Value, error) {
	if len(args) == 0 {
		// 缺少函数时 pcall 本身成功返回 false 和可见错误对象。
		return []runtime.Value{runtime.BooleanValue(false), runtime.StringValue(runtime.ErrExpectedCallable.Error())}, nil
	}
	baseCallDepth := state.CallDepth()
	finishEvent, eventErr := beginProtectedGluaFunctionEvent(state, args[0], args[1:])
	if eventErr != nil {
		// call 事件错误属于 protected target 调用失败，由 pcall 转换为 false/error。
		return finishPCallContinuation(nil, eventErr), nil
	}
	callResults, err := callWithDebugName(state, args[0], "", "", args[1:]...)
	if errors.Is(err, runtime.ErrCoroutineYield) {
		// yield 不是 pcall 捕获的 Lua 错误；保存外层包装现场并交给 coroutine.resume 返回 yield 值。
		saveProtectedCallContinuation(state, func(resumeArgs []Value, callErr error) ([]Value, error) {
			// target 最终恢复后补齐 function_return/error/exit，再生成 pcall 布局。
			if finishErr := finishEvent(resumeArgs, callErr); finishErr != nil {
				callErr = finishErr
			}
			return resumePCallContinuation(resumeArgs, callErr)
		})
		return nil, err
	}
	if finishErr := finishEvent(callResults, err); finishErr != nil {
		// 生命周期回调错误与普通 target 错误一样由 pcall 捕获。
		err = finishErr
	}
	if err != nil {
		// 普通错误由 pcall 捕获；被调函数留下的错误帧必须裁剪到 pcall 入口边界。
		popCallFramesAbove(state, baseCallDepth)
		state.ClearPendingErrorTracebackFrames()
	}
	return finishPCallContinuation(callResults, err), nil
}

// resumePCallContinuation 在内层 yield 恢复后补齐 pcall 返回布局。
//
// args 是内层 Lua continuation 的返回值；callErr 是内层最终错误。返回值始终为 pcall 的
// Lua 可见结果，不再向外抛出普通 Lua error。
func resumePCallContinuation(args []Value, callErr error) ([]Value, error) {
	// pcall 恢复点复用普通收尾逻辑，确保错误对象和成功多返回值布局一致。
	return finishPCallContinuation(args, callErr), nil
}

// finishPCallContinuation 生成 pcall 的最终返回值。
func finishPCallContinuation(callResults []Value, callErr error) []Value {
	if callErr != nil {
		// 被调函数错误需要转换为 false/errorObject，而不是继续上抛。
		return []runtime.Value{runtime.BooleanValue(false), runtime.ErrorObject(callErr)}
	}
	results := []runtime.Value{runtime.BooleanValue(true)}
	results = append(results, callResults...)
	return results
}

// xPCallWithContinuation 实现支持 coroutine.yield 的 Lua 5.3 xpcall。
//
// 第一个参数是被调函数，第二个参数是错误处理函数。被调函数或错误处理函数 yield 时均保存
// 对应 continuation，恢复后继续生成 xpcall 可见的 true/false 返回布局。
func xPCallWithContinuation(state *State, args ...runtime.Value) ([]runtime.Value, error) {
	if len(args) < 2 {
		// 缺少函数或 handler 时，xpcall 本身成功返回 false 和可见错误对象。
		return []runtime.Value{runtime.BooleanValue(false), runtime.StringValue(runtime.ErrExpectedCallable.Error())}, nil
	}
	handler := args[1]
	baseCallDepth := state.CallDepth()
	finishEvent, eventErr := beginProtectedGluaFunctionEvent(state, args[0], args[2:])
	if eventErr != nil {
		// call 事件错误进入 xpcall 的 handler 流程。
		return finishXPCallCall(state, handler, nil, eventErr, baseCallDepth)
	}
	callResults, err := callWithDebugName(state, args[0], "", "", args[2:]...)
	if errors.Is(err, runtime.ErrCoroutineYield) {
		// 被调函数 yield 时先保存 xpcall 的 call 阶段现场，待其恢复后再决定是否调用 handler。
		saveProtectedCallContinuation(state, func(resumeArgs []Value, callErr error) ([]Value, error) {
			// target 最终恢复后先补齐事件，再执行 xpcall 收尾。
			if finishErr := finishEvent(resumeArgs, callErr); finishErr != nil {
				callErr = finishErr
			}
			return resumeXPCallCallContinuation(state, handler, resumeArgs, callErr, baseCallDepth)
		})
		return nil, err
	}
	if finishErr := finishEvent(callResults, err); finishErr != nil {
		// 生命周期回调错误由 xpcall handler 处理。
		err = finishErr
	}
	return finishXPCallCall(state, handler, callResults, err, baseCallDepth)
}

// resumeXPCallCallContinuation 在 xpcall 被调函数恢复后继续收尾。
func resumeXPCallCallContinuation(state *State, handler Value, args []Value, callErr error, baseCallDepth int) ([]Value, error) {
	// 被调函数恢复后复用普通 xpcall call 阶段收尾逻辑。
	return finishXPCallCall(state, handler, args, callErr, baseCallDepth)
}

// finishXPCallCall 生成 xpcall 被调函数阶段的结果，必要时调用错误处理函数。
func finishXPCallCall(state *State, handler Value, callResults []Value, callErr error, baseCallDepth int) ([]Value, error) {
	if callErr == nil {
		// 成功路径返回 true 后接被调函数返回值。
		results := []runtime.Value{runtime.BooleanValue(true)}
		results = append(results, callResults...)
		return results, nil
	}
	errorFrames := state.TracebackFrames()
	if isCallDepthOverflow(callErr) {
		// 调用深度溢出会在最深失败点提前保存 traceback；外层 xpcall 不能用较浅现场覆盖它。
		if pendingFrames := state.PendingErrorTracebackFrames(); len(pendingFrames) > 0 {
			errorFrames = pendingFrames
		}
	}
	// 普通错误必须刷新为当前失败现场，避免前序 protected call 残留快照污染错误处理器。
	state.SetPendingErrorTracebackFrames(errorFrames)
	// 调用错误处理器前释放真实调用帧，避免深栈溢出后 handler 再次压帧失败。
	popCallFramesAbove(state, baseCallDepth)
	state.EnterErrorHandler()
	handlerResults, handlerErr := callWithDebugName(state, handler, "", "", runtime.ErrorObject(callErr))
	state.ExitErrorHandler()
	if handlerErr != nil {
		// handler 失败也属于 xpcall 捕获范围，返回前必须裁剪 handler 留下的错误帧。
		popCallFramesAbove(state, baseCallDepth)
	}
	if errors.Is(handlerErr, runtime.ErrCoroutineYield) {
		// handler 也允许 yield；恢复后需要按 handler 的最终结果生成 false/errorObject。
		saveProtectedCallContinuation(state, func(resumeArgs []Value, resumeErr error) ([]Value, error) {
			// handler 恢复后再裁剪失败函数留下的帧，确保 debug.traceback 在 handler 内能看到错误现场。
			popCallFramesAbove(state, baseCallDepth)
			state.ClearPendingErrorTracebackFrames()
			return resumeXPCallHandlerContinuation(resumeArgs, resumeErr)
		})
		return nil, handlerErr
	}
	// handler 已经读取完错误现场；返回 xpcall 结果前清除快照，避免后续 debug.traceback 误用旧现场。
	state.ClearPendingErrorTracebackFrames()
	return finishXPCallHandler(handlerResults, handlerErr), nil
}

// resumeXPCallHandlerContinuation 在 xpcall handler 恢复后生成最终错误返回。
func resumeXPCallHandlerContinuation(args []Value, handlerErr error) ([]Value, error) {
	// handler 恢复后仍属于 xpcall 错误处理阶段，返回 false 和 handler 结果或 handler 错误对象。
	return finishXPCallHandler(args, handlerErr), nil
}

// finishXPCallHandler 生成 xpcall handler 阶段的最终返回值。
func finishXPCallHandler(handlerResults []Value, handlerErr error) []Value {
	if handlerErr != nil {
		// handler 自身失败时，xpcall 返回 handler 的错误对象。
		errorObject := runtime.ErrorObject(handlerErr)
		if errorObject.IsNil() {
			// error handler 以 nil 再次抛错时，Lua 5.3 返回字符串错误对象而不是 nil。
			errorObject = runtime.StringValue("error in error handling")
		}
		return []runtime.Value{runtime.BooleanValue(false), errorObject}
	}
	if len(handlerResults) == 0 {
		// handler 无返回值时，错误对象按 nil 处理。
		return []runtime.Value{runtime.BooleanValue(false), runtime.NilValue()}
	}
	return []runtime.Value{runtime.BooleanValue(false), handlerResults[0]}
}

// saveProtectedCallContinuation 保存 pcall/xpcall 的 Go 层恢复现场。
//
// state 必须正在某个非主 coroutine 中执行；resume 在内层 Lua continuation 完成后被调用，用于
// 把 Lua 结果或错误转换为 protected-call 可见的返回值。
func saveProtectedCallContinuation(state *State, resume func(args []Value, callErr error) ([]Value, error)) {
	if state == nil || resume == nil {
		// 缺少 State 或恢复函数时没有可保存现场。
		return
	}
	thread := currentCoroutineThread(state)
	if thread == nil {
		// coroutine.yield 会先把 runningThread 切回父线程；此时从已保存的 Lua continuation 反查挂起线程。
		thread = pendingContinuationThread(state)
		if thread == nil {
			// 非 coroutine yield 路径不应保存 continuation，错误继续向上传播。
			return
		}
	}
	saveLuaContinuation(&luaCoroutineContinuation{
		thread:   thread,
		goResume: resume,
	})
}

// pendingContinuationThread 返回当前 State 上刚保存的挂起 coroutine continuation 所属线程。
//
// pcall/xpcall 收到 ErrCoroutineYield 时，runtime.Thread.Yield 已经恢复父 running thread，不能再
// 依赖 State.Running 定位挂起协程。本函数扫描 pending continuation 表，选择同一 State 的线程。
func pendingContinuationThread(state *State) *runtime.Thread {
	if state == nil {
		// nil State 没有可匹配的 continuation。
		return nil
	}
	luaContinuationMu.Lock()
	defer luaContinuationMu.Unlock()
	if thread := luaLastContinuationThreads[(*runtime.State)(state)]; thread != nil {
		// 最近挂起线程是当前 State 最精确的 yield 归属，优先使用它避免命中旧协程残留。
		return thread
	}
	for thread := range luaContinuations {
		if thread != nil && thread.State() == (*runtime.State)(state) {
			// 命中同一 State 的挂起线程，返回给 protected-call continuation 追加使用。
			return thread
		}
	}
	return nil
}

// popCallFramesAbove 裁剪指定调用深度之上的帧。
//
// state 必须来自当前执行路径；baseDepth 是进入 protected-call 前记录的调用深度。该 helper 用于
// pcall/xpcall 捕获普通错误后清理被调函数保留的错误帧，yield 路径不调用它以保留 continuation。
func popCallFramesAbove(state *State, baseDepth int) {
	if state == nil {
		// nil State 没有调用帧。
		return
	}
	for state.CallDepth() > baseDepth {
		// 逐层弹出错误路径留下的 Lua/Go 调用帧。
		_, _ = state.PopCallFrame()
	}
}

// openCoroutineLibrary 注册 Lua 5.3 coroutine 标准库的最小可执行面。
//
// runtimeState 必须非 nil 且未关闭；当前实现支持 create、resume、yield、status、running 和 wrap，
// 其中 Lua closure 协程通过 State 注入的 runner 执行，yield 使用内部哨兵中断当前 VM。
func openCoroutineLibrary(runtimeState *runtime.State) error {
	if runtimeState == nil {
		// nil State 无法注册 coroutine 表。
		return ErrNilState
	}
	if runtimeState.IsClosed() {
		// 已关闭 State 不能写入全局环境。
		return ErrClosedState
	}
	state := (*State)(runtimeState)
	state.SetLuaThreadRunner(func(thread *runtime.Thread, args ...runtime.Value) ([]runtime.Value, error) {
		// Lua closure 协程入口优先恢复 yield continuation；首次 resume 才从函数入口执行。
		return executeLuaThreadClosure(state, thread, args...)
	})

	coroutineTable := runtime.NewTable()
	coroutineTable.RawSetString("create", runtime.ReferenceValue(runtime.KindGoClosure, runtime.GoResultsFunction(func(args ...runtime.Value) ([]runtime.Value, error) {
		// create 需要当前 State 创建 Thread，因此通过闭包捕获 state。
		return coroutineCreate(state, args...)
	})))
	coroutineTable.RawSetString("resume", runtime.ReferenceValue(runtime.KindGoClosure, runtime.GoResultsFunction(coroutineResume)))
	coroutineTable.RawSetString("yield", runtime.ReferenceValue(runtime.KindGoClosure, runtime.GoResultsFunction(func(args ...runtime.Value) ([]runtime.Value, error) {
		// yield 需要读取当前 running thread，因此通过闭包捕获 state。
		return coroutineYield(state, args...)
	})))
	coroutineTable.RawSetString("status", runtime.ReferenceValue(runtime.KindGoClosure, runtime.GoResultsFunction(coroutineStatus)))
	coroutineTable.RawSetString("running", runtime.ReferenceValue(runtime.KindGoClosure, runtime.GoResultsFunction(func(args ...runtime.Value) ([]runtime.Value, error) {
		// running 需要读取当前 State 运行线程。
		return coroutineRunning(state, args...)
	})))
	coroutineTable.RawSetString("isyieldable", runtime.ReferenceValue(runtime.KindGoClosure, runtime.GoResultsFunction(func(args ...runtime.Value) ([]runtime.Value, error) {
		// isyieldable 需要读取当前 State 的运行线程与 Go 回调边界。
		return coroutineIsYieldable(state, args...)
	})))
	coroutineTable.RawSetString("wrap", runtime.ReferenceValue(runtime.KindGoClosure, runtime.GoResultsFunction(func(args ...runtime.Value) ([]runtime.Value, error) {
		// wrap 需要当前 State 创建 Thread。
		return coroutineWrap(state, args...)
	})))
	runtimeState.Globals().RawSetString("coroutine", runtime.ReferenceValue(runtime.KindTable, coroutineTable))
	return nil
}

// coroutineCreate 实现 coroutine.create。
//
// state 必须非 nil；第一个参数必须是 Go closure 或 Lua closure。返回值是 thread 引用，真正执行
// 延迟到 resume。
func coroutineCreate(state *State, args ...runtime.Value) ([]runtime.Value, error) {
	if state == nil {
		// nil State 无法创建协程。
		return nil, ErrNilState
	}
	if len(args) == 0 || (args[0].Kind != runtime.KindGoClosure && args[0].Kind != runtime.KindLuaClosure) {
		// create 的第一个参数必须可调用。
		return nil, runtime.RaiseError(runtime.StringValue("bad argument #1 to 'create' (function expected)"))
	}
	thread := state.NewThread(args[0])
	if thread == nil {
		// State 生命周期异常时无法创建线程。
		return nil, runtime.ErrNilThreadState
	}
	return []runtime.Value{runtime.ReferenceValue(runtime.KindThread, thread)}, nil
}

// coroutineResume 实现 coroutine.resume。
//
// 第一个参数必须是 thread；成功返回 true 加协程返回值，失败返回 false 加错误字符串，匹配 Lua
// 5.3 resume 的非抛错语义。
func coroutineResume(args ...runtime.Value) ([]runtime.Value, error) {
	thread, err := coroutineThreadArgument(args, "resume")
	if err != nil {
		// 参数类型错误直接按 Lua 参数错误抛出。
		return nil, err
	}
	results, resumeErr := thread.Resume(args[1:]...)
	if resumeErr != nil {
		// resume 失败不抛出，返回 false 和原始 Lua error object；非 Lua error 会退化为字符串对象。
		return []runtime.Value{runtime.BooleanValue(false), runtime.ErrorObject(resumeErr)}, nil
	}
	return append([]runtime.Value{runtime.BooleanValue(true)}, results...), nil
}

// coroutineYield 实现 coroutine.yield。
//
// state 用于定位当前 running thread；成功 yield 后返回内部 ErrCoroutineYield，供 Lua closure
// 执行循环中断并由 Thread.Resume 转换为 resume 成功返回。
func coroutineYield(state *State, args ...runtime.Value) ([]runtime.Value, error) {
	if state == nil {
		// nil State 没有当前协程。
		return nil, ErrNilState
	}
	thread, isMain := state.Running()
	if thread == nil || isMain {
		// 主线程或无运行线程都不能 yield。
		return nil, runtime.NewRuntimeError(runtime.StringValue("attempt to yield from outside a coroutine"), runtime.ErrYieldFromMainThread)
	}
	if err := thread.Yield(args...); err != nil {
		// yield 状态错误原样返回。
		return nil, err
	}
	return nil, runtime.ErrCoroutineYield
}

// coroutineStatus 实现 coroutine.status。
//
// 第一个参数必须是 thread；返回当前协程可见状态字符串。
func coroutineStatus(args ...runtime.Value) ([]runtime.Value, error) {
	thread, err := coroutineThreadArgument(args, "status")
	if err != nil {
		// 参数类型错误直接按 Lua 参数错误抛出。
		return nil, err
	}
	return []runtime.Value{runtime.StringValue(string(thread.Status()))}, nil
}

// coroutineRunning 实现 coroutine.running。
//
// 返回当前 running thread 与是否主线程的布尔值；无运行线程时返回 nil 和 false。
func coroutineRunning(state *State, args ...runtime.Value) ([]runtime.Value, error) {
	_ = args
	if state == nil {
		// nil State 没有 running thread。
		return []runtime.Value{runtime.NilValue(), runtime.BooleanValue(false)}, nil
	}
	thread, isMain := state.Running()
	if thread == nil {
		// 运行态缺失时按无当前协程返回。
		return []runtime.Value{runtime.NilValue(), runtime.BooleanValue(false)}, nil
	}
	return []runtime.Value{runtime.ReferenceValue(runtime.KindThread, thread), runtime.BooleanValue(isMain)}, nil
}

// coroutineIsYieldable 实现 coroutine.isyieldable。
//
// 返回当前执行点是否允许 `coroutine.yield`；主线程、nil State、关闭 State 和 Go 回调边界
// 均返回 false，不抛出参数错误，兼容 Lua 5.3 官方 coroutine.lua 的基础断言。
func coroutineIsYieldable(state *State, args ...runtime.Value) ([]runtime.Value, error) {
	_ = args
	if state == nil {
		// nil State 没有运行上下文，按不可 yield 返回。
		return []runtime.Value{runtime.BooleanValue(false)}, nil
	}

	// 运行时集中判断主线程、关闭状态、running thread 与 Go 回调边界。
	return []runtime.Value{runtime.BooleanValue(state.IsYieldable())}, nil
}

// coroutineWrap 实现 coroutine.wrap。
//
// 第一个参数必须是函数；返回一个 Go closure 包装同一 thread。resume 失败时 wrap 直接抛出错误，
// 成功时只透传协程返回值。
func coroutineWrap(state *State, args ...runtime.Value) ([]runtime.Value, error) {
	if state == nil {
		// nil State 无法创建协程。
		return nil, ErrNilState
	}
	if len(args) == 0 || (args[0].Kind != runtime.KindGoClosure && args[0].Kind != runtime.KindLuaClosure) {
		// wrap 的第一个参数必须可调用。
		return nil, runtime.RaiseError(runtime.StringValue("bad argument #1 to 'wrap' (function expected)"))
	}
	thread := state.NewThread(args[0])
	if thread == nil {
		// State 生命周期异常时无法创建线程。
		return nil, runtime.ErrNilThreadState
	}
	wrapper := &runtime.GoClosureWithUpvalues{Function: runtime.GoResultsFunction(func(callArgs ...runtime.Value) ([]runtime.Value, error) {
		results, err := thread.Resume(callArgs...)
		if err != nil {
			// coroutine.wrap 失败时按抛错语义传播。
			return nil, err
		}
		return results, nil
	})}
	return []runtime.Value{runtime.ReferenceValue(runtime.KindGoClosure, wrapper)}, nil
}

// coroutineThreadArgument 读取 coroutine API 的 thread 参数。
//
// args 必须至少有一个参数且第一个参数为 KindThread；methodName 用于构造 Lua 风格错误文本。
func coroutineThreadArgument(args []runtime.Value, methodName string) (*runtime.Thread, error) {
	if len(args) == 0 || args[0].Kind != runtime.KindThread {
		// 缺少 thread 或类型不符时返回参数错误。
		return nil, runtime.RaiseError(runtime.StringValue("bad argument #1 to '" + methodName + "' (thread expected)"))
	}
	thread, ok := args[0].Ref.(*runtime.Thread)
	if !ok || thread == nil {
		// 损坏的 thread 引用按参数错误处理。
		return nil, runtime.RaiseError(runtime.StringValue("bad argument #1 to '" + methodName + "' (thread expected)"))
	}
	return thread, nil
}

// OpenBase 注册 base 标准库。
//
// state 必须非 nil 且未关闭；成功后 `_G`、`_VERSION` 和基础函数写入全局环境。
func OpenBase(state *State) error {
	if state == nil {
		// nil State 没有全局环境，无法注册 base 库。
		return ErrNilState
	}
	registerLuaMetamethodRunner(state)
	registerTableFinalizerRunner(state)
	return baselib.Open((*runtime.State)(state))
}

// OpenPackage 注册 package 标准库。
//
// state 必须非 nil 且未关闭；成功后全局 `package` 和 `require` 可用。
func OpenPackage(state *State) error {
	if state == nil {
		// nil State 没有全局环境，无法注册 package 库。
		return ErrNilState
	}
	return openPackageWithStateCaller(state)
}

// OpenTable 注册 table 标准库。
//
// state 必须非 nil 且未关闭；成功后全局 `table` 表可用。
func OpenTable(state *State) error {
	if state == nil {
		// nil State 没有全局环境，无法注册 table 库。
		return ErrNilState
	}
	return tablelib.Open((*runtime.State)(state))
}

// OpenString 注册 string 标准库。
//
// state 必须非 nil 且未关闭；成功后全局 `string` 表可用。
func OpenString(state *State) error {
	if state == nil {
		// nil State 没有全局环境，无法注册 string 库。
		return ErrNilState
	}
	registerLuaMetamethodRunner(state)
	return stringlib.Open((*runtime.State)(state))
}

// OpenMath 注册 math 标准库。
//
// state 必须非 nil 且未关闭；成功后全局 `math` 表可用。
func OpenMath(state *State) error {
	if state == nil {
		// nil State 没有全局环境，无法注册 math 库。
		return ErrNilState
	}
	return mathlib.Open((*runtime.State)(state))
}

// OpenIO 注册 io 标准库。
//
// state 必须非 nil 且未关闭；成功后全局 `io` 表和标准文件 userdata 可用。
func OpenIO(state *State) error {
	if state == nil {
		// nil State 没有全局环境，无法注册 io 库。
		return ErrNilState
	}
	return iolib.Open((*runtime.State)(state))
}

// OpenOS 注册 os 标准库。
//
// state 必须非 nil 且未关闭；成功后全局 `os` 表可用。
func OpenOS(state *State) error {
	if state == nil {
		// nil State 没有全局环境，无法注册 os 库。
		return ErrNilState
	}
	return oslib.Open((*runtime.State)(state))
}

// OpenUTF8 注册 utf8 标准库。
//
// state 必须非 nil 且未关闭；成功后全局 `utf8` 表可用。
func OpenUTF8(state *State) error {
	if state == nil {
		// nil State 没有全局环境，无法注册 utf8 库。
		return ErrNilState
	}
	return utf8lib.Open((*runtime.State)(state))
}

// OpenDebug 注册 debug 标准库。
//
// state 必须非 nil 且未关闭；成功后全局 `debug` 表可用。
func OpenDebug(state *State) error {
	if state == nil {
		// nil State 没有全局环境，无法注册 debug 库。
		return ErrNilState
	}
	return debuglib.Open((*runtime.State)(state))
}

// LoadString 编译 Lua 源码字符串并把 Lua closure 压入 State 栈顶。
//
// state 必须非 nil 且未关闭；source 是完整 Lua chunk；chunkName 写入 Proto.Source，空字符串时
// 使用 `=(string)`。语法或 codegen 错误会原样返回，成功后栈顶新增一个 Lua closure。
func LoadString(state *State, source string, chunkName string) error {
	if state == nil {
		// nil State 没有栈，无法接收编译后的 closure。
		return ErrNilState
	}
	if chunkName == "" {
		// 空 chunk 名称使用稳定占位，便于 debug.getinfo 和错误信息展示。
		chunkName = "=(string)"
	}
	closure, err := loadStringOrBinaryChunk(state, []byte(source), chunkName)
	if err != nil {
		// 解析或 codegen 失败时不修改 State 栈。
		return err
	}
	return state.Push(closure)
}

// LoadFile 读取 Lua 源文件并把编译后的 Lua closure 压入 State 栈顶。
//
// state 必须非 nil 且未关闭；path 是本地文件路径。读取失败、语法错误或 codegen 错误会返回
// Go error；成功时 chunkName 使用 `@path` 形式，便于 Lua 5.3 调试信息识别文件来源。
func LoadFile(state *State, path string) error {
	if state == nil {
		// nil State 没有栈，无法接收编译后的 closure。
		return ErrNilState
	}
	sourceBytes, err := readAPIFile(state, path)
	if err != nil {
		// 文件读取失败时直接返回原始错误，保留 os.PathError 供调用方 errors.As。
		return err
	}
	return loadFileChunk(state, sourceBytes, "@"+path)
}

// loadFileChunk 按 Lua 文件入口规则加载源码或 binary chunk。
//
// state 必须非 nil；chunkBytes 来自文件系统，首字节为 ESC 时按 Lua 5.3 binary chunk 读取，
// 否则剥离首行 shebang 后按源码编译。成功后把 closure 压入 State 栈顶。
func loadFileChunk(state *State, chunkBytes []byte, chunkName string) error {
	closure, err := loadStringOrBinaryChunk(state, chunkBytes, chunkName)
	if err != nil {
		// 文件加载失败时不修改 State 栈。
		return err
	}
	return state.Push(closure)
}

// loadStringOrBinaryChunk 将源码文本或 Lua 5.3 binary chunk 转成 Lua closure。
//
// state 必须非 nil；chunkBytes 首字节为 ESC 时必须走 binary loader，即使签名损坏也不能按源码解析。
// 非 binary 输入会先剥离 shebang，再按当前 State 语法扩展编译。
func loadStringOrBinaryChunk(state *State, chunkBytes []byte, chunkName string) (Value, error) {
	if len(chunkBytes) > 0 && chunkBytes[0] == byte(bytecode.ChunkSignature[0]) {
		// Lua 5.3 文件入口遇到 ESC 开头必须按预编译 chunk 处理。
		proto, err := bytecode.LoadBinaryChunk(bytes.NewReader(chunkBytes))
		if err != nil {
			// binary chunk 损坏时返回 loader 错误，避免误报源码语法错误。
			return runtime.NilValue(), err
		}
		upvalues := bindTopLevelUpvalues(state, proto)
		closure := runtime.NewLuaClosure(proto, upvalues, closedUpvalueCells(upvalues))
		return runtime.ReferenceValue(runtime.KindLuaClosure, closure), nil
	}
	source := lexer.StripInitialShebang(string(chunkBytes))
	return compileString(state, source, chunkName)
}

// readAPIFile 按 State 选项读取 Go API 指定的 Lua 文件。
//
// 配置 VirtualFilesystem 或 AllowHostFilesystem 时使用 Options 策略；普通 Go API 直接调用未配置
// 文件策略的 State 时保持历史宿主读取行为，避免破坏显式 Go 宿主加载文件的嵌入用法。
func readAPIFile(state *State, path string) ([]byte, error) {
	// State 已由调用方校验，读取选项用于判断是否进入 VFS/权限路径。
	options := state.Options()
	if options.VirtualFilesystem != nil || options.AllowHostFilesystem {
		// 显式配置文件策略后由 runtime 统一处理 VFS、宿主权限和优先级。
		return runtime.ReadFileWithOptions(options, path)
	}
	return os.ReadFile(path)
}

// DoString 加载 Lua 源码字符串并在 protected call 边界内执行。
//
// 加载成功后会弹出编译得到的 closure 并执行；返回值当前按脚本入口语义丢弃。
func DoString(state *State, source string) error {
	if state == nil {
		// nil State 不能建立执行边界。
		return ErrNilState
	}
	return ProtectedCall(state, func(callState *State) error {
		// 先复用 LoadString，保证 DoString 与 LoadString 的编译行为一致。
		if err := LoadString(callState, source, "=(string)"); err != nil {
			return err
		}
		closureValue, err := callState.Pop()
		if err != nil {
			// 加载成功后栈顶必须存在 closure；弹出失败说明 State 栈边界异常。
			return err
		}
		_, err = Call(callState, closureValue)
		return err
	})
}

// DoFile 加载 Lua 源文件并在 protected call 边界内执行。
//
// 文件读取和编译失败会原样返回；加载成功后会弹出编译得到的 closure 并执行。
func DoFile(state *State, path string) error {
	if state == nil {
		// nil State 不能建立执行边界。
		return ErrNilState
	}
	return ProtectedCall(state, func(callState *State) error {
		// 先复用 LoadFile，保证 DoFile 与 LoadFile 的文件读取和编译行为一致。
		if err := LoadFile(callState, path); err != nil {
			return err
		}
		closureValue, err := callState.Pop()
		if err != nil {
			// 加载成功后栈顶必须存在 closure；弹出失败说明 State 栈边界异常。
			return err
		}
		_, err = Call(callState, closureValue)
		return err
	})
}

// ProtectedCall 在 Lua State 上执行受保护调用。
//
// state 必须非 nil；call 不能为空。该函数直接复用 runtime.State.ProtectedCall，错误或 panic 会
// 回滚栈和调用帧边界，并以 RuntimeError 形式携带 Lua error object。
func ProtectedCall(state *State, call func(state *State) error) error {
	if state == nil {
		// nil State 不能建立 protected call 边界。
		return ErrNilState
	}
	if call == nil {
		// 空回调没有可执行内容，保持 runtime.ErrNilProtectedCall 语义。
		return ErrNilProtectedCall
	}
	if err := state.CheckContext(); err != nil {
		// context 已取消时不进入 protected call，避免回调继续执行宿主副作用。
		return err
	}
	return state.ProtectedCall(func(runtimeState *runtime.State) error {
		// 桥接 runtime.State 到 lua.State 别名，确保调用方只感知 lua 包类型。
		return call((*State)(runtimeState))
	})
}

// Call 调用一个 Lua 或 Go 函数值。
//
// state 必须非 nil；function 必须是 KindGoClosure 或 KindLuaClosure。Go closure 直接调用；
// Lua closure 使用当前最小 Proto 执行循环同步执行并返回结果列表。
func Call(state *State, function Value, args ...Value) ([]Value, error) {
	// 先校验 State 生命周期入口，避免宿主把 nil State 误认为脚本调用错误。
	if state == nil {
		// nil State 没有调用栈，无法执行任何函数。
		return nil, ErrNilState
	}
	if err := state.CheckContext(); err != nil {
		// context 已取消或 State 已关闭时，不进入 Go/Lua 回调，避免继续产生副作用。
		return nil, err
	}

	switch function.Kind {
	case KindGoClosure:
		// Go closure 当前阶段可直接在宿主侧调用。
		return callGoClosureValue(function, args...)
	case KindLuaClosure:
		// Lua closure 通过最小 VM 执行循环运行 Proto。
		return executeLuaClosure(state, function, args...)
	default:
		// 非函数值调用必须按 Lua 运行期错误分类返回。
		return nil, runtime.NewRuntimeError(runtime.StringValue(ErrExpectedCallable.Error()), ErrExpectedCallable)
	}
}

// callWithDebugName 按调用点推断名称执行函数。
//
// state 必须非 nil；function 必须是 Go 或 Lua closure。name 与 nameWhat 为空表示调用点无法
// 提供名称，此时行为与 Call 一致；Lua closure 会把名称写入调用帧供 debug.getinfo 读取。
func callWithDebugName(state *State, function Value, name string, nameWhat string, args ...Value) ([]Value, error) {
	// 默认命名调用不是 tail call，保留普通 call hook 和调试帧语义。
	return callWithDebugNameTailArgs(state, function, name, nameWhat, false, args)
}

// callWithDebugNameTail 按调用点推断名称执行函数，并可标记 tail call。
//
// state 必须非 nil；function 必须是 Go 或 Lua closure。tailCall=true 表示该函数由 TAILCALL
// 进入，调用帧需要暴露 istailcall=true，并通过 `tail call` hook 通知 debug 库。
func callWithDebugNameTail(state *State, function Value, name string, nameWhat string, tailCall bool, args ...Value) ([]Value, error) {
	// 变参入口保留给公开 API 和低频调用；VM 内部 CALL 使用 slice 版避免参数逃逸。
	return callWithDebugNameTailArgs(state, function, name, nameWhat, tailCall, args)
}

// callWithDebugNameArgs 按调用点推断名称执行函数。
//
// args 只在本次调用期间读取；调用方可传入栈上小数组切片，避免 VM 内部 CALL 为 Go 变参额外分配。
func callWithDebugNameArgs(state *State, function Value, name string, nameWhat string, args []Value) ([]Value, error) {
	// 默认命名调用不是 tail call，保留普通 call hook 和调试帧语义。
	return callWithDebugNameTailArgs(state, function, name, nameWhat, false, args)
}

// callWithDebugNameTailArgs 按调用点推断名称执行函数，并可标记 tail call。
//
// args 只在本次调用期间读取；Lua closure 路径不会保留该切片，vararg 会在需要时自行复制。
func callWithDebugNameTailArgs(state *State, function Value, name string, nameWhat string, tailCall bool, args []Value) ([]Value, error) {
	// 先复用公开调用入口的 State 生命周期校验，避免命名调用绕过取消和关闭检查。
	if state == nil {
		// nil State 没有调用栈，无法执行任何函数。
		return nil, ErrNilState
	}
	if err := state.CheckContext(); err != nil {
		// context 已取消或 State 已关闭时，不进入 Go/Lua 回调，避免继续产生副作用。
		return nil, err
	}

	switch function.Kind {
	case KindGoClosure:
		// Go closure 也压入临时调试帧，供 call/return hook 与 debug.getinfo 观察被调函数。
		return callGoClosureWithDebugFrame(state, function, name, nameWhat, tailCall, args...)
	case KindLuaClosure:
		// Lua closure 需要把调用点名称写入帧，供 debug.getinfo("n") 读取。
		return executeLuaClosureWithDebugNameTailArgs(state, function, name, nameWhat, tailCall, args)
	default:
		// 非函数值调用必须按 Lua 运行期错误分类返回。
		return nil, runtime.NewRuntimeError(runtime.StringValue(ErrExpectedCallable.Error()), ErrExpectedCallable)
	}
}

// Register 把 Go 函数注册为 Lua 全局函数。
//
// state 必须非 nil；name 是全局变量名；fn 不能为空。注册后的值类型为 KindGoClosure，可通过
// GetGlobal 读取并通过 Call 调用。
func Register(state *State, name string, fn Function) error {
	// 先校验 State，确保后续全局写入不会触发 nil 指针。
	if state == nil {
		// nil State 没有全局环境，无法注册函数。
		return ErrNilState
	}
	if fn == nil {
		// 空 Go 函数没有可调用目标，按不可调用错误返回。
		return ErrExpectedCallable
	}

	state.SetGlobal(name, runtime.ReferenceValue(runtime.KindGoClosure, runtime.GoResultsFunction(fn)))
	return nil
}

// Push 把一个 Lua 值压入 State 栈顶。
//
// state 必须非 nil；value 可以是任意 Lua 值。底层 State 会检查关闭状态和栈深度限制。
func Push(state *State, value Value) error {
	// 先校验 State，避免包装层对 nil 指针调用方法。
	if state == nil {
		// nil State 没有栈，无法压入值。
		return ErrNilState
	}
	return state.Push(value)
}

// PushNil 把 Lua nil 压入 State 栈顶。
//
// state 必须非 nil；该 helper 对齐 Lua C API 的 lua_pushnil 便捷语义。
func PushNil(state *State) error {
	// nil 值没有负载，统一复用 Push 保持错误路径一致。
	return Push(state, runtime.NilValue())
}

// PushBoolean 把 Lua boolean 压入 State 栈顶。
//
// state 必须非 nil；value 为 true/false 时分别对应 Lua true/false。
func PushBoolean(state *State, value bool) error {
	// boolean 值由 runtime.BooleanValue 构造，确保 Kind 标记一致。
	return Push(state, runtime.BooleanValue(value))
}

// PushInteger 把 Lua integer 压入 State 栈顶。
//
// state 必须非 nil；value 使用 int64 表达 Lua 5.3 默认整数语义。
func PushInteger(state *State, value int64) error {
	// integer 值由 runtime.IntegerValue 构造，避免浮点转换损失精度。
	return Push(state, runtime.IntegerValue(value))
}

// PushNumber 把 Lua float number 压入 State 栈顶。
//
// state 必须非 nil；value 使用 float64 表达 Lua 5.3 默认 lua_Number 语义。
func PushNumber(state *State, value float64) error {
	// number 值由 runtime.NumberValue 构造，保留浮点负载。
	return Push(state, runtime.NumberValue(value))
}

// PushString 把 Lua string 压入 State 栈顶。
//
// state 必须非 nil；value 按字节字符串保存，允许包含任意二进制内容。
func PushString(state *State, value string) error {
	// string 值由 runtime.StringValue 构造，保持 Lua string 的字节语义。
	return Push(state, runtime.StringValue(value))
}

// ToValue 读取 State 栈上指定索引的 Lua 值。
//
// state 必须非 nil；index 支持正索引和负索引。索引无效、越界或 State 已关闭时返回 Lua nil。
func ToValue(state *State, index int) (Value, error) {
	// 先校验 State，避免包装层对 nil 指针读取栈。
	if state == nil {
		// nil State 没有栈，无法读取值。
		return runtime.NilValue(), ErrNilState
	}
	return state.ValueAt(index), nil
}

// ToBoolean 按 Lua 条件判断语义读取栈值为 bool。
//
// state 必须非 nil；index 支持正索引和负索引。Lua 5.3 只有 nil 和 false 为假，因此该转换
// 总是返回 ok=true，除非 State 为 nil。
func ToBoolean(state *State, index int) (bool, bool, error) {
	// 先读取原始值，确保索引和 nil State 行为集中在 ToValue。
	value, err := ToValue(state, index)
	if err != nil {
		// State 错误需要向上传播，ok=false 表示未能完成转换。
		return false, false, err
	}
	return value.Truthy(), true, nil
}

// ToInteger 按 Lua 5.3 number-to-integer 规则读取栈值。
//
// state 必须非 nil；index 支持正索引和负索引。integer 直接返回，有限且无小数的 float number
// 可转换；其他类型返回 ok=false。
func ToInteger(state *State, index int) (int64, bool, error) {
	// 先读取原始值，避免在多个 To helper 中重复索引处理。
	value, err := ToValue(state, index)
	if err != nil {
		// State 错误需要向上传播，ok=false 表示未能完成转换。
		return 0, false, err
	}
	integerValue, ok := value.ToInteger()
	return integerValue, ok, nil
}

// ToNumber 按 Lua 5.3 number 语义读取栈值。
//
// state 必须非 nil；index 支持正索引和负索引。integer 会转换为 float64，float number 直接返回；
// 其他类型返回 ok=false。
func ToNumber(state *State, index int) (float64, bool, error) {
	// 先读取原始值，避免在多个 To helper 中重复索引处理。
	value, err := ToValue(state, index)
	if err != nil {
		// State 错误需要向上传播，ok=false 表示未能完成转换。
		return 0, false, err
	}
	numberValue, ok := value.ToNumber()
	return numberValue, ok, nil
}

// ToString 按 Lua 基础 tostring 语义读取栈值为 string。
//
// state 必须非 nil；index 支持正索引和负索引。该函数复用 runtime.ToString，因此 table 等引用值
// 会走当前已实现的 `__tostring` 或调试文本路径；转换失败返回 ok=false 和错误。
func ToString(state *State, index int) (string, bool, error) {
	// 先读取原始值，保持索引处理与其他 To helper 一致。
	value, err := ToValue(state, index)
	if err != nil {
		// State 错误需要向上传播，ok=false 表示未能完成转换。
		return "", false, err
	}
	stringValue, err := runtime.ToString(value)
	if err != nil {
		// tostring 元方法失败时，向调用方返回错误并标记转换未完成。
		return "", false, err
	}
	return stringValue.String, true, nil
}

// PCall 执行 Lua 5.3 风格的 protected call。
//
// 成功返回格式为 [true, ...results]；失败返回 [false, errorObject]，同时不向上抛出 Go error。
func PCall(state *State, call func(state *State) error) ([]Value, error) {
	if state == nil {
		// nil State 不能建立 pcall 边界。
		return nil, ErrNilState
	}
	if call == nil {
		// 空回调没有可执行内容，保持 protected call 的错误语义。
		return nil, ErrNilProtectedCall
	}
	if err := state.CheckContext(); err != nil {
		// context 已取消时不进入 pcall，避免吞掉宿主取消信号。
		return nil, err
	}
	return runtime.PCall(
		state,
		runtime.ProtectedCallFunc(func(runtimeState *runtime.State) error {
			// runtime.PCall 要求 runtime.ProtectedCallFunc，这里做签名桥接。
			return call((*State)(runtimeState))
		}),
	)
}

// XPCall 执行 Lua 5.3 风格的 xpcall。
//
// handler 在 protected call 失败后接收 Lua error object，失败返回 [false, handlerResult]。
func XPCall(state *State, call func(state *State) error, handler ErrorHandler) ([]Value, error) {
	if state == nil {
		// nil State 不能建立 xpcall 边界。
		return nil, ErrNilState
	}
	if call == nil {
		// 空回调没有可执行内容，保持 protected call 的错误语义。
		return nil, ErrNilProtectedCall
	}
	if err := state.CheckContext(); err != nil {
		// context 已取消时不进入 xpcall，避免错误处理器误处理宿主取消信号。
		return nil, err
	}
	return runtime.XPCall(
		state,
		runtime.ProtectedCallFunc(func(runtimeState *runtime.State) error {
			// runtime.XPCall 要求 runtime.ProtectedCallFunc，该匿名函数桥接类型。
			return call((*State)(runtimeState))
		}),
		handler,
	)
}

// registerTableFinalizerRunner 为 State 注入 table `__gc` 元方法执行器。
//
// state 必须非 nil；runner 通过专用调试帧调用 `__gc`，因此 table finalizer 可执行 Go closure
// 或 Lua closure，并让 finalizer 内部 `debug.getinfo(2, "n")` 观察到 `metamethod/__gc`。
func registerTableFinalizerRunner(state *State) {
	if state == nil {
		// nil State 没有 runtime 承载体。
		return
	}

	// runtime.State 是 lua.State 的底层别名，直接设置 runner。
	((*runtime.State)(state)).SetTableFinalizerRunner(func(tableValue Value, finalizerValue Value) error {
		err := callTableFinalizerWithDebugFrame(state, tableValue, finalizerValue)
		return err
	})
}

// callTableFinalizerWithDebugFrame 在 `__gc` 元方法调用帧下执行 table finalizer。
//
// state 必须非 nil；finalizerValue 必须是 Go 或 Lua closure，tableValue 是待终结对象。返回值仅
// 表示 finalizer 抛出的错误；成功路径不保留 finalizer 返回值。Lua 5.3 的 db.lua 在 finalizer
// 内读取 `debug.getinfo(2, "n")`，因此这里需要额外压入一个可见 caller 帧表示 `__gc` 元方法。
func callTableFinalizerWithDebugFrame(state *State, tableValue Value, finalizerValue Value) (err error) {
	// 构造一个轻量 Go 帧作为 finalizer 的调用方，供 debug.getinfo(level=2) 读取元方法名称。
	frame := runtime.NewGoCallFrame(finalizerValue, state.StackTop()+1, -1)
	frame.Name = "__gc"
	frame.NameWhat = "metamethod"
	if err := state.PushCallFrame(frame); err != nil {
		// 调用帧压入失败时不能进入 finalizer，避免 debug 栈不平衡。
		return err
	}
	finalizerBaseCallDepth := state.CallDepth()
	defer func() {
		for state.CallDepth() > finalizerBaseCallDepth {
			// finalizer 错误会直接传播给 collectgarbage/pcall；内部 Lua/Go 帧必须在这里清理。
			_, _ = state.PopCallFrame()
		}
		if state.CallDepth() == finalizerBaseCallDepth {
			// synthetic caller 只服务执行期间的 debug.getinfo，返回后必须清理。
			_, _ = state.PopCallFrame()
		}
	}()

	runFinalizer := func() error {
		// finalizer 自身可以执行 Lua/Go closure；返回值被 Lua 5.3 GC 语义丢弃。
		_, callErr := Call(state, finalizerValue, tableValue)
		return callErr
	}
	if environment, ok := debuglib.EnvironmentForState((*runtime.State)(state)); ok {
		// GC finalizer 不应被当前用户 hook 观察，否则官方 db.lua 会把 gc.lua 的 print 当成被测调用。
		err = environment.RunWithHooksSuppressed(runFinalizer)
	} else {
		// 未打开 debug 库时直接执行 finalizer。
		err = runFinalizer()
	}
	if err != nil {
		// finalizer 错误交给 GC 路径决定传播或吞掉，当前 helper 不包装错误。
		return err
	}
	return nil
}

// registerLuaMetamethodRunner 为 State 注入 Lua closure 元方法执行器。
//
// state 必须非 nil；执行器复用 lua 包完整调用路径，让 runtime/base/string 等低层包在不形成
// import cycle 的前提下执行 `__tostring` 等 Lua closure 元方法。
func registerLuaMetamethodRunner(state *State) {
	if state == nil {
		// nil State 无法保存元方法执行器。
		return
	}

	((*runtime.State)(state)).SetLuaMetamethodRunner(func(method Value, name string, args ...Value) ([]Value, error) {
		// 元方法执行走命名 Call 路径，保证 Lua closure 的 upvalue、栈帧和 debug namewhat 语义一致。
		if name != "" {
			// 元方法内部 debug.getinfo(1) 需要看到 namewhat=metamethod 与具体元方法名。
			return callWithDebugName(state, method, name, "metamethod", args...)
		}
		return Call(state, method, args...)
	})
}

// SetContext 设置 State 运行上下文。
//
// nil context 会被拒绝，返回 ErrNilContext；与 runtime.SetContext 保持一致。
func SetContext(state *State, ctx context.Context) error {
	if state == nil {
		// nil State 没有可设置的上下文。
		return ErrNilState
	}
	return state.SetContext(ctx)
}

// ErrorObject 从错误链中提取 Lua error object。
//
// err 为 nil 时返回 Lua nil；错误链中包含 RuntimeError 时返回其 Object；普通 Go error 返回
// 错误文本字符串。该语义对齐 pcall/xpcall 的错误对象传播。
func ErrorObject(err error) Value {
	// 直接委托 runtime，确保错误对象提取规则在一个地方维护。
	return runtime.ErrorObject(err)
}

// RaiseError 构造 Lua `error` 语义的 RuntimeError。
//
// object 可以是任意 Lua 值；返回错误链携带 ErrLuaError，调用方可通过 ErrorObject 取回原对象。
func RaiseError(object Value) error {
	// 直接委托 runtime，保持 Lua error object 的保留语义。
	return runtime.RaiseError(object)
}

// ClassifyError 返回 runtime 层可识别的错误分类。
//
// 资源限制错误优先于普通运行期错误；nil 和未知 Go error 返回 ErrorClassOther。
func ClassifyError(err error) ErrorClass {
	// 直接委托 runtime，避免 lua 包复制错误分类规则。
	return runtime.ClassifyError(err)
}

// IsRuntimeError 判断错误是否属于 Lua 运行期错误。
//
// err 链中包含 RuntimeError、ErrLuaError 或 ErrExpectedCallable 时返回 true；资源限制错误单独归类。
func IsRuntimeError(err error) bool {
	// 直接委托 runtime，保持错误分类规则一致。
	return runtime.IsRuntimeError(err)
}

// IsResourceLimitError 判断错误链中是否包含资源限制错误。
//
// 返回 true 时调用方可继续使用 errors.As 提取 ResourceLimitError 结构化字段。
func IsResourceLimitError(err error) bool {
	// 直接委托 runtime，保持资源限制识别规则一致。
	return runtime.IsResourceLimitError(err)
}

// GetGlobal 读取全局变量。
//
// state 必须非 nil；name 是全局变量名。变量不存在或 State 已关闭时返回 Lua nil。
func GetGlobal(state *State, name string) (Value, error) {
	if state == nil {
		// nil State 没有全局环境，返回错误而不是 panic。
		return runtime.NilValue(), ErrNilState
	}
	return state.GetGlobal(name), nil
}

// SetGlobal 写入全局变量。
//
// state 必须非 nil；value 为 Lua nil 时等价于删除该全局变量。State 已关闭时 runtime 会忽略写入。
func SetGlobal(state *State, name string, value Value) error {
	if state == nil {
		// nil State 没有全局环境，返回错误而不是 panic。
		return ErrNilState
	}
	state.SetGlobal(name, value)
	return nil
}

// packageLuaFileLoader 构造 package.searchers[2] 使用的 Lua 文件 loader。
//
// state 必须非 nil；filename 由 package.searchpath 命中得到。返回的 loader 会在 require 调用时
// 编译并执行文件，返回 chunk 的多返回值，错误会作为 require 错误向上传播。
func packageLuaFileLoader(state *State) packagelib.LuaFileLoader {
	return func(filename string) runtime.GoResultsFunction {
		// 每个命中文件生成独立闭包，避免 require 执行时再次解析 package.path。
		return func(args ...runtime.Value) ([]runtime.Value, error) {
			// 文件 loader 会把 require 传入的模块名和文件名继续作为 chunk 的 vararg。
			if err := LoadFile(state, filename); err != nil {
				// 文件读取、解析或 codegen 错误需要包装成 require 的加载错误语义。
				moduleName := filename
				if len(args) > 0 && args[0].Kind == runtime.KindString {
					// require 会把模块名作为第一个参数传入 loader，用于构造兼容错误文本。
					moduleName = args[0].String
				}
				return nil, runtime.RaiseError(runtime.StringValue(fmt.Sprintf("error loading module '%s' from file '%s':\n\t%v", moduleName, filename, err)))
			}
			closureValue, err := state.Pop()
			if err != nil {
				// LoadFile 成功后栈顶必须是待执行 closure。
				return nil, err
			}
			return Call(state, closureValue, args...)
		}
	}
}

// openPackageWithStateCaller 注册 package 库并注入 Lua closure loader 调用器。
//
// state 必须非 nil；require 命中 package.preload 中的 Lua closure 时，通过当前 State 的 Call
// 执行 loader，保持 loader 参数、错误传播和 package.loaded 写回语义与 Lua 5.3 一致。
func openPackageWithStateCaller(state *State) error {
	environment := packagelib.NewEnvironmentWithOptions(packageLuaFileLoader(state), state.Options())
	if stateAwareLoader := state.Options().PackageDynamicLibraryLoaderForState; stateAwareLoader != nil {
		// native_modules 需要把 luaopen_* 调用绑定到当前 State；状态感知 loader 优先于无状态 loader。
		environment.SetDynamicLibraryLoader(stateAwareLoader((*runtime.State)(state)))
	}
	environment.SetLoaderCaller(func(loader runtime.Value, args ...runtime.Value) ([]runtime.Value, error) {
		// Go 和 Lua closure loader 都复用 lua.Call，避免 package 包直接依赖执行循环。
		return Call(state, loader, args...)
	})
	runtimeState := (*runtime.State)(state)
	runtimeState.SetGlobal("require", runtime.ReferenceValue(runtime.KindGoClosure, runtime.GoResultsFunction(environment.Require)))
	runtimeState.SetGlobal("package", runtime.ReferenceValue(runtime.KindTable, environment.Table()))
	if diagnostic := environment.Table().RawGetString("_glua_loadlib_diag"); !diagnostic.IsNil() {
		// LPeg/native 定位模式下同步注册全局诊断 closure，用于区分全局调用和 package 字段调用边界。
		runtimeState.SetGlobal("_glua_loadlib_diag", diagnostic)
	}
	return nil
}

// registerLoadedLibrary 把已打开的标准库登记到 package.loaded。
//
// state 必须已打开 package 库；name 是全局库名。全局值为 nil 时跳过写入，避免为尚未实现库制造
// 虚假缓存。
func registerLoadedLibrary(state *State, name string) error {
	packageValue := state.GetGlobal("package")
	if packageValue.Kind != runtime.KindTable {
		// package 全局缺失时无法登记 require 缓存。
		return runtime.RaiseError(runtime.StringValue("package library is not initialized"))
	}
	packageTable, ok := packageValue.Ref.(*runtime.Table)
	if !ok || packageTable == nil {
		// 损坏的 package 表会导致 require 行为不可预测，直接返回错误。
		return runtime.RaiseError(runtime.StringValue("package library is not initialized"))
	}
	loadedValue := packageTable.RawGetString("loaded")
	if loadedValue.Kind != runtime.KindTable {
		// package.loaded 必须是表。
		return runtime.RaiseError(runtime.StringValue("package.loaded is not a table"))
	}
	loadedTable, ok := loadedValue.Ref.(*runtime.Table)
	if !ok || loadedTable == nil {
		// 损坏的 loaded 表无法安全写入。
		return runtime.RaiseError(runtime.StringValue("package.loaded is not a table"))
	}
	libraryValue := state.GetGlobal(name)
	if libraryValue.IsNil() {
		// 未注册的库不写入缓存，避免 require 误判模块已加载。
		return nil
	}
	loadedTable.RawSetString(name, libraryValue)
	return nil
}

// compileString 把 Lua 源码字符串编译为 Lua closure 值。
//
// state 必须非 nil；source 是完整 Lua chunk；chunkName 会写入 Proto.Source。返回值是可压入
// State 栈的 KindLuaClosure。解析或 codegen 失败时返回错误且不产生 closure。
func compileString(state *State, source string, chunkName string) (Value, error) {
	// 先解析源码为 AST，确保语法和基础语义通过。
	chunkParser := parser.NewWithSyntax(source, state.Options().SyntaxExtensions)
	chunk, err := chunkParser.ParseChunk()
	if err != nil {
		// parser 错误包装成 Go API 可读的结构化语法错误，同时保留原始错误链。
		return runtime.NilValue(), newSyntaxError(err, chunkName, source)
	}
	proto, err := codegen.CompileChunk(chunk, chunkName)
	if err != nil {
		// codegen 错误表示 AST 当前无法生成有效 Proto。
		return runtime.NilValue(), err
	}
	upvalues := bindTopLevelUpvalues(state, proto)
	closure := runtime.NewLuaClosure(proto, upvalues, closedUpvalueCells(upvalues))
	return runtime.ReferenceValue(runtime.KindLuaClosure, closure), nil
}

// newSyntaxError 将 parser 错误包装为 lua 包对外的结构化语法错误。
//
// err 必须来自 parser 语法或语义阶段；sourceName 是 Lua chunk name；source 是完整源码，
// 用于补充错误行文本。无法提取 parser.ParseError 时原样返回 err。
func newSyntaxError(err error, sourceName string, source string) error {
	parseError, ok := firstParseError(err)
	if !ok {
		// 非 parser 错误不伪装成语法错误，避免误导调用方。
		return err
	}
	details := SyntaxErrorDetails{
		SourceName: sourceName,
		SourceID:   parser.SourceID(sourceName),
		Line:       parseError.Position.Line,
		Column:     parseError.Position.Column,
		Near:       parseError.Near,
		Expected:   parseError.Message,
		Hint:       syntaxErrorHint(parseError),
		LineText:   sourceLineText(source, parseError.Position.Line),
	}
	return &SyntaxError{
		Message: parser.SyntaxErrorMessage(sourceName, parseError),
		Details: details,
		Cause:   err,
	}
}

// syntaxErrorHint 将 parser 内部 expected 文本转换为面向用户的诊断提示。
//
// 返回值只进入 SyntaxErrorDetails，不改变 Error() 主消息，也不影响 Lua load 的兼容错误文本。
func syntaxErrorHint(parseError parser.ParseError) string {
	if parseError.Message == `expected operator "="` {
		// 标识符开头的语句若不是函数调用，就必须形成赋值语句。
		return `expected assignment operator "=" or function call arguments`
	}
	return parseError.Message
}

// firstParseError 从 parser 错误链提取第一处具体错误。
//
// err 可为 parser.ParseError、parser.ParseErrorList 或包装后的错误；返回 false 表示不是
// parser 结构化错误。
func firstParseError(err error) (parser.ParseError, bool) {
	var parseError parser.ParseError
	if errors.As(err, &parseError) {
		// 单个 parser 错误直接作为主错误。
		return parseError, true
	}
	var parseErrors parser.ParseErrorList
	if errors.As(err, &parseErrors) && len(parseErrors) > 0 {
		// 聚合语义错误按第一处错误生成主消息。
		return parseErrors[0], true
	}
	return parser.ParseError{}, false
}

// sourceLineText 返回指定源码行的原始文本。
//
// line 使用 1 起始；行号越界时返回空字符串。返回值会去掉 CR/LF，保留其他源码字符。
func sourceLineText(source string, line int) string {
	if line <= 0 {
		// 非法行号没有可展示源码行。
		return ""
	}
	currentLine := 1
	lineStart := 0
	for index := 0; index < len(source); index++ {
		if source[index] != '\n' {
			// 非换行字符继续扫描当前行。
			continue
		}
		if currentLine == line {
			// 命中目标行时返回不含换行符的片段。
			return strings.TrimSuffix(source[lineStart:index], "\r")
		}
		currentLine++
		lineStart = index + 1
	}
	if currentLine == line {
		// 最后一行没有换行符时从 lineStart 返回到源码末尾。
		return strings.TrimSuffix(source[lineStart:], "\r")
	}
	return ""
}

// closedUpvalueCells 为顶层 closure 的 upvalue 快照创建闭合 cell。
//
// Lua 5.3 允许 debug.setupvalue 和 SETUPVAL 修改 load/string.dump 得到的顶层函数 upvalue；
// 顶层函数没有外层栈帧可捕获，因此必须使用闭合 cell 保存跨调用的可变状态。
func closedUpvalueCells(upvalues []Value) []*runtime.UpvalueCell {
	if len(upvalues) == 0 {
		// 没有 upvalue 时保持 nil，避免无意义分配。
		return nil
	}
	cells := make([]*runtime.UpvalueCell, 0, len(upvalues))
	for _, upvalue := range upvalues {
		// 每个顶层 upvalue 都创建独立闭合 cell，初值来自绑定阶段的快照。
		cells = append(cells, runtime.NewClosedUpvalueCell(upvalue))
	}
	return cells
}

// bindTopLevelUpvalues 绑定顶层 closure 的外部 upvalue。
//
// state 必须非 nil；当前只识别 Lua 5.3 chunk 默认 `_ENV`，并将它绑定到 State 的 globals 表。
func bindTopLevelUpvalues(state *State, proto *bytecode.Proto) []Value {
	upvalues := make([]Value, 0, len(proto.Upvalues))
	for _, upvalueDesc := range proto.Upvalues {
		// 顶层 `_ENV` 由宿主 State 注入，支持全局变量读写。
		if upvalueDesc.Name == "_ENV" {
			globals := state.Globals()
			if globals == nil {
				// State 已关闭时没有可用 globals，保留 nil 让运行期按 Lua 错误路径处理。
				upvalues = append(upvalues, runtime.NilValue())
				continue
			}
			upvalues = append(upvalues, runtime.ReferenceValue(runtime.KindTable, globals))
			continue
		}
		// 其他顶层 upvalue 暂无宿主绑定来源，按 nil 占位保持索引稳定。
		upvalues = append(upvalues, runtime.NilValue())
	}

	// 返回与 Proto.Upvalues 顺序一致的运行期 upvalue 列表。
	return upvalues
}

// executeLuaClosure 执行 Lua closure 并返回多返回值。
//
// state 必须非 nil；function 必须是 KindLuaClosure 且 Ref 为 *runtime.LuaClosure。当前执行循环
// 支持固定寄存器窗口、Go/Lua 同步调用和 RETURN 退出；开放调用栈语义后续继续补齐。
func executeLuaClosure(state *State, function Value, args ...Value) (results []Value, err error) {
	// 默认调用入口没有调用点名称，使用空名称执行同一套 Lua closure 逻辑。
	return executeLuaClosureWithDebugName(state, function, "", "", args...)
}

// executeLuaThreadClosure 执行或恢复 Lua closure 协程。
//
// thread 必须是当前被 resume 的协程；若存在 continuation，args 会作为上次 coroutine.yield 的
// 返回值写回调用寄存器并从保存 PC 继续；否则从协程入口函数开始执行。
func executeLuaThreadClosure(state *State, thread *runtime.Thread, args ...Value) (results []Value, err error) {
	if continuation := takeLuaContinuation(thread); continuation != nil {
		// continuation 链从最内层开始恢复；每一层的返回值都会作为上一层 yield 点的恢复入参。
		currentContinuation := continuation
		currentArgs := args
		var currentErr error
		for currentContinuation != nil {
			if currentErr != nil && currentContinuation.goResume == nil {
				// 只有 pcall/xpcall 这类 Go 层 protected continuation 能捕获内层 Lua 错误。
				return nil, currentErr
			}
			if currentContinuation.goResume != nil {
				// Go 层 continuation 负责把内层结果或错误转换为 pcall/xpcall 的返回布局。
				results, err = currentContinuation.goResume(currentArgs, currentErr)
			} else {
				// Lua VM continuation 从保存 PC 继续执行。
				results, err = executeLuaClosureWithDebugNameTailFrom(state, currentContinuation.function, "", "", false, currentContinuation, currentArgs...)
			}
			if err != nil {
				if errors.Is(err, runtime.ErrCoroutineYield) {
					// 恢复过程再次 yield 时，新内层现场已保存；尚未执行的父 continuation 必须挂回去。
					appendContinuationParent(thread, currentContinuation.parent)
					return results, err
				}
				// 普通错误先交给后续 protected continuation；没有父 continuation 时循环后向外传播。
				currentErr = err
			} else {
				// 当前 continuation 成功时清空之前已被捕获的错误。
				currentErr = nil
			}
			currentArgs = results
			currentContinuation = currentContinuation.parent
		}
		if currentErr != nil {
			// 所有 continuation 都执行完后仍有错误，说明没有 protected-call 边界捕获。
			return nil, currentErr
		}
		return currentArgs, nil
	}

	// 没有 continuation 时按首次 resume 路径执行入口函数。
	return executeLuaClosure(state, thread.Function(), args...)
}

// instructionContinuationRegister 返回指令级 yield 恢复后需要写回的目标寄存器。
//
// 读取型和运算型元方法会把元方法返回值写入 A 寄存器；写入型元方法只需恢复到下一条指令，
// SELF 同时写 A 与 A+1，当前阶段不在这里做不完整恢复。
func instructionContinuationRegister(instruction bytecode.Instruction) int {
	switch instruction.OpCode() {
	case bytecode.OpGetTable, bytecode.OpGetTabUp,
		bytecode.OpAdd, bytecode.OpSub, bytecode.OpMul, bytecode.OpMod, bytecode.OpPow,
		bytecode.OpDiv, bytecode.OpIDiv,
		bytecode.OpBAnd, bytecode.OpBOr, bytecode.OpBXor, bytecode.OpShl, bytecode.OpShr,
		bytecode.OpUnm, bytecode.OpBNot, bytecode.OpLen, bytecode.OpConcat:
		// GETTABLE/GETTABUP 与运算元方法的 Lua 返回值需要落到 A 寄存器。
		return instruction.A()
	default:
		// SETTABLE/SETTABUP 等指令级 yield 无返回寄存器，只恢复 PC。
		return -1
	}
}

// isComparisonContinuationInstruction 判断指令是否是可由比较元方法 yield 挂起的测试指令。
//
// OP_EQ、OP_LT 和 OP_LE 的元方法返回值都不写寄存器，而是恢复 skipNext 测试语义。
func isComparisonContinuationInstruction(instruction bytecode.Instruction) bool {
	switch instruction.OpCode() {
	case bytecode.OpEq, bytecode.OpLt, bytecode.OpLe:
		// 三种比较测试指令都通过 skipNext 驱动后续 PC 调整。
		return true
	default:
		// 其他指令不是比较 continuation，按已有寄存器或普通 PC 恢复路径处理。
		return false
	}
}

// isConcatContinuationInstruction 判断指令是否是可由 __concat 元方法 yield 挂起的 CONCAT。
func isConcatContinuationInstruction(instruction bytecode.Instruction) bool {
	switch instruction.OpCode() {
	case bytecode.OpConcat:
		// CONCAT 可能需要在一个 opcode 内连续触发多个 __concat 元方法。
		return true
	default:
		// 其他指令的元方法返回值可由通用寄存器写回或比较恢复逻辑处理。
		return false
	}
}

// saveInstructionContinuation 保存 VM 单条指令触发 coroutine.yield 后的外层执行现场。
//
// 该 continuation 不绑定 CallRequest：恢复时只按 opcode 需要写回元方法结果，然后从下一条
// PC 继续执行，覆盖 table 读写元方法 yield 的 Lua 5.3 行为。
func saveInstructionContinuation(state *State, coroutineThread *runtime.Thread, function Value, closure *runtime.LuaClosure, proto *bytecode.Proto, vm *runtime.VM, frame runtime.CallFrame, instruction bytecode.Instruction, pc int, previousPC int, lastHookLine int64) {
	if coroutineThread == nil {
		// 主线程或缺少运行线程时没有 coroutine continuation 可保存。
		return
	}

	// 保存触发 yield 的外层指令现场；若已有内层 continuation，saveLuaContinuation 会挂到链尾。
	saveLuaContinuation(&luaCoroutineContinuation{
		thread:             coroutineThread,
		function:           function,
		closure:            closure,
		proto:              proto,
		vm:                 vm,
		frame:              frame,
		outerFrames:        continuationOuterFrames(coroutineThread),
		resumeRegister:     instructionContinuationRegister(instruction),
		resumeInstruction:  instruction,
		snapshotLevel:      continuationSnapshotLevel(coroutineThread, frame),
		nextPC:             pc + 1,
		previousPC:         pc,
		previousPreviousPC: previousPC,
		lastHookLine:       lastHookLine,
	})
}

// saveResumedInstructionContinuation 保存指令 continuation 恢复过程中再次 yield 的外层现场。
//
// 当前 continuation 已经被 takeLuaContinuation 取出；若恢复同一条指令时又触发元方法 yield，
// 必须重新保存一个不带 parent 的副本。未执行的父 continuation 会由 executeLuaThreadClosure
// 在捕获 ErrCoroutineYield 后统一挂回，避免父链重复。
func saveResumedInstructionContinuation(continuation *luaCoroutineContinuation) {
	if continuation == nil {
		// 缺少 continuation 时没有可重挂现场。
		return
	}
	resumedContinuation := *continuation
	resumedContinuation.parent = nil
	saveLuaContinuation(&resumedContinuation)
}

// prepareLuaExecutionState 准备 Lua closure 执行所需的 closure、Proto 和 VM。
//
// continuation 为 nil 时创建新 VM；非 nil 时复用保存 VM，确保 yield 后的寄存器、upvalue 和
// openTop 状态不丢失。
func prepareLuaExecutionState(state *State, function Value, continuation *luaCoroutineContinuation, args ...Value) (*runtime.LuaClosure, *bytecode.Proto, *runtime.VM, int, []Value, error) {
	// 变参入口保留给外部调用，内部热路径使用 slice 版。
	return prepareLuaExecutionStateArgs(state, function, continuation, args)
}

// prepareLuaExecutionStateArgs 准备 Lua closure 执行所需的 closure、Proto 和 VM。
//
// args 只在初始化固定参数寄存器时读取；vararg 函数会自行复制多余参数，避免持有调用方切片。
func prepareLuaExecutionStateArgs(state *State, function Value, continuation *luaCoroutineContinuation, args []Value) (*runtime.LuaClosure, *bytecode.Proto, *runtime.VM, int, []Value, error) {
	if continuation != nil {
		// continuation 必须携带完整 VM 现场，否则不能安全恢复。
		if continuation.closure == nil || continuation.proto == nil || continuation.vm == nil {
			return nil, nil, nil, 0, nil, runtime.ErrNonCallableThread
		}
		return continuation.closure, continuation.proto, continuation.vm, continuation.vm.RegisterCount(), nil, nil
	}

	// 首次执行需要解析 closure，并按入参创建新的寄存器窗口。
	closure, ok := function.Ref.(*runtime.LuaClosure)
	if !ok || closure == nil || closure.Proto == nil {
		// Lua closure 引用负载异常时按不可调用错误处理，避免 nil 指针。
		return nil, nil, nil, 0, nil, runtime.NewRuntimeError(runtime.StringValue(ErrExpectedCallable.Error()), ErrExpectedCallable)
	}
	proto := closure.Proto
	varargs := luaClosureVarargs(proto, args)
	registerCount := luaClosureRegisterCount(proto, len(args), len(varargs))
	fixedArgumentCount := luaClosureFixedArgumentCount(proto, len(args))
	vm := ((*runtime.State)(state)).BorrowLuaVMAfterResetSkippingClearPrefix(registerCount, proto.Constants, luaClosureExecutionUpvalues(closure), proto.Protos, varargs, fixedArgumentCount)
	vm.BindPrototype(proto)
	vm.BindBorrowedUpvalueCells(closure.UpvalueCells)
	vm.BindLuaMetamethodRunner(((*runtime.State)(state)).LuaMetamethodRunner())
	return closure, proto, vm, registerCount, varargs, nil
}

// luaExecutionFrame 构造或恢复 Lua 执行帧。
//
// continuation 非 nil 时直接复用挂起时保存的 frame；首次执行时按调用点名称与 vararg 创建新帧。
func luaExecutionFrame(state *State, function Value, name string, nameWhat string, tailCall bool, continuation *luaCoroutineContinuation, varargs []Value) runtime.CallFrame {
	if continuation != nil {
		// 恢复路径使用保存帧，保留 Name、NameWhat、TailCall、Varargs 等调试字段。
		return continuation.frame
	}

	// 首次进入 Lua closure 时创建新的调用帧。
	frame := runtime.NewLuaCallFrame(function, state.StackTop()+1, -1)
	frame.Name = name
	frame.NameWhat = nameWhat
	frame.TailCall = tailCall
	if len(varargs) > 0 {
		// 可变参数需要复制到调用帧快照，供 debug.getlocal 负索引读取。
		frame.Varargs = &runtime.VarargSnapshot{Values: append([]Value(nil), varargs...)}
	}
	return frame
}

// continuationOuterFrames 从线程最近一次 yield traceback 中提取当前帧之外的协程内部外层帧。
//
// Thread.TracebackFrames 顺序为当前帧到最早帧，通常是 yield Go 帧、当前 Lua 帧、外层 Lua 帧、
// resume 边界。恢复 VM 时只需要把当前 Lua 帧之后、resume 之前的外层帧按调用顺序压回 State。
func continuationOuterFrames(thread *runtime.Thread) []runtime.CallFrame {
	if thread == nil {
		// nil 线程没有 traceback 快照。
		return nil
	}
	frames := thread.TracebackFrames()
	if len(frames) == 0 {
		// 没有 yield 快照时无法恢复外层帧。
		return nil
	}
	startIndex := 0
	if frames[0].Kind == runtime.CallFrameKindGo && frames[0].Name == "yield" {
		// 跳过 coroutine.yield 自身的 Go 帧。
		startIndex = 1
	}
	if startIndex < len(frames) && frames[startIndex].Kind == runtime.CallFrameKindLua {
		// 跳过当前 continuation 自己的 Lua 帧；该帧会由执行器单独压入并更新 PC。
		startIndex++
	}
	outerFrames := make([]runtime.CallFrame, 0, len(frames)-startIndex)
	for frameIndex := startIndex; frameIndex < len(frames); frameIndex++ {
		// Go/C 帧表示进入当前协程的宿主边界；边界之后属于调用方或父协程，不应恢复进目标协程内部。
		if frames[frameIndex].Kind == runtime.CallFrameKindGo {
			break
		}
		outerFrames = append(outerFrames, frames[frameIndex])
	}
	return outerFrames
}

// continuationSnapshotLevel 计算 continuation 对应的挂起协程局部寄存器快照层级。
//
// Thread 保存的 local 快照按 debug level 排列，最内层 Lua VM 是 level 1。递归调用同一个函数时，
// 多层帧可能拥有相同函数值和 PC；此时通过已经保存的内层 continuation 链长度把外层帧映射到
// 更外层快照，避免 numeric-for 控制寄存器被内层递归帧覆盖。
func continuationSnapshotLevel(thread *runtime.Thread, frame runtime.CallFrame) int {
	if thread == nil {
		// 缺少线程时无法读取快照，回退到最内层以保持旧行为。
		return 1
	}
	tracebackLevel := continuationTracebackSnapshotLevel(thread, frame)
	chainLevel := pendingContinuationDepth(thread) + 1
	if chainLevel > tracebackLevel {
		// 已保存的内层 continuation 数量能区分递归同函数同 PC 的多层父调用。
		return chainLevel
	}
	return tracebackLevel
}

// continuationTracebackSnapshotLevel 通过 traceback 帧匹配估算快照层级。
func continuationTracebackSnapshotLevel(thread *runtime.Thread, frame runtime.CallFrame) int {
	frames := thread.TracebackFrames()
	luaLevel := 0
	for frameIndex := 0; frameIndex < len(frames); frameIndex++ {
		// resume 是主线程进入协程的边界，边界之后不再属于目标协程内部。
		if frames[frameIndex].Kind == runtime.CallFrameKindGo && frames[frameIndex].Name == "resume" {
			break
		}
		if frames[frameIndex].Kind != runtime.CallFrameKindLua {
			// Go/C 帧不占用 Lua debug local 层级。
			continue
		}
		luaLevel++
		if frames[frameIndex].Function.RawEqual(frame.Function) && frames[frameIndex].CurrentPC == frame.CurrentPC {
			// 找到同一 Lua 函数帧与挂起 PC，返回对应 debug level。
			return luaLevel
		}
	}
	return 1
}

// pendingContinuationDepth 返回当前线程已经保存的 continuation 链长度。
func pendingContinuationDepth(thread *runtime.Thread) int {
	if thread == nil {
		// nil 线程没有 continuation。
		return 0
	}
	luaContinuationMu.Lock()
	defer luaContinuationMu.Unlock()
	depth := 0
	for continuation := luaContinuations[thread]; continuation != nil; continuation = continuation.parent {
		// 统计已保存的内层 continuation 数。
		depth++
	}
	return depth
}

// pushContinuationOuterFrames 按调用顺序恢复 continuation 外层帧。
//
// outerFrames 的顺序来自 traceback：较内层在前，较外层在后。State.PushCallFrame 需要较外层先入栈，
// 因此这里反向压入。返回成功压入数量，供恢复结束时弹出。
func pushContinuationOuterFrames(state *State, outerFrames []runtime.CallFrame) (int, error) {
	if state == nil || len(outerFrames) == 0 {
		// 没有外层帧时无需操作。
		return 0, nil
	}
	pushedCount := 0
	for frameIndex := len(outerFrames) - 1; frameIndex >= 0; frameIndex-- {
		// 从最外层到最内层恢复调用栈。
		if err := state.PushCallFrame(outerFrames[frameIndex]); err != nil {
			popContinuationOuterFrames(state, pushedCount)
			return pushedCount, err
		}
		pushedCount++
	}
	return pushedCount, nil
}

// popContinuationOuterFrames 弹出恢复时补压的外层帧。
//
// 该方法只在 continuation 执行完成或再次 yield 后清理恢复帧，避免污染主线程调用栈。
func popContinuationOuterFrames(state *State, count int) {
	if state == nil || count <= 0 {
		// 无恢复帧时无需清理。
		return
	}
	for index := 0; index < count; index++ {
		// 忽略弹出错误，外层 defer 只做 best-effort 清理，真实错误已由执行路径返回。
		_, _ = state.PopCallFrame()
	}
}

// applyThreadLocalSnapshotToVM 将挂起线程的 debug local 快照写回 VM。
//
// debug.setlocal(thread, ...) 在协程挂起期间只能修改 Thread 快照；恢复前必须同步回真实寄存器，
// 才能让后续 VM 指令读取到修改后的 local。
func applyThreadLocalSnapshotToVM(thread *runtime.Thread, vm *runtime.VM, level int) {
	if thread == nil || vm == nil {
		// 缺少线程或 VM 时没有可同步目标。
		return
	}
	if level <= 0 {
		// 历史 continuation 没有记录层级时，按最内层快照恢复。
		level = 1
	}
	for registerIndex := 0; registerIndex < vm.RegisterCount(); registerIndex++ {
		// 使用 continuation 记录的 debug level，避免尾调用 yield 时误用更内层 VM 的快照。
		value, ok := thread.LocalRegisterAtLevel(int64(level), registerIndex)
		if !ok {
			// 没有快照值时跳过，保持 VM 原值。
			continue
		}
		_ = vm.SetRegister(registerIndex, value)
	}
}

// currentCoroutineThread 返回当前正在运行的非主协程。
//
// 该 helper 只在 coroutine.yield 已成功后保存 continuation；若当前是主线程，返回 nil 让保存逻辑跳过。
func currentCoroutineThread(state *State) *runtime.Thread {
	if state == nil {
		// nil State 没有运行线程。
		return nil
	}
	thread, isMain := state.Running()
	if thread == nil || isMain {
		// 主线程不应保存 coroutine continuation。
		return nil
	}
	return thread
}

// executeLuaClosureWithDebugName 执行 Lua closure 并在调用帧上保存调试名称。
//
// state 必须非 nil；function 必须是 KindLuaClosure 且 Ref 为 *runtime.LuaClosure。name 和
// nameWhat 来自调用点推断，允许为空；当前执行循环支持固定寄存器窗口、Go/Lua 同步调用和
// RETURN 退出，开放调用栈语义后续继续补齐。
func executeLuaClosureWithDebugName(state *State, function Value, name string, nameWhat string, args ...Value) (results []Value, err error) {
	// 普通命名调用不带 tail call 标记，保持历史 call hook 语义。
	return executeLuaClosureWithDebugNameTailArgs(state, function, name, nameWhat, false, args)
}

// executeLuaClosureWithDebugNameTail 执行 Lua closure 并可标记 tail call 调试语义。
//
// state 必须非 nil；function 必须是 KindLuaClosure 且 Ref 为 *runtime.LuaClosure。tailCall=true
// 表示当前帧由 TAILCALL 进入，debug.getinfo(..., "t") 需要返回 istailcall=true，且 call hook
// 事件名需要使用 Lua 5.3 的 `tail call`。
func executeLuaClosureWithDebugNameTail(state *State, function Value, name string, nameWhat string, tailCall bool, args ...Value) (results []Value, err error) {
	// 普通 Lua closure 调用没有 coroutine continuation，从函数入口完整执行。
	return executeLuaClosureWithDebugNameTailFromArgs(state, function, name, nameWhat, tailCall, nil, args)
}

// executeLuaClosureWithDebugNameTailArgs 执行 Lua closure 并可标记 tail call 调试语义。
//
// args 只在本次调用期间读取；固定参数会立即写入寄存器，vararg 会在需要时复制到 VM。
func executeLuaClosureWithDebugNameTailArgs(state *State, function Value, name string, nameWhat string, tailCall bool, args []Value) (results []Value, err error) {
	// slice 入口避免 VM 内部 CALL 为 Go 变参构造临时堆对象。
	return executeLuaClosureWithDebugNameTailFromArgs(state, function, name, nameWhat, tailCall, nil, args)
}

// executeLuaClosureWithDebugNameTailFrom 执行 Lua closure，并可从 coroutine continuation 恢复。
//
// continuation 为 nil 时从函数入口创建 VM；非 nil 时复用保存的 VM、frame 和 PC，并把 args
// 写作上次 coroutine.yield 的返回值后继续执行。
func executeLuaClosureWithDebugNameTailFrom(state *State, function Value, name string, nameWhat string, tailCall bool, continuation *luaCoroutineContinuation, args ...Value) (results []Value, err error) {
	// 变参恢复入口转到 slice 实现，保持外部调用兼容。
	return executeLuaClosureWithDebugNameTailFromArgs(state, function, name, nameWhat, tailCall, continuation, args)
}

// executeLuaClosureWithDebugNameTailFromArgs 执行 Lua closure，并可从 coroutine continuation 恢复。
//
// continuation 为 nil 时从函数入口创建 VM；非 nil 时复用保存 VM。args 只在本次调用期间读取。
func executeLuaClosureWithDebugNameTailFromArgs(state *State, function Value, name string, nameWhat string, tailCall bool, continuation *luaCoroutineContinuation, args []Value) (results []Value, err error) {
	// 先解析 closure 和 Proto，确保后续调试帧可绑定真实函数对象。
	closure, proto, vm, registerCount, varargs, prepareErr := prepareLuaExecutionStateArgs(state, function, continuation, args)
	if prepareErr != nil {
		// Lua closure 引用负载异常或 continuation 损坏时停止执行。
		return nil, prepareErr
	}
	if continuation == nil {
		// 首次执行需要把 resume 参数写入固定形参寄存器；这些槽在 VM reset 阶段已跳过清零。
		fixedArgumentCount := luaClosureFixedArgumentCount(proto, len(args))
		for argumentIndex := 0; argumentIndex < fixedArgumentCount; argumentIndex++ {
			// 固定参数从 R0 开始写入；多余参数已经进入 varargs，写入也不会影响固定参数读取。
			if argumentIndex >= registerCount {
				// 寄存器窗口不足时终止执行，保持错误路径明确。
				return nil, runtime.ErrRegisterOutOfRange
			}
			if err := vm.SetRegister(argumentIndex, args[argumentIndex]); err != nil {
				// 写入参数失败时直接返回底层寄存器错误。
				return nil, err
			}
		}
	}
	releaseVM := continuation == nil && vm != nil
	results, err = executePreparedLuaClosureWithDebugNameTailFromArgs(state, function, name, nameWhat, tailCall, continuation, closure, proto, vm, registerCount, varargs, args)
	if releaseVM && !errors.Is(err, runtime.ErrCoroutineYield) {
		((*runtime.State)(state)).ReturnLuaVMToPool(vm)
	}
	return results, err
}

// executePreparedLuaClosureWithDebugNameTailFromArgs 执行已经准备好 VM 的 Lua closure。
//
// 调用方必须确保 closure/proto/vm/registerCount/varargs 彼此匹配；首次调用时固定参数寄存器也必须已写入。
// 该函数承载真实 VM 执行循环，供普通调用和 direct CALL 共享。
func executePreparedLuaClosureWithDebugNameTailFromArgs(state *State, function Value, name string, nameWhat string, tailCall bool, continuation *luaCoroutineContinuation, closure *runtime.LuaClosure, proto *bytecode.Proto, vm *runtime.VM, registerCount int, varargs []Value, args []Value) (results []Value, err error) {
	state.PushActiveVM(vm)
	runtimeState := ((*runtime.State)(state))
	frame := luaExecutionFrame(state, function, name, nameWhat, tailCall, continuation, varargs)
	restoredOuterFrameCount := 0
	if continuation != nil {
		// 续执行前恢复外层 Lua 调用帧，使后续 yield/error traceback 能看到完整递归链。
		var pushErr error
		restoredOuterFrameCount, pushErr = pushContinuationOuterFrames(state, continuation.outerFrames)
		if pushErr != nil {
			return nil, pushErr
		}
	}
	if err := state.PushCallFrame(frame); err != nil {
		// 调用帧压入失败说明调用深度或 State 生命周期不可用。
		popContinuationOuterFrames(state, restoredOuterFrameCount)
		return nil, err
	}
	debugEnvironment, hasDebugEnvironment := debuglib.EnvironmentForState(runtimeState)
	debugObserver := runtimeState.Options().DebugObserver
	hooksEnabled := hasDebugEnvironment && debugEnvironment.HasActiveHook()
	gluaEventsEnabled := gluaHasAnyEvent(state)
	vm.SetStringTableReadCacheDisabled(gluaEventsEnabled)
	coroutinesCreated := runtimeState.HasCreatedCoroutines()
	preciseFrameSync := hooksEnabled || gluaEventsEnabled || debugObserver != nil || continuation != nil || coroutinesCreated
	refreshHookState := func() {
		coroutinesCreated = runtimeState.HasCreatedCoroutines()
		gluaEventsEnabled = gluaHasAnyEvent(state)
		vm.SetStringTableReadCacheDisabled(gluaEventsEnabled)
		if !hasDebugEnvironment {
			// 未打开 debug 库时仍需刷新 coroutine 创建状态。
			preciseFrameSync = gluaEventsEnabled || debugObserver != nil || continuation != nil || coroutinesCreated
			vm.EnableBorrowedSelfRecursiveLocalFunctionClosure(!preciseFrameSync)
			return
		}
		hooksEnabled = debugEnvironment.HasActiveHook()
		preciseFrameSync = hooksEnabled || gluaEventsEnabled || debugObserver != nil || continuation != nil || coroutinesCreated
		vm.EnableBorrowedSelfRecursiveLocalFunctionClosure(!preciseFrameSync && !hooksEnabled)
	}
	if hooksEnabled && continuation == nil {
		// call/tail call hook 在目标调用帧入栈后触发，使 hook 中 debug.getinfo(2, "f") 指向被调 closure。
		hookErr := error(nil)
		if tailCall && debugEnvironment.HookEnabledFor(debuglib.HookEventTailCall) {
			// 尾调用进入目标帧时，Lua 5.3 使用 tail call 事件而不是普通 call 事件。
			hookErr = triggerLuaTailCallHook(state, debugEnvironment)
		} else if !tailCall && debugEnvironment.HookEnabledFor(debuglib.HookEventCall) {
			// 普通调用保持 call 事件，兼容既有 hook 序列。
			hookErr = triggerLuaCallHook(state, debugEnvironment)
		}
		if hookErr != nil {
			// hook 错误必须中断当前调用，并保留帧交由 protected call 边界恢复。
			return nil, hookErr
		}
	}
	if gluaEventsEnabled && continuation == nil {
		// glua 文件进度开始事件在目标调用帧可见后触发，允许 callback 读取参数 local。
		vm.SetCurrentPC(0)
		if err := state.UpdateCurrentFramePC(0); err != nil {
			// 当前帧缺失说明调用栈边界被破坏。
			return nil, err
		}
		if err := triggerGluaProgressLifecycleEvent(state, proto, GluaEventProgressStart, runtime.NilValue()); err != nil {
			// progress.start 同步事件错误会中断当前代码块。
			return nil, err
		}
	}
	returnHookTriggered := false
	triggerReturnHook := func() error {
		// return hook 只在成功离开当前帧前触发一次，避免多个 return 出口重复派发。
		if !hooksEnabled || returnHookTriggered {
			// 没有 debug 环境或已触发过时保持无副作用。
			return nil
		}
		if debugEnvironment.HookActive() {
			// hook 回调自身返回时不再派发 return hook，避免用户 hook 观察到 hook 函数而漏掉真实返回帧。
			return nil
		}
		if !debugEnvironment.HookEnabledFor(debuglib.HookEventReturn) {
			// 当前没有 return hook 时不进入派发路径，避免无 hook 场景产生临时状态。
			return nil
		}
		if currentFrame, ok := state.CurrentCallFrame(); ok && currentFrame.NameWhat == "hook" {
			// hook 回调自身返回时不再派发 return hook，避免用户 hook 观察到 hook 函数而漏掉真实返回帧。
			return nil
		}
		returnHookTriggered = true
		return triggerLuaReturnHook(state, debugEnvironment)
	}
	defer func() {
		shouldPopFrames := err == nil || errors.Is(err, runtime.ErrCoroutineYield)
		if gluaEventsEnabled && !errors.Is(err, runtime.ErrCoroutineYield) {
			// glua 文件进度 exit/error 在调用帧仍可见时触发，便于 callback 读取局部变量快照。
			originalErr := err
			if err != nil {
				// 错误路径先触发 error 事件，并把原始错误对象作为 payload。
				errorPayload := runtime.ErrorObject(originalErr)
				if eventErr := triggerGluaProgressLifecycleEvent(state, proto, GluaEventProgressError, errorPayload); eventErr != nil {
					// progress.error 事件错误覆盖原结果，按同步回调失败处理。
					err = eventErr
				}
			}
			if eventErr := triggerGluaProgressLifecycleEvent(state, proto, GluaEventProgressExit, runtime.NilValue()); eventErr != nil {
				// progress.exit 事件错误覆盖原结果，按同步回调失败处理。
				err = eventErr
			}
		}
		vm.EnableBorrowedSelfRecursiveLocalFunctionClosure(false)
		if shouldPopFrames {
			// 成功路径和协程 yield 都弹出当前帧；普通错误路径保留帧到 protected call 边界统一恢复。
			_, _ = state.PopCallFrame()
			popContinuationOuterFrames(state, restoredOuterFrameCount)
		}
		state.PopActiveVM(vm)
	}()
	lastHookLine := int64(-1)
	lastGluaProgressLine := int64(-1)
	previousPC := -1
	previousPreviousPC := -1
	pc := 0
	lastSyncedFramePC := -2
	syncCurrentFrame := func(currentPC int) error {
		if lastSyncedFramePC == currentPC {
			// 当前帧已同步到该 PC，无需重复写回调用帧栈。
			return nil
		}
		frame.CurrentPC = currentPC
		if err := state.UpdateCurrentFramePC(currentPC); err != nil {
			// 当前帧缺失说明调用栈边界被破坏，停止执行避免 debug 信息错配。
			return err
		}
		lastSyncedFramePC = currentPC
		return nil
	}
	if continuation != nil {
		resumeNextPC := continuation.nextPC
		if continuation.callRequest != nil && continuation.thread.HasLocalRegisterSnapshotUpdates() {
			// 只有 debug.setlocal(thread, ...) 修改过挂起快照时才写回，避免普通恢复覆盖循环控制寄存器。
			applyThreadLocalSnapshotToVM(continuation.thread, vm, continuation.snapshotLevel)
		}
		if continuation.callRequest != nil {
			// 普通 Lua CALL yield 后，resume 参数作为 coroutine.yield 的返回值写回调用结果区。
			if err := writeLuaCallResults(vm, proto, resumeNextPC-1, continuation.callRequest, args); err != nil {
				return nil, err
			}
		} else if isConcatContinuationInstruction(continuation.resumeInstruction) {
			// CONCAT 元方法 yield 后，返回值只是当前相邻对的折叠结果，可能还要继续折叠左侧寄存器。
			resultValue := runtime.NilValue()
			if len(args) > 0 {
				// 元方法返回至少一个值时，Lua 运算只读取第一个返回值。
				resultValue = args[0]
			}
			if err := vm.ApplyConcatContinuationResult(resultValue); err != nil {
				if errors.Is(err, runtime.ErrCoroutineYield) {
					// 连续 CONCAT 可能再次触发 __concat yield，需要把同一条外层指令现场重新挂回。
					saveResumedInstructionContinuation(continuation)
				}
				return nil, err
			}
		} else if continuation.resumeRegister >= 0 {
			// 元方法驱动的取值、算术、位运算或一元运算 yield 后，元方法返回值写回原目标寄存器。
			resultValue := runtime.NilValue()
			if len(args) > 0 {
				resultValue = args[0]
			}
			if err := vm.SetRegister(continuation.resumeRegister, resultValue); err != nil {
				return nil, err
			}
		} else if isComparisonContinuationInstruction(continuation.resumeInstruction) {
			// 比较元方法 yield 后，resume 参数是元方法返回值，需恢复比较测试的 skipNext。
			resultValue := runtime.NilValue()
			if len(args) > 0 {
				// 元方法返回至少一个值时，Lua 比较只读取第一个返回值。
				resultValue = args[0]
			}
			skipNext, err := vm.ApplyComparisonContinuationResult(resultValue)
			if err != nil {
				return nil, err
			}
			if skipNext {
				// 比较恢复时已经站在下一条指令前，需直接跨过该指令，避免 Step 重置 skipNext。
				resumeNextPC++
			}
		}
		lastHookLine = continuation.lastHookLine
		previousPC = continuation.previousPC
		previousPreviousPC = continuation.previousPreviousPC
		pc = resumeNextPC
	}
	addForLoopSuperInstructionEnabled := false
	mulAddSubForLoopSuperInstructionEnabled := false
	mixArithmeticForLoopSuperInstructionEnabled := false
	functionCallAssignForLoopSuperInstructionEnabled := false
	functionCallAddForLoopSuperInstructionEnabled := false
	closureUpvalueForLoopSuperInstructionEnabled := false
	stdlibMathStringForLoopSuperInstructionEnabled := false
	formatLenAddForLoopSuperInstructionEnabled := false
	stringAppendForLoopSuperInstructionEnabled := false
	tableWriteForLoopSuperInstructionEnabled := false
	tableReadAddForLoopSuperInstructionEnabled := false
	vm.EnableBorrowedSelfRecursiveLocalFunctionClosure(!preciseFrameSync && !hooksEnabled)
	if !preciseFrameSync && !hooksEnabled {
		// 普通主线程无 hook 路径可提前准备 superinstruction 表；hook/coroutine 路径必须保留逐 PC 语义。
		addForLoopSuperInstructionEnabled = vm.PrepareAddForLoopSuperInstructions()
		mulAddSubForLoopSuperInstructionEnabled = vm.PrepareMulAddSubForLoopSuperInstructions()
		mixArithmeticForLoopSuperInstructionEnabled = vm.PrepareMixArithmeticForLoopSuperInstructions()
		functionCallAssignForLoopSuperInstructionEnabled = vm.PrepareFunctionCallAssignForLoopSuperInstructions()
		functionCallAddForLoopSuperInstructionEnabled = vm.PrepareFunctionCallAddForLoopSuperInstructions()
		closureUpvalueForLoopSuperInstructionEnabled = vm.PrepareClosureUpvalueForLoopSuperInstructions()
		stdlibMathStringForLoopSuperInstructionEnabled = vm.PrepareStdlibMathStringForLoopSuperInstructions()
		formatLenAddForLoopSuperInstructionEnabled = vm.PrepareFormatLenAddForLoopSuperInstructions()
		stringAppendForLoopSuperInstructionEnabled = vm.PrepareStringAppendForLoopSuperInstructions()
		tableWriteForLoopSuperInstructionEnabled = vm.PrepareTableWriteForLoopSuperInstructions()
		tableReadAddForLoopSuperInstructionEnabled = vm.PrepareTableReadAddForLoopSuperInstructions()
	}
	contextCheckCountdown := 0
	for pc >= 0 && pc < len(proto.Code) {
		// 先同步当前 PC，供 collectgarbage 执行时按 local 生命周期裁剪活动寄存器根。
		vm.SetCurrentPC(pc)
		if gluaEventsEnabled {
			// 异步 glua 事件只在 VM 安全点执行，避免触发点直接重入 Lua。
			if err := drainGluaEventQueue(state); err != nil {
				if syncErr := syncCurrentFrame(pc); syncErr != nil {
					return nil, syncErr
				}
				return nil, err
			}
		}
		if preciseFrameSync || hooksEnabled || contextCheckCountdown <= 0 {
			// 需要精确调试语义的路径逐指令检查；普通热路径按固定窗口检查，降低 tight loop 固定开销。
			if err := state.CheckContext(); err != nil {
				if syncErr := syncCurrentFrame(pc); syncErr != nil {
					return nil, syncErr
				}
				return nil, err
			}
			contextCheckCountdown = luaContextCheckInstructionInterval
		} else {
			// 本轮处于普通热路径窗口内，延后到窗口耗尽再检查 context。
			contextCheckCountdown--
		}
		if preciseFrameSync {
			if err := syncCurrentFrame(pc); err != nil {
				// active hook、coroutine 和 continuation 路径需要逐指令同步调用帧 PC。
				return nil, err
			}
		}
		if debugObserver != nil {
			// DAP 等外部调试器需要在指令执行前观察源码行，并可在此处阻塞等待继续。
			if err := debugObserver.BeforeInstruction(runtimeState, vm, proto, pc); err != nil {
				// 调试器返回错误表示会话取消或断开，直接中断当前 Lua 执行。
				return nil, err
			}
		}
		if hooksEnabled && debugEnvironment.HookEnabledFor(debuglib.HookEventLine) {
			if err := syncCurrentFrame(pc); err != nil {
				// line hook 需要当前调用帧 PC 与即将执行指令一致。
				return nil, err
			}
			// line hook 需要在指令执行前触发，确保 hook 观察到即将执行的源码行。
			if err := triggerLuaLineHook(state, debugEnvironment, proto, pc, previousPC, previousPreviousPC, &lastHookLine); err != nil {
				// hook 内抛错必须中断当前 VM，交给 protected call 边界包装 traceback。
				return nil, err
			}
		}
		if gluaEventsEnabled && proto != nil && pc >= 0 && pc < len(proto.LineInfo) {
			// progress.line 按源码行变化触发，复用精确帧同步路径提供当前 local 快照。
			line := int64(proto.LineInfo[pc])
			if line > 0 && line != lastGluaProgressLine {
				// 同一连续源码行只触发一次，避免一行多指令产生噪声。
				if err := syncCurrentFrame(pc); err != nil {
					return nil, err
				}
				if err := triggerGluaProgressLineEvent(state, proto, line); err != nil {
					return nil, err
				}
				lastGluaProgressLine = line
			}
		}
		if tableWriteForLoopSuperInstructionEnabled && !preciseFrameSync && !hooksEnabled && contextCheckCountdown > 0 && vm.HasTableWriteForLoopAt(pc) {
			// 两指令 table 连续写入循环会额外跳过 FORLOOP，批量 N 轮会额外跳过 2*N-1 个入口。
			setTablePC := pc
			if batch, ok := vm.PrepareTableWriteForLoopBatch(setTablePC); ok {
				// 当前 SETTABLE 入口的 context 已在本轮循环顶部消费；N 轮 batch 按窗口上限保守提交。
				maxIterations := (contextCheckCountdown + 1) / 2
				nextPC, handledIterations, handled := vm.TryExecuteTableWriteForLoopBatch(batch, maxIterations)
				if handled {
					// superinstruction 已完成若干轮 SETTABLE 与 FORLOOP；补偿被跳过的 SETTABLE/FORLOOP 入口。
					contextCheckCountdown -= handledIterations*2 - 1
					previousPreviousPC = setTablePC
					previousPC = setTablePC + 1
					pc = nextPC
					continue
				}
			}
		}
		if tableReadAddForLoopSuperInstructionEnabled && !preciseFrameSync && !hooksEnabled && contextCheckCountdown >= 2 && vm.HasTableReadAddForLoopAt(pc) {
			// 三指令 table 连续读取求和循环会额外跳过 ADD、FORLOOP，批量 N 轮会额外跳过 3*N-1 个入口。
			getTablePC := pc
			if batch, ok := vm.PrepareTableReadAddForLoopBatch(getTablePC); ok {
				// 当前 GETTABLE 入口的 context 已在本轮循环顶部消费；N 轮 batch 按窗口上限保守提交。
				maxIterations := (contextCheckCountdown + 1) / 3
				nextPC, handledIterations, handled := vm.TryExecuteTableReadAddForLoopBatch(batch, maxIterations)
				if handled {
					// superinstruction 已完成若干轮 GETTABLE、ADD 与 FORLOOP；补偿被跳过的入口。
					contextCheckCountdown -= handledIterations*3 - 1
					previousPreviousPC = getTablePC + 1
					previousPC = getTablePC + 2
					pc = nextPC
					continue
				}
			}
		}
		if stdlibMathStringForLoopSuperInstructionEnabled && !preciseFrameSync && !hooksEnabled && contextCheckCountdown >= 15 && vm.HasStdlibMathStringForLoopAt(pc) {
			// 十六指令 stdlib_math_string 完整循环体会额外跳过后续十五个入口；
			// 倒计时至少为 15 才能证明逐指令路径不会在被跳过区间触发 context 检查。
			loopPC := pc
			maxIterations := (contextCheckCountdown + 1) / 16
			nextPC, handledIterations, handled, err := vm.TryExecuteStdlibMathStringForLoopBatch(loopPC, maxIterations, state)
			if err != nil {
				// context 取消时同步到当前循环体入口，保持标准库 CALL fast path 的错误 PC 边界。
				if syncErr := syncCurrentFrame(loopPC); syncErr != nil {
					return nil, syncErr
				}
				return nil, err
			}
			if handled {
				// superinstruction 已完成若干轮完整循环体；补偿被跳过的指令入口。
				contextCheckCountdown -= handledIterations*16 - 1
				previousPreviousPC = loopPC + 14
				previousPC = loopPC + 15
				pc = nextPC
				continue
			}
		}
		if formatLenAddForLoopSuperInstructionEnabled && !preciseFrameSync && !hooksEnabled && contextCheckCountdown >= 7 && vm.HasFormatLenAddForLoopAt(pc) {
			// 八指令 string.format 长度消费尾部会额外跳过 GETTABLE、LOADK、MOVE、CALL、LEN、ADD、FORLOOP 七个入口；
			// 倒计时至少为 7 才能证明逐指令路径不会在被跳过区间触发 context 检查。
			if err := state.CheckContext(); err != nil {
				// 普通 CALL 入口会检查 context，superinstruction 在跳过 CALL 前保留该取消边界。
				if syncErr := syncCurrentFrame(pc); syncErr != nil {
					return nil, syncErr
				}
				return nil, err
			}
			formatPC := pc
			if nextPC, handled := vm.TryExecuteFormatLenAddForLoop(formatPC); handled {
				// superinstruction 已完成 GETTABUP、GETTABLE、LOADK、MOVE、CALL、LEN、ADD 与 FORLOOP。
				contextCheckCountdown -= 7
				previousPreviousPC = formatPC + 6
				previousPC = formatPC + 7
				pc = nextPC
				continue
			}
		}
		if stringAppendForLoopSuperInstructionEnabled && !preciseFrameSync && !hooksEnabled && contextCheckCountdown >= 3 && vm.HasStringAppendForLoopAt(pc) {
			// 四指令字符串自追加循环会额外跳过 LOADK、CONCAT、FORLOOP 三个入口；
			// 倒计时至少为 3 才能证明逐指令路径不会在被跳过区间触发 context 检查。
			movePC := pc
			nextPC, handledIterations, updatedCountdown, handled, err := vm.TryExecuteStringAppendForLoopWholeBatch(movePC, contextCheckCountdown, luaContextCheckInstructionInterval, state.CheckContext)
			if err != nil {
				// context 取消时同步到循环体入口，保持字符串自追加 fast path 的错误 PC 边界。
				if syncErr := syncCurrentFrame(movePC); syncErr != nil {
					return nil, syncErr
				}
				return nil, err
			}
			if handled {
				// 整段 builder 已在内部模拟 context 检查；直接采用返回的倒计时状态。
				contextCheckCountdown = updatedCountdown
				previousPreviousPC = movePC + 2
				previousPC = movePC + 3
				pc = nextPC
				continue
			}
			maxIterations := (contextCheckCountdown + 1) / 4
			nextPC, handledIterations, handled = vm.TryExecuteStringAppendForLoopBatch(movePC, maxIterations)
			if handled {
				// superinstruction 已完成若干轮 MOVE、LOADK、CONCAT 与 FORLOOP；补偿被跳过入口。
				contextCheckCountdown -= handledIterations*4 - 1
				previousPreviousPC = movePC + 2
				previousPC = movePC + 3
				pc = nextPC
				continue
			}
		}
		if functionCallAssignForLoopSuperInstructionEnabled && !preciseFrameSync && !hooksEnabled && contextCheckCountdown >= 5 && vm.HasFunctionCallAssignForLoopAt(pc) {
			// 六指令官方 function_call 循环会额外跳过 MOVE、MOVE、CALL、MOVE、FORLOOP 五个入口；
			// 倒计时至少为 5 才能证明逐指令路径不会在被跳过区间触发 context 检查。
			movePC := pc
			callContextChecked := false
			if batch, ok := vm.PrepareFunctionCallAssignForLoopBatch(movePC); ok {
				// 保守批量路径只复用静态 guard；runtime 内部每个虚拟 CALL 前仍执行 context 检查。
				nextPC, handledIterations, handled, err := vm.TryExecuteFunctionCallAssignForLoopBatch(batch, contextCheckCountdown/5, state)
				callContextChecked = true
				if err != nil {
					// context 取消时同步到当前循环体入口，保持 direct CALL fast path 的错误 PC 边界。
					if syncErr := syncCurrentFrame(movePC); syncErr != nil {
						return nil, syncErr
					}
					return nil, err
				}
				if handled {
					// superinstruction 已完成若干轮 MOVE、MOVE、MOVE、CALL、MOVE 与 FORLOOP；补偿被跳过入口。
					contextCheckCountdown -= handledIterations * 5
					previousPreviousPC = movePC + 4
					previousPC = movePC + 5
					pc = nextPC
					continue
				}
			}
			if !callContextChecked {
				// 静态 PC 命中但 batch guard 失败时，仍保留一次 CALL 入口取消边界。
				if err := state.CheckContext(); err != nil {
					if syncErr := syncCurrentFrame(pc); syncErr != nil {
						return nil, syncErr
					}
					return nil, err
				}
			}
		}
		if closureUpvalueForLoopSuperInstructionEnabled && !preciseFrameSync && !hooksEnabled && contextCheckCountdown >= 4 && vm.HasClosureUpvalueForLoopAt(pc) {
			// 五指令 closure_upvalue 循环会额外跳过 LOADK、CALL、MOVE、FORLOOP 四个入口；
			// 倒计时至少为 4 才能证明逐指令路径不会在被跳过区间触发 context 检查。
			movePC := pc
			callContextChecked := false
			if batch, ok := vm.PrepareClosureUpvalueForLoopBatch(movePC); ok {
				// 保守批量路径只复用静态 guard；runtime 内部每个虚拟 CALL 前仍执行 context 检查。
				nextPC, handledIterations, handled, err := vm.TryExecuteClosureUpvalueForLoopBatch(batch, contextCheckCountdown/4, state)
				callContextChecked = true
				if err != nil {
					// context 取消时同步到当前循环体入口，保持 closure_upvalue fast path 的错误 PC 边界。
					if syncErr := syncCurrentFrame(movePC); syncErr != nil {
						return nil, syncErr
					}
					return nil, err
				}
				if handled {
					// superinstruction 已完成若干轮 MOVE、LOADK、CALL、MOVE 与 FORLOOP；补偿被跳过入口。
					contextCheckCountdown -= handledIterations * 4
					previousPreviousPC = movePC + 3
					previousPC = movePC + 4
					pc = nextPC
					continue
				}
			}
			if !callContextChecked {
				// 静态 PC 命中但 batch guard 失败时，仍保留一次 CALL 入口取消边界。
				if err := state.CheckContext(); err != nil {
					if syncErr := syncCurrentFrame(pc); syncErr != nil {
						return nil, syncErr
					}
					return nil, err
				}
			}
		}
		if functionCallAddForLoopSuperInstructionEnabled && !preciseFrameSync && !hooksEnabled && contextCheckCountdown >= 5 && vm.HasFunctionCallAddForLoopAt(pc) {
			// 六指令 function_call 循环会额外跳过 MOVE、LOADK、CALL、ADD、FORLOOP 五个入口；
			// 倒计时至少为 5 才能证明逐指令路径不会在被跳过区间触发 context 检查。
			movePC := pc
			callContextChecked := false
			if batch, ok := vm.PrepareFunctionCallAddForLoopBatch(movePC); ok {
				// 保守批量路径只复用静态 guard；runtime 内部每个虚拟 CALL 前仍执行 context 检查。
				nextPC, handledIterations, handled, err := vm.TryExecuteFunctionCallAddForLoopBatch(batch, contextCheckCountdown/5, state)
				callContextChecked = true
				if err != nil {
					// context 取消时同步到当前循环体入口，保持旧 function_call fast path 的错误 PC 边界。
					if syncErr := syncCurrentFrame(movePC); syncErr != nil {
						return nil, syncErr
					}
					return nil, err
				}
				if handled {
					// superinstruction 已完成若干轮 MOVE、MOVE、LOADK、CALL、ADD 与 FORLOOP；补偿被跳过入口。
					contextCheckCountdown -= handledIterations * 5
					previousPreviousPC = movePC + 4
					previousPC = movePC + 5
					pc = nextPC
					continue
				}
			}
			if !callContextChecked {
				// 静态 PC 命中但 batch guard 失败时，仍按旧单轮路径保留一次 CALL 入口取消边界。
				if err := state.CheckContext(); err != nil {
					if syncErr := syncCurrentFrame(pc); syncErr != nil {
						return nil, syncErr
					}
					return nil, err
				}
			}
		}
		if mixArithmeticForLoopSuperInstructionEnabled && !preciseFrameSync && !hooksEnabled && contextCheckCountdown >= 6 && vm.HasMixArithmeticForLoopAt(pc) {
			// 七指令混合算术链会额外跳过 ADD、SUB、IDIV、MOD、ADD、FORLOOP 六个入口；
			// 倒计时至少为 6 才能证明普通逐指令路径不会在被跳过区间触发 context 检查。
			mixPC := pc
			if batch, ok := vm.PrepareMixArithmeticForLoopBatch(mixPC); ok {
				// 当前 MUL 入口的 context 已在本轮循环顶部消费；N 轮 batch 额外跳过 7*N-1 个指令入口。
				maxIterations := (contextCheckCountdown + 1) / 7
				nextPC, handledIterations, handled := vm.TryExecuteMixArithmeticForLoopBatch(batch, maxIterations)
				if handled {
					// superinstruction 已完成若干轮混合算术链与 FORLOOP；补偿被跳过的入口。
					contextCheckCountdown -= handledIterations*7 - 1
					previousPreviousPC = mixPC + 5
					previousPC = mixPC + 6
					pc = nextPC
					continue
				}
			}
			if nextPC, handled := vm.TryExecuteMixArithmeticForLoop(mixPC); handled {
				// superinstruction 已完成完整混合算术循环体；补偿被跳过六条指令的倒计时。
				contextCheckCountdown -= 6
				previousPreviousPC = mixPC + 5
				previousPC = mixPC + 6
				pc = nextPC
				continue
			}
		}
		if mulAddSubForLoopSuperInstructionEnabled && !preciseFrameSync && !hooksEnabled && contextCheckCountdown >= 3 {
			// 四指令算术链会额外跳过 ADD、SUB、FORLOOP 三个入口；倒计时至少为 3
			// 才能证明普通逐指令路径也不会在被跳过的 FORLOOP 前触发 context 检查。
			mulPC := pc
			if batch, ok := vm.PrepareMulAddSubForLoopBatch(mulPC); ok {
				// 当前 MUL 入口的 context 已在本轮循环顶部消费；N 轮 batch 额外跳过 4*N-1 个指令入口。
				maxIterations := (contextCheckCountdown + 1) / 4
				nextPC, handledIterations, handled := vm.TryExecuteMulAddSubForLoopBatch(batch, maxIterations)
				if handled {
					// superinstruction 已完成若干轮 MUL、ADD、SUB 与 FORLOOP；补偿被跳过的入口。
					contextCheckCountdown -= handledIterations*4 - 1
					previousPreviousPC = mulPC + 2
					previousPC = mulPC + 3
					pc = nextPC
					continue
				}
			}
			if nextPC, handled := vm.TryExecuteMulAddSubForLoop(mulPC); handled {
				// superinstruction 已完成 MUL、ADD、SUB 与 FORLOOP；补偿被跳过三条指令的倒计时。
				contextCheckCountdown -= 3
				previousPreviousPC = mulPC + 2
				previousPC = mulPC + 3
				pc = nextPC
				continue
			}
		}
		if addForLoopSuperInstructionEnabled && !preciseFrameSync && !hooksEnabled && contextCheckCountdown > 0 {
			// 普通无 hook、无 coroutine 的热路径可尝试两条指令合并；contextCheckCountdown > 0
			// 表示被合并的 FORLOOP 也不会越过本轮 context 检查边界。
			addPC := pc
			if batch, ok := vm.PrepareAddForLoopBatch(addPC); ok {
				// 当前 ADD 入口的 context 已在本轮循环顶部消费；N 轮 batch 额外跳过 2*N-1 个指令入口。
				maxIterations := (contextCheckCountdown + 1) / 2
				nextPC, handledIterations, handled := vm.TryExecuteAddForLoopBatch(batch, maxIterations)
				if handled {
					// superinstruction 已完成若干轮 ADD 与 FORLOOP；补偿被跳过的 ADD/FORLOOP 入口。
					contextCheckCountdown -= handledIterations*2 - 1
					previousPreviousPC = addPC
					previousPC = addPC + 1
					pc = nextPC
					continue
				}
			}
			if nextPC, handled := vm.TryExecuteAddForLoop(addPC); handled {
				// superinstruction 已完成单轮 ADD 与后续 FORLOOP；补偿被跳过 FORLOOP 入口的一次 context 倒计时。
				contextCheckCountdown--
				previousPreviousPC = addPC
				previousPC = addPC + 1
				pc = nextPC
				continue
			}
		}
		instruction := proto.Code[pc]
		opCode := instruction.OpCode()
		var coroutineThread *runtime.Thread
		if coroutinesCreated {
			// 只有 State 创建过协程时才需要查询当前运行线程；普通主线程热路径避免每条指令调用 Running。
			coroutineThread = currentCoroutineThread(state)
		}
		if err := vm.Step(instruction); err != nil {
			if syncErr := syncCurrentFrame(pc); syncErr != nil {
				return nil, syncErr
			}
			if errors.Is(err, runtime.ErrExpectedCallable) {
				// CALL/TAILCALL 在 VM 阶段发现非函数值时，执行循环仍持有调用点上下文，可补齐 Lua 风格错误名。
				err = luaCallErrorAtInstruction(state, proto, vm, pc, instruction, err)
			} else if errors.Is(err, runtime.ErrExpectedTable) {
				// GETTABLE/SELF/SETTABLE 对非 table 访问失败时，同样可通过指令上下文补齐来源名称。
				err = luaIndexErrorAtInstruction(proto, vm, pc, instruction, err)
			} else if errors.Is(err, runtime.ErrArithmeticOperand) || errors.Is(err, runtime.ErrIntegerOperand) {
				// 算术和位运算错误需要指出失败操作数的 local/upvalue/global 来源。
				err = luaOperationErrorAtInstruction(proto, vm, pc, instruction, err)
			}
			if errors.Is(err, runtime.ErrCoroutineYield) {
				// VM 指令内部 Lua 元方法 yield 时，保存当前指令外层现场，供内层元方法恢复后继续执行。
				saveInstructionContinuation(state, coroutineThread, function, closure, proto, vm, frame, instruction, pc, previousPC, lastHookLine)
			} else {
				// 普通 VM 运行期错误补当前 Lua chunk 的 source/line；stripped chunk 使用 ?:-1。
				err = decorateLuaRuntimeErrorAtPC(proto, pc, err)
			}
			// VM 单步错误或 yield 交给 protected call / coroutine 边界处理。
			return nil, err
		}
		if !hooksEnabled && isLuaHotNoPostProcessOpcode(opCode) {
			// 普通无 hook 热路径中的算术和 FORLOOP 不会产生 CALL/RETURN/JMP close/分配后处理，直接推进 PC。
			previousPreviousPC = previousPC
			previousPC = pc
			pc = vm.NextPC(pc)
			continue
		}
		if hooksEnabled && debugEnvironment.HookEnabledFor(debuglib.HookEventCount) && shouldTriggerLuaCountHook(proto, pc) {
			if err := syncCurrentFrame(pc); err != nil {
				// count hook 需要当前调用帧 PC 与刚执行指令一致。
				return nil, err
			}
			// count hook 在当前指令执行后触发，避免 hook 修改影响同一表达式正在读取的值。
			if err := triggerLuaCountHook(state, debugEnvironment); err != nil {
				// count hook 内抛错同样中断当前 VM，保持 hook 可作为调试中断点。
				return nil, err
			}
		}
		if opCode == bytecode.OpJmp {
			if closeFrom, ok := vm.CloseFrom(); ok {
				// JMP A 非零表示离开局部作用域，必须闭合对应寄存器及之后的 open upvalue。
				vm.CloseUpvaluesFrom(closeFrom)
			}
		}
		if opCode == bytecode.OpNewTable || opCode == bytecode.OpClosure || opCode == bytecode.OpConcat {
			// 分配压力指令后给自动 GC 一次推进机会，覆盖 table、closure 和字符串拼接。
			((*runtime.State)(state)).NoteTableAllocation()
		}
		if returnValues := vm.BorrowReturnValues(); returnValues != nil {
			if err := syncCurrentFrame(pc); err != nil {
				// return hook 与 traceback 需要看到返回指令所在 PC。
				return nil, err
			}
			// RETURN 指令结束当前 closure，并返回快照。
			if err := triggerReturnHook(); err != nil {
				// return hook 错误需要覆盖正常返回，保持 Lua hook 可中断调用。
				return nil, err
			}
			if gluaEventsEnabled {
				if err := triggerGluaProgressLifecycleEvent(state, proto, GluaEventProgressEnd, runtime.NilValue()); err != nil {
					// progress.end 表示当前代码块正常完成，错误会覆盖返回路径。
					return nil, err
				}
				if err := drainGluaEventQueue(state); err != nil {
					return nil, err
				}
			}
			return returnValues, nil
		}
		if callRequest := vm.LastCallRequest(); callRequest != nil {
			if err := syncCurrentFrame(pc); err != nil {
				// CALL 路径可能进入 debug 库或触发错误，调用帧必须同步到调用点。
				return nil, err
			}
			if callRequest.Tail {
				// TAILCALL 会让被调函数替换当前 caller 的调试可见帧，并把结果作为本 closure 结果返回。
				arguments, err := luaCallArguments(vm, callRequest)
				if err != nil {
					// 参数区间读取失败时停止当前调用。
					return nil, err
				}
				functionValue, ok := vm.Register(callRequest.FunctionIndex)
				if !ok {
					// 函数寄存器缺失说明 codegen 或 VM 状态异常。
					return nil, runtime.ErrRegisterOutOfRange
				}
				debugName, debugNameWhat := luaCallDebugNameAtCall(state, functionValue, false, proto, vm, pc)
				if isSameLuaClosure(functionValue, closure) {
					// 自尾调用不应增长 Go 调用栈；复用当前 VM 和调用帧重新进入函数入口。
					nextVarargs := luaClosureVarargs(proto, arguments)
					vm.ResetForTailCall(nextVarargs)
					nextFixedArgumentCount := luaClosureFixedArgumentCount(proto, len(arguments))
					for argumentIndex := 0; argumentIndex < nextFixedArgumentCount; argumentIndex++ {
						// 固定参数仍从 R0 开始写入，覆盖上一轮调用的寄存器内容。
						if argumentIndex >= registerCount {
							// 自尾调用复用同一 Proto，理论上窗口足够；不足时返回明确寄存器错误。
							return nil, runtime.ErrRegisterOutOfRange
						}
						if err := vm.SetRegister(argumentIndex, arguments[argumentIndex]); err != nil {
							// 写入参数失败时直接返回底层寄存器错误。
							return nil, err
						}
					}
					frame.Name = debugName
					frame.NameWhat = debugNameWhat
					frame.TailCall = true
					if len(nextVarargs) > 0 {
						// 下一轮调用存在 vararg 时刷新调试帧快照。
						frame.Varargs = &runtime.VarargSnapshot{Values: append([]Value(nil), nextVarargs...)}
					} else {
						// 下一轮调用没有 vararg 时清空旧快照，避免 debug.getlocal 读到历史参数。
						frame.Varargs = nil
					}
					frame.CurrentPC = 0
					if err := state.ReplaceCurrentCallFrame(frame); err != nil {
						// 当前帧缺失说明调用栈边界被破坏，停止执行避免 debug 信息错配。
						return nil, err
					}
					if hooksEnabled && debugEnvironment.HookEnabledFor(debuglib.HookEventTailCall) {
						// 自尾调用仍需要派发 tail call hook，事件由 call mask 控制。
						if err := triggerLuaTailCallHook(state, debugEnvironment); err != nil {
							// hook 错误必须中断当前调用，并保留帧交由 protected call 边界恢复。
							return nil, err
						}
					}
					lastHookLine = -1
					previousPC = -1
					previousPreviousPC = -1
					pc = 0
					continue
				}
				results, err := callWithDebugNameTail(state, functionValue, debugName, debugNameWhat, true, arguments...)
				if err != nil {
					// 尾调用进入被调函数前也可能触发调用深度限制，需要按当前 TAILCALL 位置补齐源码行。
					if currentCoroutineThread(state) == nil {
						// 主线程 Lua 调用深度溢出按官方 errors.lua 展示为 source:line: stack overflow。
						return nil, decorateLuaCallDepthOverflowAtPC(proto, pc, err)
					}
					return nil, err
				}
				return results, nil
			}
			// CALL/TAILCALL/TFORCALL 产生的请求由 API 层递归消费并写回结果。
			var coroutineThread *runtime.Thread
			if coroutinesCreated {
				// 只有 State 创建过协程时才需要查询当前运行线程；普通主线程 CALL 避免每次进入 Running。
				coroutineThread = currentCoroutineThread(state)
			}
			if err := executeLuaCallRequest(state, vm, proto, pc, callRequest, hooksEnabled, coroutinesCreated); err != nil {
				if errors.Is(err, runtime.ErrCoroutineYield) && coroutineThread != nil {
					// coroutine.yield 中断当前 Lua CALL 时保存当前 VM 现场；已有内层现场会作为链式 continuation 先恢复。
					saveLuaContinuation(&luaCoroutineContinuation{
						thread:             coroutineThread,
						function:           function,
						closure:            closure,
						proto:              proto,
						vm:                 vm,
						frame:              frame,
						outerFrames:        continuationOuterFrames(coroutineThread),
						callRequest:        callRequest,
						resumeRegister:     -1,
						snapshotLevel:      continuationSnapshotLevel(coroutineThread, frame),
						nextPC:             pc + 1,
						previousPC:         pc,
						previousPreviousPC: previousPC,
						lastHookLine:       lastHookLine,
					})
				}
				return nil, err
			}
			refreshHookState()
		}
		previousPreviousPC = previousPC
		previousPC = pc
		pc = vm.NextPC(pc)
	}

	// Proto 正常落出指令数组时按无返回值处理。
	if err := triggerReturnHook(); err != nil {
		// 隐式返回同样需要允许 return hook 中断调用。
		return nil, err
	}
	if gluaEventsEnabled {
		if err := triggerGluaProgressLifecycleEvent(state, proto, GluaEventProgressEnd, runtime.NilValue()); err != nil {
			// progress.end 同样覆盖隐式返回路径。
			return nil, err
		}
		if err := drainGluaEventQueue(state); err != nil {
			return nil, err
		}
	}
	return nil, nil
}

// isLuaHotNoPostProcessOpcode 判断 opcode 成功执行后能否跳过通用后处理。
//
// 只允许不会生成 CALL/RETURN 请求、不会触发 JMP close、不会产生自动 GC 推进机会的高频 opcode。
// debug hook 路径不使用本 helper，确保 count hook、line hook 和调试帧同步语义保持完整。
func isLuaHotNoPostProcessOpcode(opCode bytecode.OpCode) bool {
	switch opCode {
	case bytecode.OpAdd, bytecode.OpSub, bytecode.OpMul, bytecode.OpMod, bytecode.OpDiv, bytecode.OpIDiv, bytecode.OpForLoop:
		// 算术和数值 for 步进只影响寄存器与 pcOffset；成功后可直接进入下一条 PC 计算。
		return true
	default:
		// 其他 opcode 仍走通用 CALL/RETURN/JMP/GC 后处理，避免遗漏语义副作用。
		return false
	}
}

// callGoClosureWithDebugFrame 在调试帧保护下执行 Go closure。
//
// state 必须非 nil；function 必须是 KindGoClosure。该路径只用于 VM 内部命名调用，公开 Call 仍保持
// 原有轻量入口；成功返回前触发 return hook，错误路径保留调用帧给 protected call 恢复。
func callGoClosureWithDebugFrame(state *State, function Value, name string, nameWhat string, tailCall bool, args ...Value) (results []Value, err error) {
	// Go closure 帧不占用真实寄存器窗口，只保存函数与调用点名称供 debug API 查询。
	frame := runtime.NewGoCallFrame(function, state.StackTop()+1, -1)
	frame.Name = name
	frame.NameWhat = nameWhat
	frame.TailCall = tailCall
	if err := state.PushCallFrame(frame); err != nil {
		// 调用帧压入失败时无法安全进入 Go 回调。
		return nil, err
	}
	debugEnvironment, hasDebugEnvironment := debuglib.EnvironmentForState((*runtime.State)(state))
	if hasDebugEnvironment {
		// call/tail call hook 在 Go closure 帧入栈后触发，对齐 Lua 5.3 的被调函数可见性。
		hookErr := error(nil)
		if tailCall && debugEnvironment.HookEnabledFor(debuglib.HookEventTailCall) {
			// Go closure 也可能由 TAILCALL 进入，debug hook 需要看到 tail call 事件。
			hookErr = triggerLuaTailCallHook(state, debugEnvironment)
		} else if !tailCall && debugEnvironment.HookEnabledFor(debuglib.HookEventCall) {
			// 普通 Go closure 调用保持 call 事件。
			hookErr = triggerLuaCallHook(state, debugEnvironment)
		}
		if hookErr != nil {
			// hook 错误时保留帧给 protected call 边界恢复。
			return nil, hookErr
		}
	}
	defer func() {
		if err == nil || errors.Is(err, runtime.ErrCoroutineYield) {
			// 成功路径和协程 yield 都弹出当前 Go 帧；普通错误路径保留帧以便 ProtectedCall 生成 traceback 后恢复。
			_, _ = state.PopCallFrame()
		}
	}()
	results, err = callGoClosureValue(function, args...)
	if err != nil {
		// Go 回调的参数错误可结合调用点补齐完整库函数名。
		err = rewriteGoClosureBadArgumentName(err, goClosureBadArgumentName(state, function, name, nameWhat), nameWhat)
		// Go 回调错误返回后，调用帧保留给上层错误边界。
		return nil, err
	}
	if hasDebugEnvironment && debugEnvironment.HookEnabledFor(debuglib.HookEventReturn) {
		// return hook 在 Go closure 帧仍可见时触发。
		if err := triggerLuaReturnHook(state, debugEnvironment); err != nil {
			return nil, err
		}
	}
	return results, nil
}

// callGoFunctionWithDebugFrame 在调试帧保护下执行单返回 Go closure。
//
// state 必须非 nil；function 必须是 KindGoClosure 且 goFunction 是其实际入口。该路径用于 VM
// 内部单返回 CALL 快路径，保留 call/return hook、错误帧和 bad argument 名称重写语义。
func callGoFunctionWithDebugFrame(state *State, function Value, goFunction runtime.GoFunction, name string, nameWhat string, tailCall bool, args ...Value) (result Value, err error) {
	// Go closure 帧不占用真实寄存器窗口，只保存函数与调用点名称供 debug API 查询。
	frame := runtime.NewGoCallFrame(function, state.StackTop()+1, -1)
	frame.Name = name
	frame.NameWhat = nameWhat
	frame.TailCall = tailCall
	if err := state.PushCallFrame(frame); err != nil {
		// 调用帧压入失败时无法安全进入 Go 回调。
		return runtime.NilValue(), err
	}
	debugEnvironment, hasDebugEnvironment := debuglib.EnvironmentForState((*runtime.State)(state))
	if hasDebugEnvironment {
		// call/tail call hook 在 Go closure 帧入栈后触发，对齐 Lua 5.3 的被调函数可见性。
		hookErr := error(nil)
		if tailCall && debugEnvironment.HookEnabledFor(debuglib.HookEventTailCall) {
			// Go closure 也可能由 TAILCALL 进入，debug hook 需要看到 tail call 事件。
			hookErr = triggerLuaTailCallHook(state, debugEnvironment)
		} else if !tailCall && debugEnvironment.HookEnabledFor(debuglib.HookEventCall) {
			// 普通 Go closure 调用保持 call 事件。
			hookErr = triggerLuaCallHook(state, debugEnvironment)
		}
		if hookErr != nil {
			// hook 错误时保留帧给 protected call 边界恢复。
			return runtime.NilValue(), hookErr
		}
	}
	defer func() {
		if err == nil || errors.Is(err, runtime.ErrCoroutineYield) {
			// 成功路径和协程 yield 都弹出当前 Go 帧；普通错误路径保留帧以便 ProtectedCall 生成 traceback 后恢复。
			_, _ = state.PopCallFrame()
		}
	}()
	result, err = goFunction(args...)
	if err != nil {
		// Go 回调的参数错误可结合调用点补齐完整库函数名。
		err = rewriteGoClosureBadArgumentName(err, goClosureBadArgumentName(state, function, name, nameWhat), nameWhat)
		// Go 回调错误返回后，调用帧保留给上层错误边界。
		return runtime.NilValue(), err
	}
	if hasDebugEnvironment && debugEnvironment.HookEnabledFor(debuglib.HookEventReturn) {
		// return hook 在 Go closure 帧仍可见时触发。
		if err := triggerLuaReturnHook(state, debugEnvironment); err != nil {
			return runtime.NilValue(), err
		}
	}
	return result, nil
}

// callGoUnaryFunctionWithDebugFrame 在调试帧保护下执行单参数单返回 Go closure。
//
// state 必须非 nil；function 必须是 KindGoClosure 且 unaryFunction 是其实际入口。该路径用于
// 标准库一元函数 CALL 快路径，保留 call/return hook、错误帧和 bad argument 名称重写语义。
func callGoUnaryFunctionWithDebugFrame(state *State, function Value, unaryFunction runtime.GoUnaryFunction, name string, nameWhat string, tailCall bool, argument Value) (result Value, err error) {
	// Go closure 帧不占用真实寄存器窗口，只保存函数与调用点名称供 debug API 查询。
	frame := runtime.NewGoCallFrame(function, state.StackTop()+1, -1)
	frame.Name = name
	frame.NameWhat = nameWhat
	frame.TailCall = tailCall
	if err := state.PushCallFrame(frame); err != nil {
		// 调用帧压入失败时无法安全进入 Go 回调。
		return runtime.NilValue(), err
	}
	debugEnvironment, hasDebugEnvironment := debuglib.EnvironmentForState((*runtime.State)(state))
	if hasDebugEnvironment {
		// call/tail call hook 在 Go closure 帧入栈后触发，对齐 Lua 5.3 的被调函数可见性。
		hookErr := error(nil)
		if tailCall && debugEnvironment.HookEnabledFor(debuglib.HookEventTailCall) {
			// Go closure 也可能由 TAILCALL 进入，debug hook 需要看到 tail call 事件。
			hookErr = triggerLuaTailCallHook(state, debugEnvironment)
		} else if !tailCall && debugEnvironment.HookEnabledFor(debuglib.HookEventCall) {
			// 普通 Go closure 调用保持 call 事件。
			hookErr = triggerLuaCallHook(state, debugEnvironment)
		}
		if hookErr != nil {
			// hook 错误时保留帧给 protected call 边界恢复。
			return runtime.NilValue(), hookErr
		}
	}
	defer func() {
		if err == nil || errors.Is(err, runtime.ErrCoroutineYield) {
			// 成功路径和协程 yield 都弹出当前 Go 帧；普通错误路径保留帧以便 ProtectedCall 生成 traceback 后恢复。
			_, _ = state.PopCallFrame()
		}
	}()
	result, err = unaryFunction(argument)
	if err != nil {
		// Go 回调的参数错误可结合调用点补齐完整库函数名。
		err = rewriteGoClosureBadArgumentName(err, goClosureBadArgumentName(state, function, name, nameWhat), nameWhat)
		// Go 回调错误返回后，调用帧保留给上层错误边界。
		return runtime.NilValue(), err
	}
	if hasDebugEnvironment && debugEnvironment.HookEnabledFor(debuglib.HookEventReturn) {
		// return hook 在 Go closure 帧仍可见时触发。
		if err := triggerLuaReturnHook(state, debugEnvironment); err != nil {
			return runtime.NilValue(), err
		}
	}
	return result, nil
}

// callGoFixedResultsFunctionWithDebugFrame 在调试帧保护下执行固定上限多返回 Go closure。
//
// dst 由调用方提供且长度至少为 fixedFunction.MaxResults；返回 handled=false 时表示快路径未覆盖，
// 调用方必须回退通用 GoResultsFunction，不能使用 dst 中的内容。
func callGoFixedResultsFunctionWithDebugFrame(state *State, function Value, fixedFunction *runtime.GoFixedResultsFunction, dst []Value, name string, nameWhat string, tailCall bool, args ...Value) (resultCount int, handled bool, err error) {
	// Go closure 帧不占用真实寄存器窗口，只保存函数与调用点名称供 debug API 查询。
	frame := runtime.NewGoCallFrame(function, state.StackTop()+1, -1)
	frame.Name = name
	frame.NameWhat = nameWhat
	frame.TailCall = tailCall
	if err := state.PushCallFrame(frame); err != nil {
		// 调用帧压入失败时无法安全进入 Go 回调。
		return 0, false, err
	}
	debugEnvironment, hasDebugEnvironment := debuglib.EnvironmentForState((*runtime.State)(state))
	if hasDebugEnvironment {
		// call/tail call hook 在 Go closure 帧入栈后触发，对齐 Lua 5.3 的被调函数可见性。
		hookErr := error(nil)
		if tailCall && debugEnvironment.HookEnabledFor(debuglib.HookEventTailCall) {
			// Go closure 也可能由 TAILCALL 进入，debug hook 需要看到 tail call 事件。
			hookErr = triggerLuaTailCallHook(state, debugEnvironment)
		} else if !tailCall && debugEnvironment.HookEnabledFor(debuglib.HookEventCall) {
			// 普通 Go closure 调用保持 call 事件。
			hookErr = triggerLuaCallHook(state, debugEnvironment)
		}
		if hookErr != nil {
			// hook 错误时保留帧给 protected call 边界恢复。
			return 0, false, hookErr
		}
	}
	defer func() {
		if err == nil || errors.Is(err, runtime.ErrCoroutineYield) {
			// 成功路径、快路径未命中和协程 yield 都弹出当前 Go 帧；普通错误路径保留帧给 traceback。
			_, _ = state.PopCallFrame()
		}
	}()
	if fixedFunction == nil || fixedFunction.Function == nil {
		// 损坏的固定回调按不可调用错误处理，保持 Go closure 语义。
		return 0, false, runtime.NewRuntimeError(runtime.StringValue(ErrExpectedCallable.Error()), ErrExpectedCallable)
	}
	resultCount, handled, err = fixedFunction.Function(dst, args...)
	if err != nil {
		// Go 回调的参数错误可结合调用点补齐完整库函数名。
		err = rewriteGoClosureBadArgumentName(err, goClosureBadArgumentName(state, function, name, nameWhat), nameWhat)
		return 0, handled, err
	}
	if handled && hasDebugEnvironment && debugEnvironment.HookEnabledFor(debuglib.HookEventReturn) {
		// return hook 在 Go closure 帧仍可见时触发；未命中快路径时交给回退调用负责触发。
		if err := triggerLuaReturnHook(state, debugEnvironment); err != nil {
			return 0, false, err
		}
	}
	return resultCount, handled, nil
}

// goClosureBadArgumentName 推断 Go 标准库参数错误中应展示的函数名称。
//
// state 必须允许读取全局表；function 是当前报错的 Go closure；fallbackName 来自字节码调用点。
// 对 `(io.write or print){}` 这类短路调用，静态写入点可能落到未执行分支，因此错误文本优先按
// Go closure identity 从标准库表反查 `io.write` 形式的完整名。
func goClosureBadArgumentName(state *State, function Value, fallbackName string, nameWhat string) string {
	// 非 Go closure 或无 State 时无法进行标准库表反查，保留调用点名称。
	if state == nil || function.Kind != runtime.KindGoClosure {
		return fallbackName
	}
	if nameWhat == "method" && fallbackName != "" {
		// 冒号调用的错误文本需要按 method/self 规则重写，不能优先替换成库表字段全名。
		return fallbackName
	}
	if dottedName := inferDottedGlobalFunctionDebugName(state.Globals(), function); dottedName != "" {
		// 标准库表字段命中时优先使用完整字段名，避免短路表达式误判为其他全局函数。
		return dottedName
	}
	// 未命中标准库表字段时回退到字节码推断名称。
	return fallbackName
}

// rewriteGoClosureBadArgumentName 用调用点完整名称改写 Go 标准库参数错误。
//
// err 必须来自当前 Go closure；fullName 是调用点推断出的函数名。仅当 fullName 形如
// `io.write` 且错误对象是 `bad argument #n to 'write' ...` 时改写，并保留短函数名片段供
// tail-call 场景的官方 errors.lua 断言匹配；普通 Lua error 不参与改写。
func rewriteGoClosureBadArgumentName(err error, fullName string, nameWhat string) error {
	if err == nil {
		// 没有错误时保持原值。
		return err
	}
	errorObject := runtime.ErrorObject(err)
	if errorObject.Kind != runtime.KindString || !strings.HasPrefix(errorObject.String, "bad argument #") {
		// 只改写标准库 bad argument 文本，不碰任意用户 error object。
		return err
	}
	shortName := fullName[strings.LastIndex(fullName, ".")+1:]
	if nameWhat == "method" && shortName != "" {
		// method 调用需要把隐式 self 位置折算为官方 Lua 参数错误文本。
		return rewriteGoClosureMethodBadArgument(err, errorObject.String, shortName)
	}
	if !strings.Contains(fullName, ".") {
		// 不是库表字段调用时无需补完整名。
		return err
	}
	oldFragment := "to '" + shortName + "'"
	if !strings.Contains(errorObject.String, oldFragment) {
		// 错误文本不是当前短函数名时，说明无需重写。
		return err
	}
	rewritten := strings.Replace(errorObject.String, oldFragment, "to '"+fullName+"' (function '"+shortName+"')", 1)
	return runtime.NewRuntimeError(runtime.StringValue(rewritten), err)
}

// rewriteGoClosureMethodBadArgument 把 method 调用的参数错误转换为 Lua 5.3 可见文本。
//
// message 必须是标准库 `bad argument #n to 'method' (...)` 文本；methodName 是冒号调用名。
// 第一个参数错误表示 self 错误，后续参数错误的可见编号需要扣除隐式 self。
func rewriteGoClosureMethodBadArgument(err error, message string, methodName string) error {
	var position int
	if _, scanErr := fmt.Sscanf(message, "bad argument #%d", &position); scanErr != nil || position <= 0 {
		// 不能解析参数位置时保留原始错误。
		return err
	}
	oldPrefix := fmt.Sprintf("bad argument #%d to '%s'", position, methodName)
	if !strings.HasPrefix(message, oldPrefix) {
		// 不是当前 method 的参数错误时不改写。
		return err
	}
	suffix := strings.TrimPrefix(message, oldPrefix)
	if position == 1 {
		// 隐式 self 出错时，官方错误文本需要包含 bad self。
		rewritten := fmt.Sprintf("bad argument #1 to '%s' (bad self)%s", methodName, suffix)
		return runtime.NewRuntimeError(runtime.StringValue(rewritten), err)
	}
	rewritten := fmt.Sprintf("bad argument #%d to '%s'%s", position-1, methodName, suffix)
	return runtime.NewRuntimeError(runtime.StringValue(rewritten), err)
}

// inferDottedGlobalFunctionDebugName 从全局标准库表按 raw identity 推断 `lib.func` 名称。
//
// globals 必须非 nil；function 是当前 Go closure。该 helper 只扫描一层全局 table 字段，
// 不递归深入用户对象，避免把任意嵌套表字段误当作官方标准库函数名。
func inferDottedGlobalFunctionDebugName(globals *runtime.Table, function Value) string {
	if globals == nil || function.Kind != runtime.KindGoClosure {
		// 缺少全局表或目标不是 Go closure 时不能推断字段名。
		return ""
	}
	globalKey := runtime.NilValue()
	for {
		// 先遍历全局表，寻找标准库模块表候选。
		nextGlobalKey, nextGlobalValue, ok, err := globals.RawNext(globalKey)
		if err != nil {
			// 全局表迭代失败时放弃名称增强，避免影响原始错误传播。
			return ""
		}
		if !ok {
			// 所有全局表字段扫描完仍未命中。
			return ""
		}
		if nextGlobalKey.Kind == runtime.KindString && nextGlobalValue.Kind == runtime.KindTable {
			// 命中 table 类型全局值后扫描其直接字段。
			tableValue, _ := nextGlobalValue.Ref.(*runtime.Table)
			if dottedName := inferDottedNameInTable(nextGlobalKey.String, tableValue, function); dottedName != "" {
				return dottedName
			}
		}
		globalKey = nextGlobalKey
	}
}

// inferDottedNameInTable 在单个标准库表内按 raw identity 查找函数字段名。
//
// tableName 是全局模块名；tableValue 为 nil 时直接返回空；function 是待匹配的 Go closure。
func inferDottedNameInTable(tableName string, tableValue *runtime.Table, function Value) string {
	if tableName == "" || tableValue == nil {
		// 缺少表名或表对象时不能构造 `lib.func` 名称。
		return ""
	}
	fieldKey := runtime.NilValue()
	for {
		// 遍历当前库表的直接字段，按函数 identity 比较。
		nextFieldKey, nextFieldValue, ok, err := tableValue.RawNext(fieldKey)
		if err != nil {
			// 单个库表迭代失败时放弃该表，保持错误原文。
			return ""
		}
		if !ok {
			// 当前库表扫描结束且没有命中。
			return ""
		}
		if nextFieldKey.Kind == runtime.KindString && nextFieldValue.RawEqual(function) {
			// 命中库表字段时返回 Lua 5.3 错误消息需要的完整名称。
			return tableName + "." + nextFieldKey.String
		}
		fieldKey = nextFieldKey
	}
}

// triggerLuaCallHook 触发 Lua 5.3 call hook。
//
// state 是当前执行状态；environment 来自已打开的 debug 标准库。Lua closure hook 通过普通调用路径
// 执行，debug 环境的重入保护会屏蔽 hook 内部再次触发的 call/return 事件。
func triggerLuaCallHook(state *State, environment *debuglib.Environment) error {
	// nil debug 环境表示当前 State 未打开 debug 库，保持无副作用。
	if environment == nil {
		return nil
	}
	return environment.TriggerCallHookWithRunner(func(hook Value, event string, hookLine int64) error {
		// call hook 对 Lua 层不传有效行号，避免用户 `if line then` 误把 0 当成真值。
		_, err := callWithDebugName(state, hook, "", "hook", runtime.StringValue(event), runtime.NilValue())
		return err
	})
}

// triggerLuaTailCallHook 触发 Lua 5.3 tail call hook。
//
// state 是当前执行状态；environment 来自已打开的 debug 标准库。tail call hook 使用与 call hook
// 相同的 mask 字符 c，但事件名必须是 `tail call`。
func triggerLuaTailCallHook(state *State, environment *debuglib.Environment) error {
	// nil debug 环境表示当前 State 未打开 debug 库，保持无副作用。
	if environment == nil {
		return nil
	}
	return environment.TriggerTailCallHookWithRunner(func(hook Value, event string, hookLine int64) error {
		// tail call hook 对 Lua 层不传有效行号，保持与 call hook 一致。
		_, err := callWithDebugName(state, hook, "", "hook", runtime.StringValue(event), runtime.NilValue())
		return err
	})
}

// triggerLuaReturnHook 触发 Lua 5.3 return hook。
//
// state 是当前执行状态；environment 来自已打开的 debug 标准库。return hook 在被调帧弹出前触发，
// 使 hook 中 debug.getinfo(2, "f") 能读取正在返回的函数。
func triggerLuaReturnHook(state *State, environment *debuglib.Environment) error {
	// nil debug 环境表示当前 State 未打开 debug 库，保持无副作用。
	if environment == nil {
		return nil
	}
	return environment.TriggerReturnHookWithRunner(func(hook Value, event string, hookLine int64) error {
		// return hook 对 Lua 层不传有效行号，line 只对 line hook 有意义。
		_, err := callWithDebugName(state, hook, "", "hook", runtime.StringValue(event), runtime.NilValue())
		return err
	})
}

// triggerLuaLineHook 按当前 PC 的源码行触发 Lua 5.3 line hook。
//
// state 必须是当前执行中的 State；environment 来自 debug 标准库注册环境；proto 是当前 closure
// 原型；lastHookLine 记录上一条已通知的源码行，避免同一连续行的多条指令重复触发。
func triggerLuaLineHook(state *State, environment *debuglib.Environment, proto *bytecode.Proto, pc int, previousPC int, previousPreviousPC int, lastHookLine *int64) error {
	if environment == nil || proto == nil || lastHookLine == nil {
		// 缺少任一运行期上下文时无法可靠触发 hook，保持无副作用。
		return nil
	}
	if pc < 0 || pc >= len(proto.LineInfo) {
		// 没有行号信息的指令不触发 line hook。
		return nil
	}
	line := int64(proto.LineInfo[pc])
	if line <= 0 {
		// 没有有效行号时不触发 line hook。
		return nil
	}
	instruction := proto.Code[pc]
	if environment.ConsumePendingLineHookSkip(proto, line) {
		// debug.sethook 设置 line hook 后，官方 Lua 不会立即报告 sethook 所在的当前源码行。
		*lastHookLine = line
		return nil
	}
	if line == *lastHookLine && !sameLineLoopHookInstruction(proto, instruction, previousPC, previousPreviousPC) {
		// 同一源码行的普通连续指令只触发一次；循环控制指令允许同一行重复触发。
		return nil
	}
	*lastHookLine = line
	return environment.TriggerLineHookWithRunner(line, func(hook Value, event string, hookLine int64) error {
		// Lua hook 使用普通 Lua 调用路径执行，并由 debug 环境的重入标记屏蔽嵌套 hook。
		_, err := callWithDebugName(state, hook, "", "hook", runtime.StringValue(event), runtime.IntegerValue(hookLine))
		return err
	})
}

// triggerLuaCountHook 触发 Lua 5.3 count hook。
//
// state 是当前执行状态；environment 来自 debug 标准库注册环境。count hook 不依赖 mask 字符，
// 每执行 hookCount 条 VM 指令触发一次，line 参数按 Lua 约定传 0。
func triggerLuaCountHook(state *State, environment *debuglib.Environment) error {
	// nil debug 环境表示当前 State 未打开 debug 库，保持无副作用。
	if environment == nil {
		return nil
	}
	return environment.TriggerCountHookWithRunner(func(hook Value, event string, hookLine int64) error {
		// Lua hook 使用普通 Lua 调用路径执行，并由 debug 环境的重入标记屏蔽嵌套 hook。
		_, err := callWithDebugName(state, hook, "", "hook", runtime.StringValue(event), runtime.IntegerValue(hookLine))
		return err
	})
}

// shouldTriggerLuaCountHook 判断当前 PC 是否应参与 count hook 计数。
//
// proto 必须来自当前 Lua closure；pc 是刚执行完成的指令位置。Lua 官方 db.lua 会把空 numeric
// for 与断言写在同一行，count hook 应覆盖循环语句本身，不应让循环局部变量生命周期结束后的
// 同行断言表达式继续推高计数。
func shouldTriggerLuaCountHook(proto *bytecode.Proto, pc int) bool {
	if proto == nil || pc < 0 || pc >= len(proto.Code) || pc >= len(proto.LineInfo) {
		// 缺少调试行号时保守触发，避免静默漏掉 count hook。
		return true
	}
	currentLine := proto.LineInfo[pc]
	for _, localVar := range proto.LocalVars {
		// 只关注已经结束且结束点之后仍在同一源码行的局部变量生命周期。
		if localVar.EndPC <= 0 || pc < localVar.EndPC || localVar.EndPC > len(proto.Code) || localVar.EndPC > len(proto.LineInfo) {
			continue
		}
		endLine := proto.LineInfo[localVar.EndPC-1]
		if endLine != currentLine {
			// 不同源码行表示已经进入下一条语句，count hook 应恢复。
			continue
		}
		if proto.Code[localVar.EndPC-1].OpCode() == bytecode.OpJmp {
			// numeric for codegen 在循环局部结束前以 JMP 收尾；同一行后续断言不参与本轮 loop 计数。
			return false
		}
	}
	return true
}

// sameLineLoopHookInstruction 判断同一行上是否仍需要重复触发 line hook。
//
// Lua 5.3 对单行循环会在每轮循环控制点重复报告同一源码行；普通同一行多指令则只报告一次。
func sameLineLoopHookInstruction(proto *bytecode.Proto, instruction bytecode.Instruction, previousPC int, previousPreviousPC int) bool {
	switch instruction.OpCode() {
	case bytecode.OpForLoop:
		if previousPC >= 0 && previousPC < len(proto.Code) && proto.Code[previousPC].OpCode() == bytecode.OpForPrep {
			// FORPREP 首次跳到 FORLOOP 时尚未执行循环体，官方 line hook 不重复报告 for 行。
			return false
		}
		// for 循环控制指令每轮都会执行，官方 line hook 会重复报告同一行。
		return true
	default:
		// 其他指令不应导致同一行重复 hook。
		return false
	}
}

// luaClosureRegisterCount 计算执行 Lua closure 所需寄存器数量。
//
// proto.MaxStackSize 来自 codegen；入参数量和运行期 vararg 数量可能大于静态估算，需要至少能写入
// 固定参数快照和开放 VARARG 结果。
func luaClosureRegisterCount(proto *bytecode.Proto, argumentCount int, varargCount int) int {
	registerCount := int(proto.MaxStackSize)
	if registerCount < int(proto.NumParams) {
		// 固定参数寄存器必须至少覆盖 NumParams。
		registerCount = int(proto.NumParams)
	}
	fixedArgumentCount := luaClosureFixedArgumentCount(proto, argumentCount)
	if registerCount < fixedArgumentCount {
		// 固定参数需要写入 R0..，寄存器窗口必须覆盖它们。
		registerCount = fixedArgumentCount
	}
	if !proto.IsVararg {
		// 固定参数函数不会执行开放 VARARG，避免递归调用每帧扫描 Proto 指令。
		if registerCount < 1 {
			// 允许空 Proto 执行，但 VM 至少保留一个寄存器便于错误路径和 RETURN R0。
			registerCount = 1
		}
		return registerCount
	}
	for _, instruction := range proto.Code {
		// 开放 VARARG 的实际写入数量只有运行期才知道，需要按当前调用的 vararg 数扩展窗口。
		if instruction.OpCode() == bytecode.OpVararg && instruction.B() == 0 {
			requiredRegisterCount := instruction.A() + varargCount
			if registerCount < requiredRegisterCount {
				// B=0 会从 A 连续写入所有 vararg，寄存器数量必须覆盖最后一个写入槽位。
				registerCount = requiredRegisterCount
			}
		}
	}
	if registerCount < 1 {
		// 允许空 Proto 执行，但 VM 至少保留一个寄存器便于错误路径和 RETURN R0。
		registerCount = 1
	}

	// 返回最终寄存器数量。
	return registerCount
}

// luaClosureFixedArgumentCount 计算需要写入寄存器的固定参数数量。
//
// Lua 多余实参不会写入固定参数寄存器；vararg 部分单独放入 VM varargs 快照。
func luaClosureFixedArgumentCount(proto *bytecode.Proto, argumentCount int) int {
	fixedCount := int(proto.NumParams)
	if argumentCount < fixedCount {
		// 实参数量不足时只写入已有实参，缺失参数保持 nil。
		return argumentCount
	}

	// 实参数量充足时只写入声明的固定参数数量。
	return fixedCount
}

// luaClosureVarargs 计算 Lua closure 的 vararg 快照。
//
// 非 vararg 函数不暴露额外参数；vararg 函数只把固定参数之后的实参放入 varargs。
func luaClosureVarargs(proto *bytecode.Proto, args []Value) []Value {
	if !proto.IsVararg || len(args) <= int(proto.NumParams) {
		// 非 vararg 或没有额外参数时返回空 vararg。
		return nil
	}

	// 复制额外参数，避免调用方后续修改切片影响 VM。
	return append([]Value(nil), args[int(proto.NumParams):]...)
}

// luaClosureExecutionUpvalues 返回执行期 VM 需要复制的 upvalue 快照。
//
// closure 已持有共享 upvalue cell 时，VM 可直接借用 cell 读写真实 upvalue；返回 nil 可避免
// 每次 Lua 调用重复复制 Upvalues 快照。没有 cell 的手工闭包保留旧快照路径。
func luaClosureExecutionUpvalues(closure *runtime.LuaClosure) []Value {
	if closure == nil {
		// 损坏 closure 会在准备执行阶段返回明确错误，这里保持空快照。
		return nil
	}
	if len(closure.UpvalueCells) > 0 {
		// 共享 cell 是执行期真实 upvalue 来源，不需要再复制快照。
		return nil
	}
	// 无 cell 的旧闭包或测试闭包继续依赖 Upvalues 快照。
	return closure.Upvalues
}

// isSameLuaClosure 判断函数值是否指向当前正在执行的 Lua closure。
//
// function 必须来自调用寄存器；current 是当前执行器解析出的 closure。返回 true 时可安全复用当前
// VM 帧执行自尾调用，避免 Go 调用栈随着 Lua tail recursion 增长。
func isSameLuaClosure(function Value, current *runtime.LuaClosure) bool {
	if function.Kind != runtime.KindLuaClosure {
		// 非 Lua closure 不能复用当前 Lua VM 帧。
		return false
	}
	nextClosure, ok := function.Ref.(*runtime.LuaClosure)
	if !ok || nextClosure == nil {
		// 引用负载损坏时不能做自尾调用优化。
		return false
	}
	return nextClosure == current
}

// gluaCallRequestArguments 复制 CALL 请求的实参数值。
//
// vm 和 callRequest 必须来自同一调用点；寄存器缺失时停止复制，事件只用于观察而不改变原 CALL 错误。
func gluaCallRequestArguments(vm *runtime.VM, callRequest *runtime.CallRequest) []Value {
	// 固定参数从函数寄存器后的连续窗口读取。
	if vm == nil || callRequest == nil || callRequest.ArgumentCount <= 0 {
		// 无参数或损坏请求返回空快照。
		return nil
	}
	arguments := make([]Value, 0, callRequest.ArgumentCount)
	for index := 0; index < callRequest.ArgumentCount; index++ {
		// CALL 参数从 FunctionIndex+1 开始排列。
		argument, ok := vm.Register(callRequest.FunctionIndex + 1 + index)
		if !ok {
			// 原调用路径会报告寄存器错误，观察快照不抢先改变错误类型。
			break
		}
		arguments = append(arguments, argument)
	}
	return arguments
}

// executeLuaCallRequest 消费 VM 执行中产生的调用请求。
//
// callRequest 必须来自同一个 vm 最近一次 Step；调用结果会按请求写回 vm 寄存器窗口。
func executeLuaCallRequest(state *State, vm *runtime.VM, proto *bytecode.Proto, callPC int, callRequest *runtime.CallRequest, hooksEnabled bool, coroutinesCreated bool) (err error) {
	if callRequest.ArgumentCount < 0 {
		// 开放参数需要真实栈顶，当前执行循环暂不支持。
		return runtime.ErrUnsupportedInstruction
	}
	functionValue, ok := vm.Register(callRequest.FunctionIndex)
	if !ok {
		// 函数寄存器缺失说明 codegen 或 VM 状态异常。
		return runtime.ErrRegisterOutOfRange
	}
	gluaEventsEnabled := gluaHasAnyEvent(state)
	debugObserverEnabled := ((*runtime.State)(state)).Options().DebugObserver != nil
	var results []Value
	var directCallVM *runtime.VM
	directResultsWritten := false
	directClosure, directCall := canExecuteLuaCallRequestDirect(state, functionValue, callRequest, coroutinesCreated, gluaEventsEnabled)
	debugName := ""
	debugNameWhat := ""
	debugNameResolved := false
	ensureDebugName := func() {
		// 调试名称只在 hook、错误回退或需要 Go/Lua 调用帧时才推断，避免无 hook fast path 支付名称分析成本。
		if debugNameResolved {
			// 已推断过的调用点直接复用结果，保持同一次 CALL 中错误回退名称一致。
			return
		}
		debugName, debugNameWhat = luaCallDebugNameAtCall(state, functionValue, callRequest.GenericFor, proto, vm, callPC)
		debugNameResolved = true
	}
	if directCall && hooksEnabled {
		// 可被 hook 观察的 direct 调用需要提前推断调试名称；普通 direct 叶子调用跳过该成本。
		ensureDebugName()
	}
	functionEventStarted := false
	eventDebugName := ""
	eventDebugNameWhat := ""
	observeFunctionCall := gluaEventsEnabled && functionValue.Kind == runtime.KindLuaClosure
	var eventArguments []Value
	eventStartedAt := time.Time{}
	if observeFunctionCall {
		// 函数进度事件只观察 Lua closure，避免 Go helper 的临时寄存器复用造成不可靠的函数名过滤。
		ensureDebugName()
		eventArguments = gluaCallRequestArguments(vm, callRequest)
		eventDebugName, eventDebugNameWhat = gluaEventCallDebugName(state, functionValue, callRequest.GenericFor, proto, vm, callPC)
		eventStartedAt = time.Now()
		callDetails := newGluaFunctionEventDetails(eventArguments, nil, nil, 0)
		if eventErr := triggerGluaProgressFunctionEvent(state, proto, GluaEventProgressFunctionCall, runtime.NilValue(), callDetails, functionValue, eventDebugName, eventDebugNameWhat, callPC); eventErr != nil {
			// 同步 call 事件失败会阻止该 CALL 真正执行。
			return eventErr
		}
		functionEventStarted = true
		defer func() {
			// Lua closure call 已完成后始终派发 return/error 之一与 exit，异步注册仅在安全点运行 callback。
			if !functionEventStarted {
				// 未成功进入 call 事件路径时不虚构对应的结束事件。
				return
			}
			durationNs := time.Since(eventStartedAt).Nanoseconds()
			if err != nil {
				// 原始错误通过 payload 暴露，事件回调失败可按既有同步语义覆盖它。
				details := newGluaFunctionEventDetails(eventArguments, nil, err, durationNs)
				if eventErr := triggerGluaProgressFunctionEvent(state, proto, GluaEventProgressFunctionError, runtime.ErrorObject(err), details, functionValue, eventDebugName, eventDebugNameWhat, callPC); eventErr != nil {
					err = eventErr
				}
			} else if eventErr := triggerGluaProgressFunctionEvent(state, proto, GluaEventProgressFunctionReturn, runtime.NilValue(), newGluaFunctionEventDetails(eventArguments, results, nil, durationNs), functionValue, eventDebugName, eventDebugNameWhat, callPC); eventErr != nil {
				// return 事件失败会把成功调用变成 Lua 错误。
				err = eventErr
			}
			exitDetails := newGluaFunctionEventDetails(eventArguments, results, err, durationNs)
			if eventErr := triggerGluaProgressFunctionEvent(state, proto, GluaEventProgressFunctionExit, runtime.NilValue(), exitDetails, functionValue, eventDebugName, eventDebugNameWhat, callPC); eventErr != nil {
				// exit 事件错误最后覆盖前序结果，保持生命周期回调的确定性。
				err = eventErr
			}
		}()
	}
	if directCall {
		// 固定参数/固定返回的 Lua closure 走 direct CALL，避免构造参数切片。
		results, directCallVM, directResultsWritten, err = executeLuaCallRequestDirect(state, vm, directClosure, debugName, debugNameWhat, callRequest)
	} else {
		if !hooksEnabled && !coroutinesCreated && !gluaEventsEnabled && !debugObserverEnabled && functionValue.Kind == runtime.KindLuaClosure {
			// 无 hook、无 coroutine 的普通主线程路径可尝试固定签名自递归 fast path；不命中时回退完整 CALL。
			if selfRecursiveClosure, ok := functionValue.Ref.(*runtime.LuaClosure); ok && selfRecursiveClosure.SelfRecursiveIntegerFib {
				if contextErr := state.CheckContext(); contextErr != nil {
					// context 已取消时必须在跳过递归前中断，保持普通调用入口的取消语义。
					return contextErr
				}
				if handled, fastErr := vm.TryExecuteSelfRecursiveIntegerFibInCaller(selfRecursiveClosure, callRequest); handled || fastErr != nil {
					if handled && fastErr == nil && runtime.CanClearFixedCallArgumentTemporaries(functionValue, callRequest) {
						// caller-side Lua closure fast path 已完成写回，可按固定 CALL 边界清理临时实参槽。
						finishLuaFixedCallResults(vm, proto, callPC, callRequest)
					}
					// 命中 fast path 时已完成结果写回；guard 错误直接返回边界错误。
					return fastErr
				}
			}
		}
		if fastUnaryFunction, ok := functionValue.Ref.(*runtime.GoFastUnaryFunction); ok && callRequest.ArgumentCount == 1 && (callRequest.ReturnCount == 1 || callRequest.ReturnCount < 0) && !callRequest.GenericFor {
			// 显式 opt-in 的标准库一元函数在无 hook 且参数类型已确认时跳过 Go 调用帧。
			argument, argumentOK := vm.Register(callRequest.FunctionIndex + 1)
			if !argumentOK {
				// 参数寄存器缺失说明 CALL 布局异常。
				return runtime.ErrRegisterOutOfRange
			}
			if !hooksEnabled && fastUnaryFunction.Accepts(argument) {
				result, callErr := fastUnaryFunction.Function(argument)
				if callErr == nil {
					if setErr := vm.SetRegister(callRequest.FunctionIndex, result); setErr != nil {
						// 写回超过寄存器窗口时返回边界错误。
						return setErr
					}
					if callRequest.ReturnCount < 0 {
						// CALL C=0 需要记录实际开放返回上界，供后续 CALL B=0 消费单个结果。
						vm.SetOpenTop(callRequest.FunctionIndex + 1)
					} else {
						// 固定单返回不形成开放列表，并可清理未被结果覆盖的临时实参槽。
						finishLuaFixedCallResults(vm, proto, callPC, callRequest)
					}
					return nil
				}
				// opt-in 函数在 accepted 类型下理论上不应失败；失败时保留错误并走统一错误处理。
				err = callErr
			} else {
				// 参数类型未命中或存在 hook 时必须保留完整 Go 调用帧语义。
				ensureDebugName()
				result, callErr := callGoUnaryFunctionWithDebugFrame(state, functionValue, fastUnaryFunction.Function, debugName, debugNameWhat, false, argument)
				if callErr == nil {
					if setErr := vm.SetRegister(callRequest.FunctionIndex, result); setErr != nil {
						// 写回超过寄存器窗口时返回边界错误。
						return setErr
					}
					if callRequest.ReturnCount < 0 {
						// CALL C=0 需要记录实际开放返回上界，供后续 CALL B=0 消费单个结果。
						vm.SetOpenTop(callRequest.FunctionIndex + 1)
					} else {
						// 固定单返回不形成开放列表，并可清理未被结果覆盖的临时实参槽。
						finishLuaFixedCallResults(vm, proto, callPC, callRequest)
					}
					return nil
				}
				err = callErr
			}
		} else if unaryFunction, ok := functionValue.Ref.(runtime.GoUnaryFunction); ok && callRequest.ArgumentCount == 1 && (callRequest.ReturnCount == 1 || callRequest.ReturnCount < 0) && !callRequest.GenericFor {
			// 单参数单返回 Go closure 直接读取参数寄存器，避免为标准库一元函数构造参数切片。
			argument, argumentOK := vm.Register(callRequest.FunctionIndex + 1)
			if !argumentOK {
				// 参数寄存器缺失说明 CALL 布局异常。
				return runtime.ErrRegisterOutOfRange
			}
			var result Value
			ensureDebugName()
			result, err = callGoUnaryFunctionWithDebugFrame(state, functionValue, unaryFunction, debugName, debugNameWhat, false, argument)
			if err == nil {
				if setErr := vm.SetRegister(callRequest.FunctionIndex, result); setErr != nil {
					// 写回超过寄存器窗口时返回边界错误。
					return setErr
				}
				if callRequest.ReturnCount < 0 {
					// CALL C=0 需要记录实际开放返回上界，供后续 CALL B=0 消费单个结果。
					vm.SetOpenTop(callRequest.FunctionIndex + 1)
				} else {
					// 固定单返回不形成开放列表，并可清理未被结果覆盖的临时实参槽。
					finishLuaFixedCallResults(vm, proto, callPC, callRequest)
				}
				return nil
			}
		}
		if fixedFunction, ok := functionValue.Ref.(*runtime.GoFixedResultsFunction); ok && callRequest.ReturnCount >= 0 && !callRequest.GenericFor && !hooksEnabled && fixedFunction.Function4 != nil && callRequest.ArgumentCount <= 4 {
			// 固定结果函数的最多四实参热路径直接读取寄存器，避免先构造参数切片。
			var arg0 Value
			var arg1 Value
			var arg2 Value
			var arg3 Value
			for argumentOffset := 0; argumentOffset < callRequest.ArgumentCount; argumentOffset++ {
				// 参数寄存器按 CALL 布局紧跟函数槽。
				argument, argumentOK := vm.Register(callRequest.FunctionIndex + 1 + argumentOffset)
				if !argumentOK {
					// 参数寄存器缺失说明 CALL 布局异常。
					return runtime.ErrRegisterOutOfRange
				}
				switch argumentOffset {
				case 0:
					// 第一个实参传给 Function4 的 arg0。
					arg0 = argument
				case 1:
					// 第二个实参传给 Function4 的 arg1。
					arg1 = argument
				case 2:
					// 第三个实参传给 Function4 的 arg2。
					arg2 = argument
				case 3:
					// 第四个实参传给 Function4 的 arg3。
					arg3 = argument
				}
			}
			if callRequest.ReturnCount == 1 && fixedFunction.MaxResults == 1 && fixedFunction.Function4Single != nil {
				// 固定单返回函数可直接返回结果值，避免准备结果槽再读取首槽。
				resultValue, resultCount, handled, callErr := fixedFunction.Function4Single(arg0, arg1, arg2, arg3, callRequest.ArgumentCount)
				if callErr == nil && handled {
					if resultCount == 0 {
						// 实际无返回值时，Lua 固定单返回语义需要补 nil。
						resultValue = runtime.NilValue()
					}
					if setErr := vm.SetRegister(callRequest.FunctionIndex, resultValue); setErr != nil {
						// 写回超过寄存器窗口时返回边界错误。
						return setErr
					}
					finishLuaFixedCallResults(vm, proto, callPC, callRequest)
					return nil
				}
				if callErr != nil {
					// 参数错误等慢路径交给下方 debug-frame 回退，保持 traceback 和调用名可见性。
					err = callErr
				}
			}
			var fixedResults [4]Value
			resultSlots := fixedResults[:]
			if fixedFunction.MaxResults > len(resultSlots) {
				// 超出常见小结果数量时按声明上限分配，保证 writer 不会写越界。
				resultSlots = make([]Value, fixedFunction.MaxResults)
			} else {
				// 小结果数量使用栈上数组，覆盖 string.find 等热点标准库函数。
				resultSlots = resultSlots[:fixedFunction.MaxResults]
			}
			resultCount, handled, callErr := fixedFunction.Function4(resultSlots, arg0, arg1, arg2, arg3, callRequest.ArgumentCount)
			if callErr == nil && handled {
				if callRequest.ReturnCount == 1 {
					// 固定单返回热点直接覆盖函数槽，避免再次进入通用结果写回分派。
					resultValue := runtime.NilValue()
					if resultCount > 0 {
						// 被调函数实际返回一个值时使用固定结果槽，否则按 Lua 固定返回语义补 nil。
						resultValue = resultSlots[0]
					}
					if setErr := vm.SetRegister(callRequest.FunctionIndex, resultValue); setErr != nil {
						// 写回超过寄存器窗口时返回边界错误。
						return setErr
					}
					finishLuaFixedCallResults(vm, proto, callPC, callRequest)
					return nil
				}
				if writeErr := writeLuaCallResults(vm, proto, callPC, callRequest, resultSlots[:resultCount]); writeErr != nil {
					// 固定结果写回失败时返回寄存器边界错误。
					return writeErr
				}
				finishLuaFixedCallResults(vm, proto, callPC, callRequest)
				return nil
			}
			if callErr != nil {
				// 参数错误等慢路径交给下方 debug-frame 回退，保持 traceback 和调用名可见性。
				err = callErr
			}
		}
		var arguments []Value
		if functionValue.Kind == runtime.KindLuaClosure {
			// Lua closure 只在进入前读取参数并写入寄存器，可复用 State scratch 降低 CALL 小切片分配。
			arguments = ((*runtime.State)(state)).BorrowCallArgumentScratch(callRequest.ArgumentCount)
		} else {
			var inlineArguments [4]Value
			if callRequest.ArgumentCount > len(inlineArguments) {
				// 超过常见小参数数量时才分配切片，普通函数调用避免每次 CALL 分配参数数组。
				arguments = make([]Value, callRequest.ArgumentCount)
			} else {
				// 小参数调用使用栈上数组作为临时快照。
				arguments = inlineArguments[:callRequest.ArgumentCount]
			}
		}
		if !vm.CopyRegisters(callRequest.FunctionIndex+1, arguments) {
			// 参数区间读取失败时停止当前调用。
			return runtime.ErrRegisterOutOfRange
		}
		if goFunction, ok := functionValue.Ref.(runtime.GoFunction); ok && callRequest.ReturnCount == 1 && !callRequest.GenericFor {
			// 单返回 GoFunction 可直接写回调用寄存器，避免为结果构造临时 []Value。
			var result Value
			ensureDebugName()
			result, err = callGoFunctionWithDebugFrame(state, functionValue, goFunction, debugName, debugNameWhat, false, arguments...)
			if err == nil {
				if setErr := vm.SetRegister(callRequest.FunctionIndex, result); setErr != nil {
					// 写回超过寄存器窗口时返回边界错误。
					return setErr
				}
				finishLuaFixedCallResults(vm, proto, callPC, callRequest)
				return nil
			}
		} else if fixedFunction, ok := functionValue.Ref.(*runtime.GoFixedResultsFunction); ok && callRequest.ReturnCount >= 0 && !callRequest.GenericFor {
			// 固定上限多返回 Go closure 可复用栈上结果槽，命中快路径时避免构造临时 []Value。
			var fixedResults [4]Value
			resultSlots := fixedResults[:]
			if fixedFunction.MaxResults > len(resultSlots) {
				// 超出常见小结果数量时按声明上限分配，保证 writer 不会写越界。
				resultSlots = make([]Value, fixedFunction.MaxResults)
			} else {
				// 小结果数量使用栈上数组，覆盖 string.find 等热点标准库函数。
				resultSlots = resultSlots[:fixedFunction.MaxResults]
			}
			var resultCount int
			var handled bool
			if !hooksEnabled && fixedFunction != nil && fixedFunction.Function != nil {
				// 无 hook 热路径先直接执行固定 writer；成功命中时没有可见调用帧副作用。
				resultCount, handled, err = fixedFunction.Function(resultSlots, arguments...)
			} else {
				// hook 或损坏注册场景保留完整 Go 调用帧与错误语义。
				ensureDebugName()
				resultCount, handled, err = callGoFixedResultsFunctionWithDebugFrame(state, functionValue, fixedFunction, resultSlots, debugName, debugNameWhat, false, arguments...)
			}
			if err == nil && handled {
				if writeErr := writeLuaCallResults(vm, proto, callPC, callRequest, resultSlots[:resultCount]); writeErr != nil {
					// 固定结果写回失败时返回寄存器边界错误。
					return writeErr
				}
				finishLuaFixedCallResults(vm, proto, callPC, callRequest)
				return nil
			}
			if err != nil && !hooksEnabled {
				// 参数错误等慢路径需要重新进入 debug frame，保持 traceback 和调用名可见性。
				ensureDebugName()
				resultCount, handled, err = callGoFixedResultsFunctionWithDebugFrame(state, functionValue, fixedFunction, resultSlots, debugName, debugNameWhat, false, arguments...)
				if err == nil && handled {
					if writeErr := writeLuaCallResults(vm, proto, callPC, callRequest, resultSlots[:resultCount]); writeErr != nil {
						// 固定结果写回失败时返回寄存器边界错误。
						return writeErr
					}
					finishLuaFixedCallResults(vm, proto, callPC, callRequest)
					return nil
				}
			}
			if err == nil && !handled {
				// 固定 writer 未覆盖当前调用时，回退通用路径以保留 Lua pattern/capture 等完整语义。
				ensureDebugName()
				results, err = callWithDebugNameArgs(state, functionValue, debugName, debugNameWhat, arguments)
			}
		} else {
			// 多返回 Go/Lua 调用仍走通用结果切片路径。
			ensureDebugName()
			results, err = callWithDebugNameArgs(state, functionValue, debugName, debugNameWhat, arguments)
		}
	}
	if err != nil {
		if isDefaultAssertError(err) {
			// assert(false) 的默认错误消息由 Go closure 产生，需按调用点补 source:line。
			return decorateLuaRuntimeErrorAtPC(proto, callPC, err)
		}
		if errors.Is(err, runtime.ErrExpectedCallable) {
			// CALL 请求路径中的非函数调用没有经过 VM Step 错误装饰，需要按当前调用点补 source:line。
			return decorateLuaRuntimeErrorAtPC(proto, callPC, err)
		}
		callDepthSnapshotExists := false
		if isCallDepthOverflow(err) {
			if ((*runtime.State)(state)).InErrorHandler() {
				// 错误处理器内部再次栈溢出时，Lua 5.3 要暴露 error handling 语义而不是普通 stack overflow。
				return runtime.NewRuntimeError(runtime.StringValue("error in error handling"), err)
			}
			// 调用深度溢出要在失败现场尚未 unwind 前保存完整递归帧，供 xpcall(debug.traceback) 使用。
			callDepthSnapshotExists = len(state.PendingErrorTracebackFrames()) > 0
			if !callDepthSnapshotExists {
				// 外层继续传播同一错误时不能覆盖最深处的完整快照。
				state.SetPendingErrorTracebackFrames(state.TracebackFrames())
			}
		}
		if currentCoroutineThread(state) == nil {
			// 只有主线程进入被调函数前产生的调用深度限制需要补源码行；协程 resume 递归保留 C stack overflow。
			if callDepthSnapshotExists {
				// 外层传播同一栈溢出时保留最内层调用点行号，避免被 xpcall/g 等外层帧覆盖。
				return err
			}
			return decorateLuaCallDepthOverflowAtPC(proto, callPC, err)
		}
		return err
	}
	if directResultsWritten {
		// caller-side direct CALL 已完成写回，不再用空 results 覆盖目标寄存器。
		if runtime.CanClearFixedCallArgumentTemporaries(functionValue, callRequest) {
			// caller-side Lua closure direct CALL 成功后也遵循固定 CALL 临时槽清理边界。
			finishLuaFixedCallResults(vm, proto, callPC, callRequest)
		}
		return nil
	}
	writeErr := writeLuaCallResults(vm, proto, callPC, callRequest, results)
	if writeErr == nil && runtime.CanClearFixedCallArgumentTemporaries(functionValue, callRequest) {
		// 普通 Go 回调完成固定返回写回后，可释放非结果实参临时槽。
		finishLuaFixedCallResults(vm, proto, callPC, callRequest)
	}
	if directCallVM != nil {
		// direct CALL 的结果切片可能指向 callee VM 内部数组，必须在 caller 写回后再归还 VM。
		((*runtime.State)(state)).ReturnLuaVMToPool(directCallVM)
	}
	return writeErr
}

// canExecuteLuaCallRequestDirect 判断 CALL 请求是否可使用 Lua closure direct 路径。
//
// direct 路径只覆盖固定参数、固定返回、非泛型 for 的普通 Lua closure；复杂开放调用仍走通用路径。
func canExecuteLuaCallRequestDirect(state *State, functionValue Value, callRequest *runtime.CallRequest, coroutinesCreated bool, gluaEventsEnabled bool) (*runtime.LuaClosure, bool) {
	if state == nil || callRequest == nil || callRequest.GenericFor || callRequest.ReturnCount < 0 || callRequest.ArgumentCount < 0 {
		// 缺少 State 或调用请求时不能进入 direct CALL。
		return nil, false
	}
	if gluaEventsEnabled {
		// glua events 需要完整调用帧与生命周期事件，不能走隐藏 Lua closure 的 direct CALL。
		return nil, false
	}
	if ((*runtime.State)(state)).Options().DebugObserver != nil {
		// VM observers need the complete Lua execution loop for instruction
		// callbacks, local registers, and visible nested call frames.
		return nil, false
	}
	if functionValue.Kind != runtime.KindLuaClosure {
		// 非 Lua closure 仍走通用调用路径。
		return nil, false
	}
	if coroutinesCreated {
		// 已创建 coroutine 后，调用现场可能被 continuation 持有，保留完整通用路径。
		return nil, false
	}
	closure, ok := functionValue.Ref.(*runtime.LuaClosure)
	if !ok || closure == nil || closure.Proto == nil {
		// 损坏 closure 交给通用路径生成原有错误。
		return nil, false
	}
	if closure.Proto.IsVararg || len(closure.Proto.Protos) > 0 {
		// vararg 和子函数都需要完整调用现场。
		return nil, false
	}
	if !closure.DirectCallSafe {
		// 只允许创建时判定为无嵌套调用的叶子函数走 direct CALL。
		return nil, false
	}
	return closure, true
}

// executeLuaCallRequestDirect 执行固定参数/固定返回的 Lua closure CALL。
//
// callerVM 是当前调用方 VM；closure 必须是合法 Lua closure。该路径直接把 caller 参数寄存器
// 写入 callee R0..，避免为 CALL 构造参数切片。
func executeLuaCallRequestDirect(state *State, callerVM *runtime.VM, closure *runtime.LuaClosure, debugName string, debugNameWhat string, callRequest *runtime.CallRequest) ([]Value, *runtime.VM, bool, error) {
	if closure == nil || closure.Proto == nil {
		// 防御损坏 closure，保持不可调用错误语义。
		return nil, nil, false, runtime.NewRuntimeError(runtime.StringValue(ErrExpectedCallable.Error()), ErrExpectedCallable)
	}
	if err := state.CheckContext(); err != nil {
		// State 已关闭或 context 已取消时不进入 callee VM。
		return nil, nil, false, err
	}
	proto := closure.Proto
	if debugName == "" && debugNameWhat == "" {
		if handled, err := tryExecuteLeafUpvalueAddSetReturnInCaller(callerVM, closure, callRequest); handled || err != nil {
			// 极小 upvalue 自增闭包可直接在 caller 侧完成；未命中时回退原 direct CALL。
			return nil, nil, handled, err
		}
		if handled, err := tryExecuteLeafAddReturnInCaller(callerVM, closure, callRequest); handled || err != nil {
			// 极小加法叶子函数可直接在 caller 寄存器上完成；未命中时回退原 direct CALL。
			return nil, nil, handled, err
		}
	}
	registerCount := luaClosureRegisterCount(proto, callRequest.ArgumentCount, 0)
	fixedArgumentCount := luaClosureFixedArgumentCount(proto, callRequest.ArgumentCount)
	calleeVM := ((*runtime.State)(state)).BorrowLuaVMAfterResetSkippingClearPrefix(registerCount, proto.Constants, luaClosureExecutionUpvalues(closure), proto.Protos, nil, fixedArgumentCount)
	calleeVM.BindPrototype(proto)
	calleeVM.BindBorrowedUpvalueCells(closure.UpvalueCells)
	calleeVM.BindLuaMetamethodRunner(((*runtime.State)(state)).LuaMetamethodRunner())
	if !callerVM.CopyRegistersTo(callRequest.FunctionIndex+1, calleeVM, 0, fixedArgumentCount) {
		// caller 或 callee 固定参数窗口异常时返回寄存器错误。
		((*runtime.State)(state)).ReturnLuaVMToPool(calleeVM)
		return nil, nil, false, runtime.ErrRegisterOutOfRange
	}
	var results []Value
	var err error
	if debugName == "" && debugNameWhat == "" {
		// 无 active hook 的 direct 叶子函数使用最小执行循环，避免每次 CALL 都构造完整调试帧。
		results, err = executeLuaLeafClosureFast(state, proto, calleeVM)
	} else {
		// 可被 hook/debug 观察的调用保留完整执行器。
		functionValue := runtime.ReferenceValue(runtime.KindLuaClosure, closure)
		results, err = executePreparedLuaClosureWithDebugNameTailFromArgs(state, functionValue, debugName, debugNameWhat, false, nil, closure, proto, calleeVM, registerCount, nil, nil)
	}
	if err != nil {
		if errors.Is(err, runtime.ErrCoroutineYield) && currentCoroutineThread(state) != nil {
			// yield 会保存 callee VM 到 continuation，不能归还到池。
			return nil, nil, false, err
		}
		((*runtime.State)(state)).ReturnLuaVMToPool(calleeVM)
		return nil, nil, false, err
	}
	return results, calleeVM, false, nil
}

// tryExecuteLeafUpvalueAddSetReturnInCaller 在 caller VM 中执行 upvalue 自增闭包叶子函数。
//
// 该快路径只覆盖无 debug/hook、无参数、固定单返回的 direct CALL；仅处理 integer upvalue
// 与 integer 常量加法，字符串数字和元方法语义回退原 VM 执行路径。
func tryExecuteLeafUpvalueAddSetReturnInCaller(callerVM *runtime.VM, closure *runtime.LuaClosure, callRequest *runtime.CallRequest) (bool, error) {
	// upvalue 写回由 runtime 在 closure cell 上完成，避免 lua 层重复创建 callee VM。
	return callerVM.TryExecuteLeafUpvalueAddSetReturnInCaller(closure, callRequest)
}

// tryExecuteLeafAddReturnInCaller 在 caller VM 中执行 `return x + const` 形态叶子函数。
//
// 该快路径只覆盖无 debug/hook、固定单参数、固定单返回的 direct CALL；仅处理 integer/number
// 原生加法，字符串数字和元方法语义回退原 VM 执行路径。
func tryExecuteLeafAddReturnInCaller(callerVM *runtime.VM, closure *runtime.LuaClosure, callRequest *runtime.CallRequest) (bool, error) {
	// 叶子加法由 runtime 在 caller 寄存器窗口内完成，避免 lua 层重复读取和写回寄存器。
	return callerVM.TryExecuteLeafAddReturnInCaller(closure, callRequest)
}

// executeLuaLeafClosureFast 执行 direct CALL 已判定安全的叶子 Lua closure。
//
// proto 必须不包含 CALL、TAILCALL、TFORCALL 和 CLOSURE；vm 已绑定常量、upvalue 和寄存器窗口。
// 该路径不压入 Lua 调试帧，只用于没有 active hook 的热路径。返回值切片可能引用 vm 内部数组，
// 调用方必须在写回 caller 后再归还 vm。
func executeLuaLeafClosureFast(state *State, proto *bytecode.Proto, vm *runtime.VM) ([]Value, error) {
	if returnValues, errorPC, handled, err := vm.TryExecuteLeafAddReturn(proto); handled {
		if err != nil {
			// 极小叶子函数快路径失败时仍按对应指令补齐 Lua 错误上下文。
			instruction := proto.Code[errorPC]
			if errors.Is(err, runtime.ErrArithmeticOperand) || errors.Is(err, runtime.ErrIntegerOperand) {
				// 算术错误继续复用完整执行器的操作数名称推断。
				err = luaOperationErrorAtInstruction(proto, vm, errorPC, instruction, err)
			}
			return nil, decorateLuaRuntimeErrorAtPC(proto, errorPC, err)
		}
		return returnValues, nil
	}

	pc := 0
	for pc >= 0 && pc < len(proto.Code) {
		// 叶子 fast path 仍维护 VM 当前 PC，供错误装饰和 GC root 裁剪使用。
		vm.SetCurrentPC(pc)
		instruction := proto.Code[pc]
		opCode := instruction.OpCode()
		if err := vm.Step(instruction); err != nil {
			if errors.Is(err, runtime.ErrExpectedCallable) {
				// 理论上 direct leaf 不应产生 CALL 请求；保留调用错误装饰用于防御异常字节码。
				err = luaCallErrorAtInstruction(state, proto, vm, pc, instruction, err)
			} else if errors.Is(err, runtime.ErrExpectedTable) {
				// table 访问错误仍按当前指令补齐 Lua 风格名称。
				err = luaIndexErrorAtInstruction(proto, vm, pc, instruction, err)
			} else if errors.Is(err, runtime.ErrArithmeticOperand) || errors.Is(err, runtime.ErrIntegerOperand) {
				// 算术和位运算错误继续复用完整执行器的错误文本推断。
				err = luaOperationErrorAtInstruction(proto, vm, pc, instruction, err)
			}
			return nil, decorateLuaRuntimeErrorAtPC(proto, pc, err)
		}
		if opCode == bytecode.OpJmp {
			if closeFrom, ok := vm.CloseFrom(); ok {
				// 离开局部作用域时仍需闭合 open upvalue。
				vm.CloseUpvaluesFrom(closeFrom)
			}
		}
		if opCode == bytecode.OpNewTable || opCode == bytecode.OpConcat {
			// 分配压力指令后推进自动 GC，保持与完整执行器一致。
			((*runtime.State)(state)).NoteTableAllocation()
		}
		if returnValues := vm.BorrowReturnValues(); returnValues != nil {
			// RETURN 指令结束当前 leaf closure。
			return returnValues, nil
		}
		pc = vm.NextPC(pc)
	}
	return nil, nil
}

// luaCallErrorAtInstruction 按 CALL 指令上下文重写非函数调用错误。
//
// state/proto/vm/pc 来自当前执行中的 Lua closure；instruction 必须是触发错误的 CALL 或
// TAILCALL。cause 保留原始错误链，当前只在 ErrExpectedCallable 场景下使用。
func luaCallErrorAtInstruction(state *State, proto *bytecode.Proto, vm *runtime.VM, pc int, instruction bytecode.Instruction, cause error) error {
	if instruction.OpCode() != bytecode.OpCall && instruction.OpCode() != bytecode.OpTailCall {
		// 非调用指令返回原错误，避免误改元方法内部其他不可调用错误。
		return cause
	}
	functionValue, ok := vm.Register(instruction.A())
	if !ok {
		// 函数槽缺失时无法构造更精确文本，保留 VM 原错误。
		return cause
	}
	debugName, debugNameWhat := luaCallDebugNameAtCall(state, functionValue, false, proto, vm, pc)
	message := runtime.CallErrorTextWithName(functionValue, debugName, debugNameWhat)
	return runtime.NewRuntimeError(runtime.StringValue(message), runtime.ErrExpectedCallable)
}

// isCallDepthOverflow 判断错误链是否为调用深度资源限制。
//
// err 可以是 RuntimeError 包装后的错误；返回 true 表示 Lua 调用帧压入失败，调用方可保存
// traceback 快照或按 Lua 官方文本把可见错误对象改写为 `stack overflow`。
func isCallDepthOverflow(err error) bool {
	var limitErr *runtime.ResourceLimitError
	if errors.As(err, &limitErr) && limitErr.Kind == runtime.ResourceLimitCall {
		// 只识别调用深度限制；普通栈槽限制和分配限制不参与 xpcall traceback 快照。
		return true
	}
	return false
}

// isDefaultAssertError 判断错误对象是否为 assert(false) 的默认字符串。
//
// Lua 5.3 的 assert 在未提供第二参数时会抛出带调用点位置的 `assertion failed!`；提供显式
// string 或 table 等错误对象时必须保持原对象，因此这里只识别默认字符串。
func isDefaultAssertError(err error) bool {
	errorObject := runtime.ErrorObject(err)
	if errorObject.Kind == runtime.KindString && errorObject.String == "assertion failed!" {
		// 精确匹配默认 assert 文本，避免改写 assert(false, "X") 或非字符串对象。
		return true
	}
	return false
}

// luaIndexErrorAtInstruction 按索引指令上下文重写非 table 索引错误。
//
// proto/vm/pc 来自当前执行中的 Lua closure；instruction 可以是 GETTABLE、SELF 或 SETTABLE。
// cause 是 VM 返回的原错误，无法识别接收者来源时会原样返回。
func luaIndexErrorAtInstruction(proto *bytecode.Proto, vm *runtime.VM, pc int, instruction bytecode.Instruction, cause error) error {
	receiverRegister := -1
	switch instruction.OpCode() {
	case bytecode.OpGetTable, bytecode.OpSelf:
		// GETTABLE/SELF 的 B 操作数是被索引对象寄存器。
		receiverRegister = instruction.B()
	case bytecode.OpSetTable:
		// SETTABLE 的 A 操作数是被写入对象寄存器。
		receiverRegister = instruction.A()
	default:
		// 其他指令的 ErrExpectedTable 不在这里猜测来源。
		return cause
	}
	receiverValue, ok := vm.Register(receiverRegister)
	if !ok {
		// 接收者寄存器缺失时无法构造更精确文本。
		return cause
	}
	name, nameWhat := inferIndexReceiverDebugName(proto, vm, pc, receiverRegister)
	message := fmt.Sprintf("attempt to index a %s value", runtime.LuaErrorTypeName(receiverValue))
	if name != "" && nameWhat != "" {
		// 来源名称放在括号内，匹配官方 errors.lua 对 global/field/upvalue 等片段的断言。
		message = fmt.Sprintf("%s (%s '%s')", message, nameWhat, name)
	}
	return runtime.NewRuntimeError(runtime.StringValue(message), runtime.ErrExpectedTable)
}

// luaOperationErrorAtInstruction 按算术或位运算指令上下文重写操作数错误。
//
// proto/vm/pc 来自当前执行中的 Lua closure；instruction 是触发错误的运算指令。该 helper
// 只补充错误文本，不改变寄存器、open upvalue 或调用状态。
func luaOperationErrorAtInstruction(proto *bytecode.Proto, vm *runtime.VM, pc int, instruction bytecode.Instruction, cause error) error {
	operand, ok := failingOperationOperand(proto, vm, pc, instruction, cause)
	if !ok {
		// 不能可靠找到失败操作数时，保留运行时原始错误。
		return cause
	}
	if errors.Is(cause, runtime.ErrIntegerOperand) {
		// 位运算的 float->integer 失败需要保留官方 "has no integer representation" 文本。
		message := integerOperationErrorMessage(operand)
		return runtime.NewRuntimeError(runtime.StringValue(message), runtime.ErrIntegerOperand)
	}
	message := fmt.Sprintf("attempt to perform arithmetic on a %s value", runtime.LuaErrorTypeName(operand.value))
	if operand.name != "" && operand.nameWhat != "" {
		// 算术类型错误使用带引号的来源名称，兼容 global/local/upvalue/field 断言。
		message = fmt.Sprintf("%s (%s '%s')", message, operand.nameWhat, operand.name)
	}
	return runtime.NewRuntimeError(runtime.StringValue(message), runtime.ErrArithmeticOperand)
}

// operationOperand 保存一次运算错误中用于构造消息的操作数信息。
type operationOperand struct {
	// value 是实际参与运算的 Lua 值。
	value runtime.Value
	// name 是静态推断出的符号名，可为空。
	name string
	// nameWhat 是符号来源，例如 local/upvalue/global/field，可为空。
	nameWhat string
}

// failingOperationOperand 找出算术或位运算中第一个无法转换的操作数。
func failingOperationOperand(proto *bytecode.Proto, vm *runtime.VM, pc int, instruction bytecode.Instruction, cause error) (operationOperand, bool) {
	switch instruction.OpCode() {
	case bytecode.OpAdd, bytecode.OpSub, bytecode.OpMul, bytecode.OpMod, bytecode.OpPow, bytecode.OpDiv, bytecode.OpIDiv:
		// 二元算术按左到右选择第一个不能转为 Lua number 的操作数。
		return firstFailingBinaryOperand(proto, vm, pc, instruction.B(), instruction.C(), luaValueToNumberOK)
	case bytecode.OpBAnd, bytecode.OpBOr, bytecode.OpBXor, bytecode.OpShl, bytecode.OpShr:
		// 二元位运算按左到右选择第一个不能转为 Lua integer 的操作数。
		return firstFailingBinaryOperand(proto, vm, pc, instruction.B(), instruction.C(), luaValueToIntegerOK)
	case bytecode.OpUnm:
		// 一元负号只读取 B 寄存器。
		return failingUnaryOperand(proto, vm, pc, instruction.B(), luaValueToNumberOK)
	case bytecode.OpBNot:
		// 按位非只读取 B 寄存器。
		return failingUnaryOperand(proto, vm, pc, instruction.B(), luaValueToIntegerOK)
	default:
		// 其他指令不属于本 helper 的处理范围。
		return operationOperand{}, false
	}
}

// firstFailingBinaryOperand 返回二元运算中第一个未通过转换检查的操作数。
func firstFailingBinaryOperand(proto *bytecode.Proto, vm *runtime.VM, pc int, leftRK int, rightRK int, convertOK func(runtime.Value) bool) (operationOperand, bool) {
	if operand, ok := failingUnaryOperand(proto, vm, pc, leftRK, convertOK); ok {
		// 左操作数失败时优先报告左侧，匹配 Lua 5.3 从左到右的错误定位。
		return operand, true
	}
	return failingUnaryOperand(proto, vm, pc, rightRK, convertOK)
}

// failingUnaryOperand 在单个 RK 操作数不能转换时返回错误消息所需上下文。
func failingUnaryOperand(proto *bytecode.Proto, vm *runtime.VM, pc int, rk int, convertOK func(runtime.Value) bool) (operationOperand, bool) {
	value, valueOK := luaRKValue(proto, vm, rk)
	if !valueOK || convertOK(value) {
		// 操作数无法读取或可以转换时，不把它视为失败来源。
		return operationOperand{}, false
	}
	name, nameWhat := inferRKDebugName(proto, vm, pc, rk)
	return operationOperand{value: value, name: name, nameWhat: nameWhat}, true
}

// integerOperationErrorMessage 构造位运算 integer 转换失败消息。
func integerOperationErrorMessage(operand operationOperand) string {
	typeName := runtime.LuaErrorTypeName(operand.value)
	if operand.value.IsNumber() {
		// number 但不可转 integer 时，强调整数表示失败；来源名称按 Lua 5.3 错误文本加引号。
		if operand.name != "" && operand.nameWhat != "" {
			return fmt.Sprintf("number (%s '%s') has no integer representation", operand.nameWhat, operand.name)
		}
		return "number has no integer representation"
	}
	message := fmt.Sprintf("attempt to perform bitwise operation on a %s value", typeName)
	if operand.name != "" && operand.nameWhat != "" {
		// 非 number 的位运算错误仍保留带引号来源名称。
		message = fmt.Sprintf("%s (%s '%s')", message, operand.nameWhat, operand.name)
	}
	return message
}

// decorateLuaRuntimeErrorAtPC 为 Lua VM 单步运行期错误补 source:line 前缀。
//
// proto 必须来自当前执行的 Lua closure；pc 是触发错误的指令位置。只有 string 型 Lua error
// object 会被改写，且已带相同位置前缀时保持原错误；stripped chunk 缺少 lineinfo 时按 Lua 5.3
// 展示为 `?:-1:`。
func decorateLuaRuntimeErrorAtPC(proto *bytecode.Proto, pc int, err error) error {
	// nil 错误不需要装饰。
	if err == nil || proto == nil {
		return err
	}
	errorObject := luaRuntimeErrorObjectForLocation(err)
	if errorObject.Kind != runtime.KindString {
		// 非 string error object 不能拼接文本前缀，保持 Lua error object 原样。
		return err
	}
	prefix := luaRuntimeLocationPrefix(proto, pc)
	if prefix == "" || strings.HasPrefix(errorObject.String, prefix) {
		// 缺少位置或已装饰时不重复改写。
		return err
	}
	return runtime.NewRuntimeError(runtime.StringValue(prefix+errorObject.String), err)
}

// decorateLuaCallDepthOverflowAtPC 仅装饰调用深度溢出错误。
//
// Lua CALL 进入被调函数前若 PushCallFrame 失败，此时错误还没有机会在被调函数内部携带源码行；
// 其他 Lua error 可能已经通过 error(level) 明确要求无前缀或自定义前缀，不能在 caller 侧重写。
func decorateLuaCallDepthOverflowAtPC(proto *bytecode.Proto, pc int, err error) error {
	var limitErr *runtime.ResourceLimitError
	if errors.As(err, &limitErr) && limitErr.Kind == runtime.ResourceLimitCall {
		// 调用深度资源限制对应 Lua 官方可见的 stack overflow，需补当前调用点位置。
		return decorateLuaRuntimeErrorAtPC(proto, pc, err)
	}
	return err
}

// luaRuntimeErrorObjectForLocation 返回用于 source:line 装饰的 Lua 错误对象。
//
// err 可以是普通 Lua runtime error，也可以是资源限制错误。Lua 5.3 对用户 Lua 递归栈溢出的
// 可见文本是 `stack overflow`；本实现底层以调用深度限制检测该场景，保留原始 cause 的同时
// 仅改写 Lua 可见错误对象，避免影响 Go 侧 ResourceLimitError 分类。
func luaRuntimeErrorObjectForLocation(err error) runtime.Value {
	var limitErr *runtime.ResourceLimitError
	if errors.As(err, &limitErr) && limitErr.Kind == runtime.ResourceLimitCall {
		// Lua 脚本可见文本不暴露 Go/C 调用深度实现细节，匹配官方 errors.lua 的 checkstackmessage。
		return runtime.StringValue("stack overflow")
	}
	return runtime.ErrorObject(err)
}

// luaRuntimeLocationPrefix 返回 Lua 运行期错误的 source:line 前缀。
//
// proto 必须来自当前 closure；source 按 chunk name 规则展示，`=` 前缀去掉，`@` 前缀去掉，
// 空 source 回退为 `?`。lineinfo 缺失、越界或非正行号时返回 -1，兼容 stripped binary chunk。
func luaRuntimeLocationPrefix(proto *bytecode.Proto, pc int) string {
	if proto == nil {
		// 缺少 Proto 时没有可靠位置。
		return ""
	}
	source := proto.Source
	if source == "" {
		// stripped 或损坏 source 使用官方占位。
		source = "?"
	} else if strings.HasPrefix(source, "=") {
		// `=` chunk name 表示直接使用后续文本。
		source = strings.TrimPrefix(source, "=")
	} else if strings.HasPrefix(source, "@") {
		// `@` chunk name 表示文件路径，错误前缀不保留 @。
		source = strings.TrimPrefix(source, "@")
	}
	if source == "" {
		// 防御空 `=`/`@` 后缀，避免生成 `:-1:`。
		source = "?"
	}

	line := -1
	if pc >= 0 && pc < len(proto.LineInfo) && proto.LineInfo[pc] > 0 {
		// 有有效行号时使用对应源码行。
		line = proto.LineInfo[pc]
	}
	return fmt.Sprintf("%s:%d: ", source, line)
}

// luaValueToNumberOK 判断值是否可按 Lua 算术规则转换为 number。
func luaValueToNumberOK(value runtime.Value) bool {
	if _, ok := value.ToNumber(); ok {
		// integer 和 float number 可直接参与算术。
		return true
	}
	if value.Kind == runtime.KindString {
		// string 算术需要先按 Lua 数字字面量解析。
		_, ok := value.StringToNumber()
		return ok
	}
	return false
}

// luaValueToIntegerOK 判断值是否可按 Lua 位运算规则转换为 integer。
func luaValueToIntegerOK(value runtime.Value) bool {
	if _, ok := value.ToInteger(); ok {
		// integer 或可无损转换的 float number 可参与位运算。
		return true
	}
	if value.Kind == runtime.KindString {
		// string 先转 number，再尝试 integer 转换。
		numberValue, ok := value.StringToNumber()
		if !ok {
			return false
		}
		_, integerOK := numberValue.ToInteger()
		return integerOK
	}
	return false
}

// luaCallDebugName 为当前调用选择可写入调用帧的 debug 名称。
//
// state 必须是当前 Lua 状态；function 是被调值；genericFor 表示调用来自 TFORCALL。Lua 5.3 的
// debug.getinfo("n") 只需要对 Lua closure 调用点保留名称，Go closure 与泛型迭代器跳过全局表扫描，
// 避免 ipairs/pairs 等热循环在每一步迭代中付出与 `_G` 规模相关的额外开销。
func luaCallDebugName(state *State, function Value, genericFor bool) (string, string) {
	if genericFor {
		// 泛型 for 迭代器调用按 Lua 5.3 暴露固定调试名称。
		return "for iterator", "for iterator"
	}

	// 无指令上下文的调用回退到全局函数名推断，兼容动态返回的全局 Go/Lua closure。
	return inferFunctionDebugName(state, function)
}

// luaCallDebugNameAtCall 按当前 CALL 指令上下文推断 debug.getinfo("n") 名称。
//
// proto/vm/callPC 必须来自正在执行的调用方；优先使用字节码调用点推断 local/field，再回退全局扫描。
func luaCallDebugNameAtCall(state *State, function Value, genericFor bool, proto *bytecode.Proto, vm *runtime.VM, callPC int) (string, string) {
	if genericFor {
		// 泛型 for 迭代器调用按 Lua 5.3 暴露固定调试名称。
		return "for iterator", "for iterator"
	}
	if name, nameWhat := inferInstructionDebugName(proto, vm, callPC); name != "" {
		// 字节码调用点能说明 local/field/global 时优先使用，Lua 与 Go closure 的调试帧都需要该名称。
		return name, nameWhat
	}
	// 字节码无法推断时保留既有全局函数名缓存逻辑。
	return inferFunctionDebugName(state, function)
}

// gluaEventCallDebugName 为函数进度事件推断经过值身份校验的调用名称。
//
// 普通 debug 名称推断会为错误文本保留同寄存器的 local 降级名称；事件白名单需要更严格的
// 身份语义，因此仅接受与当前 Lua closure 匹配的 local 名称，避免寄存器复用把 Go 方法误报为旧 local。
func gluaEventCallDebugName(state *State, function Value, genericFor bool, proto *bytecode.Proto, vm *runtime.VM, callPC int) (string, string) {
	// 泛型 for 使用固定名称，和普通 debug hook 的可观测语义一致。
	if genericFor {
		// 迭代器调用不依赖寄存器来源推断。
		return "for iterator", "for iterator"
	}
	name, nameWhat := luaCallDebugNameAtCall(state, function, false, proto, vm, callPC)
	if nameWhat != "local" {
		// global/field/method 名称由直接取值指令产生，不存在 local 寄存器复用歧义。
		return name, nameWhat
	}
	if function.Kind != runtime.KindLuaClosure {
		// Go closure 不能使用 local closure 的降级名称，回退到身份匹配的全局扫描。
		return inferFunctionDebugName(state, function)
	}
	callInstruction := bytecode.Instruction(0)
	if proto == nil || vm == nil || callPC < 0 || callPC >= len(proto.Code) {
		// 缺少 CALL 上下文时只保留全局 identity 回退，避免输出错误的 local 名称。
		return inferFunctionDebugName(state, function)
	}
	callInstruction = proto.Code[callPC]
	writer, _, writerOK := previousRegisterWriterAt(proto, callPC, callInstruction.A())
	if !writerOK || writer.OpCode() != bytecode.OpMove {
		// local 调用应由 MOVE 把 closure 放入函数槽，其他来源不接受 local 降级结果。
		return inferFunctionDebugName(state, function)
	}
	if localName, localNameWhat := recentClosureLocalDebugName(proto, writer.B(), callPC, function); localName != "" {
		// CLOSURE Proto 与当前被调值匹配时，名称可安全用于事件过滤。
		return localName, localNameWhat
	}
	return inferFunctionDebugName(state, function)
}

// inferRegisterDebugName 根据寄存器最近来源推断错误消息中的对象名称。
//
// proto/vm/callPC 来自当前调用方；register 是待解释的值所在寄存器。该 helper 用于索引、
// 算术等错误消息，不要求寄存器值本身是函数。
func inferRegisterDebugName(proto *bytecode.Proto, vm *runtime.VM, callPC int, register int) (string, string) {
	return inferRegisterDebugNameWithShortCircuit(proto, vm, callPC, register, true)
}

// inferIndexReceiverDebugName 根据索引接收者寄存器来源推断错误消息中的对象名称。
//
// 索引接收者位于短路分支内部时仍是直接被访问对象，例如 `x and aaa[k]` 中的 `aaa`；因此这里不
// 使用短路临时值屏蔽规则，避免丢失官方错误文本中的 global/upvalue/local 来源。
func inferIndexReceiverDebugName(proto *bytecode.Proto, vm *runtime.VM, callPC int, register int) (string, string) {
	return inferRegisterDebugNameWithShortCircuit(proto, vm, callPC, register, false)
}

// inferRegisterDebugNameWithShortCircuit 根据寄存器最近来源推断错误消息中的对象名称。
//
// suppressShortCircuit 为 true 时会隐藏 and/or 复合表达式末端临时值名称；索引接收者场景可关闭
// 该规则，以保留短路分支内部直接访问对象的名称。
func inferRegisterDebugNameWithShortCircuit(proto *bytecode.Proto, vm *runtime.VM, callPC int, register int, suppressShortCircuit bool) (string, string) {
	if proto == nil || vm == nil || callPC < 0 || callPC > len(proto.Code) {
		// 缺少调用方上下文时无法从指令推断。
		return "", ""
	}
	if instruction, writerPC, ok := previousRegisterWriterAt(proto, callPC, register); ok {
		if suppressShortCircuit && isShortCircuitBranchValue(proto, writerPC) {
			// and/or 短路表达式的寄存器结果是复合表达式临时值，Lua 5.3 不展示最后分支变量名。
			return "", ""
		}
		// 优先使用最近写入该寄存器的指令来源。
		switch instruction.OpCode() {
		case bytecode.OpMove:
			// MOVE 来源通常是 local。
			return inferLocalDebugName(proto, vm, instruction.B(), callPC)
		case bytecode.OpGetUpval:
			// GETUPVAL 来源是 upvalue 名称。
			return upvalueDebugName(proto, instruction.B())
		case bytecode.OpGetTable:
			// GETTABLE 常见于 table.field 链式访问。
			if receiverName, _ := inferRegisterDebugNameWithShortCircuit(proto, vm, writerPC, instruction.B(), suppressShortCircuit); receiverName == "_ENV" {
				// 显式 local _ENV 上的字段读取在错误消息中仍按 Lua 5.3 展示为 global。
				return keyStringDebugName(proto, writerPC, instruction.C(), "global")
			}
			return keyStringDebugName(proto, writerPC, instruction.C(), "field")
		case bytecode.OpGetTabUp:
			// GETTABUP _ENV K(name) 常见于全局变量读取。
			return keyStringDebugName(proto, writerPC, instruction.C(), "global")
		case bytecode.OpSelf:
			// SELF 的 A 槽表示 method 名称。
			return keyStringDebugName(proto, writerPC, instruction.C(), "method")
		default:
			// 动态调用结果等来源无法静态解释。
		}
	}
	if name, nameWhat := inferLocalDebugName(proto, vm, register, callPC); name != "" {
		// 没有最近写入指令时，尝试直接按活动 local 解释当前寄存器。
		return name, nameWhat
	}
	return "", ""
}

// inferRKDebugName 根据 RK 操作数推断错误消息中的对象名称。
//
// 常量 RK 没有源码变量来源，因此只对寄存器 RK 反查 local/upvalue/global/field。
func inferRKDebugName(proto *bytecode.Proto, vm *runtime.VM, callPC int, rk int) (string, string) {
	if bytecode.IsK(rk) {
		// 常量表值不对应用户变量名。
		return "", ""
	}
	return inferRegisterDebugName(proto, vm, callPC, bytecode.IndexK(rk))
}

// luaRKValue 读取当前 Lua 执行上下文中的 RK 操作数。
//
// proto 提供常量表，vm 提供寄存器窗口；该 helper 只做只读转换，避免调用 runtime 未导出的
// rkValue 造成状态耦合。
func luaRKValue(proto *bytecode.Proto, vm *runtime.VM, rk int) (runtime.Value, bool) {
	index := bytecode.IndexK(rk)
	if bytecode.IsK(rk) {
		// 常量 RK 从 Proto 常量表转换为 runtime.Value。
		if proto == nil || index < 0 || index >= len(proto.Constants) {
			return runtime.NilValue(), false
		}
		return luaConstantValue(proto.Constants[index])
	}
	if vm == nil {
		// 没有 VM 时无法读取寄存器 RK。
		return runtime.NilValue(), false
	}
	return vm.Register(index)
}

// luaConstantValue 把 bytecode 常量转换为 Lua runtime 值。
func luaConstantValue(constant bytecode.Constant) (runtime.Value, bool) {
	switch constant.Kind {
	case bytecode.ConstantNil:
		// nil 常量转换为 Lua nil。
		return runtime.NilValue(), true
	case bytecode.ConstantBoolean:
		// boolean 常量保留布尔负载。
		return runtime.BooleanValue(constant.Bool), true
	case bytecode.ConstantInteger:
		// integer 常量保留 int64 精确负载。
		return runtime.IntegerValue(constant.Integer), true
	case bytecode.ConstantNumber:
		// number 常量保留 float64 负载。
		return runtime.NumberValue(constant.Number), true
	case bytecode.ConstantString:
		// string 常量保留原始字节串。
		return runtime.StringValue(constant.String), true
	default:
		// 未知常量类型来自损坏 chunk，不能参与错误消息推断。
		return runtime.NilValue(), false
	}
}

// inferInstructionDebugName 从 CALL 前置取值指令推断调用点名称。
//
// 当前覆盖 Lua 5.3 官方 db.lua 需要的形态：local 函数 MOVE、upvalue 函数 GETUPVAL、
// table 字段 GETTABLE 与全局 GETTABUP。
func inferInstructionDebugName(proto *bytecode.Proto, vm *runtime.VM, callPC int) (string, string) {
	if proto == nil || vm == nil || callPC <= 0 || callPC > len(proto.Code) {
		// 缺少调用方上下文时无法从指令推断。
		return "", ""
	}
	callInstruction := proto.Code[callPC]
	if callInstruction.OpCode() != bytecode.OpCall && callInstruction.OpCode() != bytecode.OpTailCall {
		// 只处理普通 CALL/TAILCALL 调用点。
		return "", ""
	}
	functionRegister := callInstruction.A()
	previousInstruction, writerPC, ok := previousRegisterWriterAt(proto, callPC, functionRegister)
	if !ok {
		// 找不到被调寄存器来源时，说明调用点形态暂不支持。
		return "", ""
	}
	if isShortCircuitCallValue(proto, writerPC, callPC) {
		// and/or 短路表达式结果被调用时，错误文本只保留值类型，不暴露最后分支变量名。
		return "", ""
	}
	switch previousInstruction.OpCode() {
	case bytecode.OpMove:
		if isShortCircuitTemporaryMove(proto, writerPC, callPC) {
			// 短路表达式最终 MOVE 到调用槽时，Lua 5.3 不把最后一个分支变量当作调用名称。
			return "", ""
		}
		// local 函数调用通常由 MOVE 把局部函数值搬到调用寄存器。
		return inferLocalDebugName(proto, vm, previousInstruction.B(), callPC)
	case bytecode.OpGetUpval:
		// 闭包递归调用可能通过 GETUPVAL 读取同名外层 local，例如官方 db.lua 的 f。
		return upvalueDebugName(proto, previousInstruction.B())
	case bytecode.OpGetTable:
		// table.field() 会通过 GETTABLE 把字段函数加载到调用寄存器。
		return keyStringDebugName(proto, writerPC, previousInstruction.C(), "field")
	case bytecode.OpGetTabUp:
		// 全局函数调用可由 GETTABUP _ENV K(name) 直接形成调用寄存器。
		return keyStringDebugName(proto, writerPC, previousInstruction.C(), "global")
	case bytecode.OpSelf:
		// method 调用通过 SELF 把方法函数写入 A，并把 receiver 写入 A+1。
		return keyStringDebugName(proto, writerPC, previousInstruction.C(), "method")
	default:
		// 其他取值形态后续按官方测试继续扩展。
		return "", ""
	}
}

// previousRegisterWriter 向前查找最近一次写入目标寄存器的指令。
//
// 参数准备可能在函数寄存器写入后继续写 R(A+1)..，因此不能只看 CALL 前一条指令。
func previousRegisterWriter(proto *bytecode.Proto, callPC int, targetRegister int) (bytecode.Instruction, bool) {
	instruction, _, ok := previousRegisterWriterAt(proto, callPC, targetRegister)
	return instruction, ok
}

// previousRegisterWriterAt 向前查找最近一次写入目标寄存器的指令及其 PC。
func previousRegisterWriterAt(proto *bytecode.Proto, callPC int, targetRegister int) (bytecode.Instruction, int, bool) {
	for pc := callPC - 1; pc >= 0; pc-- {
		// 从后往前查找最近一次覆盖目标寄存器的指令，避免越过 LOADK/ADD 等写入而误读更早来源。
		instruction := proto.Code[pc]
		if instructionWritesRegister(instruction, targetRegister) {
			// 命中最近写入者即返回；调用方决定该指令能否解释成 local/global/field/method。
			return instruction, pc, true
		}
	}
	return bytecode.Instruction(0), -1, false
}

// isShortCircuitTemporaryMove 判断 MOVE 是否来自短路表达式的末端临时值。
func isShortCircuitTemporaryMove(proto *bytecode.Proto, writerPC int, callPC int) bool {
	if proto == nil || writerPC <= 0 || writerPC > len(proto.Code) {
		// 缺少前置指令时不能判定为短路表达式。
		return false
	}
	if writerPC != callPC-1 {
		// 真实函数调用会在函数 MOVE 后继续准备参数；只有紧贴 CALL 的 MOVE 才是表达式结果。
		return false
	}
	previousInstruction := proto.Code[writerPC-1]
	if previousInstruction.OpCode() == bytecode.OpJmp || previousInstruction.OpCode() == bytecode.OpTest || previousInstruction.OpCode() == bytecode.OpTestSet {
		if previousInstruction.OpCode() == bytecode.OpJmp && previousInstruction.SBx() <= 0 {
			// 非正向 JMP 常来自 close-only 占位或循环回跳，不代表短路表达式末端。
			return false
		}
		if previousInstruction.OpCode() == bytecode.OpJmp && previousJumpTarget(writerPC-1, previousInstruction) != callPC {
			// 短路表达式的 JMP 会直接落到 CALL；if/else 分支会跳过整个 then 块。
			return false
		}
		// 短路编译会在最后一个分支 MOVE 前放置跳转或测试指令。
		return true
	}
	return false
}

// isShortCircuitCallValue 判断 CALL 的函数槽是否来自紧邻的短路表达式结果。
func isShortCircuitCallValue(proto *bytecode.Proto, writerPC int, callPC int) bool {
	if writerPC != callPC-1 {
		// 分支内部直接调用会先写函数槽、再准备参数；这类调用仍应保留函数名。
		return false
	}
	if proto != nil && writerPC > 0 && writerPC < len(proto.Code) {
		// 紧邻 CALL 的 MOVE 若跟在 JMP 后，只有跳转目标正好落到 CALL 才是短路表达式。
		previousInstruction := proto.Code[writerPC-1]
		if previousInstruction.OpCode() == bytecode.OpJmp {
			return previousJumpTarget(writerPC-1, previousInstruction) == callPC
		}
	}
	return isShortCircuitBranchValue(proto, writerPC)
}

// isShortCircuitBranchValue 判断最近写入值是否位于 and/or 短路分支后。
func isShortCircuitBranchValue(proto *bytecode.Proto, writerPC int) bool {
	if proto == nil || writerPC <= 0 || writerPC >= len(proto.Code) {
		// 缺少前置指令或写入位置越界时不能判定为短路表达式。
		return false
	}
	previousInstruction := proto.Code[writerPC-1]
	if previousInstruction.OpCode() == bytecode.OpJmp || previousInstruction.OpCode() == bytecode.OpTest || previousInstruction.OpCode() == bytecode.OpTestSet {
		if previousInstruction.OpCode() == bytecode.OpJmp && previousInstruction.SBx() <= 0 {
			// 非正向 JMP 常用于 close-only 占位或循环回跳，不代表 and/or 短路分支。
			return false
		}
		// codegen 为 and/or 分支生成 TEST/JMP 后再写入候选结果，此时寄存器不是直接变量来源。
		return true
	}
	return false
}

// previousJumpTarget 计算指定 JMP 指令的目标 PC。
//
// pc 是 JMP 自身的 0-based 指令位置；Lua VM 执行 JMP 时会先前进到下一条指令再叠加 sBx。
func previousJumpTarget(pc int, instruction bytecode.Instruction) int {
	// 返回和 codegen/VM 一致的跳转目标位置。
	return pc + 1 + instruction.SBx()
}

// inferLocalDebugName 根据寄存器和局部变量生命周期推断 local 调用名称。
//
// register 是 MOVE 源寄存器；callPC 是 CALL 指令位置，必须落在局部变量可见范围内。
func inferLocalDebugName(proto *bytecode.Proto, vm *runtime.VM, register int, callPC int) (string, string) {
	hasFunctionValue := false
	functionValue := runtime.NilValue()
	if vm != nil {
		// VM 存在时读取当前被调函数值，用于过滤共享寄存器写入指令造成的同槽误判。
		functionValue, hasFunctionValue = vm.Register(register)
	}
	if hasFunctionValue {
		if name, nameWhat := recentClosureLocalDebugName(proto, register, callPC, functionValue); name != "" {
			// 最近一次写入源寄存器的 CLOSURE 与当前被调函数一致时，优先使用对应活动 local。
			return name, nameWhat
		}
	}
	for localIndex := len(proto.LocalVars) - 1; localIndex >= 0; localIndex-- {
		// 优先按当前寄存器槽与 local 记录槽位同时匹配，避免多个 local function 声明共用写入指令时误选后声明名称。
		localVar := proto.LocalVars[localIndex]
		if localVar.Register != register || !localVar.ActiveAt(callPC) || localVar.Name == "" {
			// 不是当前函数槽、非活动或无名称时不能作为精确 local 调用名。
			continue
		}
		// MOVE 源寄存器精确命中活动 local 时，静态调用点名称优先于 return hook 期间的寄存器快照。
		return localVar.Name, "local"
	}
	for localIndex := len(proto.LocalVars) - 1; localIndex >= 0; localIndex-- {
		// dump/load 重建的局部生命周期可能在 return hook 可见 CALL PC 处偏短；同寄存器已开始的最近 local 仍是 MOVE 静态来源。
		localVar := proto.LocalVars[localIndex]
		if localVar.Register != register || localVar.Name == "" || localVar.StartPC > callPC {
			// 寄存器、名称或起始 PC 不匹配时不能作为降级 local 名称。
			continue
		}
		return localVar.Name, "local"
	}
	for localIndex := len(proto.LocalVars) - 1; localIndex >= 0; localIndex-- {
		// 优先用声明前一条写寄存器指令反查，避免 binary chunk 重建寄存器误命中其他活动 local。
		localVar := proto.LocalVars[localIndex]
		if localVar.Name == "" || !localVar.ActiveAt(callPC) || localVar.StartPC <= 0 || localVar.StartPC > len(proto.Code) {
			// 无名称、非活动或缺少声明前置指令时不能作为可靠来源。
			continue
		}
		declarationInstruction := proto.Code[localVar.StartPC-1]
		if declarationInstruction.OpCode() == bytecode.OpClosure && !declarationClosureMatchesFunction(proto, declarationInstruction, functionValue) {
			// local function 声明必须匹配当前被调闭包 Proto，避免同寄存器复用时误选其他局部函数名。
			continue
		}
		if instructionWritesRegister(declarationInstruction, register) || declarationClosureMatchesFunction(proto, declarationInstruction, functionValue) {
			// 命中 local 创建指令的目标寄存器时，按 Lua 5.3 返回 local 名称来源。
			return localVar.Name, "local"
		}
	}
	for localIndex := len(proto.LocalVars) - 1; localIndex >= 0; localIndex-- {
		// 从后往前找同寄存器 local，优先命中更内层或更晚声明的变量。
		localVar := proto.LocalVars[localIndex]
		if localVar.Register == register && localVar.ActiveAt(callPC) && localVar.Name != "" {
			// 命中活动 local 时按 Lua 5.3 返回 local 名称来源。
			return localVar.Name, "local"
		}
	}
	return "", ""
}

// recentClosureLocalDebugName 根据源寄存器最近一次 CLOSURE 写入推断 local 调用名。
func recentClosureLocalDebugName(proto *bytecode.Proto, register int, callPC int, function Value) (string, string) {
	for pc := callPC - 1; pc >= 0; pc-- {
		// 只关心写入源寄存器的最近指令。
		instruction := proto.Code[pc]
		if !instructionWritesRegister(instruction, register) {
			// 未写入源寄存器时继续向前查找。
			continue
		}
		if instruction.OpCode() != bytecode.OpClosure || !declarationClosureMatchesFunction(proto, instruction, function) {
			// 最近写入不是当前闭包时不能用更早声明猜测名称。
			return "", ""
		}
		localName := recentClosureLocalName(proto, register, callPC, pc)
		if localName == "" {
			// 找不到活动 local 时保持匿名。
			return "", ""
		}
		return localName, "local"
	}
	return "", ""
}

// recentClosureLocalName 为指定 CLOSURE 写入点选择对应活动 local 名称。
func recentClosureLocalName(proto *bytecode.Proto, register int, callPC int, writerPC int) string {
	fallbackName := ""
	fallbackStartPC := -1
	for localIndex := len(proto.LocalVars) - 1; localIndex >= 0; localIndex-- {
		// 优先选择同寄存器活动 local；若 binary chunk 寄存器重建缺失，则退回到写入点之前最近启用的 local。
		localVar := proto.LocalVars[localIndex]
		if localVar.Name == "" || !localVar.ActiveAt(callPC) || localVar.StartPC > writerPC+1 {
			// 无名、非活动或晚于写入点才开始的 local 不能作为候选。
			continue
		}
		if localVar.Register == register {
			// 寄存器精确命中最可靠。
			return localVar.Name
		}
		if localVar.StartPC > fallbackStartPC {
			// 寄存器不可用时选择离写入点最近的活动 local。
			fallbackName = localVar.Name
			fallbackStartPC = localVar.StartPC
		}
	}
	return fallbackName
}

// declarationClosureMatchesFunction 判断 local 声明闭包是否就是当前被调函数。
//
// proto 是调用方 Proto；instruction 必须是 local 声明前的候选指令；function 是当前被调函数值。
// binary chunk 重建 locvar 寄存器时，寄存器号可能不足以区分同一作用域内的多个 local function，
// 此时用闭包 Proto identity 过滤名称候选。
func declarationClosureMatchesFunction(proto *bytecode.Proto, instruction bytecode.Instruction, function Value) bool {
	// 只有 Lua closure 值和 CLOSURE 声明指令具备 Proto identity。
	if proto == nil || instruction.OpCode() != bytecode.OpClosure || function.Kind != runtime.KindLuaClosure {
		return false
	}
	closure, ok := function.Ref.(*runtime.LuaClosure)
	if !ok || closure == nil || closure.Proto == nil {
		// 引用负载损坏时不能用于名称推断。
		return false
	}
	childIndex := instruction.Bx()
	if childIndex < 0 || childIndex >= len(proto.Protos) {
		// 损坏的子 Proto 索引不能参与匹配。
		return false
	}
	return proto.Protos[childIndex] == closure.Proto
}

// instructionWritesRegister 判断指令是否会把结果写入指定寄存器。
//
// 该 helper 只用于 debug 调用名 fallback；覆盖局部变量声明常见的单目标写入指令以及
// LOADNIL 的连续清空范围，避免 binary chunk 缺失 locvar 寄存器号时丢失 local 调用名。
func instructionWritesRegister(instruction bytecode.Instruction, register int) bool {
	// 按 opcode 分类判断 A 或 A..A+B 是否覆盖目标寄存器。
	switch instruction.OpCode() {
	case bytecode.OpMove, bytecode.OpLoadK, bytecode.OpLoadKX, bytecode.OpLoadBool, bytecode.OpGetUpval,
		bytecode.OpGetTabUp, bytecode.OpGetTable, bytecode.OpNewTable, bytecode.OpSelf, bytecode.OpAdd,
		bytecode.OpSub, bytecode.OpMul, bytecode.OpMod, bytecode.OpPow, bytecode.OpDiv, bytecode.OpIDiv,
		bytecode.OpBAnd, bytecode.OpBOr, bytecode.OpBXor, bytecode.OpShl, bytecode.OpShr, bytecode.OpUnm,
		bytecode.OpBNot, bytecode.OpNot, bytecode.OpLen, bytecode.OpConcat, bytecode.OpClosure:
		// 这些指令均以 A 作为主结果寄存器。
		return instruction.A() == register
	case bytecode.OpLoadNil:
		// LOADNIL 写入 A 到 A+B 的连续寄存器区间。
		return register >= instruction.A() && register <= instruction.A()+instruction.B()
	case bytecode.OpCall, bytecode.OpTailCall, bytecode.OpVararg:
		// 多返回指令至少写入 A；具体数量依赖 B/C，本 fallback 只判断声明起点的主槽位。
		return instruction.A() == register
	default:
		// 其他指令不作为 local 声明写入来源。
		return false
	}
}

// upvalueDebugName 根据 upvalue 描述推断调用点名称。
//
// proto 必须来自当前调用方函数；upvalueIndex 是 GETUPVAL 的 B 操作数。Lua 5.3 会把外层
// local 函数复写后的递归调用编译为 GETUPVAL，调试帧需要继续展示该 upvalue 的源码名称。
func upvalueDebugName(proto *bytecode.Proto, upvalueIndex int) (string, string) {
	if proto == nil || upvalueIndex < 0 || upvalueIndex >= len(proto.Upvalues) {
		// 缺少有效 upvalue 描述时不能推断名称。
		return "", ""
	}
	upvalueName := proto.Upvalues[upvalueIndex].Name
	if upvalueName == "" {
		// 无名 upvalue 不能暴露为稳定 debug 名称。
		return "", ""
	}
	return upvalueName, "upvalue"
}

// constantStringDebugName 从 RK 常量操作数提取字符串名称。
//
// operand 必须引用常量池中的 string；nameWhat 是调用方判定出的名称来源。
func constantStringDebugName(proto *bytecode.Proto, operand int, nameWhat string) (string, string) {
	if !bytecode.IsK(operand) {
		// 寄存器 key 暂无静态名称。
		return "", ""
	}
	return constantIndexStringDebugName(proto, bytecode.IndexK(operand), nameWhat)
}

// keyStringDebugName 从字段访问 key 操作数提取字符串名称。
//
// operand 可以是普通 RK 常量，也可以是在 RK 上限溢出后由 LOADK/LOADKX 预先写入的 key 寄存器；
// writerPC 是消费该 key 的 GETTABLE/GETTABUP/SELF 指令位置，用于向前追溯最近写入。
func keyStringDebugName(proto *bytecode.Proto, writerPC int, operand int, nameWhat string) (string, string) {
	if bytecode.IsK(operand) {
		// RK 常量直接从常量表提取字段名称。
		return constantStringDebugName(proto, operand, nameWhat)
	}
	keyRegister := bytecode.IndexK(operand)
	instruction, instructionPC, ok := previousRegisterWriterAt(proto, writerPC, keyRegister)
	if !ok {
		// 找不到静态 key 写入时不能把运行期动态 key 猜成源码名称。
		return "", ""
	}
	switch instruction.OpCode() {
	case bytecode.OpLoadK:
		// RK 上限溢出后 codegen 会把字符串 key 先 LOADK 到临时寄存器。
		return constantIndexStringDebugName(proto, instruction.Bx(), nameWhat)
	case bytecode.OpLoadKX:
		// LOADKX 的常量索引由紧随其后的 EXTRAARG 承载。
		if instructionPC+1 >= len(proto.Code) {
			// 损坏 chunk 缺失 EXTRAARG 时不能推断名称。
			return "", ""
		}
		extraInstruction := proto.Code[instructionPC+1]
		if extraInstruction.OpCode() != bytecode.OpExtraArg {
			// LOADKX 后不是 EXTRAARG 表示字节码不完整，放弃名称推断。
			return "", ""
		}
		return constantIndexStringDebugName(proto, extraInstruction.Ax(), nameWhat)
	default:
		// 动态表达式 key 不应暴露为静态 field/global/method 名称。
		return "", ""
	}
}

// constantIndexStringDebugName 从常量表索引提取非空字符串 debug 名称。
//
// constantIndex 必须指向 string 常量；nameWhat 由调用方提供，表示 global/field/method 等来源。
func constantIndexStringDebugName(proto *bytecode.Proto, constantIndex int, nameWhat string) (string, string) {
	if constantIndex < 0 || constantIndex >= len(proto.Constants) {
		// 损坏常量索引不能用于 debug 名称。
		return "", ""
	}
	constant := proto.Constants[constantIndex]
	if constant.Kind != bytecode.ConstantString || constant.String == "" {
		// 只有非空字符串 key 能作为可见名称。
		return "", ""
	}
	return constant.String, nameWhat
}

// inferFunctionDebugName 从当前 State 环境推断函数调试名称。
//
// state 必须允许读取 globals；function 是待调用值。当前先支持全局变量名推断，满足 Lua 5.3
// debug.getinfo("n") 对 `function F()` 这类调用点的 name/namewhat 语义。
func inferFunctionDebugName(state *State, function Value) (string, string) {
	// 关闭或 nil State 无法读取全局表，返回空名称表示无法推断。
	if state == nil {
		// nil State 没有 globals，调用帧不能带误导性名称。
		return "", ""
	}
	globals := state.Globals()
	if globals == nil {
		// State 已关闭或 globals 不可用时不能扫描全局环境。
		return "", ""
	}
	if function.Kind == runtime.KindGoClosure {
		// Go closure 没有 Lua Proto 缓存键，但官方 traceback 要求动态调用的全局 C 函数显示名称。
		return inferGlobalFunctionDebugName(globals, function, false)
	}
	closure, ok := function.Ref.(*runtime.LuaClosure)
	if !ok || closure == nil {
		// 非标准 Lua closure 引用不能安全建立 identity 缓存。
		return "", ""
	}
	cacheKey := debugNameCacheKey{state: state, closure: closure}
	globalsVersion := globals.MutationVersion()
	if cachedValue, ok := luaDebugNameCache.Load(cacheKey); ok {
		// 命中缓存时仅在全局表版本一致时复用，避免全局变量改写后名称过期。
		cachedEntry := cachedValue.(debugNameCacheEntry)
		if cachedEntry.globalsVersion == globalsVersion {
			// 正缓存和负缓存都可复用，空 name 表示当前版本下无法推断。
			return cachedEntry.name, cachedEntry.nameWhat
		}
	}

	name, nameWhat := inferGlobalFunctionDebugName(globals, function, true)
	luaDebugNameCache.Store(cacheKey, debugNameCacheEntry{
		name:           name,
		nameWhat:       nameWhat,
		globalsVersion: globalsVersion,
	})
	return name, nameWhat
}

// inferGlobalFunctionDebugName 从全局表按 raw identity 推断函数名。
//
// globals 必须非 nil；function 是当前被调函数。onlyLuaClosure 为 true 时只服务 Lua closure 缓存路径；
// false 时允许 Go closure 直接扫描全局表以兼容 `pcall(debug.traceback)` 这类动态 C 函数调用。
func inferGlobalFunctionDebugName(globals *runtime.Table, function Value, onlyLuaClosure bool) (string, string) {
	if globals == nil {
		// 没有全局表时无法推断名称。
		return "", ""
	}
	if onlyLuaClosure && function.Kind != runtime.KindLuaClosure {
		// Lua closure 缓存路径不处理其他函数类型。
		return "", ""
	}
	if !onlyLuaClosure && function.Kind != runtime.KindLuaClosure && function.Kind != runtime.KindGoClosure {
		// 动态回退路径只处理可调用函数值，其他类型保持匿名。
		return "", ""
	}
	key := runtime.NilValue()
	for {
		// 逐项 raw 遍历 globals，按引用 identity 匹配当前被调函数。
		nextKey, nextValue, ok, err := globals.RawNext(key)
		if err != nil {
			// globals 迭代异常时放弃名称推断，不影响函数调用本身。
			return "", ""
		}
		if !ok {
			// 遍历结束仍未命中，返回空名称。
			return "", ""
		}
		if nextKey.Kind == runtime.KindString && nextValue.RawEqual(function) {
			// 命中 string 全局变量时按 Lua 5.3 约定标记为 global 名称。
			return nextKey.String, "global"
		}
		key = nextKey
	}
}

// luaCallArguments 从 VM 寄存器窗口读取调用实参。
//
// 当前最小执行循环只支持固定参数数量；开放参数数量需要完整栈顶语义，后续补齐。
func luaCallArguments(vm *runtime.VM, callRequest *runtime.CallRequest) ([]Value, error) {
	if callRequest.ArgumentCount < 0 {
		// 开放参数需要真实栈顶，当前执行循环暂不支持。
		return nil, runtime.ErrUnsupportedInstruction
	}
	arguments := make([]Value, callRequest.ArgumentCount)
	if !vm.CopyRegisters(callRequest.FunctionIndex+1, arguments) {
		// 实参紧跟函数寄存器之后连续保存，区间越界说明 codegen 或 VM 状态异常。
		return nil, runtime.ErrRegisterOutOfRange
	}

	// 返回调用实参数组。
	return arguments, nil
}

// finishLuaFixedCallResults 完成固定返回 CALL 的开放栈顶收口和临时实参槽清理。
//
// proto/callPC 来自调用方闭包；callRequest 必须是刚完成结果写回的 CALL 请求。开放返回、开放实参
// 和 TFORCALL 会由 runtime helper 自动跳过，只保留 SetOpenTop(-1) 的固定返回语义。
func finishLuaFixedCallResults(vm *runtime.VM, proto *bytecode.Proto, callPC int, callRequest *runtime.CallRequest) {
	// 固定返回不形成开放列表，先关闭 openTop 供后续 CALL/SETLIST 判定。
	vm.SetOpenTop(-1)
	vm.ClearFixedCallArgumentTemporaries(proto, callRequest, callPC+1)
}

// writeLuaCallResults 将调用结果写回 VM 寄存器窗口。
//
// 固定返回数量会用 nil 补齐；开放返回数量写入所有结果，当前由寄存器窗口边界控制。
func writeLuaCallResults(vm *runtime.VM, proto *bytecode.Proto, callPC int, callRequest *runtime.CallRequest, results []Value) error {
	resultCount := callRequest.ReturnCount
	if resultCount == 1 && !callRequest.GenericFor {
		// 普通 Lua 函数最常见的单返回值直接覆盖函数槽，避免进入通用结果循环和开放返回分支。
		resultValue := runtime.NilValue()
		if len(results) > 0 {
			// 被调函数实际返回的第一个值优先写入。
			resultValue = results[0]
		}
		if err := vm.SetRegister(callRequest.FunctionIndex, resultValue); err != nil {
			// 写回超过寄存器窗口时返回边界错误。
			return err
		}
		vm.SetOpenTop(-1)
		return nil
	}
	if resultCount == 2 && !callRequest.GenericFor {
		// string.find 等固定双返回热点直接写两个结果槽，避免进入通用结果循环。
		firstResult := runtime.NilValue()
		if len(results) > 0 {
			// 被调函数实际返回的第一个值优先写入。
			firstResult = results[0]
		}
		secondResult := runtime.NilValue()
		if len(results) > 1 {
			// 被调函数实际返回的第二个值优先写入。
			secondResult = results[1]
		}
		if err := vm.SetRegister(callRequest.FunctionIndex, firstResult); err != nil {
			// 第一个结果写回超过寄存器窗口时返回边界错误。
			return err
		}
		if err := vm.SetRegister(callRequest.FunctionIndex+1, secondResult); err != nil {
			// 第二个结果写回超过寄存器窗口时返回边界错误。
			return err
		}
		vm.SetOpenTop(-1)
		return nil
	}
	if resultCount < 0 {
		// 开放返回写入被调函数实际返回的所有结果。
		resultCount = len(results)
		if !callRequest.GenericFor {
			// 开放返回数量运行时才知道，先扩展寄存器窗口再逐项写回。
			vm.EnsureRegisterCount(callRequest.FunctionIndex + resultCount)
		}
	}
	for resultIndex := 0; resultIndex < resultCount; resultIndex++ {
		// 普通 CALL 返回值从函数寄存器开始覆盖；TFORCALL 返回值写入迭代变量区。
		resultValue := runtime.NilValue()
		if resultIndex < len(results) {
			// 被调函数实际返回的结果优先写入。
			resultValue = results[resultIndex]
		}
		resultRegister := callRequest.FunctionIndex + resultIndex
		if callRequest.GenericFor {
			// 泛型 for 不能覆盖迭代函数/state/control，否则下一轮 next 会收到错误控制变量。
			resultRegister = callRequest.ResultIndex + resultIndex
		}
		if err := vm.SetRegister(resultRegister, resultValue); err != nil {
			// 写回超过寄存器窗口时返回边界错误。
			return err
		}
	}
	if callRequest.ReturnCount < 0 && !callRequest.GenericFor {
		// CALL C=0 表示开放返回，记录实际结果上界供后续 SETLIST B=0 或 CALL B=0 消费。
		vm.SetOpenTop(callRequest.FunctionIndex + len(results))
	} else {
		// 固定返回数量不形成开放列表。
		vm.SetOpenTop(-1)
	}

	// 调用结果写回完成。
	return nil
}

// callGoClosureValue 执行 Go closure 引用值。
//
// function 必须是 KindGoClosure；args 是调用实参快照。支持 GoResultsFunction、GoFunction、
// GoUnaryFunction 和 GoFastUnaryFunction 等 runtime 回调形态，其他引用负载按不可调用运行期错误处理。
func callGoClosureValue(function Value, args ...Value) ([]Value, error) {
	// 根据 Ref 中保存的实际 Go 回调形态分发。
	switch callback := function.Ref.(type) {
	case runtime.GoResultsFunction:
		// 多返回 Go 回调直接返回结果列表。
		return callback(args...)
	case *runtime.GoFixedResultsFunction:
		// 固定上限多返回回调用声明上限构造结果槽；未命中时回退完整函数，避免截断变长返回。
		if callback == nil || callback.Function == nil {
			return nil, runtime.NewRuntimeError(runtime.StringValue(ErrExpectedCallable.Error()), ErrExpectedCallable)
		}
		results := make([]Value, callback.MaxResults)
		resultCount, handled, err := callback.Function(results, args...)
		if err != nil {
			// 固定回调错误原样向上传播，外层负责 bad argument 名称重写。
			return nil, err
		}
		if handled {
			// 命中快路径时只返回实际写入的前缀。
			return results[:resultCount], nil
		}
		if callback.Fallback == nil {
			// 没有回退函数时按不可调用错误暴露损坏注册。
			return nil, runtime.NewRuntimeError(runtime.StringValue(ErrExpectedCallable.Error()), ErrExpectedCallable)
		}
		return callback.Fallback(args...)
	case *runtime.GoClosureWithUpvalues:
		// 带 debug upvalue 元数据的 Go closure 仍通过内部 Function 执行。
		if callback == nil || callback.Function == nil {
			return nil, runtime.NewRuntimeError(runtime.StringValue(ErrExpectedCallable.Error()), ErrExpectedCallable)
		}
		return callback.Function(args...)
	case runtime.GoFunction:
		// 单返回 Go 回调转换成单元素结果列表，保持 Call 总是返回 []Value。
		result, err := callback(args...)
		if err != nil {
			// 回调错误原样向上传播，ProtectedCall 会负责转换为 Lua error object。
			return nil, err
		}
		return []Value{result}, nil
	case runtime.GoUnaryFunction:
		// 单参数单返回 Go 回调在通用路径中也需要可调用，覆盖开放参数和公开 Call 入口。
		argument := runtime.NilValue()
		if len(args) > 0 {
			// Lua 标准库允许多余参数；一元入口只读取第一个实参。
			argument = args[0]
		}
		result, err := callback(argument)
		if err != nil {
			// 回调错误原样向上传播，ProtectedCall 会负责转换为 Lua error object。
			return nil, err
		}
		return []Value{result}, nil
	case *runtime.GoFastUnaryFunction:
		// fast unary 包装在通用调用路径仍按普通一元函数执行，保留外层 debug frame 语义。
		if callback == nil || callback.Function == nil {
			// 损坏注册不能进入 nil 函数调用，按不可调用错误暴露。
			return nil, runtime.NewRuntimeError(runtime.StringValue(ErrExpectedCallable.Error()), ErrExpectedCallable)
		}
		argument := runtime.NilValue()
		if len(args) > 0 {
			// Lua 标准库允许多余参数；一元入口只读取第一个实参。
			argument = args[0]
		}
		result, err := callback.Function(argument)
		if err != nil {
			// 回调错误原样向上传播，ProtectedCall 会负责转换为 Lua error object。
			return nil, err
		}
		return []Value{result}, nil
	default:
		// KindGoClosure 但 Ref 不是已知回调签名时，按不可调用错误处理。
		return nil, runtime.NewRuntimeError(runtime.StringValue(ErrExpectedCallable.Error()), ErrExpectedCallable)
	}
}
