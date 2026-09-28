package runtime

import (
	"fmt"
	"io/fs"

	"github.com/ZingYao/go-lua-vm/bytecode"
	"github.com/ZingYao/go-lua-vm/extensions"
)

const (
	// DefaultMaxStackDepth 对齐 Lua 5.3 在 32 位以上 int 环境下的 LUAI_MAXSTACK 默认值。
	DefaultMaxStackDepth = 1000000
	// DefaultMaxCallDepth 为 Lua 调用帧提供默认预算，预留主 chunk、标准库和 debug 包装帧余量。
	DefaultMaxCallDepth = 1000
	// DefaultMaxGluaEventListeners 限制单个 State 可同时持有的 Event 监听器数量。
	DefaultMaxGluaEventListeners = 4096
	// DefaultMaxGluaEventQueuedTasks 限制单个 State 异步 Event 队列的任务总数。
	DefaultMaxGluaEventQueuedTasks = 65536
	// DefaultMaxGluaEventTasksPerDrain 限制单次安全点或显式 flush 执行的 Event 任务数。
	DefaultMaxGluaEventTasksPerDrain = 4096
)

// Options 描述 Lua State 的资源限制配置。
//
// 零值会被 NormalizeOptions 转换为项目默认值；MaxAllocationBudget 为 0 表示不限制分配预算。
// 宿主文件系统和环境变量默认开放。进程能力为兼容旧调用方默认开放，但嵌入方可通过
// AllowProcessSet 显式保留 AllowProcess=false。
type Options struct {
	// MaxStackDepth 限制 Lua 栈最大槽位数量。
	MaxStackDepth int
	// MaxCallDepth 限制嵌套调用深度。
	MaxCallDepth int
	// MaxAllocationBudget 限制运行期可分配预算，单位暂定为字节。
	MaxAllocationBudget int64
	// AllowHostFilesystem 为兼容字段；NormalizeOptions 会始终开启宿主文件系统访问。
	AllowHostFilesystem bool
	// AllowEnvironment 为兼容字段；NormalizeOptions 会始终开启宿主环境变量访问。
	AllowEnvironment bool
	// AllowProcess 控制 os.execute 与 io.popen 是否可以启动宿主进程。
	AllowProcess bool
	// AllowProcessSet 表示调用方显式设置过 AllowProcess；false 保留旧版默认开放行为。
	AllowProcessSet bool
	// PackageDynamicLibraryLoader 保存 package.loadlib 使用的可选动态库 loader。
	//
	// nil 表示默认 CGO-free 构建不启用动态库加载；非 nil 时由宿主负责按 filename 和 symbol
	// 打开外部动态库并返回 Lua 可调用函数。该回调不要求也不引入 CGO，宿主可自行选择插件、
	// 系统动态库、CGO 或纯 Go 适配层。
	PackageDynamicLibraryLoader func(filename string, symbol string) (Value, error)
	// PackageDynamicLibraryLoaderForState 为指定 State 创建 package.loadlib 使用的动态库 loader。
	//
	// native_modules 这类 Lua C API shim 需要把 luaopen_* 调用绑定到真实 State；该工厂在 package
	// 库注册时执行，返回值优先于 PackageDynamicLibraryLoader。nil 表示沿用无状态 loader。
	PackageDynamicLibraryLoaderForState func(state *State) func(filename string, symbol string) (Value, error)
	// VirtualFilesystem 保存只读 Go fs.FS 虚拟文件系统。
	VirtualFilesystem fs.FS
	// PreferHostFilesystem 表示只读路径查找时是否优先尝试宿主文件系统。
	PreferHostFilesystem bool
	// SyntaxExtensions 保存源码编译阶段启用的可选语法扩展集合。
	SyntaxExtensions extensions.SyntaxSet
	// SyntaxExtensionsSet 表示调用方是否显式设置过 SyntaxExtensions。
	SyntaxExtensionsSet bool
	// GluaEventsEnabled 表示 OpenLibs 是否注册 glua 自定义事件全局 API。
	GluaEventsEnabled bool
	// GluaEventsEnabledSet 表示调用方是否显式设置过 GluaEventsEnabled。
	GluaEventsEnabledSet bool
	// MaxGluaEventListeners 限制单个 State 的 Event 监听器总数。
	MaxGluaEventListeners int
	// MaxGluaEventQueuedTasks 限制单个 State 的异步 Event 待执行任务总数。
	MaxGluaEventQueuedTasks int
	// MaxGluaEventTasksPerDrain 限制单次 VM 安全点或显式 flush 最多执行的异步任务数。
	MaxGluaEventTasksPerDrain int
	// DebugObserver 保存可选 VM 调试观察器。
	//
	// nil 表示不启用外部调试能力；非 nil 时执行循环会在每条 Lua 指令前调用观察器，调用方可据此实现
	// DAP 断点、单步和挂起控制。观察器必须自行处理并发和取消。
	DebugObserver DebugObserver
}

// DebugObserver 表示 VM 指令级调试观察器。
//
// state 是当前 Lua 状态；vm/proto/pc 指向即将执行的 Lua 指令。返回错误会中断当前 Lua 执行，
// 用于调试会话断开、宿主取消或观察器内部失败。
type DebugObserver interface {
	BeforeInstruction(state *State, vm *VM, proto *bytecode.Proto, pc int) error
}

// NormalizeOptions 规范化 State 资源限制选项。
//
// 入参 options 可以是零值；返回值会填充默认栈深度、默认调用深度，并保留用户设置的分配预算。
func NormalizeOptions(options Options) Options {
	// 文件系统和环境变量保留旧版默认开放行为。
	options.AllowHostFilesystem = true
	options.AllowEnvironment = true
	if !options.AllowProcessSet {
		options.AllowProcess = true
	}

	// 栈深度未设置时使用 Lua 5.3 默认上限。
	if options.MaxStackDepth <= 0 {
		options.MaxStackDepth = DefaultMaxStackDepth
	}
	// 调用深度未设置时使用项目默认调用帧预算。
	if options.MaxCallDepth <= 0 {
		options.MaxCallDepth = DefaultMaxCallDepth
	}
	// 负分配预算没有业务意义，归一化为 0 表示不限制。
	if options.MaxAllocationBudget < 0 {
		options.MaxAllocationBudget = 0
	}
	if !options.SyntaxExtensionsSet {
		// 未显式配置语法时，默认启用当前构建产物包含的扩展。
		options.SyntaxExtensions = extensions.Default()
	} else {
		// 显式配置时裁剪未编译扩展，保证 runtime 选项不会绕过 build tag。
		options.SyntaxExtensions &= extensions.Compiled()
	}
	if !options.GluaEventsEnabledSet {
		// 未显式配置事件能力时，默认跟随当前构建产物是否编译进 glua events。
		options.GluaEventsEnabled = gluaEventsCompiled()
	} else if options.GluaEventsEnabled {
		// 显式开启时仍受 build tag 裁剪，避免未编译事件文件时暴露半能力。
		options.GluaEventsEnabled = gluaEventsCompiled()
	}
	if options.MaxGluaEventListeners <= 0 {
		// 未配置或负数都使用安全默认值，避免零值 State 获得无限监听器。
		options.MaxGluaEventListeners = DefaultMaxGluaEventListeners
	}
	if options.MaxGluaEventQueuedTasks <= 0 {
		// 异步任务总量必须有 State 级上限，单监听器 queueLimit 不能替代全局预算。
		options.MaxGluaEventQueuedTasks = DefaultMaxGluaEventQueuedTasks
	}
	if options.MaxGluaEventTasksPerDrain <= 0 {
		// 单次 drain 使用独立预算，避免一个安全点长时间占用宿主线程。
		options.MaxGluaEventTasksPerDrain = DefaultMaxGluaEventTasksPerDrain
	}

	// 返回已经填充默认值的选项。
	return options
}

// WithProcessAccess 返回显式配置宿主进程能力后的 Options 副本。
func (options Options) WithProcessAccess(enabled bool) Options {
	options.AllowProcess = enabled
	options.AllowProcessSet = true
	return options
}

// WithGluaEvents 返回配置 glua 自定义事件能力后的 Options 副本。
func (options Options) WithGluaEvents(enabled bool) Options {
	// 显式标记事件配置，NormalizeOptions 会继续按 build tag 裁剪不可用能力。
	options.GluaEventsEnabled = enabled
	options.GluaEventsEnabledSet = true
	return options
}

// WithSyntaxExtensions 返回启用指定语法集合后的 Options 副本。
//
// syntax 会裁剪到当前构建产物已编译集合；该方法适合嵌入方从 DefaultOptions 开始切换
// lua53、extended 或自定义扩展组合。
func (options Options) WithSyntaxExtensions(syntax extensions.SyntaxSet) Options {
	// 显式标记语法配置，允许调用方传入 0 表示关闭所有扩展。
	options.SyntaxExtensions = syntax & extensions.Compiled()
	options.SyntaxExtensionsSet = true
	return options
}

// WithoutSyntaxExtensions 返回关闭指定语法扩展后的 Options 副本。
//
// 未显式配置过语法集合时先使用默认集合，再移除 disabled 指定的扩展。
func (options Options) WithoutSyntaxExtensions(disabled extensions.SyntaxSet) Options {
	// 先规范化，确保零值 options 也有默认语法集合可供裁剪。
	normalized := NormalizeOptions(options)
	normalized.SyntaxExtensions = normalized.SyntaxExtensions.Without(disabled)
	normalized.SyntaxExtensionsSet = true
	return normalized
}

// ResourceLimitKind 表示触发的资源限制类型。
//
// VM、栈、GC 和 bridge 层都应使用该分类构造 ResourceLimitError。
type ResourceLimitKind string

const (
	// ResourceLimitStack 表示 Lua 栈深度超过限制。
	ResourceLimitStack ResourceLimitKind = "stack"
	// ResourceLimitCall 表示调用深度超过限制。
	ResourceLimitCall ResourceLimitKind = "call"
	// ResourceLimitAllocation 表示分配预算超过限制。
	ResourceLimitAllocation ResourceLimitKind = "allocation"
)

// ResourceLimitError 表示 Lua VM 运行中触发资源限制。
//
// 该错误可通过 errors.As 识别，Message 提供接近 Lua 错误文本的说明。
type ResourceLimitError struct {
	// Kind 标识触发的资源限制类别。
	Kind ResourceLimitKind
	// Limit 是配置上限。
	Limit int64
	// Actual 是实际请求或使用量。
	Actual int64
	// Message 是面向调用方的错误说明。
	Message string
}

// Error 返回资源限制错误文本。
//
// 当 Message 非空时优先返回 Message，否则返回包含 kind、actual 和 limit 的通用说明。
func (err *ResourceLimitError) Error() string {
	// 自定义消息通常用于对齐 Lua 的 "stack overflow" 等错误文本。
	if err.Message != "" {
		return err.Message
	}

	// 没有自定义消息时返回结构化通用文本。
	return fmt.Sprintf("%s resource limit exceeded: actual=%d limit=%d", err.Kind, err.Actual, err.Limit)
}

// CheckStackDepth 检查给定栈深度是否超过配置限制。
//
// currentDepth 表示请求后的栈槽位数量；超过 MaxStackDepth 时返回 ResourceLimitError。
func (options Options) CheckStackDepth(currentDepth int) error {
	// 先规范化选项，确保零值 options 也有默认限制。
	normalized := NormalizeOptions(options)
	if currentDepth > normalized.MaxStackDepth {
		// 超过栈上限时返回接近 Lua 语义的 stack overflow。
		return &ResourceLimitError{Kind: ResourceLimitStack, Limit: int64(normalized.MaxStackDepth), Actual: int64(currentDepth), Message: "stack overflow"}
	}

	// 未超过限制时返回 nil。
	return nil
}

// CheckCallDepth 检查给定调用深度是否超过配置限制。
//
// currentDepth 表示请求后的调用深度；超过 MaxCallDepth 时返回 ResourceLimitError。
func (options Options) CheckCallDepth(currentDepth int) error {
	// 先规范化选项，确保零值 options 也有默认限制。
	normalized := NormalizeOptions(options)
	if currentDepth > normalized.MaxCallDepth {
		// 超过调用深度时返回接近 Lua 语义的 C stack overflow。
		return &ResourceLimitError{Kind: ResourceLimitCall, Limit: int64(normalized.MaxCallDepth), Actual: int64(currentDepth), Message: "C stack overflow"}
	}

	// 未超过限制时返回 nil。
	return nil
}

// CheckAllocationBudget 检查给定分配量是否超过配置预算。
//
// usedBytes 表示请求后的累计分配量；MaxAllocationBudget 为 0 时表示不限制。
func (options Options) CheckAllocationBudget(usedBytes int64) error {
	// 先规范化选项，确保负预算被转换为不限制。
	normalized := NormalizeOptions(options)
	if normalized.MaxAllocationBudget == 0 {
		// 预算为 0 表示不限制，直接通过。
		return nil
	}
	if usedBytes > normalized.MaxAllocationBudget {
		// 超过分配预算时返回资源限制错误，后续 GC/allocator 可据此中断执行。
		return &ResourceLimitError{Kind: ResourceLimitAllocation, Limit: normalized.MaxAllocationBudget, Actual: usedBytes, Message: "allocation budget exceeded"}
	}

	// 未超过限制时返回 nil。
	return nil
}
