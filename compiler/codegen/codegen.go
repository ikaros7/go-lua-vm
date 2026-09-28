// Package codegen 提供 Lua 5.3 AST 到 Proto 字节码的生成能力。
//
// 当前阶段实现最小表达式与局部赋值编译闭环，后续会逐步补齐控制流、闭包、upvalue 和标准调试信息。
package codegen

import (
	"fmt"

	"github.com/ZingYao/go-lua-vm/bytecode"
	"github.com/ZingYao/go-lua-vm/compiler/lexer"
	"github.com/ZingYao/go-lua-vm/compiler/parser"
)

const (
	// envUpvalueName 是 Lua 5.3 chunk 默认捕获的环境 upvalue 名称。
	envUpvalueName = "_ENV"
	// tableGluaConstName 是 GLua 用于把 table 字段投影为只读 ROOT 常量的保留字段。
	tableGluaConstName = "_glua_const"
	// tableGluaConstMarkerName 是顶层 const 包装表的内部标记字段。
	tableGluaConstMarkerName = "_glua_const_marker"
	// tableGluaConstValueName 是顶层 const 包装表的真实值字段。
	tableGluaConstValueName = "_glua_const_value"
	// maxProtoRegisters 是 Proto.MaxStackSize 可表达的最大寄存器数量。
	maxProtoRegisters = 255
	// initialCodeCapacity 是 codegen Proto 指令和行号表的最小预留容量。
	initialCodeCapacity = 2
	// initialConstantCapacity 是 codegen Proto 常量表的最小预留容量。
	initialConstantCapacity = 1
	// initialLocalVarCapacity 是 codegen Proto 局部变量调试表的最小预留容量。
	initialLocalVarCapacity = 1
)

// CompileChunk 将 parser.Chunk 编译为 Lua 5.3 Proto。
//
// chunk 必须已经通过 parser.ParseChunk 语法和基础语义校验；source 写入 Proto.Source 便于调试输出。
func CompileChunk(chunk *parser.Chunk, source string) (*bytecode.Proto, error) {
	// 创建函数级生成器，并从顶层 block 开始生成指令。
	generator := newGenerator(source)
	// Lua chunk 本身按 vararg 函数执行，CLI 脚本参数会通过 `...` 暴露。
	generator.proto.IsVararg = true
	generator.prepareDirectFunctionBlockCapacity(chunk.Block)
	if err := generator.compileBlock(chunk.Block); err != nil {
		// 编译失败时返回带上下文的错误，调用方不能使用部分 Proto。
		return nil, err
	}
	if err := generator.patchPendingGotos(); err != nil {
		// parser 正常会先完成 goto 合法性校验；这里保留 codegen 防御错误。
		return nil, err
	}
	if !generator.returned {
		// 没有显式 return 时补默认无返回值 RETURN。
		generator.emitABC(bytecode.OpReturn, 0, 1, 0)
	}
	generator.closeLocals(len(generator.proto.Code))
	generator.proto.MaxStackSize = generator.maxStackSize()

	// 返回完整 Proto。
	return generator.proto, nil
}

// generator 保存单个 Proto 的 codegen 状态。
//
// 寄存器、常量池和局部变量映射都只在当前函数原型内有效。
type generator struct {
	// proto 保存正在生成的函数原型。
	proto *bytecode.Proto
	// parent 保存外层函数生成器，用于嵌套函数捕获 upvalue。
	parent *generator
	// childProtoArena 保存当前函数预估直接子 Proto 的内嵌所有权，减少逐个 NewProto 分配。
	childProtoArena []bytecode.Proto
	// childProtoNext 保存 childProtoArena 中下一个可借用的子 Proto 下标。
	childProtoNext int
	// childGeneratorScratch 保存当前函数正在编译的直接子函数 codegen 状态，仅在编译期顺序复用。
	childGeneratorScratch *generator
	// nextRegister 保存下一个可分配寄存器。
	nextRegister int
	// maxRegister 保存生成过程中到达过的最大寄存器数量。
	maxRegister int
	// constants 保存常量去重索引，按常量类型拆分以对齐 Lua 5.3 addk 的原值 key 语义。
	constants constantIndexes
	// localInlineName 保存单局部 inline 槽的名称。
	localInlineName string
	// localInlineBinding 保存单局部 inline 槽的绑定。
	localInlineBinding localBinding
	// localInlineValid 表示单局部 inline 槽当前是否有效。
	localInlineValid bool
	// locals 保存当前函数第二个及之后局部变量到寄存器的 overflow 映射。
	locals map[string]localBinding
	// upvalues 保存当前函数已捕获 upvalue 到 upvalue 索引的映射。
	upvalues map[string]int
	// breakJumps 保存嵌套循环中待回填的 break 跳转列表。
	breakJumps [][]int
	// continueJumps 保存嵌套循环中待回填的 continue 跳转列表。
	continueJumps [][]int
	// loopCloseRegisters 保存每层循环跳出时 OP_JMP A 字段需要使用的 close 起点。
	loopCloseRegisters []int
	// labelPCs 保存当前函数内已遇到的 label 名称到目标 PC 和作用域水位列表的映射。
	labelPCs map[string][]labelInfo
	// pendingGotos 保存当前函数内尚未回填的 goto 占位跳转。
	pendingGotos []pendingGoto
	// globalConsts 保存顶层 chunk const 声明出的只读全局名称，用于编译期拦截同 chunk 后续写入。
	globalConsts map[string]lexer.Position
	// scopes 按 parser scope ID 保存当前函数内 block 作用域，供 goto/label 可见性解析。
	scopes map[int]*parser.ScopeInfo
	// inlineScopeStack 保存最常见的函数顶层 block 作用域，避免为 scopeStack 单独分配底层数组。
	inlineScopeStack [1]*parser.ScopeInfo
	// scopeStack 保存当前正在生成的 block 作用域栈。
	scopeStack []*parser.ScopeInfo
	// returned 表示当前函数已经生成显式 return 或 tail call 终结指令。
	returned bool
	// currentLine 保存当前正在编译的源码行，用于给后续 emit 的指令填充 LineInfo。
	currentLine int
}

// localBinding 描述 codegen 阶段的局部变量绑定。
//
// register 保存局部变量所在寄存器，localVarIndex 保存对应 Proto.LocalVars 下标。
type localBinding struct {
	// register 保存局部变量寄存器。
	register int
	// localVarIndex 保存调试局部变量表下标。
	localVarIndex int
	// scopeID 保存声明该局部变量的 parser 作用域编号，用于区分同作用域重名和内层遮蔽。
	scopeID int
	// constLocal 表示该 local 由 const 语法糖声明，不允许后续赋值覆盖。
	constLocal bool
	// captured 表示该局部变量已被子函数捕获为 open upvalue。
	captured bool
}

// lookupLocal 返回当前函数内名称对应的可见局部变量绑定。
//
// 先查单局部 inline 槽，再查 overflow map；调用方不需要关心当前函数是否已经创建 map。
func (generator *generator) lookupLocal(name string) (localBinding, bool) {
	if generator.localInlineValid && generator.localInlineName == name {
		// 命中 inline 槽时直接返回，避免单 local 函数创建 map。
		return generator.localInlineBinding, true
	}
	// nil map 读取与空 map 等价，直接返回 overflow 查找结果。
	binding, ok := generator.locals[name]
	return binding, ok
}

// setLocal 写入当前函数内名称对应的可见局部变量绑定。
//
// 第一个局部绑定优先使用 inline 槽；同名更新保持原槽位，第二个及之后名称进入 overflow map。
func (generator *generator) setLocal(name string, binding localBinding) {
	if generator.localInlineValid && generator.localInlineName == name {
		// 同名重声明或 captured 标记写回必须更新原 inline 槽。
		generator.localInlineBinding = binding
		return
	}
	if _, exists := generator.locals[name]; exists {
		// 已经位于 overflow map 的绑定保持在 map 中，避免同名同时存在两份状态。
		generator.locals[name] = binding
		return
	}
	if !generator.localInlineValid && len(generator.locals) == 0 {
		// 第一个局部变量使用 inline 槽，覆盖 compile_3000_functions 的单参数子函数热形态。
		generator.localInlineName = name
		generator.localInlineBinding = binding
		generator.localInlineValid = true
		return
	}
	if generator.locals == nil {
		// 第二个不同名称局部变量才创建 overflow map，保留多 local 的完整名称解析能力。
		generator.locals = make(map[string]localBinding)
	}
	generator.locals[name] = binding
}

// forEachLocal 遍历当前函数内所有可见局部变量绑定。
//
// 调用方不能依赖遍历顺序；inline 槽和 overflow map 都只是当前函数可见绑定集合。
func (generator *generator) forEachLocal(fn func(name string, binding localBinding)) {
	if generator.localInlineValid {
		// inline 槽是可见局部集合的一部分，必须和 overflow map 一起参与生命周期处理。
		fn(generator.localInlineName, generator.localInlineBinding)
	}
	for name, binding := range generator.locals {
		// 把 overflow 绑定交给调用方处理，保持原 map 遍历行为。
		fn(name, binding)
	}
}

// localCount 返回当前函数可见局部变量绑定数量。
func (generator *generator) localCount() int {
	count := len(generator.locals)
	if generator.localInlineValid {
		// inline 槽有效时需要计入可见局部数量。
		count++
	}
	return count
}

// snapshotLocals 复制当前函数 overflow 局部变量绑定。
//
// inline 槽由 scopeSnapshot 单独保存；空 overflow map 返回 nil，保持既有 map 快照行为。
func (generator *generator) snapshotLocals() map[string]localBinding {
	if len(generator.locals) == 0 {
		// 没有 overflow 绑定时无需创建快照 map。
		return nil
	}
	locals := make(map[string]localBinding, len(generator.locals))
	for name, binding := range generator.locals {
		// 浅拷贝 overflow 绑定，退出作用域时恢复外层同名变量。
		locals[name] = binding
	}
	return locals
}

// restoreLocalSnapshot 恢复 block 入口处保存的局部变量绑定快照。
func (generator *generator) restoreLocalSnapshot(snapshot scopeSnapshot) {
	// inline 槽和 overflow map 必须一起恢复，确保退出 block 后外层遮蔽关系完全回到入口状态。
	generator.localInlineName = snapshot.localInlineName
	generator.localInlineBinding = snapshot.localInlineBinding
	generator.localInlineValid = snapshot.localInlineValid
	generator.locals = snapshot.locals
}

// constantIndexes 保存当前 Proto 的常量去重索引。
//
// Lua 5.3 addk 使用常量原值作为索引 key，并额外区分 integer 与 float；这里按类型拆分 map，
// 避免为每个常量构造格式化字符串，同时避免使用完整 Constant 结构体作为大 map key。
type constantIndexes struct {
	// hasNil 表示 nil 常量是否已经进入常量表。
	hasNil bool
	// nilIndex 保存 nil 常量表下标。
	nilIndex int
	// hasBool 表示 false/true 常量是否已经进入常量表。
	hasBool [2]bool
	// boolIndex 保存 false/true 常量表下标。
	boolIndex [2]int
	// hasInlineInteger 表示 integer inline 槽是否已保存首个 integer 常量。
	hasInlineInteger bool
	// inlineIntegerValue 保存首个 integer 常量原值。
	inlineIntegerValue int64
	// inlineIntegerIndex 保存首个 integer 常量表下标。
	inlineIntegerIndex int
	// integers 保存第二个及之后不同 integer 原值到常量表下标的 overflow 映射。
	integers map[int64]int
	// numbers 保存 float number 原值到常量表下标的映射。
	numbers map[float64]int
	// strings 保存 string 字节序列到常量表下标的映射。
	strings map[string]int
}

// pendingGoto 描述 codegen 阶段尚未确定目标 PC 的 goto 跳转。
//
// label 保存目标 label 名称；jumpPC 保存已经生成的 JMP 指令位置。
type pendingGoto struct {
	// label 保存 goto 目标 label 名称。
	label string
	// jumpPC 保存待回填 JMP 指令位置。
	jumpPC int
	// sourceNextRegister 保存 goto 发出时的寄存器水位，用于判断是否跳出局部作用域。
	sourceNextRegister int
	// sourceScope 保存 goto 所在 block 作用域。
	sourceScope *parser.ScopeInfo
}

// labelInfo 描述 codegen 阶段 label 的跳转目标和作用域水位。
//
// pc 是 label 对应的下一条指令位置；nextRegister 是 label 处可见局部变量之后的寄存器水位。
type labelInfo struct {
	// pc 保存 label 目标指令位置。
	pc int
	// nextRegister 保存 label 处的寄存器水位。
	nextRegister int
	// scope 保存 label 所在 block 作用域。
	scope *parser.ScopeInfo
}

// assignmentTarget 描述普通赋值左值已经求值后的写入位置。
//
// name 非空表示名称左值；tableRegister 非负表示 table/index 左值。keyOperand 是 SETTABLE
// 使用的 RK key 操作数，keyRegister 仅在 key 常量溢出 RK 范围时保存临时寄存器下标。
type assignmentTarget struct {
	// name 保存名称左值；空字符串表示不是名称写回。
	name string
	// position 保存名称左值源码位置，用于 upvalue 上限错误行号。
	position lexer.Position
	// resolvedUpvalue 保存左值地址分析阶段已解析的 upvalue 下标，-1 表示未预解析。
	resolvedUpvalue int
	// resolvedEnv 表示左值是未声明 `_ENV`，写回时直接替换当前环境 upvalue。
	resolvedEnv bool
	// tableRegister 保存 table/index 左值的接收者寄存器。
	tableRegister int
	// keyOperand 保存 SETTABLE 使用的 key 操作数。
	keyOperand int
	// keyRegister 保存 key 常量溢出时额外分配的寄存器，-1 表示未分配。
	keyRegister int
}

// scopeSnapshot 保存进入嵌套 block 前的局部变量和寄存器状态。
//
// inline local 和 locals 都是函数级局部绑定的浅拷贝；localVarCount 用于关闭本作用域新增
// 调试局部变量；nextRegister 用于退出作用域后复用内层临时和局部寄存器。
type scopeSnapshot struct {
	// localInlineName 保存进入 block 前 inline 局部变量名称。
	localInlineName string
	// localInlineBinding 保存进入 block 前 inline 局部变量绑定。
	localInlineBinding localBinding
	// localInlineValid 表示进入 block 前 inline 局部变量是否有效。
	localInlineValid bool
	// locals 保存进入 block 前 overflow 局部变量绑定。
	locals map[string]localBinding
	// localVarCount 保存进入 block 前 Proto.LocalVars 数量。
	localVarCount int
	// nextRegister 保存进入 block 前下一个可分配寄存器。
	nextRegister int
}

// newGenerator 创建函数级 codegen 状态。
//
// source 会写入 Proto.Source；返回值已初始化常量表索引和局部变量表。
func newGenerator(source string) *generator {
	// 初始化最小状态，寄存器从 0 开始按 Lua VM 约定分配。
	proto := bytecode.NewProto(source)
	prepareCodegenProto(proto)
	return &generator{
		proto: proto,
	}
}

// prepareCodegenProto 为 codegen 新建或借用的 Proto 准备短表容量。
//
// proto 必须是调用方独占的可写原型；该 helper 不改变已有切片长度，只为 compiler 热路径准备内嵌槽。
func prepareCodegenProto(proto *bytecode.Proto) {
	// codegen 创建的 Proto 显式 opt-in 短表内嵌槽，binary chunk loader 和手写 Proto 不受影响。
	proto.PrepareInlineCodeLineInfo(initialCodeCapacity)
	proto.PrepareInlineConstants(initialConstantCapacity)
	proto.PrepareInlineLocalVars(initialLocalVarCapacity)
}

// newChildGenerator 创建嵌套函数 codegen 状态。
//
// child 使用独立 Proto、寄存器和常量池，但保留 parent 用于 upvalue 捕获。
func newChildGenerator(parent *generator, source string) *generator {
	// 子函数状态与顶层一致，只额外记录 parent。
	return parent.borrowChildGenerator(source)
}

// borrowChildGenerator 返回当前生成器拥有的下一个子函数 codegen 状态。
//
// 子函数按源码顺序同步编译，调用方不会在一个子函数编译未完成时再借用同一父函数的下一个 generator；
// 因此可复用父 generator 内嵌 scratch，避免为大量直接函数声明预留一整段 generator 数组。
func (parent *generator) borrowChildGenerator(source string) *generator {
	if parent != nil {
		// 直接子函数的 codegen 状态只在当前调用栈内使用，最终 Proto 只保存 child.proto。
		if parent.childGeneratorScratch == nil {
			// 首次借用时只分配一个 scratch；后续直接子函数顺序复用同一对象。
			parent.childGeneratorScratch = &generator{}
		}
		child := parent.childGeneratorScratch
		*child = generator{proto: parent.borrowChildProto(source), parent: parent}
		return child
	}

	// 预估之外的子函数保持原独立 generator 分配路径。
	return &generator{proto: parent.borrowChildProto(source), parent: parent}
}

// borrowChildProto 返回当前生成器拥有的下一个子函数 Proto。
//
// 预估容量命中时返回 childProtoArena 中的稳定元素指针；未命中时回退 NewProto，保持复杂嵌套 block 的语义。
func (generator *generator) borrowChildProto(source string) *bytecode.Proto {
	if generator != nil && generator.childProtoNext < len(generator.childProtoArena) {
		// 直接子函数使用父 generator 的精确 Proto arena，最终 Protos 仍保存普通 *Proto。
		proto := &generator.childProtoArena[generator.childProtoNext]
		generator.childProtoNext++
		*proto = bytecode.Proto{Source: source}
		prepareCodegenProto(proto)
		return proto
	}

	// 预估之外的嵌套 block 子函数保持原 NewProto 路径，避免复杂作用域下错误复用 arena。
	proto := bytecode.NewProto(source)
	prepareCodegenProto(proto)
	return proto
}

// prepareDirectFunctionBlockCapacity 按当前 block 的直接函数声明预留 codegen 短表容量。
//
// block 为 nil 或没有直接函数声明时保持现有短槽；该方法只改变容量，不改变 Proto.p、Code、LineInfo 或
// Constants 的长度、顺序、CLOSURE Bx 索引或 binary chunk 输出。
func (gen *generator) prepareDirectFunctionBlockCapacity(block *parser.Block) {
	if gen == nil || gen.proto == nil || block == nil {
		// 缺少生成器或 block 时没有可预留对象，保持调用方状态不变。
		return
	}
	stats := directFunctionBlockStatsFor(block)
	if stats.instructionCapacity > cap(gen.proto.Code) && len(gen.proto.Code) == 0 && len(gen.proto.LineInfo) == 0 {
		// 直接函数声明会稳定产生 CLOSURE/SETGLOBAL 指令；只在尚未写入指令前扩大容量。
		gen.proto.PrepareInlineCodeLineInfo(stats.instructionCapacity)
	}
	if stats.nameConstantCapacity > cap(gen.proto.Constants) && len(gen.proto.Constants) == 0 {
		// 普通 function 名称会进入当前 Proto 常量表；只预留容量，不写入常量。
		gen.proto.PrepareInlineConstants(stats.nameConstantCapacity)
	}
	if stats.nameConstantCapacity > 0 && gen.constants.strings == nil {
		// 普通 function 名称同时会登记到 string 常量索引；按已知数量预留 map，避免 3000 函数场景逐步扩容。
		gen.constants.strings = make(map[string]int, stats.nameConstantCapacity)
	}
	if stats.childCount > 0 && len(gen.proto.Protos) == 0 && cap(gen.proto.Protos) == 0 {
		// 直接函数声明会在当前 Proto.p 追加子 Proto；无子函数时保持 nil 切片。
		gen.proto.Protos = make([]*bytecode.Proto, 0, stats.childCount)
	}
	if stats.childCount > 0 && gen.childProtoArena == nil {
		// 直接子函数 Proto 本体由父 generator 精确持有，避免每个子函数一次 NewProto 对象分配。
		gen.childProtoArena = make([]bytecode.Proto, stats.childCount)
	}
}

// directFunctionBlockStats 保存当前 block 直接函数声明会触发的保守容量预估。
type directFunctionBlockStats struct {
	// childCount 保存当前 Proto.p 会追加的直接子 Proto 数量。
	childCount int
	// instructionCapacity 保存当前 block 直接函数声明至少需要的指令容量，包含默认 RETURN 余量。
	instructionCapacity int
	// nameConstantCapacity 保存普通 function 名称可能写入当前常量表的容量。
	nameConstantCapacity int
}

// directFunctionBlockStatsFor 统计当前 block 直接语句中的函数声明容量需求。
//
// 该统计只覆盖 codegen 会直接追加到当前 Proto 的 `function` 和 `local function` 语句；嵌套 block
// 或函数表达式由各自编译路径自然扩容，避免为了保守预留遍历完整 AST。
func directFunctionBlockStatsFor(block *parser.Block) directFunctionBlockStats {
	if block == nil {
		// nil block 没有语句，调用方无需预留任何容量。
		return directFunctionBlockStats{}
	}
	stats := directFunctionBlockStats{}
	for _, statement := range block.Statements {
		switch statement.(type) {
		case *parser.FunctionStatement, *parser.CompactFunctionStatement:
			// 普通 function 会追加一个子 Proto，并在常见全局写入路径产生 CLOSURE 和 SETGLOBAL。
			stats.childCount++
			stats.instructionCapacity += 2
			stats.nameConstantCapacity++
		case *parser.LocalFunctionStatement:
			// 直接函数声明会在当前 Proto.p 追加一个子 Proto。
			stats.childCount++
			stats.instructionCapacity++
		default:
			// 其他语句不在当前层直接追加函数声明 Proto，保持保守容量预估。
		}
	}
	if stats.instructionCapacity > 0 {
		// 多数函数声明 block 最后还会补默认 RETURN；即使已有显式 return，额外容量只影响预留。
		stats.instructionCapacity++
	}
	return stats
}

// compileBlock 编译一个 block。
//
// 当前阶段支持 local 赋值、普通赋值和 return；控制流会在后续 TODO 中接入。
func (generator *generator) compileBlock(block *parser.Block) error {
	generator.pushScope(block)
	defer generator.popScope()
	for _, statement := range block.Statements {
		// 逐条语句按源码顺序生成指令。
		if err := generator.withSourceLine(statement.Pos(), func() error {
			// 语句内生成的指令继承该语句起始行，嵌套 block 会继续覆盖自己的语句行。
			return generator.compileStatement(statement)
		}); err != nil {
			// 任一语句生成失败都会终止当前 block。
			return err
		}
	}
	if block.Return != nil {
		// return 是 block 终结语句，必须在普通语句之后生成。
		return generator.withSourceLine(block.Return.Pos(), func() error {
			// return 表达式和 RETURN 指令都归属 return 关键字所在行。
			return generator.compileReturn(block.Return)
		})
	}

	// 没有 return 时由调用方补默认 RETURN。
	return nil
}

// pushScope 进入一个 parser block 作用域。
//
// block.Scope 由 parser 语义阶段标注；codegen 使用该作用域解析同名 label 的可见目标。
func (generator *generator) pushScope(block *parser.Block) {
	var scope *parser.ScopeInfo
	if block == nil || block.Scope == nil {
		// 缺少作用域信息时仍允许生成，但后续 goto 只能走防御错误。
		scope = nil
	} else {
		// 记录当前 block 的 parser 作用域，供局部变量和 goto/label 使用。
		scope = block.Scope
	}
	if generator.scopes != nil && scope != nil {
		// 已经遇到 goto 后才需要维护作用域父链索引。
		generator.scopes[scope.ID] = scope
	}
	if len(generator.scopeStack) == 0 {
		// 单 block 函数是常见路径，首个作用域复用生成器内嵌槽。
		generator.inlineScopeStack[0] = scope
		generator.scopeStack = generator.inlineScopeStack[:1]
		return
	}

	// 嵌套 block 沿用普通切片扩展，保持递归路径顺序。
	generator.scopeStack = append(generator.scopeStack, scope)
}

// popScope 离开当前 parser block 作用域。
//
// 作用域栈只服务 codegen 当前递归路径，退出 block 后恢复外层作用域。
func (generator *generator) popScope() {
	if len(generator.scopeStack) == 0 {
		// 空栈说明调用方嵌套不平衡，防御性忽略。
		return
	}
	generator.scopeStack = generator.scopeStack[:len(generator.scopeStack)-1]
}

// ensureScopeIndex 按需创建 goto 作用域父链索引。
//
// 普通函数没有 goto 解析需求；首次遇到 goto 时才把当前作用域栈回填到索引中，
// 后续进入的新 block 会由 pushScope 增量维护。
func (generator *generator) ensureScopeIndex() {
	if generator.scopes != nil {
		// 已初始化时无需重复回填，避免热路径做多余 map 写入。
		return
	}
	generator.scopes = make(map[int]*parser.ScopeInfo)
	for _, scope := range generator.scopeStack {
		if scope == nil {
			// 缺失作用域信息的栈帧不能参与父链解析。
			continue
		}
		generator.scopes[scope.ID] = scope
	}
}

// currentScope 返回当前正在生成的 block 作用域。
//
// 返回 nil 表示 parser 未提供 scope 信息，调用方应保守延迟到最终校验。
func (generator *generator) currentScope() *parser.ScopeInfo {
	if len(generator.scopeStack) == 0 {
		// 顶层 compileBlock 进入前理论上不会查询；保留防御 nil。
		return nil
	}
	return generator.scopeStack[len(generator.scopeStack)-1]
}

// setUpvalue 记录名称到当前 Proto upvalue 下标的映射。
//
// 大多数函数没有 upvalue；首次真实捕获或登记 `_ENV` 时才分配去重表。
func (generator *generator) setUpvalue(name string, index int) {
	if generator.upvalues == nil {
		// 只有需要写入时才创建 map；nil map 读取已等价于空 map。
		generator.upvalues = make(map[string]int)
	}
	generator.upvalues[name] = index
}

// withSourceLine 在指定源码行上下文中执行 codegen 动作。
//
// position 来自 AST 节点起始 token；当行号有效时后续 emit 会把它写入 Proto.LineInfo。action
// 返回后恢复原行号，避免嵌套 block 或子表达式污染外层语句的调试信息。
func (generator *generator) withSourceLine(position lexer.Position, action func() error) error {
	previousLine := generator.currentLine
	if position.Line > 0 {
		// 只接受有效源码行号；零值表示 parser 未提供位置。
		generator.currentLine = position.Line
	}
	err := action()
	generator.currentLine = previousLine
	return err
}

// compileScopedBlock 编译会引入独立 local 生命周期的嵌套 block。
//
// block 可为 nil；进入前会保存外层 locals 和寄存器水位，退出时关闭本 block 新增 local
// 并恢复外层绑定，保证 `do local x end`、if/loop 体内 local 不泄漏到外层。
func (generator *generator) compileScopedBlock(block *parser.Block) error {
	scope := generator.beginScope()
	if block == nil {
		// nil block 没有语句，直接恢复外层作用域。
		generator.endScope(scope, len(generator.proto.Code))
		return nil
	}
	if err := generator.compileBlock(block); err != nil {
		// 编译失败前也恢复作用域，避免调用方继续使用污染后的 generator。
		generator.endScope(scope, len(generator.proto.Code))
		return err
	}
	generator.endScope(scope, len(generator.proto.Code))
	return nil
}

// compileStatement 编译单条语句。
//
// 当前阶段只覆盖局部赋值和已声明局部变量赋值，其他语句返回明确错误。
func (generator *generator) compileStatement(statement parser.Statement) error {
	switch typedStatement := statement.(type) {
	case *parser.DoStatement:
		// do 语句只引入子 block，按源码顺序递归生成内部语句。
		return generator.compileScopedBlock(typedStatement.Body)
	case *parser.LocalAssignmentStatement:
		// local 赋值会为每个局部变量分配稳定寄存器。
		return generator.compileLocalAssignment(typedStatement)
	case *parser.AssignmentStatement:
		// 普通赋值当前只支持写入已经声明的局部名称。
		return generator.compileAssignment(typedStatement)
	case *parser.LocalFunctionStatement:
		// local function 会声明局部函数名并生成子 Proto closure。
		return generator.compileLocalFunction(typedStatement)
	case *parser.FunctionStatement:
		// 普通 function 当前仅支持写入已声明局部或作为全局能力后续扩展。
		return generator.compileFunctionStatement(typedStatement)
	case *parser.CompactFunctionStatement:
		// 编译专用简单 function 直接生成等价 child Proto，避免构造完整函数体 AST。
		return generator.compileCompactFunctionStatement(typedStatement)
	case *parser.IfStatement:
		// if/elseif/else 使用 TEST/JMP 组合生成条件分支。
		return generator.compileIfStatement(typedStatement)
	case *parser.WhileStatement:
		// while 使用 TEST/JMP 组合生成条件循环。
		return generator.compileWhileStatement(typedStatement)
	case *parser.RepeatUntilStatement:
		// repeat-until 先执行循环体，再在尾部测试条件。
		return generator.compileRepeatUntilStatement(typedStatement)
	case *parser.BreakStatement:
		// break 生成待当前循环结束位置回填的 JMP。
		return generator.compileBreakStatement()
	case *parser.FunctionCallStatement:
		// 函数调用语句会生成 CALL 并丢弃返回值。
		return generator.compileFunctionCallStatement(typedStatement)
	case *parser.NumericForStatement:
		// 数值 for 使用 FORPREP/FORLOOP 和连续寄存器区间。
		return generator.compileNumericFor(typedStatement)
	case *parser.GenericForStatement:
		// 泛型 for 使用 TFORCALL/TFORLOOP 和迭代器寄存器区间。
		return generator.compileGenericFor(typedStatement)
	case *parser.LabelStatement:
		// label 不生成指令，只记录当前位置供 goto 回填。
		return generator.compileLabelStatement(typedStatement)
	case *parser.GotoStatement:
		// goto 生成待当前函数 label 位置确定后回填的 JMP。
		return generator.compileGotoStatement(typedStatement)
	case *parser.EmptyStatement:
		// 空语句不产生运行时指令。
		return nil
	default:
		if handled, err := generator.compileExtensionStatement(statement); handled || err != nil {
			// 当前语句已由编译进来的扩展 codegen 处理。
			return err
		}
		return fmt.Errorf("codegen unsupported statement %T", statement)
	}
}

// compileLabelStatement 编译 Lua label 语句。
//
// label 自身没有运行时效果；记录当前 PC 作为后续 goto 的目标位置。
func (generator *generator) compileLabelStatement(statement *parser.LabelStatement) error {
	if generator.labelPCs == nil {
		// 大多数函数没有 label/goto；只有遇到 label 时才分配目标索引。
		generator.labelPCs = make(map[string][]labelInfo)
	}
	generator.labelPCs[statement.Name] = append(generator.labelPCs[statement.Name], labelInfo{
		pc:           len(generator.proto.Code),
		nextRegister: generator.nextRegister,
		scope:        generator.currentScope(),
	})

	// label 记录完成。
	return nil
}

// compileGotoStatement 编译 Lua goto 语句。
//
// goto 先生成 JMP 占位；目标 label 可能位于后文，统一在函数编译结束时回填。
func (generator *generator) compileGotoStatement(statement *parser.GotoStatement) error {
	generator.ensureScopeIndex()
	jumpPC := generator.emitJump(0)
	generator.pendingGotos = append(generator.pendingGotos, pendingGoto{
		label:              statement.Label,
		jumpPC:             jumpPC,
		sourceNextRegister: generator.nextRegister,
		sourceScope:        generator.currentScope(),
	})

	// 所有 goto 等待函数编译结束后统一回填，确保同名 label 按最终作用域可见性解析。
	return nil
}

// compileLocalAssignment 编译 local 变量声明赋值。
//
// 未提供初始化表达式的变量按 Lua 语义生成 nil；多余表达式当前仍会求值并释放临时寄存器。
func (generator *generator) compileLocalAssignment(statement *parser.LocalAssignmentStatement) error {
	if statement.Const && generator.isTopLevelChunkScope() {
		// 顶层 const 是当前 chunk 环境上的只读全局绑定，不分配 local 寄存器。
		return generator.compileGlobalConstAssignment(statement)
	}
	if generator.isTopLevelChunkScope() {
		for _, name := range statement.Names {
			if _, exists := generator.lookupGlobalConst(name); exists {
				// 顶层同名 local 会遮蔽只读全局常量，按 const 语义拒绝。
				return constAssignmentError(name, statement.Position)
			}
		}
	}
	targetRegisters := make([]int, 0, len(statement.Names))
	for range statement.Names {
		// 每个 local 名称先占用一个长期寄存器，但暂不进入作用域，保证初始化 RHS 读取外层同名变量。
		register := generator.allocateRegister()
		targetRegisters = append(targetRegisters, register)
	}
	for index, register := range targetRegisters {
		if index < len(statement.Values) {
			if _, ok := statement.Values[index].(*parser.VarargExpression); ok && index == len(statement.Values)-1 {
				// 初始化列表最后一个 `...` 会展开到剩余 local 变量，固定数量字段为实际数量加一。
				generator.emitABC(bytecode.OpVararg, register, len(targetRegisters)-index+1, 0)
				break
			}
			if callExpression, ok := statement.Values[index].(*parser.FunctionCallExpression); ok && index == len(statement.Values)-1 {
				// 初始化列表最后一个函数调用会展开到剩余 local 变量，兼容 `local a,b = f()`。
				if err := generator.compileFunctionCallTo(callExpression, register, len(targetRegisters)-index); err != nil {
					// 函数调用初始化失败时保留原始错误上下文。
					return err
				}
				break
			}
			if callExpression, ok := statement.Values[index].(*parser.MethodCallExpression); ok && index == len(statement.Values)-1 {
				// 初始化列表最后一个方法调用同样会展开到剩余 local 变量，兼容 `local a,b = obj:f()`。
				if err := generator.compileMethodCallTo(callExpression, register, len(targetRegisters)-index); err != nil {
					// 方法调用初始化失败时保留原始错误上下文。
					return err
				}
				break
			}
			// 有对应初始化表达式时，直接生成到目标寄存器。
			compileValue := func() error {
				return generator.compileExpressionTo(statement.Values[index], register)
			}
			if functionExpression, ok := statement.Values[index].(*parser.FunctionExpression); ok {
				// Lua 5.3 对 local 函数表达式初始化的 line hook 会先报告函数 end 行，此时 local 名称尚不可见。
				compileValue = func() error {
					return generator.withSourceLine(functionExpressionClosureLine(functionExpression), func() error {
						return generator.compileExpressionTo(functionExpression, register)
					})
				}
			}
			if err := compileValue(); err != nil {
				// 初始化表达式失败时保留原始错误上下文。
				return err
			}
			continue
		}
		// 没有对应初始化表达式时，Lua local 默认初始化为 nil。
		generator.emitABC(bytecode.OpLoadNil, register, 0, 0)
	}
	for expressionIndex := len(targetRegisters); expressionIndex < len(statement.Values); expressionIndex++ {
		// 多余表达式仍需要求值以保留潜在副作用，当前表达式子集无副作用但保持结构兼容。
		tempRegister := generator.allocateRegister()
		if err := generator.compileExpressionTo(statement.Values[expressionIndex], tempRegister); err != nil {
			// 多余表达式编译失败时返回错误。
			return err
		}
		generator.releaseRegister(tempRegister)
	}
	if len(targetRegisters) > 0 {
		// 初始化 RHS 可能为了函数调用实参占用 target 之后的临时寄存器；local 声明完成前回收这些临时槽，
		// 保证后续 local 继续使用连续栈槽，binary chunk 的 LocVar 隐式寄存器顺序依赖该语义。
		generator.releaseRegistersFrom(targetRegisters[0] + len(targetRegisters))
	}
	for index, name := range statement.Names {
		// 初始化表达式全部完成后再登记局部变量，匹配 Lua 5.3 local 作用域从声明之后开始。
		generator.defineLocalWithConst(name, targetRegisters[index], statement.Position, statement.Const)
	}

	// local 声明编译完成。
	return nil
}

// compileGlobalConstAssignment 编译顶层 const 声明为当前环境上的只读全局字段。
//
// 顶层 chunk 的 const 不进入 local 寄存器表；它会构造 `_glua_const` table 并写入当前 `_ENV`，由
// runtime table 投影为 ROOT 字段并登记只读 key。RHS 求值仍遵守 Lua 多赋值规则。
func (generator *generator) compileGlobalConstAssignment(statement *parser.LocalAssignmentStatement) error {
	firstTempRegister := generator.nextRegister
	valueRegisters := make([]int, 0, len(statement.Names))
	for range statement.Names {
		// 每个 const 名称先占一个临时值寄存器，确保 RHS 全部求完后再统一写入环境表。
		valueRegisters = append(valueRegisters, generator.allocateRegister())
	}
	if err := generator.compileAssignmentValues(statement.Values, len(statement.Names), valueRegisters); err != nil {
		// RHS 编译失败时释放所有临时值寄存器。
		generator.releaseRegistersFrom(firstTempRegister)
		return err
	}
	tableRegister := generator.allocateRegister()
	generator.emitABC(bytecode.OpNewTable, tableRegister, 0, 0)
	for index, name := range statement.Names {
		if _, exists := generator.lookupGlobalConst(name); exists {
			// 同一顶层 chunk 内重复定义 const 会让只读绑定含义不稳定，直接拒绝。
			generator.releaseRegistersFrom(firstTempRegister)
			return constAssignmentError(name, statement.Position)
		}
		wrapperRegister := generator.allocateRegister()
		generator.emitABC(bytecode.OpNewTable, wrapperRegister, 0, 0)
		markerRegister := generator.allocateRegister()
		generator.emitABC(bytecode.OpLoadBool, markerRegister, 1, 0)
		if err := generator.emitSetTableStringKey(wrapperRegister, tableGluaConstMarkerName, markerRegister); err != nil {
			// marker 字段写入失败时释放临时寄存器并返回。
			generator.releaseRegistersFrom(firstTempRegister)
			return err
		}
		generator.releaseRegister(markerRegister)
		if err := generator.emitSetTableStringKey(wrapperRegister, tableGluaConstValueName, valueRegisters[index]); err != nil {
			// value 字段写入失败时释放临时寄存器并返回。
			generator.releaseRegistersFrom(firstTempRegister)
			return err
		}
		if err := generator.emitSetTableStringKey(tableRegister, name, wrapperRegister); err != nil {
			// const 名称无法编码时释放临时寄存器并返回。
			generator.releaseRegistersFrom(firstTempRegister)
			return err
		}
		generator.releaseRegister(wrapperRegister)
		generator.defineGlobalConst(name, statement.Position)
	}
	if err := generator.emitSetGlobalNameOperand(tableGluaConstName, tableRegister); err != nil {
		// `_glua_const` 写入失败时释放整段临时寄存器。
		generator.releaseRegistersFrom(firstTempRegister)
		return err
	}
	generator.releaseRegistersFrom(firstTempRegister)
	return nil
}

// emitSetTableStringKey 生成 `table[stringKey] = valueOperand`。
func (generator *generator) emitSetTableStringKey(tableRegister int, stringKey string, valueOperand int) error {
	keyIndex := generator.addConstant(bytecode.StringConstant(stringKey))
	keyOperand, keyRegister, err := generator.rkOperandForConstantIndex(keyIndex)
	if err != nil {
		// 字符串 key 无法进入 RK 操作数时返回错误，调用方负责释放值寄存器。
		return err
	}
	generator.emitABC(bytecode.OpSetTable, tableRegister, keyOperand, valueOperand)
	generator.releaseOptionalRegister(keyRegister)
	return nil
}

// compileAssignment 编译普通赋值语句。
//
// 当前阶段左侧支持已经声明的局部变量名称，未知名称按 Lua 5.3 语义写入 `_ENV[name]`。
func (generator *generator) compileAssignment(statement *parser.AssignmentStatement) error {
	if handled, err := generator.compileSingleLocalFieldSelfBinaryAssignment(statement); handled || err != nil {
		// 单 local table 字段自二元赋值可直接生成 GETTABLE/op/SETTABLE，避免 receiver 临时 MOVE。
		return err
	}
	if handled, err := generator.compileSingleSafeTableAssignment(statement); handled || err != nil {
		// 单 table 索引赋值在 receiver/key/value 均安全时可直接生成 SETTABLE，避免临时 MOVE。
		return err
	}
	if handled, err := generator.compileSingleGlobalSafeAssignment(statement); handled || err != nil {
		// 单全局安全 RHS 可直接生成 SETTABUP/SETTABLE，避免通用赋值临时值和 MOVE。
		return err
	}
	if handled, err := generator.compileSingleLocalSafeAssignment(statement); handled || err != nil {
		// 单 local 安全 RHS 可直接写回目标寄存器，避免通用赋值临时值和 MOVE。
		return err
	}
	if handled, err := generator.compileSingleLocalSelfBinaryAssignment(statement); handled || err != nil {
		// 单 local 自二元运算赋值可直接写回目标寄存器，避免通用赋值临时结果和 MOVE。
		return err
	}
	if handled, err := generator.compileSingleLocalSelfConcatAssignment(statement); handled || err != nil {
		// 单 local 自拼接赋值可直接写回目标寄存器，避免临时结果和 MOVE。
		return err
	}
	if handled, err := generator.compileSingleLocalBinaryAssignment(statement); handled || err != nil {
		// 单 local 二元表达式赋值可在 RHS 完成后直接写回目标寄存器，避免最终 MOVE。
		return err
	}

	firstTempRegister := generator.nextRegister
	targets := make([]assignmentTarget, 0, len(statement.Left))
	for _, leftExpression := range statement.Left {
		// 左值中的 receiver 和 index 表达式必须先求值，写回延后到 RHS 全部完成之后。
		target, err := generator.compileAssignmentTarget(leftExpression)
		if err != nil {
			// 左值地址编译失败时释放本语句临时寄存器并返回。
			generator.releaseRegistersFrom(firstTempRegister)
			return err
		}
		targets = append(targets, target)
	}

	valueRegisters := make([]int, 0, len(statement.Left))
	for range statement.Left {
		// RHS 结果统一写入独立临时寄存器，避免提前覆盖任一左值或后续 RHS 读取。
		valueRegisters = append(valueRegisters, generator.allocateRegister())
	}
	if err := generator.compileAssignmentValues(statement.Right, len(statement.Left), valueRegisters); err != nil {
		// RHS 编译失败时释放所有 LHS/RHS 临时寄存器。
		generator.releaseRegistersFrom(firstTempRegister)
		return err
	}

	for index, target := range targets {
		// 所有 RHS 求值完成后才按左到右顺序写回左值，匹配 Lua 5.3 赋值语义。
		if err := generator.emitAssignmentTarget(target, valueRegisters[index]); err != nil {
			// 写回失败时释放临时寄存器并返回。
			generator.releaseRegistersFrom(firstTempRegister)
			return err
		}
	}
	generator.releaseRegistersFrom(firstTempRegister)

	// 普通赋值编译完成。
	return nil
}

// compileSingleGlobalSafeAssignment 优化 `globalName = literal/localName`。
//
// 该优化只处理单左值、单右值，左值必须解析为真正全局名，RHS 必须是字面量或当前 local；
// upvalue、local `_ENV` 捕获、多重赋值和有副作用 RHS 都回退通用路径以保持 Lua 5.3 语义。
func (generator *generator) compileSingleGlobalSafeAssignment(statement *parser.AssignmentStatement) (bool, error) {
	if len(statement.Left) != 1 || len(statement.Right) != 1 {
		// 多重赋值必须先求完所有 RHS，再统一写回。
		return false, nil
	}
	targetName, ok := statement.Left[0].(*parser.NameExpression)
	if !ok {
		// 非名称左值不属于全局名写入。
		return false, nil
	}
	if _, exists := generator.lookupLocal(targetName.Name); exists {
		// 当前 local 写入交给 local 快路径或通用路径。
		return false, nil
	}
	if targetName.Name == envUpvalueName {
		// `_ENV = value` 是环境 upvalue 写入，不是 `_ENV["_ENV"]` 全局字段写入。
		return false, nil
	}
	if !generator.isSafeRKCandidate(statement.Right[0]) {
		// RHS 可能有副作用或需要运行期查找时回退通用路径。
		return false, nil
	}
	if _, captured, err := generator.resolveUpvalue(targetName.Name, targetName.Position); err != nil || captured {
		if err != nil {
			// upvalue 解析错误需要按原始编译错误返回。
			return true, err
		}
		// 外层 local 捕获为 upvalue 时不能当作全局写入。
		return false, nil
	}
	if _, exists := generator.lookupGlobalConst(targetName.Name); exists {
		// 顶层 const 全局不允许通过全局写入快路径覆盖。
		return true, constAssignmentError(targetName.Name, targetName.Position)
	}
	valueOperand, valueRegister, err := generator.safeRKOperandWithRegister(statement.Right[0])
	if err != nil {
		// RHS 常量装载失败时返回编译错误。
		return true, err
	}
	if err := generator.emitSetGlobalNameOperand(targetName.Name, valueOperand); err != nil {
		// 全局 key 无法编码时释放 RHS 临时寄存器并返回。
		generator.releaseOptionalRegister(valueRegister)
		return true, err
	}
	generator.releaseOptionalRegister(valueRegister)
	return true, nil
}

// compileSingleLocalSafeAssignment 优化 `localName = literal/localName`。
//
// 该优化只处理单左值、单右值，且 RHS 必须是字面量或当前 local 名称；不处理全局、upvalue、调用、
// table 访问和多重赋值，避免改变 Lua 5.3 的求值顺序、副作用和 upvalue 捕获时机。
func (generator *generator) compileSingleLocalSafeAssignment(statement *parser.AssignmentStatement) (bool, error) {
	if len(statement.Left) != 1 || len(statement.Right) != 1 {
		// 多重赋值必须先求完所有 RHS，再统一写回。
		return false, nil
	}
	targetName, ok := statement.Left[0].(*parser.NameExpression)
	if !ok {
		// 非名称左值需要保留普通赋值地址求值语义。
		return false, nil
	}
	binding, ok := generator.lookupLocal(targetName.Name)
	if !ok {
		// upvalue/global 写回不进入该快路径。
		return false, nil
	}
	if binding.constLocal {
		// const 绑定不允许通过安全赋值快路径覆盖。
		return true, constAssignmentError(targetName.Name, targetName.Position)
	}
	if !generator.isSafeDirectLocalAssignmentValue(statement.Right[0]) {
		// 右侧可能有副作用或需要运行期查找时回退通用路径。
		return false, nil
	}
	if err := generator.compileExpressionTo(statement.Right[0], binding.register); err != nil {
		// RHS 编译失败时保持原始错误。
		return true, err
	}
	return true, nil
}

// isSafeDirectLocalAssignmentValue 判断 RHS 是否可直接写入目标 local。
func (generator *generator) isSafeDirectLocalAssignmentValue(expression parser.Expression) bool {
	if _, ok := expression.(*parser.LiteralExpression); ok {
		// 字面量没有副作用，直接 LOAD 到目标 local 等价于通用赋值。
		return true
	}
	nameExpression, ok := expression.(*parser.NameExpression)
	if !ok {
		// 其他表达式可能触发调用、索引或全局读取。
		return false
	}
	_, exists := generator.lookupLocal(nameExpression.Name)
	return exists
}

// compileSingleSafeTableAssignment 优化 `tableExpression[keyExpression] = valueExpression`。
//
// 该优化只处理单左值、单右值，且 receiver 必须是当前 local 名称；key/value 只允许 local 名称
// 或字面量。复杂表达式仍走通用赋值路径，以保留 Lua 5.3 的左值和右值求值顺序及副作用语义。
func (generator *generator) compileSingleSafeTableAssignment(statement *parser.AssignmentStatement) (bool, error) {
	if len(statement.Left) != 1 || len(statement.Right) != 1 {
		// 多重赋值必须先求完所有 RHS，再统一写回，不能使用该快路径。
		return false, nil
	}
	indexExpression, ok := statement.Left[0].(*parser.IndexExpression)
	if !ok {
		// 只有方括号索引赋值能直接映射到 SETTABLE。
		return false, nil
	}
	receiverName, ok := indexExpression.Receiver.(*parser.NameExpression)
	if !ok {
		// receiver 不是名称时可能有调用或索引副作用，回退通用路径。
		return false, nil
	}
	receiverBinding, ok := generator.lookupLocal(receiverName.Name)
	if !ok {
		// upvalue/global receiver 读取需要额外指令或可能触发环境表访问，回退通用路径。
		return false, nil
	}

	if !generator.isSafeRKCandidate(indexExpression.Index) || !generator.isSafeRKCandidate(statement.Right[0]) {
		// key 或 value 不是当前 local/字面量时，回退通用赋值路径以避免提前生成半截指令。
		return false, nil
	}

	firstTempRegister := generator.nextRegister
	keyOperand, _, keyOK, err := generator.safeRKOperand(indexExpression.Index)
	if err != nil {
		// 安全 key 编译失败时释放临时寄存器并返回。
		generator.releaseRegistersFrom(firstTempRegister)
		return true, err
	}
	if !keyOK {
		// key 表达式不满足安全条件时回退通用路径。
		generator.releaseRegistersFrom(firstTempRegister)
		return false, nil
	}
	valueOperand, _, valueOK, err := generator.safeRKOperand(statement.Right[0])
	if err != nil {
		// 安全 value 编译失败时释放临时寄存器并返回。
		generator.releaseRegistersFrom(firstTempRegister)
		return true, err
	}
	if !valueOK {
		// value 表达式不满足安全条件时回退通用路径。
		generator.releaseRegistersFrom(firstTempRegister)
		return false, nil
	}

	if err := generator.withSourceLine(indexExpression.Position, func() error {
		// receiver/key/value 均已确认无副作用，直接复用寄存器或 RK 常量生成 SETTABLE。
		generator.emitABC(bytecode.OpSetTable, receiverBinding.register, keyOperand, valueOperand)
		return nil
	}); err != nil {
		generator.releaseRegistersFrom(firstTempRegister)
		return true, err
	}
	generator.releaseRegistersFrom(firstTempRegister)

	// 安全 table 索引赋值已完成。
	return true, nil
}

// compileSingleLocalFieldSelfBinaryAssignment 优化 `localTable.field = localTable.field <op> rhs`。
//
// 该优化只处理单左值、单右值、receiver 为当前 local 且字段名完全一致的点号字段访问；rhs 必须
// 是 local 或字面量。该形态没有调用、全局读取或动态 key 副作用，可直接对齐 Lua 5.3 官方
// GETTABLE/op/SETTABLE codegen。
func (generator *generator) compileSingleLocalFieldSelfBinaryAssignment(statement *parser.AssignmentStatement) (bool, error) {
	if len(statement.Left) != 1 || len(statement.Right) != 1 {
		// 多重赋值必须先求完所有 RHS，再统一写回，不能使用该快路径。
		return false, nil
	}
	targetField, ok := statement.Left[0].(*parser.FieldAccessExpression)
	if !ok {
		// 只有点号字段左值能复用字段常量 key。
		return false, nil
	}
	targetReceiver, ok := targetField.Receiver.(*parser.NameExpression)
	if !ok {
		// receiver 不是名称时可能包含调用或索引副作用，回退通用路径。
		return false, nil
	}
	receiverBinding, ok := generator.lookupLocal(targetReceiver.Name)
	if !ok {
		// upvalue/global receiver 读取需要额外指令或可能触发环境表访问，回退通用路径。
		return false, nil
	}
	binaryExpression, ok := statement.Right[0].(*parser.BinaryExpression)
	if !ok || binaryExpression.Operator == ".." || binaryExpression.Operator == "and" || binaryExpression.Operator == "or" || isComparisonOperator(binaryExpression.Operator) {
		// concat、短路和比较不属于普通二元 opcode。
		return false, nil
	}
	sourceField, ok := binaryExpression.Left.(*parser.FieldAccessExpression)
	if !ok || !sameLocalFieldAccess(targetField, sourceField) {
		// 只有 RHS 左操作数读取同一个 local 字段时，才能直接复用 GETTABLE 结果。
		return false, nil
	}
	opCode, ok := binaryOpCode(binaryExpression.Operator)
	if !ok {
		// 未支持的二元运算回退通用路径，让通用编译器返回原有错误。
		return false, nil
	}
	if !generator.isSafeRKCandidate(binaryExpression.Right) {
		// 右操作数若可能访问 table/global 或调用函数，必须回退通用路径保持求值顺序。
		return false, nil
	}

	firstTempRegister := generator.nextRegister
	keyIndex := generator.addConstant(bytecode.StringConstant(targetField.Field))
	keyOperand, _, err := generator.rkOperandForConstantIndex(keyIndex)
	if err != nil {
		// 字段名常量无法编码或加载时释放临时寄存器并返回。
		generator.releaseRegistersFrom(firstTempRegister)
		return true, err
	}
	rightOperand, _, err := generator.safeRKOperandWithRegister(binaryExpression.Right)
	if err != nil {
		// 右操作数常量无法编码或加载时释放临时寄存器并返回。
		generator.releaseRegistersFrom(firstTempRegister)
		return true, err
	}
	valueRegister := generator.allocateRegister()
	if err := generator.withSourceLine(sourceField.Position, func() error {
		// 同字段读取直接写入值临时寄存器。
		generator.emitABC(bytecode.OpGetTable, valueRegister, receiverBinding.register, keyOperand)
		return nil
	}); err != nil {
		generator.releaseRegistersFrom(firstTempRegister)
		return true, err
	}
	if err := generator.withSourceLine(binaryExpression.Position, func() error {
		// 二元结果继续保存在值临时寄存器，后续 SETTABLE 直接使用。
		generator.emitABC(opCode, valueRegister, valueRegister, rightOperand)
		return nil
	}); err != nil {
		generator.releaseRegistersFrom(firstTempRegister)
		return true, err
	}
	if err := generator.withSourceLine(targetField.Position, func() error {
		// 写回同一个 local receiver 的同一个字段。
		generator.emitABC(bytecode.OpSetTable, receiverBinding.register, keyOperand, valueRegister)
		return nil
	}); err != nil {
		generator.releaseRegistersFrom(firstTempRegister)
		return true, err
	}
	generator.releaseRegistersFrom(firstTempRegister)

	// local table 字段自二元赋值已完成。
	return true, nil
}

// compileSingleLocalSelfBinaryAssignment 优化 `localName = localName <op> expression`。
//
// 该优化只处理单左值、单右值、当前函数 active local 的普通二元运算；右侧表达式必须没有调用、
// table/global 访问或元方法触发风险，避免破坏 Lua 5.3 对左操作数先于右操作数求值的语义。
func (generator *generator) compileSingleLocalSelfBinaryAssignment(statement *parser.AssignmentStatement) (bool, error) {
	if len(statement.Left) != 1 || len(statement.Right) != 1 {
		// 多重赋值需要先求完所有 RHS，再统一写回，不能使用该快路径。
		return false, nil
	}
	targetName, ok := statement.Left[0].(*parser.NameExpression)
	if !ok {
		// 只有普通名称左值能直接写回寄存器。
		return false, nil
	}
	binding, ok := generator.lookupLocal(targetName.Name)
	if !ok {
		// upvalue/global 写回有额外语义，不能用当前 local 寄存器快路径。
		return false, nil
	}
	if binding.constLocal {
		// const 绑定不允许通过自二元赋值快路径覆盖。
		return true, constAssignmentError(targetName.Name, targetName.Position)
	}
	binaryExpression, ok := statement.Right[0].(*parser.BinaryExpression)
	if !ok || binaryExpression.Operator == ".." || binaryExpression.Operator == "and" || binaryExpression.Operator == "or" || isComparisonOperator(binaryExpression.Operator) {
		// concat、短路和比较已有专门语义，当前优化只处理普通二元运算。
		return false, nil
	}
	if chainHandled, err := generator.compileSelfBinaryChainViaAccumulator(targetName.Name, binding, binaryExpression); chainHandled || err != nil {
		// 左结合自二元链使用临时累加器，保持官方 Lua 的求值和最终写回时机。
		return chainHandled, err
	}
	if chainHandled, err := generator.compileSelfBinaryChainToTarget(targetName.Name, binding, binaryExpression); chainHandled || err != nil {
		// 简单自二元链已直接写回目标 local。
		return chainHandled, err
	}
	leftName, ok := binaryExpression.Left.(*parser.NameExpression)
	if !ok || leftName.Name != targetName.Name {
		// 只有 `x = x <op> rhs` 能保证左操作数就是目标寄存器当前值。
		return false, nil
	}
	opCode, ok := binaryOpCode(binaryExpression.Operator)
	if !ok {
		// 未支持的二元运算回退通用路径，让通用编译器返回原有错误。
		return false, nil
	}
	if generator.isSafePureBinaryExpression(binaryExpression.Right) {
		// 右侧纯二元树没有调用、索引或全局访问副作用，可先编入临时槽，再按官方形态读写目标 local。
		rightRegister := generator.allocateRegister()
		if err := generator.compileExpressionTo(binaryExpression.Right, rightRegister); err != nil {
			// 右侧编译失败时释放临时寄存器并返回。
			generator.releaseRegister(rightRegister)
			return true, err
		}
		if err := generator.withSourceLine(binaryExpression.Position, func() error {
			// 左操作数直接读取目标 local 旧值，结果也直接写回该 local。
			generator.emitABC(opCode, binding.register, binding.register, rightRegister)
			return nil
		}); err != nil {
			generator.releaseRegister(rightRegister)
			return true, err
		}
		generator.releaseRegister(rightRegister)
		return true, nil
	}
	rightRegister := generator.allocateRegister()
	rightHandled, err := generator.compileSelfBinaryRightExpressionTo(binaryExpression.Right, rightRegister)
	if err != nil {
		// 右操作数失败时释放临时寄存器并返回。
		generator.releaseRegister(rightRegister)
		return true, err
	}
	if !rightHandled {
		// 右侧若可能调用函数或触发元方法，就必须先保存左操作数，避免破坏左到右求值语义。
		generator.releaseRegister(rightRegister)
		return false, nil
	}
	if err := generator.withSourceLine(binaryExpression.Position, func() error {
		// 左操作数直接读取目标 local 旧值，结果也直接写回该 local。
		generator.emitABC(opCode, binding.register, binding.register, rightRegister)
		return nil
	}); err != nil {
		generator.releaseRegister(rightRegister)
		return true, err
	}
	generator.releaseRegister(rightRegister)

	// 自二元运算赋值已完成。
	return true, nil
}

// compileSelfBinaryChainViaAccumulator 编译 `x = ((x op rhs) op rhs)` 的官方兼容累加器形态。
//
// 当左侧存在子二元链时，不能提前把中间结果写回 x；Lua 5.3 官方 codegen 会把中间结果
// 放入临时累加器，直到最后一层才写回 x，避免后续运算失败时污染目标 local。
func (generator *generator) compileSelfBinaryChainViaAccumulator(targetName string, binding localBinding, expression *parser.BinaryExpression) (bool, error) {
	if !generator.isSelfBinaryChainRoot(targetName, expression) {
		// 只有左侧最终落到目标 local 的普通二元链可以使用该形态。
		return false, nil
	}
	if _, leftIsBinary := expression.Left.(*parser.BinaryExpression); !leftIsBinary && !selfBinaryChainContainsCall(expression) {
		// 简单 `x = x op rhs` 保留 direct-to-target 热路径，避免破坏 table/global 直接寄存器形态。
		return false, nil
	}
	accumulatorRegister := -1
	if _, leftIsBinary := expression.Left.(*parser.BinaryExpression); leftIsBinary {
		// 只有左侧存在子链时才需要累加器；简单 `x + call()` 可让 call 直接占用下一个临时寄存器。
		accumulatorRegister = generator.allocateRegister()
	}
	handled, err := generator.compileSelfBinaryChainAccumulatorNode(targetName, binding, expression, accumulatorRegister, binding.register)
	generator.releaseOptionalRegister(accumulatorRegister)
	return handled, err
}

// compileSelfBinaryChainAccumulatorNode 递归生成左结合自二元链。
//
// accumulatorRegister 保存非最终层的中间结果；finalRegister 是最外层最终写回的目标 local。
func (generator *generator) compileSelfBinaryChainAccumulatorNode(targetName string, binding localBinding, expression *parser.BinaryExpression, accumulatorRegister int, finalRegister int) (bool, error) {
	leftOperand := binding.register
	leftBinary, leftIsBinary := expression.Left.(*parser.BinaryExpression)
	if leftIsBinary {
		if accumulatorRegister < 0 {
			// 左侧存在子链时必须有累加器保存中间结果。
			return false, nil
		}
		// 左子链先写入累加器，但不会提前覆盖目标 local。
		handled, err := generator.compileSelfBinaryChainAccumulatorNode(targetName, binding, leftBinary, accumulatorRegister, accumulatorRegister)
		if !handled || err != nil {
			// 子链失败时直接把信号传给调用方。
			return handled, err
		}
		leftOperand = accumulatorRegister
	} else {
		leftName, ok := expression.Left.(*parser.NameExpression)
		if !ok || leftName.Name != targetName {
			// 预检已保证该分支理论不可达，保守回退通用路径。
			return false, nil
		}
	}

	rightOperand, rightRegister, err := generator.selfBinaryAccumulatorRightOperand(expression.Right, leftOperand, finalRegister)
	if err != nil {
		// 右操作数失败时返回原始编译错误。
		return true, err
	}
	opCode, ok := binaryOpCode(expression.Operator)
	if !ok {
		// 预检已保证普通二元 opcode；异常时回退原有错误路径。
		generator.releaseOptionalRegister(rightRegister)
		return false, nil
	}
	if err := generator.withSourceLine(expression.Position, func() error {
		// 当前层按官方 Lua 形态：先完成右侧求值，再读取左侧寄存器并写入目标。
		generator.emitABC(opCode, finalRegister, leftOperand, rightOperand)
		return nil
	}); err != nil {
		generator.releaseOptionalRegister(rightRegister)
		return true, err
	}
	generator.releaseOptionalRegister(rightRegister)
	return true, nil
}

// selfBinaryAccumulatorRightOperand 编译自二元累加器路径当前层的右操作数。
//
// 当 finalRegister 是临时累加器且左操作数不是同一个寄存器时，右侧复杂表达式可直接写入
// finalRegister，随后用 `final = left op final` 合成当前层结果，减少 `x = x + call()` 子链的
// 额外调用寄存器。若两者相同则必须分配独立右操作数，避免覆盖左侧中间值。
func (generator *generator) selfBinaryAccumulatorRightOperand(expression parser.Expression, leftOperand int, finalRegister int) (operand int, tempRegister int, err error) {
	if operand, register, ok, err := generator.safeRKOperand(expression); err != nil || ok {
		// 安全 RK 操作数不需要额外临时寄存器；错误保持原编译语义。
		return operand, register, err
	}
	if finalRegister >= 0 && finalRegister != leftOperand && !generator.registerHasActiveLocal(finalRegister) {
		// finalRegister 是可覆盖临时槽时，右侧直接写入该槽，避免额外调用结果寄存器。
		generator.ensureRegister(finalRegister)
		if err := generator.compileExpressionTo(expression, finalRegister); err != nil {
			// 右操作数失败时返回原始编译错误。
			return 0, -1, err
		}
		if expressionIsFixedSingleResultCall(expression) {
			// 自二元累加器路径中的固定单返回调用可立即回收实参槽，贴近 Lua 5.3 codegen 水位。
			generator.releaseCallArgumentsAfterFixedResult(finalRegister, 1)
		}
		return finalRegister, -1, nil
	}
	if expressionIsFixedSingleResultCall(expression) {
		// 外层右侧调用使用一个临时返回槽；CALL 后回收其参数槽，让后续调用复用寄存器。
		rightRegister := generator.allocateRegister()
		if err := generator.compileExpressionTo(expression, rightRegister); err != nil {
			// 右操作数失败时释放临时寄存器并返回。
			generator.releaseRegister(rightRegister)
			return 0, -1, err
		}
		generator.releaseCallArgumentsAfterFixedResult(rightRegister, 1)
		return rightRegister, rightRegister, nil
	}
	return generator.binaryRightOperand(expression)
}

// expressionIsFixedSingleResultCall 判断表达式是否会由 compileExpressionTo 生成固定单返回 CALL。
func expressionIsFixedSingleResultCall(expression parser.Expression) bool {
	switch typedExpression := expression.(type) {
	case *parser.FunctionCallExpression:
		// 普通表达式上下文中的函数调用请求单返回值；即使 B=0 开放实参也会在 CALL 后固定写回一个结果。
		return typedExpression != nil
	case *parser.MethodCallExpression:
		// method 调用在表达式上下文同样固定请求一个返回值，CALL 后可回收参数槽。
		return typedExpression != nil
	default:
		// 其他表达式没有 CALL 参数槽可回收。
		return false
	}
}

// callArgumentsEndWithOpenList 判断调用实参是否以开放列表表达式结束。
func callArgumentsEndWithOpenList(arguments []parser.Expression) bool {
	if len(arguments) == 0 {
		// 没有实参时固定参数数量为 0。
		return false
	}
	return isOpenListExpression(arguments[len(arguments)-1])
}

// isSelfBinaryChainRoot 判断表达式是否是左侧最终落到 targetName 的普通二元链。
func (generator *generator) isSelfBinaryChainRoot(targetName string, expression *parser.BinaryExpression) bool {
	if expression == nil || expression.Operator == ".." || expression.Operator == "and" || expression.Operator == "or" || isComparisonOperator(expression.Operator) {
		// concat、短路和比较不使用普通二元累加器形态。
		return false
	}
	if _, ok := binaryOpCode(expression.Operator); !ok {
		// 未支持 opcode 的操作符不能进入快路径。
		return false
	}
	if leftName, ok := expression.Left.(*parser.NameExpression); ok {
		// 链起点必须是目标 local 自身。
		return leftName.Name == targetName
	}
	leftBinary, ok := expression.Left.(*parser.BinaryExpression)
	if !ok {
		// 其他左侧形态不是左结合自二元链。
		return false
	}
	return generator.isSelfBinaryChainRoot(targetName, leftBinary)
}

// selfBinaryChainContainsCall 判断自二元链中是否包含函数或方法调用。
func selfBinaryChainContainsCall(expression parser.Expression) bool {
	switch typedExpression := expression.(type) {
	case *parser.FunctionCallExpression, *parser.MethodCallExpression:
		// 调用可能修改 open upvalue，因此需要对齐官方 Lua 的延迟读目标 local 形态。
		return true
	case *parser.BinaryExpression:
		// 二元链左右两侧任意一侧包含调用都需要累加器路径。
		return selfBinaryChainContainsCall(typedExpression.Left) || selfBinaryChainContainsCall(typedExpression.Right)
	case *parser.UnaryExpression:
		// 一元表达式内部继续检查操作数。
		return selfBinaryChainContainsCall(typedExpression.Operand)
	case *parser.PrefixExpression:
		// 括号前缀不改变求值副作用。
		return selfBinaryChainContainsCall(typedExpression.Inner)
	case *parser.FieldAccessExpression:
		// 字段访问的 receiver 子表达式可能包含调用。
		return selfBinaryChainContainsCall(typedExpression.Receiver)
	case *parser.IndexExpression:
		// table/index 子表达式本身可能包含调用。
		return selfBinaryChainContainsCall(typedExpression.Receiver) || selfBinaryChainContainsCall(typedExpression.Index)
	default:
		// 字面量、名称和其他当前表达式没有直接调用。
		return false
	}
}

// compileSelfBinaryChainToTarget 编译 `x = ((x op a) op b)` 形式的左结合自二元链。
//
// targetName 必须是当前 local；链路每一层左侧继续指向 targetName，右侧必须是安全表达式，
// 这样可以直接在目标寄存器上累计结果，避免临时 MOVE 往返。
func (generator *generator) compileSelfBinaryChainToTarget(targetName string, binding localBinding, expression *parser.BinaryExpression) (bool, error) {
	if !generator.isSelfBinaryChainExpression(targetName, expression) {
		// 不是安全左结合链时交回原有自二元快路径。
		return false, nil
	}
	if leftName, ok := expression.Left.(*parser.NameExpression); ok && leftName.Name == targetName {
		// 链起点就是目标 local，当前寄存器已保存左操作数，无需生成 MOVE。
	} else {
		leftBinary, ok := expression.Left.(*parser.BinaryExpression)
		if !ok {
			// 预检已保证该分支理论不可达，保守回退通用路径。
			return false, nil
		}
		if handled, err := generator.compileSelfBinaryChainToTarget(targetName, binding, leftBinary); !handled || err != nil {
			// 左子链无法生成时返回其错误或回退信号。
			return handled, err
		}
	}

	rightOperand, rightRegister, err := generator.selfBinaryRightOperand(expression.Right)
	if err != nil {
		// 右操作数失败时直接返回。
		return true, err
	}
	opCode, ok := binaryOpCode(expression.Operator)
	if !ok {
		// 预检已保证普通二元 opcode；异常时回退原有错误路径。
		generator.releaseOptionalRegister(rightRegister)
		return false, nil
	}
	if err := generator.withSourceLine(expression.Position, func() error {
		// 每一层都直接读写目标 local，保持左结合运算顺序。
		generator.emitABC(opCode, binding.register, binding.register, rightOperand)
		return nil
	}); err != nil {
		generator.releaseOptionalRegister(rightRegister)
		return true, err
	}
	generator.releaseOptionalRegister(rightRegister)
	return true, nil
}

// selfBinaryRightOperand 编译自二元链当前层的右操作数。
//
// 字面量和当前 local 直接作为 RK 操作数；更复杂但安全的普通二元树编译到临时寄存器。
func (generator *generator) selfBinaryRightOperand(expression parser.Expression) (operand int, tempRegister int, err error) {
	if operand, register, ok, err := generator.safeRKOperand(expression); err != nil || ok {
		// 安全 RK 操作数不需要额外临时寄存器；错误保持原编译语义。
		return operand, register, err
	}
	rightRegister := generator.allocateRegister()
	rightHandled, err := generator.compileSelfBinaryRightExpressionTo(expression, rightRegister)
	if err != nil {
		// 右操作数失败时释放临时寄存器并返回。
		generator.releaseRegister(rightRegister)
		return 0, -1, err
	}
	if !rightHandled {
		// 预检已保证该分支理论不可达，保守回退通用路径。
		generator.releaseRegister(rightRegister)
		return 0, -1, fmt.Errorf("codegen unsafe self binary right operand")
	}
	return rightRegister, rightRegister, nil
}

// isSelfBinaryChainExpression 判断表达式是否是可直接写回 targetName 的左结合自二元链。
//
// 链路左侧必须最终落到 targetName；每层右侧必须是字面量、local/upvalue 名称、局部索引读取
// 或可 RK 编译的普通二元表达式，避免改变有副作用表达式的求值时机。
func (generator *generator) isSelfBinaryChainExpression(targetName string, expression *parser.BinaryExpression) bool {
	if expression == nil || expression.Operator == ".." || expression.Operator == "and" || expression.Operator == "or" || isComparisonOperator(expression.Operator) {
		// concat、短路和比较不属于普通自二元累计链。
		return false
	}
	if _, ok := binaryOpCode(expression.Operator); !ok {
		// 未支持 opcode 的操作符不能进入快路径。
		return false
	}
	if !generator.isSelfBinarySafeRightExpression(expression.Right) && !generator.isSafePureBinaryExpression(expression.Right) {
		// 右侧可能有调用、全局查找或其他副作用时不能改变求值寄存器布局。
		return false
	}
	if leftName, ok := expression.Left.(*parser.NameExpression); ok {
		// 链起点必须是目标 local 自身。
		return leftName.Name == targetName
	}
	leftBinary, ok := expression.Left.(*parser.BinaryExpression)
	if !ok {
		// 其他左侧形态不是左结合自二元链。
		return false
	}
	return generator.isSelfBinaryChainExpression(targetName, leftBinary)
}

// isSafeRKBinaryExpression 判断表达式是否是可由 RK 快路径处理的普通二元表达式。
func (generator *generator) isSafeRKBinaryExpression(expression parser.Expression) bool {
	binaryExpression, ok := expression.(*parser.BinaryExpression)
	if !ok {
		// 非二元表达式不属于 RK 二元快路径。
		return false
	}
	if _, ok := binaryOpCode(binaryExpression.Operator); !ok {
		// 非普通二元 opcode 不属于 RK 二元快路径。
		return false
	}
	return generator.isSafeRKCandidate(binaryExpression.Left) && generator.isSafeRKCandidate(binaryExpression.Right)
}

// isSafePureBinaryExpression 判断表达式是否只由 local、字面量和普通二元 opcode 组成。
func (generator *generator) isSafePureBinaryExpression(expression parser.Expression) bool {
	if generator.isSafeRKCandidate(expression) {
		// 单个 local 或字面量没有求值副作用。
		return true
	}
	if prefixExpression, ok := expression.(*parser.PrefixExpression); ok {
		// 括号表达式只改变优先级，不引入额外求值副作用。
		return generator.isSafePureBinaryExpression(prefixExpression.Inner)
	}
	binaryExpression, ok := expression.(*parser.BinaryExpression)
	if !ok {
		// 其他表达式可能触发调用、索引或全局表读取。
		return false
	}
	if _, ok := binaryOpCode(binaryExpression.Operator); !ok {
		// 短路、比较和 concat 不属于普通算术树。
		return false
	}
	return generator.isSafePureBinaryExpression(binaryExpression.Left) && generator.isSafePureBinaryExpression(binaryExpression.Right)
}

// isSafeRKCandidate 判断表达式是否可无副作用地作为 RK 操作数候选。
func (generator *generator) isSafeRKCandidate(expression parser.Expression) bool {
	switch typedExpression := expression.(type) {
	case *parser.NameExpression:
		_, exists := generator.lookupLocal(typedExpression.Name)
		return exists
	case *parser.LiteralExpression:
		_, ok := literalConstant(typedExpression)
		return ok
	default:
		return false
	}
}

// sameLocalFieldAccess 判断两个点号字段访问是否读取同一个 local receiver 的同一个字段。
func sameLocalFieldAccess(left *parser.FieldAccessExpression, right *parser.FieldAccessExpression) bool {
	// 任一表达式缺失都不能认定为同字段访问。
	if left == nil || right == nil || left.Field != right.Field {
		return false
	}
	leftReceiver, leftOK := left.Receiver.(*parser.NameExpression)
	rightReceiver, rightOK := right.Receiver.(*parser.NameExpression)
	if !leftOK || !rightOK {
		// receiver 不是名称时可能有副作用，不能走同字段快路径。
		return false
	}

	// receiver 名称完全一致时才允许复用同字段假设。
	return leftReceiver.Name == rightReceiver.Name
}

// compileSelfBinaryRightExpressionTo 编译自二元赋值中确认安全的右操作数。
//
// targetRegister 保存右操作数结果；返回 handled=false 表示表达式不满足快路径安全条件。
func (generator *generator) compileSelfBinaryRightExpressionTo(expression parser.Expression, targetRegister int) (bool, error) {
	if generator.isSelfBinarySafeRightExpression(expression) {
		// 字面量、local 或 upvalue 名称沿用普通表达式编译即可。
		return true, generator.compileExpressionTo(expression, targetRegister)
	}
	if generator.isSafePureBinaryExpression(expression) {
		// 由 local/字面量组成的普通二元树没有额外调用或索引副作用，可作为右操作数临时值。
		return true, generator.compileExpressionTo(expression, targetRegister)
	}
	if fieldExpression, ok := expression.(*parser.FieldAccessExpression); ok {
		// receiver/key 均无副作用的点号字段访问可直接生成 GETTABLE 到右操作数目标寄存器。
		return generator.compileSafeLocalFieldAccessTo(fieldExpression, targetRegister)
	}
	indexExpression, ok := expression.(*parser.IndexExpression)
	if !ok {
		// 其他复杂表达式可能调用函数或触发未知副作用，回退通用路径。
		return false, nil
	}
	receiverName, ok := indexExpression.Receiver.(*parser.NameExpression)
	if !ok {
		// receiver 不是名称时可能有副作用，回退通用路径。
		return false, nil
	}
	receiverBinding, ok := generator.lookupLocal(receiverName.Name)
	if !ok {
		// upvalue/global receiver 需要额外读取或环境表访问，回退通用路径。
		return false, nil
	}
	firstTempRegister := generator.nextRegister
	keyOperand, keyRegister, keyOK, err := generator.safeRKOperand(indexExpression.Index)
	if err != nil {
		// key 编译失败时释放临时寄存器并返回。
		generator.releaseRegistersFrom(firstTempRegister)
		return true, err
	}
	if !keyOK {
		// key 不满足安全条件时回退通用路径。
		generator.releaseRegistersFrom(firstTempRegister)
		return false, nil
	}
	if err := generator.withSourceLine(indexExpression.Position, func() error {
		// receiver/key 均无副作用时直接生成 GETTABLE 到右操作数目标寄存器。
		generator.emitABC(bytecode.OpGetTable, targetRegister, receiverBinding.register, keyOperand)
		return nil
	}); err != nil {
		generator.releaseRegistersFrom(firstTempRegister)
		return true, err
	}
	generator.releaseOptionalRegister(keyRegister)
	generator.releaseRegistersFrom(firstTempRegister)
	return true, nil
}

// compileSafeLocalFieldAccessTo 编译 receiver 为当前 local 的点号字段读取。
//
// 字段名固定为字符串常量，receiver 当前 local 读取无副作用；若 receiver 不是 local 则返回
// handled=false，让调用方回退通用路径保持 Lua 5.3 求值顺序。
func (generator *generator) compileSafeLocalFieldAccessTo(expression *parser.FieldAccessExpression, targetRegister int) (bool, error) {
	receiverName, ok := expression.Receiver.(*parser.NameExpression)
	if !ok {
		// receiver 不是名称时可能包含调用或索引副作用，回退通用路径。
		return false, nil
	}
	receiverBinding, ok := generator.lookupLocal(receiverName.Name)
	if !ok {
		// upvalue/global receiver 需要额外读取或环境表访问，回退通用路径。
		return false, nil
	}
	firstTempRegister := generator.nextRegister
	keyIndex := generator.addConstant(bytecode.StringConstant(expression.Field))
	keyOperand, _, err := generator.rkOperandForConstantIndex(keyIndex)
	if err != nil {
		// 字段名常量无法编码或加载时释放临时寄存器并返回。
		generator.releaseRegistersFrom(firstTempRegister)
		return true, err
	}
	if err := generator.withSourceLine(expression.Position, func() error {
		// receiver/key 均无副作用时直接生成 GETTABLE 到目标寄存器。
		generator.emitABC(bytecode.OpGetTable, targetRegister, receiverBinding.register, keyOperand)
		return nil
	}); err != nil {
		generator.releaseRegistersFrom(firstTempRegister)
		return true, err
	}
	generator.releaseRegistersFrom(firstTempRegister)
	return true, nil
}

// compileSingleLocalBinaryAssignment 优化 `localName = left <op> right`。
//
// 该优化只处理单左值、单右值、当前函数 active local 的非短路二元表达式。compileBinaryTo 在目标
// 是 active local 时会把左右子表达式放到临时寄存器，最终 opcode 才写回目标 local，因此不会提前
// 覆盖 RHS 后续读取的旧 local 值；相比通用赋值路径可省去最后的 MOVE。
func (generator *generator) compileSingleLocalBinaryAssignment(statement *parser.AssignmentStatement) (bool, error) {
	if len(statement.Left) != 1 || len(statement.Right) != 1 {
		// 多重赋值需要先求完所有 RHS，再统一写回，不能使用该快路径。
		return false, nil
	}
	targetName, ok := statement.Left[0].(*parser.NameExpression)
	if !ok {
		// 只有普通名称左值能直接写回寄存器。
		return false, nil
	}
	binding, ok := generator.lookupLocal(targetName.Name)
	if !ok {
		// upvalue/global 写回有额外语义，不能用当前 local 寄存器快路径。
		return false, nil
	}
	if binding.constLocal {
		// const 绑定不允许通过普通二元赋值快路径覆盖。
		return true, constAssignmentError(targetName.Name, targetName.Position)
	}
	binaryExpression, ok := statement.Right[0].(*parser.BinaryExpression)
	if !ok || binaryExpression.Operator == "and" || binaryExpression.Operator == "or" {
		// and/or 会把短路左值先写入目标寄存器，可能提前覆盖同名 local。
		return false, nil
	}
	if err := generator.compileBinaryTo(binaryExpression, binding.register); err != nil {
		// RHS 编译失败时保留原始错误。
		return true, err
	}
	return true, nil
}

// compileSingleLocalSelfConcatAssignment 优化 `localName = localName .. expression`。
//
// 该优化只处理单左值、单右值、当前函数 active local 的自拼接赋值；多重赋值、upvalue、global
// 和 table/index 左值仍走通用路径，以保留 Lua 5.3 的求值顺序和副作用语义。
func (generator *generator) compileSingleLocalSelfConcatAssignment(statement *parser.AssignmentStatement) (bool, error) {
	if len(statement.Left) != 1 || len(statement.Right) != 1 {
		// 多重赋值需要先求完所有 RHS，再统一写回，不能使用该快路径。
		return false, nil
	}
	targetName, ok := statement.Left[0].(*parser.NameExpression)
	if !ok {
		// 只有普通名称左值能直接写回寄存器。
		return false, nil
	}
	binding, ok := generator.lookupLocal(targetName.Name)
	if !ok {
		// upvalue/global 写回有额外语义，不能用当前 local 寄存器快路径。
		return false, nil
	}
	if binding.constLocal {
		// const 绑定不允许通过自拼接赋值快路径覆盖。
		return true, constAssignmentError(targetName.Name, targetName.Position)
	}
	binaryExpression, ok := statement.Right[0].(*parser.BinaryExpression)
	if !ok || binaryExpression.Operator != ".." {
		// 只优化字符串拼接赋值。
		return false, nil
	}
	leftName, ok := binaryExpression.Left.(*parser.NameExpression)
	if !ok || leftName.Name != targetName.Name {
		// 只有 `x = x .. rhs` 能保证左操作数就是目标寄存器当前值。
		return false, nil
	}
	if !isSelfConcatSafeRightExpression(binaryExpression.Right) {
		// 右侧若可能调用函数或触发元方法，就必须先保存左操作数，避免破坏左到右求值语义。
		return false, nil
	}
	if generator.nextRegister != binding.register+1 {
		// 目标 local 后方已有活动寄存器时，把左操作数复制到连续临时区，仍让 CONCAT 直接写回目标。
		return true, generator.compileSingleLocalSelfConcatAssignmentWithTemps(binding, binaryExpression)
	}

	rightRegister := generator.allocateRegister()
	if err := generator.compileExpressionTo(binaryExpression.Right, rightRegister); err != nil {
		// 右操作数失败时释放临时寄存器并返回。
		generator.releaseRegister(rightRegister)
		return true, err
	}
	if err := generator.withSourceLine(binaryExpression.Position, func() error {
		// CONCAT 允许目标寄存器与左操作数寄存器相同，执行时会先读取区间再写回目标。
		generator.emitABC(bytecode.OpConcat, binding.register, binding.register, rightRegister)
		return nil
	}); err != nil {
		generator.releaseRegister(rightRegister)
		return true, err
	}
	generator.releaseRegister(rightRegister)

	// 自拼接赋值已完成。
	return true, nil
}

// compileSingleLocalSelfConcatAssignmentWithTemps 编译被活动寄存器隔开的 local 自拼接赋值。
//
// binding 必须是左值 local 绑定；expression 必须满足 `x = x .. rhs` 且 rhs 已确认无副作用。
// 该路径使用连续临时寄存器承载真实拼接操作数，并让 OP_CONCAT 直接写回 local，避免通用赋值
// 额外生成结果临时寄存器后的 MOVE。
func (generator *generator) compileSingleLocalSelfConcatAssignmentWithTemps(binding localBinding, expression *parser.BinaryExpression) error {
	// 分配两个连续临时寄存器，分别保存当前 local 值和右操作数。
	startRegister := generator.allocateRegister()
	rightRegister := generator.allocateRegister()
	if err := generator.withSourceLine(expression.Position, func() error {
		// 先复制左操作数，保证 CONCAT 写回目标前已经保留旧字符串值。
		generator.emitABC(bytecode.OpMove, startRegister, binding.register, 0)
		return nil
	}); err != nil {
		generator.releaseRegistersFrom(startRegister)
		return err
	}
	if err := generator.compileExpressionTo(expression.Right, rightRegister); err != nil {
		// 右操作数编译失败时释放临时区并返回。
		generator.releaseRegistersFrom(startRegister)
		return err
	}
	if err := generator.withSourceLine(expression.Position, func() error {
		// OP_CONCAT 的 B..C 现在是连续临时区，A 直接写回目标 local。
		generator.emitABC(bytecode.OpConcat, binding.register, startRegister, rightRegister)
		return nil
	}); err != nil {
		generator.releaseRegistersFrom(startRegister)
		return err
	}
	generator.releaseRegistersFrom(startRegister)
	return nil
}

// isSelfConcatSafeRightExpression 判断自拼接右侧是否不会修改目标 local。
//
// 当前只允许字面量，覆盖 `s = s .. "x"` 等热点；名称、字段、索引和调用都可能触发读取副作用或
// 间接修改目标 local，因此交给通用赋值路径保持语义。
func isSelfConcatSafeRightExpression(expression parser.Expression) bool {
	_, ok := expression.(*parser.LiteralExpression)
	if ok {
		// 字面量求值没有副作用，不会改变左操作数寄存器。
		return true
	}

	// 其他表达式保守认为可能有副作用。
	return false
}

// isSelfBinarySafeRightExpression 判断自二元运算右侧是否不会修改目标 local。
//
// 当前允许字面量、当前 local 名称、已存在的 upvalue 名称和未知全局名称；全局名称会在右操作数
// 临时寄存器中完成 `_ENV[name]` 读取，再按 Lua 5.3 官方 codegen 形态读取目标 local 执行二元运算。
// 复杂表达式、调用、字段和索引仍回退通用赋值路径。
func (generator *generator) isSelfBinarySafeRightExpression(expression parser.Expression) bool {
	if _, ok := expression.(*parser.LiteralExpression); ok {
		// 字面量求值没有副作用，不会改变左操作数寄存器。
		return true
	}
	nameExpression, ok := expression.(*parser.NameExpression)
	if !ok {
		// 非名称表达式可能包含调用、table 访问或嵌套运算，保守回退。
		return false
	}
	if _, ok := generator.lookupLocal(nameExpression.Name); ok {
		// 当前 local 读取只是寄存器 MOVE，不会触发元方法或调用。
		return true
	}
	if nameExpression.Name == envUpvalueName {
		// `_ENV` 本身作为 upvalue 读取没有 table 元方法。
		return true
	}
	_, captured, err := generator.resolveUpvalue(nameExpression.Name, nameExpression.Position)
	if err != nil {
		// upvalue 上限错误交给通用路径保留原错误时机。
		return false
	}
	if captured {
		// 已捕获或可捕获的 upvalue 读取不会调用 Lua 代码。
		return true
	}

	// 未声明名称按全局读取处理；官方 Lua 5.3 同样先求右侧全局，再以目标 local 作为左操作数。
	return true
}

// safeInlineRKOperand 将无副作用表达式转换为不会分配临时寄存器的 RK 操作数。
//
// 当前只接受当前 local 名称，以及常量索引可直接放入 RK 字段的字面量；超过 RK 常量范围的字面量
// 返回 ok=false，由调用方回退需要显式目标寄存器的通用路径。
func (generator *generator) safeInlineRKOperand(expression parser.Expression) (operand int, ok bool, err error) {
	switch typedExpression := expression.(type) {
	case *parser.NameExpression:
		binding, exists := generator.lookupLocal(typedExpression.Name)
		if !exists {
			// 非当前 local 名称可能是 upvalue 或全局访问，不能直接作为 RK 寄存器。
			return 0, false, nil
		}
		return binding.register, true, nil
	case *parser.LiteralExpression:
		constant, constantOK := literalConstant(typedExpression)
		if !constantOK {
			// 当前字面量类型无法放入常量表时回退通用路径。
			return 0, false, nil
		}
		constantIndex := generator.addConstant(constant)
		if constantIndex > bytecode.MaxIndexRK {
			// 溢出 RK 范围的常量需要 LOADK 临时寄存器，不能用于 inline RK 生命周期。
			return 0, false, nil
		}
		return bytecode.RKAsK(constantIndex), true, nil
	default:
		// 其他表达式可能有副作用，不能作为安全 RK 操作数。
		return 0, false, nil
	}
}

// safeRKOperand 将无副作用表达式转换为 RK 操作数。
//
// 当前只接受当前 local 名称和字面量；local 名称直接复用寄存器，字面量进入常量池并尽量使用
// RK 常量编码。若字面量超出 RK 常量范围，会额外生成 LOADK/LOADKX 临时寄存器并通过 register 返回；
// 调用方必须在发射完使用该操作数的指令后释放 register。返回 ok=false 表示表达式不满足安全条件。
func (generator *generator) safeRKOperand(expression parser.Expression) (operand int, register int, ok bool, err error) {
	switch typedExpression := expression.(type) {
	case *parser.NameExpression:
		binding, exists := generator.lookupLocal(typedExpression.Name)
		if !exists {
			// 非当前 local 名称可能是 upvalue 或全局访问，不能直接作为 RK 寄存器。
			return 0, -1, false, nil
		}
		return binding.register, -1, true, nil
	case *parser.LiteralExpression:
		constant, constantOK := literalConstant(typedExpression)
		if !constantOK {
			// 当前字面量类型无法放入常量表时回退通用路径。
			return 0, -1, false, nil
		}
		constantIndex := generator.addConstant(constant)
		rkOperand, register, rkErr := generator.rkOperandForConstantIndex(constantIndex)
		if rkErr != nil {
			// 常量载入失败时返回错误，保持原编译错误语义。
			return 0, -1, true, rkErr
		}
		return rkOperand, register, true, nil
	default:
		// 其他表达式可能有副作用，不能作为安全 RK 操作数。
		return 0, -1, false, nil
	}
}

// safeRKOperandWithRegister 将安全表达式转换为可释放临时寄存器的 RK 操作数。
//
// 当前只接受 active local 和字面量；字面量超过 RK 常量范围时会装载到临时寄存器，返回的
// register 需要调用方用 releaseOptionalRegister 释放。
func (generator *generator) safeRKOperandWithRegister(expression parser.Expression) (operand int, register int, err error) {
	operand, register, ok, err := generator.safeRKOperand(expression)
	if err != nil {
		// 常量装载失败时保持原始错误。
		return 0, -1, err
	}
	if !ok {
		// 调用方已筛选表达式形态；防御性返回编译错误便于定位损坏状态。
		return 0, -1, fmt.Errorf("codegen unsupported comparison operand %T", expression)
	}
	return operand, register, nil
}

// literalConstant 将可直接放入常量表的字面量转为 bytecode.Constant。
func literalConstant(expression *parser.LiteralExpression) (bytecode.Constant, bool) {
	if expression.Kind == lexer.TokenString {
		// 字符串字面量直接进入常量池。
		return bytecode.StringConstant(expression.Value), true
	}
	if expression.Kind == lexer.TokenNumber {
		// 数字按 lexer 分类保留 integer/number 双模型。
		switch expression.Number.Kind {
		case lexer.NumberDecimalInteger, lexer.NumberHexInteger:
			return bytecode.IntegerConstant(expression.Number.Integer), true
		case lexer.NumberDecimalFloat, lexer.NumberHexFloat:
			return bytecode.NumberConstant(expression.Number.Number), true
		default:
			return bytecode.NilConstant(), false
		}
	}
	if expression.Kind == lexer.TokenKeyword {
		// Lua 关键字字面量支持 nil/true/false。
		switch expression.Value {
		case "nil":
			return bytecode.NilConstant(), true
		case "true":
			return bytecode.BooleanConstant(true), true
		case "false":
			return bytecode.BooleanConstant(false), true
		default:
			return bytecode.NilConstant(), false
		}
	}
	return bytecode.NilConstant(), false
}

// compileAssignmentTarget 编译普通赋值左值地址。
//
// expression 必须是名称、点号字段或方括号索引左值。table/index 左值会立即求出 receiver
// 和 key，实际写入延后到所有 RHS 求值之后，兼容 Lua 5.3 多重赋值冲突规则。
func (generator *generator) compileAssignmentTarget(expression parser.Expression) (assignmentTarget, error) {
	switch targetExpression := expression.(type) {
	case *parser.NameExpression:
		// 名称左值不需要提前分配地址寄存器，但 upvalue 名称要先于 RHS 登记。
		target := assignmentTarget{name: targetExpression.Name, position: targetExpression.Position, resolvedUpvalue: -1, tableRegister: -1, keyRegister: -1}
		if _, ok := generator.lookupLocal(targetExpression.Name); ok {
			// 当前 local 写回不涉及 upvalue 捕获，保持普通名称目标。
			return target, nil
		}
		if targetExpression.Name == envUpvalueName {
			// 未声明 `_ENV` 作为左值时，先登记环境 upvalue，避免 RHS 捕获顺序抢先。
			target.resolvedUpvalue = generator.envUpvalueIndex()
			target.resolvedEnv = true
			return target, nil
		}
		upvalueIndex, captured, err := generator.resolveUpvalue(targetExpression.Name, targetExpression.Position)
		if err != nil {
			// upvalue 数量超过 Lua 5.3 上限时返回编译错误。
			return assignmentTarget{}, err
		}
		if captured {
			// 外层局部赋值需要在 RHS 之前登记，兼容 Lua 5.3 对赋值左值 upvalue 的枚举顺序。
			target.resolvedUpvalue = upvalueIndex
		}
		return target, nil
	case *parser.FieldAccessExpression:
		// 点号字段先求 receiver，字段名作为字符串 key。
		tableRegister := generator.allocateRegister()
		if err := generator.compileExpressionTo(targetExpression.Receiver, tableRegister); err != nil {
			// receiver 编译失败时释放已分配寄存器。
			generator.releaseRegister(tableRegister)
			return assignmentTarget{}, err
		}
		keyIndex := generator.addConstant(bytecode.StringConstant(targetExpression.Field))
		keyOperand, keyRegister, err := generator.rkOperandForConstantIndex(keyIndex)
		if err != nil {
			// key 常量无法编码时释放 receiver 寄存器。
			generator.releaseRegister(tableRegister)
			return assignmentTarget{}, err
		}
		return assignmentTarget{tableRegister: tableRegister, keyOperand: keyOperand, keyRegister: keyRegister}, nil
	case *parser.IndexExpression:
		// 方括号索引先求 receiver 和 index，二者都必须早于 RHS 写入。
		tableRegister := generator.allocateRegister()
		keyRegister := generator.allocateRegister()
		if err := generator.compileExpressionTo(targetExpression.Receiver, tableRegister); err != nil {
			// receiver 编译失败时释放地址寄存器。
			generator.releaseRegister(keyRegister)
			generator.releaseRegister(tableRegister)
			return assignmentTarget{}, err
		}
		if err := generator.compileExpressionTo(targetExpression.Index, keyRegister); err != nil {
			// index 编译失败时释放地址寄存器。
			generator.releaseRegister(keyRegister)
			generator.releaseRegister(tableRegister)
			return assignmentTarget{}, err
		}
		return assignmentTarget{tableRegister: tableRegister, keyOperand: keyRegister, keyRegister: keyRegister}, nil
	default:
		// parser 理论上已保证左值形态；这里保留错误便于暴露新语法缺口。
		return assignmentTarget{}, fmt.Errorf("codegen unsupported assignment target %T", expression)
	}
}

// compileAssignmentValues 编译普通赋值 RHS 并按左值数量调整结果。
//
// rights 是完整 RHS 表达式列表；targetCount 是左值数量；valueRegisters 长度必须等于
// targetCount。若 RHS 不足补 nil；若最后一个参与赋值的 RHS 是函数调用、方法调用或
// vararg 且没有多余 RHS，则展开到剩余左值；多余 RHS 仍会先求值以保留副作用。
func (generator *generator) compileAssignmentValues(rights []parser.Expression, targetCount int, valueRegisters []int) error {
	for index, register := range valueRegisters {
		if index >= len(rights) {
			// 右值不足时按 Lua 普通赋值语义补 nil。
			generator.emitABC(bytecode.OpLoadNil, register, 0, 0)
			continue
		}
		if index == len(rights)-1 {
			// 最后一个 RHS 参与赋值且右值不多于左值时，允许多返回展开。
			remaining := targetCount - index
			expanded, err := generator.compileTrailingAssignmentValue(rights[index], register, remaining)
			if err != nil {
				// 尾部多返回表达式编译失败时返回错误。
				return err
			}
			if expanded {
				// 多返回表达式已写入剩余寄存器，结束主要 RHS 编译。
				break
			}
		}
		// 非尾部多返回表达式只产生一个结果。
		if err := generator.compileExpressionTo(rights[index], register); err != nil {
			// 普通 RHS 编译失败时返回错误。
			return err
		}
	}
	for expressionIndex := targetCount; expressionIndex < len(rights); expressionIndex++ {
		// 多余 RHS 必须在写回前求值，以保留函数调用等副作用；其结果随后丢弃。
		tempRegister := generator.allocateRegister()
		if err := generator.compileExpressionTo(rights[expressionIndex], tempRegister); err != nil {
			// 多余 RHS 编译失败时返回错误。
			generator.releaseRegister(tempRegister)
			return err
		}
		generator.releaseRegister(tempRegister)
	}

	// RHS 调整完成。
	return nil
}

// compileTrailingAssignmentValue 尝试按多返回语义编译赋值尾部 RHS。
//
// 返回 expanded=true 表示 expression 已处理并写入 targetRegister 起的 remaining 个结果；
// expanded=false 表示调用方需要按普通单值表达式编译。
func (generator *generator) compileTrailingAssignmentValue(expression parser.Expression, targetRegister int, remaining int) (expanded bool, err error) {
	switch typedExpression := expression.(type) {
	case *parser.FunctionCallExpression:
		// 函数调用按剩余左值数量请求固定返回值。
		return true, generator.compileFunctionCallTo(typedExpression, targetRegister, remaining)
	case *parser.MethodCallExpression:
		// 方法调用同样按剩余左值数量请求固定返回值。
		return true, generator.compileMethodCallTo(typedExpression, targetRegister, remaining)
	case *parser.VarargExpression:
		// vararg 按剩余左值数量展开，不足部分由 VM 补 nil。
		generator.emitABC(bytecode.OpVararg, targetRegister, remaining+1, 0)
		return true, nil
	default:
		// 普通表达式不具备多返回展开能力。
		return false, nil
	}
}

// emitAssignmentTarget 将已求值 RHS 写回左值目标。
//
// target 来自 compileAssignmentTarget；valueRegister 保存要写入的单个 Lua 值。名称写回
// 会解析 local/upvalue/global，table/index 写回生成 SETTABLE。
func (generator *generator) emitAssignmentTarget(target assignmentTarget, valueRegister int) error {
	if target.name != "" {
		if target.resolvedEnv {
			// 左值地址阶段已确认 `_ENV` upvalue，直接写回当前环境。
			generator.emitABC(bytecode.OpSetupVal, valueRegister, target.resolvedUpvalue, 0)
			return nil
		}
		if target.resolvedUpvalue >= 0 {
			if generator.proto.Upvalues[target.resolvedUpvalue].Const {
				// const upvalue 不允许通过预解析左值写回覆盖。
				return constAssignmentError(target.name, target.position)
			}
			// 左值地址阶段已确认外层 upvalue，直接写回该 upvalue 下标。
			generator.emitABC(bytecode.OpSetupVal, valueRegister, target.resolvedUpvalue, 0)
			return nil
		}
		// 名称左值按 local、upvalue、_ENV 顺序写回。
		return generator.compileNameAssignmentFromRegister(&parser.NameExpression{Name: target.name}, valueRegister)
	}
	// table/index 左值使用先前求好的 receiver 和 key，不重新读取任何表达式。
	generator.emitABC(bytecode.OpSetTable, target.tableRegister, target.keyOperand, valueRegister)

	// table/index 写回完成。
	return nil
}

// compileNameAssignmentFromRegister 将已求值寄存器写回名称左值。
//
// target 优先匹配当前 local，其次写回捕获 upvalue，最后按 Lua 5.3 语义写入 `_ENV[name]`。
func (generator *generator) compileNameAssignmentFromRegister(target *parser.NameExpression, valueRegister int) error {
	targetBinding, ok := generator.lookupLocal(target.Name)
	if ok {
		if targetBinding.constLocal {
			// const 绑定不允许通过通用寄存器写回覆盖。
			return constAssignmentError(target.Name, target.Position)
		}
		// 当前作用域 local 直接 MOVE，避免 RHS 临时寄存器生命周期泄漏。
		generator.emitABC(bytecode.OpMove, targetBinding.register, valueRegister, 0)
		return nil
	}
	if target.Name == envUpvalueName {
		// 未声明的 `_ENV` 是 Lua 5.3 隐式环境 upvalue，赋值应替换当前环境而非写入全局字段。
		generator.emitABC(bytecode.OpSetupVal, valueRegister, generator.envUpvalueIndex(), 0)
		return nil
	}
	if upvalueIndex, captured, err := generator.resolveUpvalue(target.Name, target.Position); err != nil {
		// upvalue 数量超过 Lua 5.3 上限时返回编译错误。
		return err
	} else if captured {
		if generator.proto.Upvalues[upvalueIndex].Const {
			// 外层 const 捕获为 upvalue 后仍保持只读语义。
			return constAssignmentError(target.Name, target.Position)
		}
		// 外层变量通过 SETUPVAL 写回捕获 upvalue。
		generator.emitABC(bytecode.OpSetupVal, valueRegister, upvalueIndex, 0)
		return nil
	}
	if _, exists := generator.lookupGlobalConst(target.Name); exists {
		// 当前 chunk 顶层 const 全局不允许通过通用寄存器写回覆盖。
		return constAssignmentError(target.Name, target.Position)
	}

	// 未声明名称写入当前 `_ENV` 全局表。
	return generator.emitSetGlobalName(target.Name, valueRegister)
}

// compileNameAssignment 编译名称左值赋值。
//
// target 是赋值左侧名称；rights 是完整右侧表达式列表；index 指向当前左值下标。
func (generator *generator) compileNameAssignment(target *parser.NameExpression, rights []parser.Expression, index int) error {
	targetBinding, ok := generator.lookupLocal(target.Name)
	if !ok {
		if target.Name == envUpvalueName {
			// 未声明 `_ENV` 写回隐式环境 upvalue，兼容模块内 `_ENV = {}`。
			valueRegister := generator.allocateRegister()
			if err := generator.compileAssignmentValueTo(rights, index, valueRegister); err != nil {
				// 右值生成失败时释放临时寄存器并返回。
				generator.releaseRegister(valueRegister)
				return err
			}
			generator.emitABC(bytecode.OpSetupVal, valueRegister, generator.envUpvalueIndex(), 0)
			generator.releaseRegister(valueRegister)
			return nil
		}
		upvalueIndex, captured, err := generator.resolveUpvalue(target.Name, target.Position)
		if err != nil {
			// upvalue 数量超过 Lua 5.3 上限时返回编译错误。
			return err
		}
		if captured {
			if generator.proto.Upvalues[upvalueIndex].Const {
				// 外层 const 捕获为 upvalue 后仍保持只读语义。
				return constAssignmentError(target.Name, target.Position)
			}
			// 未命中当前局部但命中外层局部/upvalue 时，赋值应写回捕获的 upvalue。
			valueRegister := generator.allocateRegister()
			if err := generator.compileAssignmentValueTo(rights, index, valueRegister); err != nil {
				// 右值生成失败时释放临时寄存器并返回。
				generator.releaseRegister(valueRegister)
				return err
			}
			generator.emitABC(bytecode.OpSetupVal, valueRegister, upvalueIndex, 0)
			generator.releaseRegister(valueRegister)
			return nil
		}
		// 未声明且外层也未命中时，按 Lua 5.3 普通赋值语义写入当前 `_ENV` 表。
		if _, exists := generator.lookupGlobalConst(target.Name); exists {
			// 当前 chunk 顶层 const 全局不允许通过普通名称赋值覆盖。
			return constAssignmentError(target.Name, target.Position)
		}
		return generator.compileGlobalAssignment(target.Name, rights, index)
	}
	if targetBinding.constLocal {
		// const 绑定不允许通过普通名称赋值覆盖。
		return constAssignmentError(target.Name, target.Position)
	}
	if index < len(rights) {
		// 有右侧表达式时先写入临时寄存器，避免 `a = f(a)` 覆盖 RHS 读取旧局部值。
		valueRegister := generator.allocateRegister()
		if err := generator.compileExpressionTo(rights[index], valueRegister); err != nil {
			// 右侧表达式编译失败时返回错误。
			generator.releaseRegister(valueRegister)
			return err
		}
		generator.emitABC(bytecode.OpMove, targetBinding.register, valueRegister, 0)
		generator.releaseRegister(valueRegister)
		return nil
	}
	// 右侧表达式不足时，Lua 赋值会补 nil。
	generator.emitABC(bytecode.OpLoadNil, targetBinding.register, 0, 0)

	// 名称赋值编译完成。
	return nil
}

// compileFieldAssignment 编译点号字段左值赋值。
//
// expression.Receiver 在运行期必须是可写 table；字段名作为字符串 key 写入 SETTABLE。
func (generator *generator) compileFieldAssignment(expression *parser.FieldAccessExpression, rights []parser.Expression, index int) error {
	tableRegister := generator.allocateRegister()
	if err := generator.compileExpressionTo(expression.Receiver, tableRegister); err != nil {
		// 接收者表达式失败时释放临时寄存器。
		generator.releaseRegister(tableRegister)
		return err
	}
	valueRegister := generator.allocateRegister()
	if err := generator.compileAssignmentValueTo(rights, index, valueRegister); err != nil {
		// 右值表达式失败时释放临时寄存器。
		generator.releaseRegister(valueRegister)
		generator.releaseRegister(tableRegister)
		return err
	}
	keyIndex := generator.addConstant(bytecode.StringConstant(expression.Field))
	keyOperand, keyRegister, err := generator.rkOperandForConstantIndex(keyIndex)
	if err != nil {
		// 字段名常量无法编码或加载时释放临时寄存器。
		generator.releaseRegister(valueRegister)
		generator.releaseRegister(tableRegister)
		return err
	}
	generator.emitABC(bytecode.OpSetTable, tableRegister, keyOperand, valueRegister)
	generator.releaseOptionalRegister(keyRegister)
	generator.releaseRegister(valueRegister)
	generator.releaseRegister(tableRegister)

	// 字段赋值编译完成。
	return nil
}

// compileIndexAssignment 编译方括号索引左值赋值。
//
// expression.Index 会生成到临时寄存器并作为 SETTABLE 的 RK 寄存器 key。
func (generator *generator) compileIndexAssignment(expression *parser.IndexExpression, rights []parser.Expression, index int) error {
	tableRegister := generator.allocateRegister()
	if err := generator.compileExpressionTo(expression.Receiver, tableRegister); err != nil {
		// 接收者表达式失败时释放临时寄存器。
		generator.releaseRegister(tableRegister)
		return err
	}
	keyRegister := generator.allocateRegister()
	if err := generator.compileExpressionTo(expression.Index, keyRegister); err != nil {
		// 索引表达式失败时释放临时寄存器。
		generator.releaseRegister(keyRegister)
		generator.releaseRegister(tableRegister)
		return err
	}
	valueRegister := generator.allocateRegister()
	if err := generator.compileAssignmentValueTo(rights, index, valueRegister); err != nil {
		// 右值表达式失败时释放临时寄存器。
		generator.releaseRegister(valueRegister)
		generator.releaseRegister(keyRegister)
		generator.releaseRegister(tableRegister)
		return err
	}
	generator.emitABC(bytecode.OpSetTable, tableRegister, keyRegister, valueRegister)
	generator.releaseRegister(valueRegister)
	generator.releaseRegister(keyRegister)
	generator.releaseRegister(tableRegister)

	// 索引赋值编译完成。
	return nil
}

// compileAssignmentValueTo 编译赋值右侧单个结果。
//
// rights 是完整右侧表达式列表；index 超出范围时按 Lua 普通赋值语义写入 nil。
func (generator *generator) compileAssignmentValueTo(rights []parser.Expression, index int, targetRegister int) error {
	if index < len(rights) {
		// 有对应右值时直接编译到目标寄存器。
		return generator.compileExpressionTo(rights[index], targetRegister)
	}
	// 右值不足时补 nil。
	generator.emitABC(bytecode.OpLoadNil, targetRegister, 0, 0)

	// 赋值右值编译完成。
	return nil
}

// compileLocalFunction 编译 local function 语句。
//
// local 名称先分配寄存器，再把子 Proto closure 写入该寄存器。
func (generator *generator) compileLocalFunction(statement *parser.LocalFunctionStatement) error {
	register := generator.allocateRegister()
	generator.defineLocal(statement.Name, register, statement.Position)
	childIndex, err := generator.compileChildProto(statement.Body)
	if err != nil {
		// 子函数编译失败时返回错误，外层不能继续使用无效 closure。
		return err
	}
	generator.emitABx(bytecode.OpClosure, register, childIndex)

	// local function 编译完成。
	return nil
}

// functionExpressionClosureLine 返回函数表达式初始化时外层 CLOSURE 应暴露的源码行。
//
// expression 必须是 local 初始化中的函数表达式；Lua 5.3 line hook 会在 local 名称进入作用域前报告
// 函数体 end 行，用于让 debug.getlocal 看到 `(*temporary)`。若函数体没有 end 行信息，则退回
// function 关键字行，避免写入 0 行号。
func functionExpressionClosureLine(expression *parser.FunctionExpression) lexer.Position {
	if expression == nil || expression.Body == nil {
		// 损坏表达式无法读取函数体，退回零值位置交由调用方保留原行号。
		return lexer.Position{}
	}
	body := expression.Body
	if body.LastLineDefined > 0 {
		// 函数 end 行用于 local 初始化期 CLOSURE 的 line hook。
		return lexer.Position{Line: body.LastLineDefined}
	}
	// 最后退回表达式自身位置。
	return expression.Position
}

// compileFunctionStatement 编译普通 function 语句。
//
// 当前阶段支持函数名对应已有 local，未知函数名按 Lua 5.3 语义写入 `_ENV[name]`。
func (generator *generator) compileFunctionStatement(statement *parser.FunctionStatement) error {
	targetBinding, ok := generator.lookupLocal(statement.Name)
	childIndex, err := generator.compileChildProto(statement.Body)
	if err != nil {
		// 子函数编译失败时返回错误。
		return err
	}
	if !ok {
		// 普通全局函数定义等价于 `_ENV[name] = closure`。
		valueRegister := generator.allocateRegister()
		generator.emitABx(bytecode.OpClosure, valueRegister, childIndex)
		if err := generator.emitSetGlobalName(statement.Name, valueRegister); err != nil {
			// 全局函数名常量无法 RK 编码时返回错误。
			generator.releaseRegister(valueRegister)
			return err
		}
		generator.releaseRegister(valueRegister)
		return nil
	}
	if targetBinding.constLocal {
		// const 绑定不允许通过 function 语句覆盖。
		return constAssignmentError(statement.Name, statement.Position)
	}
	generator.emitABx(bytecode.OpClosure, targetBinding.register, childIndex)

	// 普通 function 编译完成。
	return nil
}

// compileCompactFunctionStatement 编译 parser compact 模式生成的简单 function 语句。
//
// 该路径只覆盖 `function name(param) return param + integer end`，并复用普通 function 的 local/global
// 绑定语义；任何非目标形态都不会进入该类型。
func (generator *generator) compileCompactFunctionStatement(statement *parser.CompactFunctionStatement) error {
	targetBinding, ok := generator.lookupLocal(statement.Name)
	childIndex, err := generator.compileCompactFunctionChildProto(statement)
	if err != nil {
		// 子函数 Proto 生成失败时返回错误，外层不能继续使用无效 closure。
		return err
	}
	if !ok {
		// 普通全局函数定义等价于 `_ENV[name] = closure`。
		valueRegister := generator.allocateRegister()
		generator.emitABx(bytecode.OpClosure, valueRegister, childIndex)
		if err := generator.emitSetGlobalName(statement.Name, valueRegister); err != nil {
			// 全局函数名常量无法 RK 编码时返回错误。
			generator.releaseRegister(valueRegister)
			return err
		}
		generator.releaseRegister(valueRegister)
		return nil
	}
	if targetBinding.constLocal {
		// const 绑定不允许通过 compact function 语句覆盖。
		return constAssignmentError(statement.Name, statement.Pos())
	}
	generator.emitABx(bytecode.OpClosure, targetBinding.register, childIndex)

	// compact function 编译完成。
	return nil
}

// compileIfStatement 编译 if/elseif/else 条件分支语句。
//
// 安全有序比较条件直译为 LT/LE 加失败跳转；其他条件先生成到临时寄存器，
// TEST C=0 搭配 JMP 表示 false/nil 时跳到下一分支。
func (generator *generator) compileIfStatement(statement *parser.IfStatement) error {
	var endJumpPCs []int
	for clauseIndex := range statement.Clauses {
		// 每个 if/elseif 条件失败时跳到下一 clause 或 else。
		clause := statement.Clauses[clauseIndex]
		falseTargetPosition := generator.ifFalseTargetPosition(statement, clauseIndex)
		falseJumpPC := 0
		if handled, err := generator.compileIfComparisonCondition(clause, falseTargetPosition, &falseJumpPC); err != nil || handled {
			if err != nil {
				// 条件比较编译失败时返回错误，避免生成不完整分支。
				return err
			}
		} else {
			conditionRegister, releaseConditionRegister := generator.ifConditionRegister(clause.Condition)
			if err := generator.withSourceLine(clause.Condition.Pos(), func() error {
				// 条件表达式自身归属表达式起始行；简单 local 名称可直接复用寄存器。
				return generator.compileIfConditionTo(clause.Condition, conditionRegister, releaseConditionRegister)
			}); err != nil {
				// 条件表达式编译失败时释放临时寄存器并返回。
				if releaseConditionRegister {
					generator.releaseRegister(conditionRegister)
				}
				return err
			}
			if err := generator.withSourceLine(clause.ThenPosition, func() error {
				// TEST 表示进入 then 检查；多行 if 条件需要暴露 then 所在行给 line hook。
				generator.emitABC(bytecode.OpTest, conditionRegister, 0, 0)
				return nil
			}); err != nil {
				// 当前闭包只生成指令，不预期返回错误；保留分支便于未来扩展。
				if releaseConditionRegister {
					generator.releaseRegister(conditionRegister)
				}
				return err
			}
			if err := generator.withSourceLine(falseTargetPosition, func() error {
				// 条件失败时执行该 JMP，因此它的行号应指向下一分支、else 或 end。
				falseJumpPC = generator.emitJump(0)
				return nil
			}); err != nil {
				// 当前闭包只生成指令，不预期返回错误；保留分支便于未来扩展。
				if releaseConditionRegister {
					generator.releaseRegister(conditionRegister)
				}
				return err
			}
			if releaseConditionRegister {
				generator.releaseRegister(conditionRegister)
			}
		}
		if err := generator.compileScopedBlock(clause.Block); err != nil {
			// 分支 block 编译失败时返回错误。
			return err
		}
		if clauseIndex+1 < len(statement.Clauses) || statement.ElseBlock != nil {
			// 当前分支后仍有 elseif/else 时，执行完 then 必须跳过后续分支。
			endJumpPC := 0
			if err := generator.withSourceLine(statement.EndPosition, func() error {
				// 分支执行完成后跳到 if 结束，该 JMP 被执行时应报告 end 行。
				endJumpPC = generator.emitJump(0)
				return nil
			}); err != nil {
				// 当前闭包只生成指令，不预期返回错误；保留分支便于未来扩展。
				return err
			}
			endJumpPCs = append(endJumpPCs, endJumpPC)
		}
		generator.patchJump(falseJumpPC, len(generator.proto.Code))
	}
	if statement.ElseBlock != nil {
		// else block 没有条件，位于所有条件失败路径之后。
		if err := generator.compileScopedBlock(statement.ElseBlock); err != nil {
			// else block 编译失败时返回错误。
			return err
		}
		elseEndJumpPC := 0
		if err := generator.withSourceLine(statement.EndPosition, func() error {
			// else 分支没有统一的 endJump；补一个零距离 JMP 让 line hook 能看到 end 行。
			elseEndJumpPC = generator.emitJump(0)
			return nil
		}); err != nil {
			// 当前闭包只生成指令，不预期返回错误；保留分支便于未来扩展。
			return err
		}
		generator.patchJump(elseEndJumpPC, len(generator.proto.Code))
	}
	for _, jumpPC := range endJumpPCs {
		// 所有已执行分支统一跳过后续分支到 if 结束位置。
		generator.patchJump(jumpPC, len(generator.proto.Code))
	}

	// if 语句编译完成。
	return nil
}

// compileIfComparisonCondition 将安全 if/elseif 有序比较条件直译成 Lua 5.3 测试跳转。
//
// 仅当条件是有序比较表达式，且左右操作数都是当前 local 或字面量时启用；等值比较暂保留通用
// boolean 物化路径，避免 debug return hook 中 `event == "return"` 的栈层级推断发生变化。
func (generator *generator) compileIfComparisonCondition(clause parser.IfClause, falseJumpPosition lexer.Position, falseJumpPC *int) (bool, error) {
	expression, ok := clause.Condition.(*parser.BinaryExpression)
	if !ok {
		// 非二元表达式继续走通用 TEST 路径。
		return false, nil
	}
	switch expression.Operator {
	case "<", "<=", ">", ">=":
		// 有序比较可以直译为条件跳转，递归和算术分支可减少 boolean 临时值。
	default:
		// 等值比较可能影响 return hook 内基于当前 PC 的调试名/层级推断，暂不直译。
		return false, nil
	}
	return generator.compileComparisonConditionJump(clause.Condition, clause.Condition.Pos(), falseJumpPosition, falseJumpPC)
}

// ifConditionRegister 选择 if 条件 TEST 使用的寄存器。
//
// 当前 local 名称条件可直接复用已有寄存器，避免 `if a then` 额外生成 MOVE；其他表达式仍分配
// 临时寄存器以保留求值、副作用和错误语义。
func (generator *generator) ifConditionRegister(expression parser.Expression) (register int, release bool) {
	if nameExpression, ok := expression.(*parser.NameExpression); ok {
		if binding, exists := generator.lookupLocal(nameExpression.Name); exists {
			// 当前 local 读取不会触发元方法或调用，TEST 可直接读取其寄存器。
			return binding.register, false
		}
	}
	return generator.allocateRegister(), true
}

// compileIfConditionTo 编译 if 条件表达式到 TEST 使用的寄存器。
func (generator *generator) compileIfConditionTo(expression parser.Expression, targetRegister int, targetIsTemporary bool) error {
	if !targetIsTemporary {
		// 非临时目标只会来自当前 local 名称，源值已在寄存器中，无需生成 MOVE。
		return nil
	}
	return generator.compileExpressionTo(expression, targetRegister)
}

// ifFalseTargetPosition 返回 if/elseif 条件失败时跳转指令应标注的源码行。
//
// statement 必须是完整 if 语句；clauseIndex 是当前条件分支下标。返回值优先指向下一 elseif，
// 其次指向 else，最后指向 end，匹配 Lua line hook 在控制流跳转上的可见行。
func (generator *generator) ifFalseTargetPosition(statement *parser.IfStatement, clauseIndex int) lexer.Position {
	if clauseIndex+1 < len(statement.Clauses) {
		// 失败后进入下一 elseif 条件，JMP 行号标注到 elseif 关键字。
		return statement.Clauses[clauseIndex+1].Position
	}
	if statement.ElseBlock != nil {
		// 失败后进入 else 分支，JMP 行号标注到 else 内第一条语句；空 else 则退回 end。
		return firstBlockLinePosition(statement.ElseBlock, statement.EndPosition)
	}
	// 没有后续分支时失败直接越过 if，JMP 行号标注到 end 关键字。
	return statement.EndPosition
}

// firstBlockLinePosition 返回 block 第一条可执行语句或 return 的源码位置。
//
// block 可为空；fallback 用于空 block 或无普通语句时保持 line hook 有稳定落点。
func firstBlockLinePosition(block *parser.Block, fallback lexer.Position) lexer.Position {
	if block == nil {
		// 空 block 没有可执行语句，使用调用方提供的兜底位置。
		return fallback
	}
	if len(block.Statements) > 0 {
		// 普通语句优先作为 block 入口行。
		return block.Statements[0].Pos()
	}
	if block.Return != nil {
		// 只有 return 的 block 以 return 行作为入口行。
		return block.Return.Pos()
	}
	return fallback
}

// compileWhileStatement 编译 while 条件循环语句。
//
// 条件每轮在循环头重新求值；TEST C=0 搭配下一条 JMP 表示 false/nil 时跳出循环。
// 循环体结束后生成回跳到条件起点的 JMP。break/goto 的跳转列表会在后续控制流任务接入。
func (generator *generator) compileWhileStatement(statement *parser.WhileStatement) error {
	conditionPC := len(generator.proto.Code)
	loopIndex := generator.beginLoop()
	falseJumpPC := 0
	if handled, err := generator.compileWhileComparisonCondition(statement, &falseJumpPC); err != nil || handled {
		if err != nil {
			// 条件比较编译失败时丢弃当前循环状态并返回。
			generator.discardLoop(loopIndex)
			return err
		}
	} else {
		conditionRegister := generator.allocateRegister()
		if err := generator.withSourceLine(statement.Condition.Pos(), func() error {
			// while 条件表达式每轮执行时报告条件所在源码行。
			return generator.compileExpressionTo(statement.Condition, conditionRegister)
		}); err != nil {
			// 条件表达式编译失败时释放临时寄存器并返回。
			generator.releaseRegister(conditionRegister)
			generator.discardLoop(loopIndex)
			return err
		}
		if err := generator.withSourceLine(statement.DoPosition, func() error {
			// TEST 对应 while 条件到 do 的控制检查，多行条件时需要保留 do 行。
			generator.emitABC(bytecode.OpTest, conditionRegister, 0, 0)
			return nil
		}); err != nil {
			// 当前闭包只生成指令，不预期返回错误；保留分支便于未来扩展。
			generator.releaseRegister(conditionRegister)
			generator.discardLoop(loopIndex)
			return err
		}
		if err := generator.withSourceLine(statement.EndPosition, func() error {
			// 条件失败时执行该 JMP，line hook 应能看到 while 的 end 行。
			falseJumpPC = generator.emitJump(0)
			return nil
		}); err != nil {
			// 当前闭包只生成指令，不预期返回错误；保留分支便于未来扩展。
			generator.releaseRegister(conditionRegister)
			generator.discardLoop(loopIndex)
			return err
		}
		generator.releaseRegister(conditionRegister)
	}

	if err := generator.compileScopedBlock(statement.Body); err != nil {
		// 循环体编译失败时返回错误。
		generator.discardLoop(loopIndex)
		return err
	}
	backJumpPC := 0
	if err := generator.withSourceLine(statement.Position, func() error {
		// 循环体末尾回跳到 while 条件，执行该 JMP 时报告 while 关键字行。
		backJumpPC = generator.emitJump(0)
		return nil
	}); err != nil {
		// 当前闭包只生成指令，不预期返回错误；保留分支便于未来扩展。
		generator.discardLoop(loopIndex)
		return err
	}
	if err := generator.patchJumpChecked(backJumpPC, conditionPC); err != nil {
		// 循环体过长导致回跳超出 sBx 编码范围时，按 Lua 5.3 编译错误返回。
		generator.discardLoop(loopIndex)
		return err
	}
	generator.patchLoopContinues(loopIndex, conditionPC)
	if err := generator.patchJumpChecked(falseJumpPC, len(generator.proto.Code)); err != nil {
		// 跳出 while 的前向距离过长时同样报告控制结构过长。
		generator.discardLoop(loopIndex)
		return err
	}
	generator.patchLoopBreaks(loopIndex, len(generator.proto.Code))

	// while 语句编译完成。
	return nil
}

// compileWhileComparisonCondition 将安全比较条件直译成 Lua 5.3 测试跳转。
//
// 仅当 while 条件是比较表达式，且左右操作数都是当前 local 或字面量时启用；其他表达式继续走
// 通用 boolean 物化路径，避免改变调用、索引、全局读取、短路逻辑和元方法错误时机。
func (generator *generator) compileWhileComparisonCondition(statement *parser.WhileStatement, falseJumpPC *int) (bool, error) {
	return generator.compileComparisonConditionJump(statement.Condition, statement.Condition.Pos(), statement.EndPosition, falseJumpPC)
}

// compileComparisonConditionJump 将安全比较条件直译为“真时跳过 false JMP”的控制流。
//
// condition 只在比较表达式且左右操作数都是当前 local 或字面量时启用；其他表达式返回
// handled=false，由调用方继续使用通用 boolean 物化路径。
func (generator *generator) compileComparisonConditionJump(condition parser.Expression, conditionPosition lexer.Position, falseJumpPosition lexer.Position, falseJumpPC *int) (bool, error) {
	handled, err := generator.compileComparisonConditionTest(condition, conditionPosition)
	if err != nil || !handled {
		// 调用方需要知道是否已消费该条件；错误保持原始编译错误。
		return handled, err
	}
	if err := generator.withSourceLine(falseJumpPosition, func() error {
		// 条件为 false 时不跳过该 JMP，从而进入调用方指定的 false 分支。
		*falseJumpPC = generator.emitJump(0)
		return nil
	}); err != nil {
		// 当前闭包只生成指令，不预期返回错误；保留分支便于未来扩展。
		return true, err
	}
	return true, nil
}

// compileComparisonConditionTest 只生成安全比较条件的测试指令。
//
// 生成的 EQ/LT/LE 指令在条件为 true 时跳过下一条指令；调用方负责紧跟 false JMP 或
// repeat-until 的回跳 JMP。
func (generator *generator) compileComparisonConditionTest(condition parser.Expression, conditionPosition lexer.Position) (bool, error) {
	expression, ok := condition.(*parser.BinaryExpression)
	if !ok || !isComparisonOperator(expression.Operator) {
		// 非比较条件仍使用通用 TEST 路径。
		return false, nil
	}
	if !generator.isSafeRKCandidate(expression.Left) || !generator.isSafeRKCandidate(expression.Right) {
		// 复杂操作数可能有副作用或运行期查找，不能直译为测试指令。
		return false, nil
	}
	leftOperand, leftRegister, err := generator.safeRKOperandWithRegister(expression.Left)
	if err != nil {
		// 左操作数常量装载失败时返回编译错误。
		return true, err
	}
	rightOperand, rightRegister, err := generator.safeRKOperandWithRegister(expression.Right)
	if err != nil {
		// 右操作数失败时释放左操作数临时寄存器。
		generator.releaseOptionalRegister(leftRegister)
		return true, err
	}
	opCode, skipOnTrueA, swapOperands := comparisonConditionOpCode(expression.Operator)
	if swapOperands {
		// `>` 和 `>=` 通过交换左右操作数复用 LT/LE。
		leftOperand, rightOperand = rightOperand, leftOperand
	}
	if err := generator.withSourceLine(conditionPosition, func() error {
		// 比较测试指令直接决定是否跳过后续 false JMP。
		generator.emitABC(opCode, skipOnTrueA, leftOperand, rightOperand)
		return nil
	}); err != nil {
		// 当前闭包只生成指令，不预期返回错误；释放临时寄存器后返回。
		generator.releaseOptionalRegister(rightRegister)
		generator.releaseOptionalRegister(leftRegister)
		return true, err
	}
	generator.releaseOptionalRegister(rightRegister)
	generator.releaseOptionalRegister(leftRegister)
	return true, nil
}

// compileRepeatUntilStatement 编译 repeat-until 后置条件循环。
//
// repeat 至少执行一次循环体；until 条件为 false/nil 时回跳到循环体起点，条件为真时继续向后。
func (generator *generator) compileRepeatUntilStatement(statement *parser.RepeatUntilStatement) error {
	bodyPC := len(generator.proto.Code)
	loopIndex := generator.beginLoop()
	scope := generator.beginScope()
	if err := generator.compileBlock(statement.Body); err != nil {
		// 循环体编译失败时恢复 repeat 作用域、丢弃 break 列表并返回。
		generator.endScope(scope, len(generator.proto.Code))
		generator.discardLoop(loopIndex)
		return err
	}
	conditionPC := len(generator.proto.Code)
	backJumpPC := 0
	if handled, err := generator.compileComparisonConditionTest(statement.Condition, statement.Condition.Pos()); err != nil || handled {
		if err != nil {
			// 条件比较编译失败时恢复 repeat 作用域、丢弃循环状态并返回。
			generator.endScope(scope, len(generator.proto.Code))
			generator.discardLoop(loopIndex)
			return err
		}
		// repeat-until 的条件表达式可以访问循环体 local；条件求值完成后立即闭合本轮作用域。
		generator.endScope(scope, len(generator.proto.Code))
		if err := generator.withSourceLine(statement.UntilPosition, func() error {
			// 条件失败时执行回跳，保持与 until 行一致。
			backJumpPC = generator.emitJump(0)
			return nil
		}); err != nil {
			// 当前闭包只生成指令，不预期返回错误；保留分支便于未来扩展。
			generator.discardLoop(loopIndex)
			return err
		}
	} else {
		conditionRegister := generator.allocateRegister()
		if err := generator.withSourceLine(statement.Condition.Pos(), func() error {
			// until 后置条件表达式归属条件起始行。
			return generator.compileExpressionTo(statement.Condition, conditionRegister)
		}); err != nil {
			// 条件表达式编译失败时释放寄存器、恢复 repeat 作用域并丢弃循环状态。
			generator.releaseRegister(conditionRegister)
			generator.endScope(scope, len(generator.proto.Code))
			generator.discardLoop(loopIndex)
			return err
		}
		// repeat-until 的条件表达式可以访问循环体 local；条件求值完成后立即闭合本轮作用域，
		// 让回跳进入下一轮时重新创建 local/upvalue cell。
		generator.endScope(scope, len(generator.proto.Code))
		if err := generator.withSourceLine(statement.UntilPosition, func() error {
			// TEST/JMP 都属于 until 条件检查，line hook 应报告 until 所在行。
			generator.emitABC(bytecode.OpTest, conditionRegister, 0, 0)
			return nil
		}); err != nil {
			// 当前闭包只生成指令，不预期返回错误；保留分支便于未来扩展。
			generator.releaseRegister(conditionRegister)
			generator.discardLoop(loopIndex)
			return err
		}
		if err := generator.withSourceLine(statement.UntilPosition, func() error {
			// 条件失败时执行回跳，保持与 until 行一致。
			backJumpPC = generator.emitJump(0)
			return nil
		}); err != nil {
			// 当前闭包只生成指令，不预期返回错误；保留分支便于未来扩展。
			generator.releaseRegister(conditionRegister)
			generator.discardLoop(loopIndex)
			return err
		}
		generator.releaseRegister(conditionRegister)
	}
	if err := generator.patchJumpChecked(backJumpPC, bodyPC); err != nil {
		// repeat-until 循环体过长时，回跳无法编码为 sBx，必须停止编译。
		generator.discardLoop(loopIndex)
		return err
	}
	generator.patchLoopContinues(loopIndex, conditionPC)
	generator.patchLoopBreaks(loopIndex, len(generator.proto.Code))

	// repeat-until 语句编译完成。
	return nil
}

// compileBreakStatement 编译 break 语句。
//
// break 只能出现在循环体内；当前生成一条待回填 JMP，循环编译结束时统一跳到循环后一条指令。
func (generator *generator) compileBreakStatement() error {
	if len(generator.breakJumps) == 0 {
		// parser 语义通常会拦截循环外 break；这里保留防御式错误。
		return fmt.Errorf("codegen break outside loop")
	}

	breakPC := generator.emitJump(generator.currentLoopCloseRegister())
	lastLoopIndex := len(generator.breakJumps) - 1
	generator.breakJumps[lastLoopIndex] = append(generator.breakJumps[lastLoopIndex], breakPC)
	return nil
}

// compileFunctionCallStatement 编译函数调用语句。
//
// Lua 调用语句会丢弃全部返回值，因此 CALL 的 C 字段使用 1。
func (generator *generator) compileFunctionCallStatement(statement *parser.FunctionCallStatement) error {
	switch callExpression := statement.Call.(type) {
	case *parser.FunctionCallExpression:
		// 普通函数调用语句丢弃全部返回值。
		callRegister := generator.allocateRegister()
		if err := generator.compileFunctionCallTo(callExpression, callRegister, 0); err != nil {
			// 调用表达式失败时释放调用寄存器后返回。
			generator.releaseRegister(callRegister)
			return err
		}
		generator.releaseRegistersFrom(callRegister)
		return nil
	case *parser.MethodCallExpression:
		// method call 语句通过 SELF 生成隐式 self 参数，并丢弃全部返回值。
		callRegister := generator.allocateRegister()
		if err := generator.compileMethodCallTo(callExpression, callRegister, 0); err != nil {
			// 调用表达式失败时释放调用寄存器后返回。
			generator.releaseRegister(callRegister)
			return err
		}
		generator.releaseRegistersFrom(callRegister)
		return nil
	default:
		// 其他表达式不能作为调用语句。
		return fmt.Errorf("codegen unsupported call statement %T", statement.Call)
	}
}

// compileNumericFor 编译数值 for 循环。
//
// 寄存器布局为 R(A)=内部 index、R(A+1)=limit、R(A+2)=step、R(A+3)=外部控制槽。
// 循环体开始时额外创建同名 body local 并从 R(A+3) 拷贝当前值，让闭包捕获每轮独立变量。
func (generator *generator) compileNumericFor(statement *parser.NumericForStatement) error {
	loopIndex := generator.beginLoop()
	scope := generator.beginScope()
	baseRegister := generator.allocateRegister()
	limitRegister := generator.allocateRegister()
	stepRegister := generator.allocateRegister()
	externalRegister := generator.allocateRegister()
	if err := generator.compileExpressionTo(statement.Init, baseRegister); err != nil {
		// 初始值表达式必须生成到内部 index 寄存器。
		generator.endScope(scope, len(generator.proto.Code))
		generator.discardLoop(loopIndex)
		return err
	}
	if err := generator.compileExpressionTo(statement.Limit, limitRegister); err != nil {
		// 边界值表达式必须生成到 limit 寄存器。
		generator.endScope(scope, len(generator.proto.Code))
		generator.discardLoop(loopIndex)
		return err
	}
	if statement.Step != nil {
		// 显式步长生成到 step 寄存器。
		if err := generator.compileExpressionTo(statement.Step, stepRegister); err != nil {
			// 步长表达式编译失败时返回错误。
			generator.endScope(scope, len(generator.proto.Code))
			generator.discardLoop(loopIndex)
			return err
		}
	} else {
		// 缺省步长按 Lua 语义使用 integer 1。
		stepIndex := generator.addConstant(bytecode.IntegerConstant(1))
		generator.emitABx(bytecode.OpLoadK, stepRegister, stepIndex)
	}
	perIterationControl := blockContainsFunction(statement.Body)
	if !perIterationControl {
		// 无闭包捕获风险时复用 R(A+3) 作为可见循环变量，保持空循环指令数接近 Lua 5.3。
		generator.defineLocal(statement.Name, externalRegister, statement.Position)
	}
	forPrepPC := generator.emitJumpFor(bytecode.OpForPrep, baseRegister)
	bodyStartPC := len(generator.proto.Code)
	bodyScope := generator.beginScope()
	if perIterationControl {
		// 循环体含闭包时，每轮创建同名可见局部，避免闭包共享 numeric for 控制槽。
		visibleRegister := generator.allocateRegister()
		generator.emitABC(bytecode.OpMove, visibleRegister, externalRegister, 0)
		generator.defineLocal(statement.Name, visibleRegister, statement.Position)
	}
	if err := generator.compileBlock(statement.Body); err != nil {
		// 循环体编译失败时返回错误。
		generator.endScope(bodyScope, len(generator.proto.Code))
		generator.endScope(scope, len(generator.proto.Code))
		generator.discardLoop(loopIndex)
		return err
	}
	generator.endScope(bodyScope, len(generator.proto.Code))
	forLoopPC := generator.emitJumpFor(bytecode.OpForLoop, baseRegister)
	generator.patchLoopContinues(loopIndex, forLoopPC)
	generator.patchJump(forPrepPC, forLoopPC)
	generator.patchJump(forLoopPC, bodyStartPC)
	endMarkerPC := 0
	if err := generator.withSourceLine(statement.EndPosition, func() error {
		// FORLOOP 退出后顺序执行该零距离 JMP，用于向 line hook 暴露 end 行。
		endMarkerPC = generator.emitJump(0)
		return nil
	}); err != nil {
		// 当前闭包只生成指令，不预期返回错误；保留分支便于未来扩展。
		generator.endScope(scope, len(generator.proto.Code))
		generator.discardLoop(loopIndex)
		return err
	}
	generator.patchJump(endMarkerPC, len(generator.proto.Code))
	generator.patchLoopBreaks(loopIndex, endMarkerPC)
	if err := generator.withSourceLine(statement.EndPosition, func() error {
		// 关闭循环局部变量的 close-only JMP 也归属 end 行，避免退出后误报 for 行。
		generator.endScope(scope, len(generator.proto.Code))
		return nil
	}); err != nil {
		// 当前闭包只生成指令，不预期返回错误；保留分支便于未来扩展。
		generator.discardLoop(loopIndex)
		return err
	}

	// numeric for 作用域结束后释放循环寄存器和循环体局部变量。
	return nil
}

// compileGenericFor 编译泛型 for 循环。
//
// 寄存器布局为 R(A)=迭代函数、R(A+1)=状态、R(A+2)=控制变量，R(A+3).. 保存迭代变量。
func (generator *generator) compileGenericFor(statement *parser.GenericForStatement) error {
	loopIndex := generator.beginLoop()
	scope := generator.beginScope()
	baseRegister := generator.allocateRegister()
	stateRegister := generator.allocateRegister()
	controlRegister := generator.allocateRegister()
	iteratorRegisters := []int{baseRegister, stateRegister, controlRegister}
	filledIteratorRegisters := 0
	for iteratorIndex, iteratorExpression := range statement.Iterators {
		if iteratorIndex >= len(iteratorRegisters) {
			// 当前最小实现只接收 Lua 泛型 for 需要的前三个迭代表达式。
			break
		}
		if iteratorIndex == len(statement.Iterators)-1 {
			// 最后一个迭代表达式需要按泛型 for 协议展开到 iterator/state/control。
			remaining := len(iteratorRegisters) - iteratorIndex
			expandedIterator := true
			switch expression := iteratorExpression.(type) {
			case *parser.FunctionCallExpression:
				if err := generator.compileFunctionCallTo(expression, iteratorRegisters[iteratorIndex], remaining); err != nil {
					// 迭代函数调用编译失败时返回错误。
					generator.endScope(scope, len(generator.proto.Code))
					generator.discardLoop(loopIndex)
					return err
				}
			case *parser.MethodCallExpression:
				if err := generator.compileMethodCallTo(expression, iteratorRegisters[iteratorIndex], remaining); err != nil {
					// 迭代方法调用编译失败时返回错误。
					generator.endScope(scope, len(generator.proto.Code))
					generator.discardLoop(loopIndex)
					return err
				}
			case *parser.VarargExpression:
				// vararg 作为最后迭代表达式时展开到剩余 iterator/state/control。
				generator.emitABC(bytecode.OpVararg, iteratorRegisters[iteratorIndex], remaining+1, 0)
			default:
				// 普通迭代表达式只产生单个值，继续走通用表达式编译。
				expandedIterator = false
			}
			if expandedIterator {
				// 已经完成剩余 iterator/state/control 写入，不再继续编译后续迭代表达式。
				filledIteratorRegisters = len(iteratorRegisters)
				break
			}
		}
		if err := generator.compileExpressionTo(iteratorExpression, iteratorRegisters[iteratorIndex]); err != nil {
			// 任一迭代表达式编译失败时返回错误。
			generator.endScope(scope, len(generator.proto.Code))
			generator.discardLoop(loopIndex)
			return err
		}
		filledIteratorRegisters = iteratorIndex + 1
	}
	for iteratorIndex := filledIteratorRegisters; iteratorIndex < len(iteratorRegisters); iteratorIndex++ {
		// 缺失的 state/control 按 Lua 多返回值补 nil 的效果处理。
		generator.emitABC(bytecode.OpLoadNil, iteratorRegisters[iteratorIndex], 0, 0)
	}
	// 迭代表达式编译可能使用 R(A+3).. 作为临时寄存器；循环变量必须紧跟隐藏三元组重新分配。
	generator.releaseRegistersFrom(baseRegister + len(iteratorRegisters))
	for _, name := range statement.Names {
		// 迭代变量从 R(A+3) 开始，供 TFORCALL 写入。
		register := generator.allocateRegister()
		generator.defineLocal(name, register, statement.Position)
	}
	entryJumpPC := generator.emitJump(0)
	bodyStartPC := len(generator.proto.Code)
	if err := generator.compileBlock(statement.Body); err != nil {
		// 循环体编译失败时返回错误。
		generator.endScope(scope, len(generator.proto.Code))
		generator.discardLoop(loopIndex)
		return err
	}
	tforCallPosition := statement.Position
	if len(statement.Iterators) > 0 {
		// TFORCALL 运行期错误应指向迭代表达式所在行，而不是 for 关键字行。
		tforCallPosition = statement.Iterators[0].Pos()
	}
	tforCallPC := 0
	if err := generator.withSourceLine(tforCallPosition, func() error {
		tforCallPC = generator.emitABC(bytecode.OpTForCall, baseRegister, 0, len(statement.Names))
		return nil
	}); err != nil {
		// 当前闭包只生成指令，不预期返回错误；保留分支便于未来扩展。
		generator.endScope(scope, len(generator.proto.Code))
		generator.discardLoop(loopIndex)
		return err
	}
	generator.patchLoopContinues(loopIndex, tforCallPC)
	tforLoopPC := generator.emitJumpFor(bytecode.OpTForLoop, baseRegister+2)
	generator.patchJump(entryJumpPC, tforCallPC)
	generator.patchJump(tforLoopPC, bodyStartPC)
	endMarkerPC := 0
	if err := generator.withSourceLine(statement.EndPosition, func() error {
		// TFORLOOP 退出后顺序执行该零距离 JMP，用于向 line hook 暴露 end 行。
		endMarkerPC = generator.emitJump(0)
		return nil
	}); err != nil {
		// 当前闭包只生成指令，不预期返回错误；保留分支便于未来扩展。
		generator.endScope(scope, len(generator.proto.Code))
		generator.discardLoop(loopIndex)
		return err
	}
	generator.patchJump(endMarkerPC, len(generator.proto.Code))
	generator.patchLoopBreaks(loopIndex, endMarkerPC)
	if err := generator.withSourceLine(statement.EndPosition, func() error {
		// 关闭循环局部变量的 close-only JMP 也归属 end 行，避免退出后误报 for 行。
		generator.endScope(scope, len(generator.proto.Code))
		return nil
	}); err != nil {
		// 当前闭包只生成指令，不预期返回错误；保留分支便于未来扩展。
		generator.discardLoop(loopIndex)
		return err
	}

	// generic for 作用域结束后释放迭代寄存器和循环体局部变量。
	return nil
}

// compileChildProto 编译嵌套函数体并追加到当前 Proto。
//
// 函数参数会作为子函数局部变量写入连续寄存器，返回值是当前 Proto.p 的子原型索引。
func (generator *generator) compileChildProto(body *parser.FunctionBody) (int, error) {
	child := newChildGenerator(generator, generator.proto.Source)
	child.proto.NumParams = uint8(len(body.Params))
	child.proto.IsVararg = body.Vararg
	child.proto.LineDefined = body.LineDefined
	child.proto.LastLineDefined = body.LastLineDefined
	child.prepareDirectFunctionBlockCapacity(body.Body)
	for _, paramName := range body.Params {
		// 参数寄存器从 R0 开始，既是入参位置也是局部变量位置。
		register := child.allocateRegister()
		child.defineLocal(paramName, register, body.Position)
	}
	if handled, err := child.compileCompactSimpleFunctionBody(body); handled || err != nil {
		// compact summary 命中时已生成完整显式 return；错误则保持原 codegen 错误语义。
		if err != nil {
			return 0, err
		}
	} else if err := child.compileBlock(body.Body); err != nil {
		// 子函数 block 编译失败时返回错误。
		return 0, err
	}
	if err := child.patchPendingGotos(); err != nil {
		// 子函数内 goto 必须只回填到子函数自己的 label。
		return 0, err
	}
	if !child.returned {
		// 子函数没有显式 return 时补默认无返回值 RETURN。
		if err := child.withSourceLine(lexer.Position{Line: body.LastLineDefined}, func() error {
			child.emitABC(bytecode.OpReturn, 0, 1, 0)
			return nil
		}); err != nil {
			// 默认 RETURN 发射失败时返回错误；当前实现不会触发，仅保持控制流一致。
			return 0, err
		}
	}
	child.closeLocals(len(child.proto.Code))
	child.proto.MaxStackSize = child.maxStackSize()

	// 追加子 Proto 并返回索引。
	return generator.proto.AddChild(child.proto), nil
}

// compileCompactFunctionChildProto 直接生成简单 function 语句的子 Proto。
//
// statement 已由 parser 精确证明为单参数整数加法返回；本方法仍按普通 codegen 规则写入参数 local、
// ADD/RETURN 行号、Proto 行范围和 MaxStackSize，保证 debug 与 luac 反汇编等价。
func (generator *generator) compileCompactFunctionChildProto(statement *parser.CompactFunctionStatement) (int, error) {
	child := generator.borrowChildProto(generator.proto.Source)
	child.NumParams = 1
	child.IsVararg = false
	child.LineDefined = statement.LineDefined
	child.LastLineDefined = statement.LastLineDefined

	child.Constants = append(child.Constants, bytecode.IntegerConstant(statement.Integer))
	child.Code = append(child.Code,
		bytecode.CreateABC(bytecode.OpAdd, 1, 0, bytecode.RKAsK(0)),
		bytecode.CreateABC(bytecode.OpReturn, 1, 2, 0),
	)
	child.LineInfo = append(child.LineInfo, statement.OperatorLine, statement.ReturnLine)
	child.LocalVars = append(child.LocalVars, bytecode.LocalVar{
		Name:     statement.ParamName,
		Register: 0,
		StartPC:  0,
		EndPC:    len(child.Code),
	})
	child.MaxStackSize = 2

	// 追加子 Proto 并返回索引。
	return generator.proto.AddChild(child), nil
}

// compileCompactSimpleFunctionBody 编译 parser compact summary 记录的简单函数体。
//
// body 必须来自 compile-only parser 入口；该 helper 会重新核对参数、vararg 与局部变量绑定。返回
// handled=false 表示 summary 缺失或校验失败，调用方必须继续普通 compileBlock 路径。
func (generator *generator) compileCompactSimpleFunctionBody(body *parser.FunctionBody) (bool, error) {
	paramName, integerValue, returnPosition, operatorPosition, _, ok := body.CompactSimpleAddInteger()
	if !ok {
		// 没有 compact summary 的函数体保持普通 codegen。
		return false, nil
	}
	if body.Vararg || len(body.Params) != 1 || body.Params[0] != paramName {
		// 参数形态被 method/self 注入或其他路径改写时，不能使用 summary。
		return false, nil
	}
	binding, ok := generator.lookupLocal(paramName)
	if !ok {
		// 参数 local 尚未登记时回退普通路径，避免生成读取未知寄存器的字节码。
		return false, nil
	}

	resultRegister := generator.allocateRegister()
	constantIndex := generator.addConstant(bytecode.IntegerConstant(integerValue))
	constantOperand, constantRegister, err := generator.rkOperandForConstantIndex(constantIndex)
	if err != nil {
		// 常量无法编码时释放结果寄存器并返回原错误。
		generator.releaseRegister(resultRegister)
		return true, err
	}
	if err := generator.withSourceLine(operatorPosition, func() error {
		// ADD 行号必须归因到 `+`，与普通二元 return codegen 保持一致。
		generator.emitABC(bytecode.OpAdd, resultRegister, binding.register, constantOperand)
		return nil
	}); err != nil {
		// 当前 emit 不会返回错误；保留释放路径以匹配 withSourceLine 签名。
		generator.releaseOptionalRegister(constantRegister)
		generator.releaseRegister(resultRegister)
		return true, err
	}
	generator.releaseOptionalRegister(constantRegister)
	if err := generator.withSourceLine(returnPosition, func() error {
		// RETURN 行号必须归因到 return 关键字，供 debug hook 与 luac -l -l 展示。
		generator.emitABC(bytecode.OpReturn, resultRegister, 2, 0)
		return nil
	}); err != nil {
		// 当前 emit 不会返回错误；保留释放路径以匹配 withSourceLine 签名。
		generator.releaseRegister(resultRegister)
		return true, err
	}
	generator.releaseRegister(resultRegister)
	generator.returned = true
	return true, nil
}

// compileReturn 编译 return 语句。
//
// 当前阶段返回表达式会连续写入从 R0 开始的寄存器区间。
func (generator *generator) compileReturn(statement *parser.ReturnStatement) error {
	if len(statement.Values) == 1 {
		// return 单个函数调用可生成 TAILCALL，保持 Lua 尾调用语义。
		if callExpression, ok := statement.Values[0].(*parser.FunctionCallExpression); ok {
			if err := generator.compileTailCallReturn(callExpression); err != nil {
				// 尾调用生成失败时返回错误。
				return err
			}
			generator.returned = true
			return nil
		}
		if callExpression, ok := statement.Values[0].(*parser.MethodCallExpression); ok {
			if err := generator.compileTailMethodCallReturn(callExpression); err != nil {
				// method 尾调用生成失败时返回错误。
				return err
			}
			generator.returned = true
			return nil
		}
	}
	if len(statement.Values) > 0 && isOpenListExpression(statement.Values[len(statement.Values)-1]) {
		// return 列表末尾为开放表达式时，必须保留全部返回值。
		return generator.compileOpenReturn(statement)
	}
	if len(statement.Values) == 0 {
		// 空 return 不读取任何返回寄存器，固定使用 R0 起始的空返回区避免越过当前栈顶。
		generator.emitABC(bytecode.OpReturn, 0, 1, 0)
		generator.returned = true
		return nil
	}
	if handled, err := generator.compileSingleSafeNameReturn(statement); handled || err != nil {
		// 单个 local/upvalue 名称 return 可避免额外临时 MOVE。
		return err
	}
	if handled, err := generator.compileSingleSafeBinaryReturn(statement); handled || err != nil {
		// 单个安全二元表达式 return 可直接使用 RK 操作数，避免为参数 local 生成临时 MOVE。
		return err
	}
	firstTempRegister := generator.nextRegister
	tempRegisters := make([]int, 0, len(statement.Values))
	for range statement.Values {
		// 返回表达式先写入临时区，避免 `return upvalue, local` 覆盖后续读取的局部寄存器。
		tempRegisters = append(tempRegisters, generator.allocateRegister())
	}
	for index, expression := range statement.Values {
		// 返回值按源码顺序求值，但暂不写入最终 RETURN 区间。
		if err := generator.withSourceLine(expression.Pos(), func() error {
			// return 列表可能跨行，表达式内部 CALL 应使用表达式自身行号。
			return generator.compileExpressionTo(expression, tempRegisters[index])
		}); err != nil {
			// 任一返回表达式失败时释放临时区并终止 return 生成。
			generator.releaseRegistersFrom(firstTempRegister)
			return err
		}
	}
	generator.emitABC(bytecode.OpReturn, firstTempRegister, len(statement.Values)+1, 0)
	generator.releaseRegistersFrom(firstTempRegister)
	generator.returned = true

	// return 指令生成完成。
	return nil
}

// compileSingleSafeNameReturn 优化 `return localName` 或 `return upvalueName`。
//
// 单返回值不会覆盖后续返回表达式，因此当前 local 可直接作为 RETURN 起点；upvalue 读取到结果
// 寄存器后返回。未知全局名称仍走通用路径，以保留 `_ENV[name]` 访问和元方法语义。
func (generator *generator) compileSingleSafeNameReturn(statement *parser.ReturnStatement) (bool, error) {
	if len(statement.Values) != 1 {
		// 多返回值需要保持完整求值顺序和连续返回区间。
		return false, nil
	}
	nameExpression, ok := statement.Values[0].(*parser.NameExpression)
	if !ok {
		// 非名称表达式交给后续快路径或通用 return。
		return false, nil
	}
	if binding, ok := generator.lookupLocal(nameExpression.Name); ok {
		// 当前 local 已在寄存器中，直接作为单返回值起点。
		generator.emitABC(bytecode.OpReturn, binding.register, 2, 0)
		generator.returned = true
		return true, nil
	}
	if nameExpression.Name != envUpvalueName && !generator.canResolveUpvalueName(nameExpression.Name) {
		// 未知名称会读取当前 `_ENV[name]`，不能当作无副作用 upvalue 处理。
		return false, nil
	}
	resultRegister := generator.allocateRegister()
	if err := generator.compileExpressionTo(nameExpression, resultRegister); err != nil {
		// upvalue 读取失败时释放临时寄存器并返回。
		generator.releaseRegister(resultRegister)
		return true, err
	}
	generator.emitABC(bytecode.OpReturn, resultRegister, 2, 0)
	generator.releaseRegister(resultRegister)
	generator.returned = true
	return true, nil
}

// compileSingleSafeBinaryReturn 优化 `return localOrLiteral <op> localOrLiteral`。
//
// 该路径只处理单返回值和普通二元 opcode；两侧必须能转为安全 RK 操作数，不能包含调用、索引、
// upvalue/global 读取或其他副作用。复杂表达式保留通用 return 编译路径。
func (generator *generator) compileSingleSafeBinaryReturn(statement *parser.ReturnStatement) (bool, error) {
	if len(statement.Values) != 1 {
		// 多返回值需要保持完整求值和返回区间语义。
		return false, nil
	}
	binaryExpression, ok := statement.Values[0].(*parser.BinaryExpression)
	if !ok {
		// 非二元表达式交给通用 return 路径。
		return false, nil
	}
	if binaryExpression.Operator == ".." {
		// CONCAT 需要连续寄存器区间，不能使用普通 RK 二元快路径。
		return false, nil
	}
	opCode, ok := binaryOpCode(binaryExpression.Operator)
	if !ok {
		// 尚未支持的操作符沿用通用路径返回原有错误。
		return false, nil
	}
	if !generator.isSafeBinaryReturnOperand(binaryExpression.Left) || !generator.isSafeBinaryReturnOperand(binaryExpression.Right) {
		// 任一操作数不满足 local/字面量/upvalue 名称条件时回退通用路径。
		return false, nil
	}

	firstTempRegister := generator.nextRegister
	resultRegister := generator.allocateRegister()
	resultRegisterInUse := false
	leftOperand, err := generator.safeBinaryReturnOperand(binaryExpression.Left, resultRegister, &resultRegisterInUse)
	if err != nil {
		// 左操作数编译失败时释放临时区并返回。
		generator.releaseRegistersFrom(firstTempRegister)
		return true, err
	}
	rightOperand, err := generator.safeBinaryReturnOperand(binaryExpression.Right, resultRegister, &resultRegisterInUse)
	if err != nil {
		// 右操作数编译失败时释放临时区并返回。
		generator.releaseRegistersFrom(firstTempRegister)
		return true, err
	}
	if err := generator.withSourceLine(binaryExpression.Position, func() error {
		// 运算错误归因到操作符所在行；结果寄存器作为单返回值起点。
		generator.emitABC(opCode, resultRegister, leftOperand, rightOperand)
		return nil
	}); err != nil {
		generator.releaseRegistersFrom(firstTempRegister)
		return true, err
	}
	generator.emitABC(bytecode.OpReturn, resultRegister, 2, 0)
	generator.releaseRegistersFrom(firstTempRegister)
	generator.returned = true
	return true, nil
}

// isSafeBinaryReturnOperand 判断 return 二元快路径是否支持该操作数。
func (generator *generator) isSafeBinaryReturnOperand(expression parser.Expression) bool {
	if _, ok := expression.(*parser.LiteralExpression); ok {
		// 字面量无副作用，可作为 RK 常量或临时常量寄存器。
		return true
	}
	nameExpression, ok := expression.(*parser.NameExpression)
	if !ok {
		// 非名称表达式可能包含索引、调用或其他副作用。
		return false
	}
	if _, ok := generator.lookupLocal(nameExpression.Name); ok {
		// 当前 local 读取只访问寄存器。
		return true
	}
	return generator.canResolveUpvalueName(nameExpression.Name)
}

// canResolveUpvalueName 判断名称是否能从外层函数读取为 upvalue，且不修改当前 Proto。
func (generator *generator) canResolveUpvalueName(name string) bool {
	if generator.parent == nil {
		// 顶层函数没有可捕获的外层 local/upvalue。
		return false
	}
	return generator.parent.hasLocalOrUpvalueName(name)
}

// hasLocalOrUpvalueName 判断当前函数或祖先函数是否存在可捕获名称。
func (generator *generator) hasLocalOrUpvalueName(name string) bool {
	if _, ok := generator.lookupLocal(name); ok {
		// 直接命中当前函数 local。
		return true
	}
	if _, ok := generator.upvalues[name]; ok {
		// 当前函数已经登记过该 upvalue，子函数可间接捕获。
		return true
	}
	if generator.parent != nil {
		// 继续向祖先函数查找可捕获名称。
		return generator.parent.hasLocalOrUpvalueName(name)
	}

	// 祖先链路没有可捕获名称。
	return false
}

// safeBinaryReturnOperand 编译 return 二元快路径的操作数并返回 RK/寄存器操作数。
func (generator *generator) safeBinaryReturnOperand(expression parser.Expression, resultRegister int, resultRegisterInUse *bool) (int, error) {
	if operand, _, ok, err := generator.safeRKOperand(expression); err != nil || ok {
		// local/字面量可以直接作为 RK 操作数；错误保持原编译语义。
		return operand, err
	}
	nameExpression, ok := expression.(*parser.NameExpression)
	if !ok {
		// 调用方已经预检过，理论上不会到达该分支。
		return 0, fmt.Errorf("codegen unsupported return operand %T", expression)
	}
	targetRegister := resultRegister
	if *resultRegisterInUse {
		// resultRegister 已保存另一个 upvalue 操作数时，分配额外临时寄存器。
		targetRegister = generator.allocateRegister()
	} else {
		// 优先把单个 upvalue 读入结果寄存器，对齐 Lua 5.3 C codegen 形态。
		*resultRegisterInUse = true
	}
	if err := generator.compileExpressionTo(nameExpression, targetRegister); err != nil {
		// upvalue 读取失败时返回原编译错误。
		return 0, err
	}
	return targetRegister, nil
}

// compileOpenReturn 编译末尾为开放表达式的 return 列表。
//
// 所有返回表达式写入一段连续临时寄存器，最后使用 RETURN B=0 返回从临时起点到开放栈顶的全部值。
func (generator *generator) compileOpenReturn(statement *parser.ReturnStatement) error {
	firstTempRegister := generator.nextRegister
	for index, expression := range statement.Values {
		// 返回值按源码顺序写入连续临时区，避免覆盖仍需读取的局部变量。
		targetRegister := firstTempRegister + index
		generator.ensureRegister(targetRegister)
		if index == len(statement.Values)-1 {
			// 末尾开放表达式负责设置 VM openTop。
			if err := generator.withSourceLine(expression.Pos(), func() error {
				// 开放 return 的末尾表达式同样需要保留自身源码行。
				return generator.compileOpenListExpressionTo(expression, targetRegister)
			}); err != nil {
				// 开放表达式编译失败时释放临时区并返回。
				generator.releaseRegistersFrom(firstTempRegister)
				return err
			}
			break
		}
		if err := generator.withSourceLine(expression.Pos(), func() error {
			// 固定返回表达式按自身行号生成，避免多行 return 调试信息全部落在 return 行。
			return generator.compileExpressionTo(expression, targetRegister)
		}); err != nil {
			// 固定返回表达式编译失败时释放临时区并返回。
			generator.releaseRegistersFrom(firstTempRegister)
			return err
		}
	}
	generator.emitABC(bytecode.OpReturn, firstTempRegister, 0, 0)
	generator.releaseRegistersFrom(firstTempRegister)
	generator.returned = true

	// 开放 return 编译完成。
	return nil
}

// blockContainsFunction 判断 block 内是否出现函数定义或匿名函数表达式。
//
// 该辅助用于 numeric for codegen：只有循环体内存在闭包时，才需要为可见循环变量创建每轮
// 独立 local；普通空循环保留更少指令，维持 debug count hook 与 Lua 5.3 官方范围接近。
func blockContainsFunction(block *parser.Block) bool {
	if block == nil {
		// nil block 没有语句。
		return false
	}
	for _, statement := range block.Statements {
		if statementContainsFunction(statement) {
			// 任一语句含函数定义即可启用保守拆分。
			return true
		}
	}
	if block.Return != nil {
		for _, expression := range block.Return.Values {
			if expressionContainsFunction(expression) {
				// return 表达式中的匿名函数同样可能捕获循环变量。
				return true
			}
		}
	}
	return false
}

// statementContainsFunction 判断语句子树是否包含函数定义。
//
// 该扫描只服务 codegen 策略选择，不改变 AST；遇到嵌套 block 会继续递归，遇到函数体本身则
// 立即返回 true，无需进入函数体内部。
func statementContainsFunction(statement parser.Statement) bool {
	switch typedStatement := statement.(type) {
	case *parser.LocalFunctionStatement, *parser.FunctionStatement, *parser.CompactFunctionStatement:
		// 显式函数语句会创建 closure。
		return true
	case *parser.AssignmentStatement:
		for _, expression := range typedStatement.Right {
			if expressionContainsFunction(expression) {
				// 赋值 RHS 中的匿名函数会创建 closure。
				return true
			}
		}
	case *parser.LocalAssignmentStatement:
		for _, expression := range typedStatement.Values {
			if expressionContainsFunction(expression) {
				// local 初始化中的匿名函数会创建 closure。
				return true
			}
		}
	case *parser.IfStatement:
		for _, clause := range typedStatement.Clauses {
			if expressionContainsFunction(clause.Condition) || blockContainsFunction(clause.Block) {
				// 条件或分支 block 中出现函数时返回 true。
				return true
			}
		}
		return blockContainsFunction(typedStatement.ElseBlock)
	case *parser.WhileStatement:
		return expressionContainsFunction(typedStatement.Condition) || blockContainsFunction(typedStatement.Body)
	case *parser.RepeatUntilStatement:
		return blockContainsFunction(typedStatement.Body) || expressionContainsFunction(typedStatement.Condition)
	case *parser.NumericForStatement:
		return expressionContainsFunction(typedStatement.Init) || expressionContainsFunction(typedStatement.Limit) || expressionContainsFunction(typedStatement.Step) || blockContainsFunction(typedStatement.Body)
	case *parser.GenericForStatement:
		for _, expression := range typedStatement.Iterators {
			if expressionContainsFunction(expression) {
				// 迭代表达式中的函数需要保守处理。
				return true
			}
		}
		return blockContainsFunction(typedStatement.Body)
	case *parser.FunctionCallStatement:
		return expressionContainsFunction(typedStatement.Call)
	}
	if contains, handled := extensionStatementContainsFunction(statement); handled {
		// 当前语句已由编译进来的扩展函数子树检测器处理。
		return contains
	}
	return false
}

// expressionContainsFunction 判断表达式子树是否包含匿名函数表达式。
func expressionContainsFunction(expression parser.Expression) bool {
	switch typedExpression := expression.(type) {
	case nil:
		// nil 表达式没有子树。
		return false
	case *parser.FunctionExpression:
		// 匿名函数表达式会创建 closure。
		return true
	case *parser.TableConstructorExpression:
		for _, field := range typedExpression.Fields {
			if expressionContainsFunction(field) {
				// 数组字段中出现函数。
				return true
			}
		}
		for _, field := range typedExpression.RecordFields {
			if expressionContainsFunction(field.Value) {
				// 记录字段值中出现函数。
				return true
			}
		}
		for _, field := range typedExpression.IndexFields {
			if expressionContainsFunction(field.Key) || expressionContainsFunction(field.Value) {
				// 动态 key 或 value 中出现函数。
				return true
			}
		}
	case *parser.PrefixExpression:
		return expressionContainsFunction(typedExpression.Inner)
	case *parser.FieldAccessExpression:
		return expressionContainsFunction(typedExpression.Receiver)
	case *parser.IndexExpression:
		return expressionContainsFunction(typedExpression.Receiver) || expressionContainsFunction(typedExpression.Index)
	case *parser.FunctionCallExpression:
		if expressionContainsFunction(typedExpression.Function) {
			// 被调表达式中出现函数。
			return true
		}
		for _, argument := range typedExpression.Arguments {
			if expressionContainsFunction(argument) {
				// 调用参数中出现函数。
				return true
			}
		}
	case *parser.MethodCallExpression:
		if expressionContainsFunction(typedExpression.Receiver) {
			// method 接收者中出现函数。
			return true
		}
		for _, argument := range typedExpression.Arguments {
			if expressionContainsFunction(argument) {
				// method 参数中出现函数。
				return true
			}
		}
	case *parser.UnaryExpression:
		return expressionContainsFunction(typedExpression.Operand)
	case *parser.BinaryExpression:
		return expressionContainsFunction(typedExpression.Left) || expressionContainsFunction(typedExpression.Right)
	}
	return false
}

// compileExpressionTo 将表达式编译到指定目标寄存器。
//
// targetRegister 必须是当前函数可用寄存器；必要时会分配临时寄存器并在使用后释放。
func (generator *generator) compileExpressionTo(expression parser.Expression, targetRegister int) error {
	generator.ensureRegister(targetRegister)
	switch typedExpression := expression.(type) {
	case *parser.LiteralExpression:
		// 字面量根据类型生成 LOADNIL、LOADBOOL 或 LOADK。
		return generator.compileLiteralTo(typedExpression, targetRegister)
	case *parser.NameExpression:
		// 名称表达式当前只支持读取局部变量。
		return generator.compileNameTo(typedExpression, targetRegister)
	case *parser.UnaryExpression:
		// 一元表达式先生成操作数再生成一元 opcode。
		return generator.compileUnaryTo(typedExpression, targetRegister)
	case *parser.BinaryExpression:
		// 二元表达式包含普通算术/位运算和 and/or 短路逻辑。
		return generator.compileBinaryTo(typedExpression, targetRegister)
	case *parser.PrefixExpression:
		// 括号表达式只影响解析优先级，codegen 直接生成内部表达式。
		return generator.compileExpressionTo(typedExpression.Inner, targetRegister)
	case *parser.TableConstructorExpression:
		// table constructor 当前支持数组字段写入。
		return generator.compileTableConstructorTo(typedExpression, targetRegister)
	case *parser.FunctionExpression:
		// 匿名函数表达式生成子 Proto closure。
		childIndex, err := generator.compileChildProto(typedExpression.Body)
		if err != nil {
			// 子函数体编译失败时返回错误。
			return err
		}
		generator.emitABx(bytecode.OpClosure, targetRegister, childIndex)
		return nil
	case *parser.FunctionCallExpression:
		// 函数调用表达式默认请求一个返回值。
		return generator.compileFunctionCallTo(typedExpression, targetRegister, 1)
	case *parser.MethodCallExpression:
		// method call 表达式默认请求一个返回值，并隐式传入 self。
		return generator.compileMethodCallTo(typedExpression, targetRegister, 1)
	case *parser.FieldAccessExpression:
		// 点号字段访问编译为 GETTABLE，字段名作为字符串 key。
		return generator.compileFieldAccessTo(typedExpression, targetRegister)
	case *parser.IndexExpression:
		// 方括号索引访问编译为 GETTABLE，索引表达式生成到 RK 寄存器。
		return generator.compileIndexAccessTo(typedExpression, targetRegister)
	case *parser.VarargExpression:
		// vararg 表达式默认读取一个返回值。
		generator.emitABC(bytecode.OpVararg, targetRegister, 2, 0)
		return nil
	default:
		return fmt.Errorf("codegen unsupported expression %T", expression)
	}
}

// compileTailCallReturn 编译 return f(...) 形式的尾调用。
//
// 普通函数调用会把被调函数放在 baseRegister，参数从 baseRegister+1 开始连续排列。
func (generator *generator) compileTailCallReturn(expression *parser.FunctionCallExpression) error {
	baseRegister := generator.allocateRegister()
	argumentCount, err := generator.prepareFunctionCall(expression, baseRegister)
	if err != nil {
		// 函数或参数编译失败时返回错误。
		generator.releaseRegister(baseRegister)
		return err
	}
	callArgumentField := argumentCount + 1
	if argumentCount < 0 {
		// 开放实参列表通过 B=0 交给 VM 按开放栈顶确定参数数量。
		callArgumentField = 0
	}
	generator.emitABC(bytecode.OpTailCall, baseRegister, callArgumentField, 0)
	generator.emitABC(bytecode.OpReturn, baseRegister, 0, 0)
	generator.releaseRegister(baseRegister)

	// 尾调用 return 编译完成。
	return nil
}

// compileTailMethodCallReturn 编译 return receiver:method(...) 形式的尾调用。
//
// method 调用会通过 SELF 指令把方法函数放在 baseRegister、self 放在 baseRegister+1，
// 显式参数从 baseRegister+2 开始，因此 TAILCALL 的 B 字段需要包含隐式 self。
func (generator *generator) compileTailMethodCallReturn(expression *parser.MethodCallExpression) error {
	baseRegister := generator.allocateRegister()
	argumentCount, err := generator.prepareMethodCall(expression, baseRegister)
	if err != nil {
		// 接收者、方法名或参数编译失败时返回错误。
		generator.releaseRegister(baseRegister)
		return err
	}
	callArgumentField := argumentCount + 2
	if argumentCount < 0 {
		// 开放实参列表通过 B=0 交给 VM 按开放栈顶确定参数数量。
		callArgumentField = 0
	}
	generator.emitABC(bytecode.OpTailCall, baseRegister, callArgumentField, 0)
	generator.emitABC(bytecode.OpReturn, baseRegister, 0, 0)
	generator.releaseRegister(baseRegister)

	// method 尾调用 return 编译完成。
	return nil
}

// compileFunctionCallTo 编译普通函数调用表达式。
//
// resultCount 为 0 表示调用语句丢弃返回值；为 1 表示表达式需要一个返回值。
func (generator *generator) compileFunctionCallTo(expression *parser.FunctionCallExpression, targetRegister int, resultCount int) error {
	argumentCount, err := generator.prepareFunctionCall(expression, targetRegister)
	if err != nil {
		// 函数或参数编译失败时返回错误。
		return err
	}
	callArgumentField := argumentCount + 1
	if argumentCount < 0 {
		// 最后一个实参是 vararg 时，CALL B=0 表示参数列表开放到当前栈顶。
		callArgumentField = 0
	}
	generator.emitABC(bytecode.OpCall, targetRegister, callArgumentField, resultCount+1)

	// 调用表达式编译完成。
	return nil
}

// compileMethodCallTo 编译 Lua method call 表达式。
//
// targetRegister 保存方法函数；targetRegister+1 保存隐式 self，显式参数从 targetRegister+2 开始。
func (generator *generator) compileMethodCallTo(expression *parser.MethodCallExpression, targetRegister int, resultCount int) error {
	argumentCount, err := generator.prepareMethodCall(expression, targetRegister)
	if err != nil {
		// 接收者、方法名或参数编译失败时返回错误。
		return err
	}
	callArgumentField := argumentCount + 2
	if argumentCount < 0 {
		// method call 的显式实参尾部为 vararg 时，同样使用开放参数列表。
		callArgumentField = 0
	}
	generator.emitABC(bytecode.OpCall, targetRegister, callArgumentField, resultCount+1)

	// method call 编译完成。
	return nil
}

// prepareFunctionCall 将函数和参数写入连续寄存器区间。
//
// baseRegister 保存函数值，后续参数从 baseRegister+1 开始连续写入。
func (generator *generator) prepareFunctionCall(expression *parser.FunctionCallExpression, baseRegister int) (int, error) {
	if len(expression.Arguments) > 0 && baseRegister+len(expression.Arguments)+1 > maxProtoRegisters {
		// 固定实参数量超过 CALL 可寻址寄存器区间时按 Lua 5.3 编译错误返回。
		return 0, fmt.Errorf("too many registers")
	}
	if err := generator.compileExpressionTo(expression.Function, baseRegister); err != nil {
		// 被调用函数表达式必须先写入 baseRegister。
		return 0, err
	}
	for argumentIndex, argument := range expression.Arguments {
		if isOpenListExpression(argument) && argumentIndex == len(expression.Arguments)-1 {
			// 最后一个实参为 vararg/function call/method call 时必须按多返回值展开，供 CALL B=0 消费。
			if err := generator.compileOpenListExpressionTo(argument, baseRegister+argumentIndex+1); err != nil {
				// 开放实参编译失败时返回错误，避免外层 CALL 读取不完整参数区。
				return 0, err
			}
			return -1, nil
		}
		// 每个参数写入函数寄存器之后的连续寄存器。
		if err := generator.compileExpressionTo(argument, baseRegister+argumentIndex+1); err != nil {
			// 任一参数编译失败时返回错误。
			return 0, err
		}
	}

	// 返回固定参数数量。
	return len(expression.Arguments), nil
}

// prepareMethodCall 将方法、self 和显式参数写入连续寄存器区间。
//
// baseRegister 保存方法函数，baseRegister+1 保存 receiver/self，显式参数从 baseRegister+2 开始。
func (generator *generator) prepareMethodCall(expression *parser.MethodCallExpression, baseRegister int) (int, error) {
	if len(expression.Arguments) > 0 && baseRegister+len(expression.Arguments)+2 > maxProtoRegisters {
		// method call 还需要隐式 self 寄存器，超过时按 Lua 5.3 编译错误返回。
		return 0, fmt.Errorf("too many registers")
	}
	if err := generator.compileExpressionTo(expression.Receiver, baseRegister); err != nil {
		// 接收者表达式必须先写入 baseRegister，随后 SELF 会搬到 baseRegister+1。
		return 0, err
	}
	methodIndex := generator.addConstant(bytecode.StringConstant(expression.Method))
	generator.ensureRegister(baseRegister + 1)
	methodOperand, methodRegister, err := generator.rkOperandForConstantIndex(methodIndex)
	if err != nil {
		// 方法名常量无法编码或加载时返回错误，调用方会回收调用寄存器区间。
		return 0, err
	}
	generator.emitABC(bytecode.OpSelf, baseRegister, baseRegister, methodOperand)
	generator.releaseOptionalRegister(methodRegister)
	for argumentIndex, argument := range expression.Arguments {
		if isOpenListExpression(argument) && argumentIndex == len(expression.Arguments)-1 {
			// 显式实参末尾为开放表达式时从 baseRegister+2 起展开，并保留隐式 self。
			if err := generator.compileOpenListExpressionTo(argument, baseRegister+argumentIndex+2); err != nil {
				// 开放实参编译失败时返回错误，避免 method call 读取不完整参数区。
				return 0, err
			}
			return -1, nil
		}
		// 显式参数紧跟隐式 self 之后写入连续寄存器。
		if err := generator.compileExpressionTo(argument, baseRegister+argumentIndex+2); err != nil {
			// 任一参数编译失败时返回错误。
			return 0, err
		}
	}

	// 返回显式参数数量，调用方会额外计算隐式 self。
	return len(expression.Arguments), nil
}

// compileLiteralTo 编译字面量表达式。
//
// Lua nil/boolean 使用专用指令，数字和字符串进入常量池并通过 LOADK 读取。
func (generator *generator) compileLiteralTo(expression *parser.LiteralExpression, targetRegister int) error {
	if expression.Kind == lexer.TokenKeyword {
		// Lua 基础关键字字面量按文本区分 nil/true/false。
		switch expression.Value {
		case "nil":
			// nil 使用 LOADNIL 写单个寄存器。
			generator.emitABC(bytecode.OpLoadNil, targetRegister, 0, 0)
			return nil
		case "true":
			// true 使用 LOADBOOL，C=0 表示不跳过下一条指令。
			generator.emitABC(bytecode.OpLoadBool, targetRegister, 1, 0)
			return nil
		case "false":
			// false 使用 LOADBOOL，B=0 表示 false。
			generator.emitABC(bytecode.OpLoadBool, targetRegister, 0, 0)
			return nil
		default:
			return fmt.Errorf("codegen unsupported keyword literal %q", expression.Value)
		}
	}
	if expression.Kind == lexer.TokenString {
		// 字符串常量去重后通过 LOADK 加载。
		index := generator.addConstant(bytecode.StringConstant(expression.Value))
		return generator.emitLoadConstantIndexTo(index, targetRegister)
	}
	if expression.Kind == lexer.TokenNumber {
		// 数字按 lexer 分类保留 integer/number 双模型。
		index := generator.addNumberConstant(expression.Number)
		return generator.emitLoadConstantIndexTo(index, targetRegister)
	}

	// 其他 token kind 不是当前阶段支持的字面量。
	return fmt.Errorf("codegen unsupported literal kind %s", expression.Kind)
}

// compileNameTo 编译名称表达式。
//
// 当前阶段优先读取局部和 upvalue；未知名称按 Lua 5.3 语义读取 `_ENV[name]`。
func (generator *generator) compileNameTo(expression *parser.NameExpression, targetRegister int) error {
	sourceBinding, ok := generator.lookupLocal(expression.Name)
	if !ok {
		if expression.Name == envUpvalueName {
			// 未声明的 `_ENV` 名称读取当前函数环境 upvalue，而不是读取 `_ENV["_ENV"]`。
			generator.emitABC(bytecode.OpGetUpval, targetRegister, generator.envUpvalueIndex(), 0)
			return nil
		}
		// 未命中局部变量时尝试捕获外层局部变量为 upvalue。
		upvalueIndex, captured, err := generator.resolveUpvalue(expression.Name, expression.Position)
		if err != nil {
			// upvalue 数量超过 Lua 5.3 上限时返回编译错误。
			return err
		}
		if !captured {
			// 未声明名称降级为 `_ENV[name]`，保持 Lua 5.3 全局变量语义。
			return generator.compileGlobalNameTo(expression.Name, targetRegister)
		}
		generator.emitABC(bytecode.OpGetUpval, targetRegister, upvalueIndex, 0)
		return nil
	}
	if sourceBinding.register == targetRegister {
		// 源和目标相同时无需生成 MOVE。
		return nil
	}
	generator.emitABC(bytecode.OpMove, targetRegister, sourceBinding.register, 0)

	// 名称读取完成。
	return nil
}

// compileFieldAccessTo 编译点号字段访问表达式。
//
// expression.Receiver 必须在运行期得到 table 或支持索引的值；字段名使用字符串常量作为 key。
func (generator *generator) compileFieldAccessTo(expression *parser.FieldAccessExpression, targetRegister int) error {
	if err := generator.compileExpressionTo(expression.Receiver, targetRegister); err != nil {
		// 接收者表达式失败时不能继续生成 GETTABLE。
		return err
	}
	keyIndex := generator.addConstant(bytecode.StringConstant(expression.Field))
	keyOperand, keyRegister, err := generator.rkOperandForConstantIndex(keyIndex)
	if err != nil {
		// 字段名常量无法编码或加载时停止生成，目标寄存器中保留接收者临时值。
		return err
	}
	generator.emitABC(bytecode.OpGetTable, targetRegister, targetRegister, keyOperand)
	generator.releaseOptionalRegister(keyRegister)

	// 点号字段访问编译完成。
	return nil
}

// compileIndexAccessTo 编译方括号索引访问表达式。
//
// receiver 运行期必须可索引；index 表达式生成到临时寄存器并作为 GETTABLE 的 RK 寄存器参数。
func (generator *generator) compileIndexAccessTo(expression *parser.IndexExpression, targetRegister int) error {
	tableRegister := generator.allocateRegister()
	keyRegister := generator.allocateRegister()
	if err := generator.compileExpressionTo(expression.Receiver, tableRegister); err != nil {
		// 接收者表达式失败时释放两个临时寄存器。
		generator.releaseRegister(keyRegister)
		generator.releaseRegister(tableRegister)
		return err
	}
	if err := generator.compileExpressionTo(expression.Index, keyRegister); err != nil {
		// 索引表达式失败时释放两个临时寄存器。
		generator.releaseRegister(keyRegister)
		generator.releaseRegister(tableRegister)
		return err
	}
	generator.emitABC(bytecode.OpGetTable, targetRegister, tableRegister, keyRegister)
	generator.releaseRegister(keyRegister)
	generator.releaseRegister(tableRegister)

	// 方括号索引访问编译完成。
	return nil
}

// compileGlobalNameTo 编译全局名称读取。
//
// name 必须是 Lua 标识符；targetRegister 接收 `_ENV[name]` 的结果，常量索引超过 RK 上限时返回错误。
func (generator *generator) compileGlobalNameTo(name string, targetRegister int) error {
	keyIndex := generator.addConstant(bytecode.StringConstant(name))
	keyOperand, keyRegister, err := generator.rkOperandForConstantIndex(keyIndex)
	if err != nil {
		// 全局名常量无法编码或加载时返回错误。
		return err
	}
	if envBinding, ok := generator.lookupLocal(envUpvalueName); ok {
		// 当前作用域显式声明 local _ENV 时，未声明名称必须访问该表而不是顶层 globals。
		generator.emitABC(bytecode.OpGetTable, targetRegister, envBinding.register, keyOperand)
	} else {
		// 没有 local _ENV 时沿用 upvalue 形式访问环境表。
		envIndex := generator.envUpvalueIndex()
		generator.emitABC(bytecode.OpGetTabUp, targetRegister, envIndex, keyOperand)
	}
	generator.releaseOptionalRegister(keyRegister)

	// 全局名称读取完成。
	return nil
}

// compileGlobalAssignment 编译普通名称的全局赋值。
//
// rights 是赋值语句完整右侧列表；index 是当前左值下标。缺少右值时按 Lua 语义写入 nil。
func (generator *generator) compileGlobalAssignment(name string, rights []parser.Expression, index int) error {
	if _, exists := generator.lookupGlobalConst(name); exists {
		// 当前 chunk 顶层 const 全局不允许通过旧全局赋值辅助覆盖。
		return constAssignmentError(name, lexer.Position{})
	}
	valueRegister := generator.allocateRegister()
	if index < len(rights) {
		// 有对应右值时先求值到临时寄存器，随后写入 `_ENV[name]`。
		if err := generator.compileExpressionTo(rights[index], valueRegister); err != nil {
			// 右值生成失败时释放临时寄存器并返回。
			generator.releaseRegister(valueRegister)
			return err
		}
	} else {
		// 右值不足时普通赋值会补 nil。
		generator.emitABC(bytecode.OpLoadNil, valueRegister, 0, 0)
	}
	if err := generator.emitSetGlobalName(name, valueRegister); err != nil {
		// 全局 key 无法编码时释放临时寄存器并返回。
		generator.releaseRegister(valueRegister)
		return err
	}
	generator.releaseRegister(valueRegister)

	// 全局赋值编译完成。
	return nil
}

// emitSetGlobalName 生成 `_ENV[name] = R(valueRegister)`。
//
// name 会写入当前函数常量池并通过 RK 编码；valueRegister 必须保存已求值的右侧结果。
func (generator *generator) emitSetGlobalName(name string, valueRegister int) error {
	return generator.emitSetGlobalNameOperand(name, valueRegister)
}

// emitSetGlobalNameOperand 生成 `_ENV[name] = RK(valueOperand)`。
//
// name 会写入当前函数常量池并通过 RK 编码；valueOperand 可以是寄存器或常量 RK 操作数，
// 调用方负责释放 valueOperand 对应的可选临时寄存器。
func (generator *generator) emitSetGlobalNameOperand(name string, valueOperand int) error {
	keyIndex := generator.addConstant(bytecode.StringConstant(name))
	keyOperand, keyRegister, err := generator.rkOperandForConstantIndex(keyIndex)
	if err != nil {
		// 全局名常量无法编码或加载时返回错误。
		return err
	}
	if envBinding, ok := generator.lookupLocal(envUpvalueName); ok {
		// 当前作用域显式声明 local _ENV 时，未声明名称写入该环境表。
		generator.emitABC(bytecode.OpSetTable, envBinding.register, keyOperand, valueOperand)
	} else {
		// 没有 local _ENV 时沿用 upvalue 形式写入环境表。
		envIndex := generator.envUpvalueIndex()
		generator.emitABC(bytecode.OpSetTabUp, envIndex, keyOperand, valueOperand)
	}
	generator.releaseOptionalRegister(keyRegister)

	// 全局写入指令生成完成。
	return nil
}

// compileUnaryTo 编译一元表达式。
//
// Lua 5.3 一元操作符映射到 UNM、BNOT、NOT 和 LEN。
func (generator *generator) compileUnaryTo(expression *parser.UnaryExpression, targetRegister int) error {
	operandRegister := targetRegister
	releaseOperandRegister := false
	if localRegister, ok := generator.unaryLocalOperandRegister(expression.Operand); ok {
		// 一元操作数是当前 local 时，可直接以该 local 寄存器作为源操作数，匹配 Lua 5.3 codegen。
		operandRegister = localRegister
	} else if generator.registerHasActiveLocal(targetRegister) {
		// 目标寄存器承载活跃 local 时不能提前覆盖，避免 `a = #f(a)` 的 RHS 读不到旧值。
		operandRegister = generator.allocateRegister()
		releaseOperandRegister = true
	}
	if !releaseOperandRegister && operandRegister != targetRegister {
		// 源操作数已是可见 local 寄存器，无需重新生成 MOVE。
	} else {
		if err := generator.compileExpressionTo(expression.Operand, operandRegister); err != nil {
			// 操作数编译失败时释放临时寄存器后返回。
			if releaseOperandRegister {
				// 只有本函数额外分配的操作数寄存器需要释放。
				generator.releaseRegister(operandRegister)
			}
			return err
		}
	}
	if err := generator.withSourceLine(expression.Position, func() error {
		switch expression.Operator {
		case "-":
			// 算术取负映射到 OP_UNM，行号使用一元操作符位置。
			generator.emitABC(bytecode.OpUnm, targetRegister, operandRegister, 0)
		case "~":
			// 按位取反映射到 OP_BNOT，行号使用一元操作符位置。
			generator.emitABC(bytecode.OpBNot, targetRegister, operandRegister, 0)
		case "not":
			// 逻辑取反映射到 OP_NOT，行号使用一元操作符位置。
			generator.emitABC(bytecode.OpNot, targetRegister, operandRegister, 0)
		case "#":
			// 长度运算映射到 OP_LEN，行号使用一元操作符位置。
			generator.emitABC(bytecode.OpLen, targetRegister, operandRegister, 0)
		default:
			return fmt.Errorf("codegen unsupported unary operator %q", expression.Operator)
		}
		return nil
	}); err != nil {
		if releaseOperandRegister {
			// 指令生成失败时释放额外分配的操作数寄存器。
			generator.releaseRegister(operandRegister)
		}
		return err
	}
	if releaseOperandRegister {
		// 操作数临时寄存器用完后回退栈顶水位。
		generator.releaseRegister(operandRegister)
	}

	// 一元表达式编译完成。
	return nil
}

// unaryLocalOperandRegister 返回一元表达式操作数可直接读取的当前 local 寄存器。
//
// 仅名称表达式可跳过操作数编译；全局、upvalue、索引和调用仍走普通路径以保留访问语义与副作用。
func (generator *generator) unaryLocalOperandRegister(expression parser.Expression) (int, bool) {
	nameExpression, ok := expression.(*parser.NameExpression)
	if !ok {
		// 非名称操作数不能直接映射寄存器。
		return 0, false
	}
	binding, ok := generator.lookupLocal(nameExpression.Name)
	if !ok {
		// 只有当前函数可见 local 能直接复用寄存器。
		return 0, false
	}
	return binding.register, true
}

// compileBinaryTo 编译二元表达式。
//
// `and` 与 `or` 使用短路生成，其余当前支持算术、位运算和连接。
func (generator *generator) compileBinaryTo(expression *parser.BinaryExpression, targetRegister int) error {
	if expression.Operator == "and" || expression.Operator == "or" {
		// 短路逻辑必须避免无条件求值右侧表达式。
		return generator.compileShortCircuitTo(expression, targetRegister)
	}
	if isComparisonOperator(expression.Operator) {
		// 比较表达式使用测试指令和 LOADBOOL 合成布尔结果。
		return generator.compileComparisonTo(expression, targetRegister)
	}
	if expression.Operator == ".." {
		// CONCAT 在 Lua 5.3 codegen 中会合并连续右结合表达式，生成单条连续寄存器范围指令。
		return generator.compileConcatTo(expression, targetRegister)
	}
	if handled, err := generator.compileBinaryRKTo(expression, targetRegister); handled || err != nil {
		// 安全 RK 二元表达式已直接生成；出错时保持原始编译错误。
		return err
	}
	if expression.Operator != ".." && !generator.registerHasActiveLocal(targetRegister) {
		if handled, err := generator.compileBinaryLeftRKRightToTargetTo(expression, targetRegister); handled || err != nil {
			// 左操作数可 RK 编码时，右侧表达式直接写入目标寄存器，减少临时寄存器压力。
			return err
		}
		// 目标寄存器不是当前 local 时，可以安全复用它保存左操作数，降低长表达式寄存器压力。
		return generator.compileBinaryWithReusableTargetTo(expression, targetRegister)
	}
	leftRegister := generator.allocateRegister()
	rightRegister := generator.allocateRegister()
	if err := generator.compileExpressionTo(expression.Left, leftRegister); err != nil {
		// 左侧表达式失败时释放两个临时寄存器。
		generator.releaseRegister(rightRegister)
		generator.releaseRegister(leftRegister)
		return err
	}
	if err := generator.compileExpressionTo(expression.Right, rightRegister); err != nil {
		// 右侧表达式失败时释放两个临时寄存器。
		generator.releaseRegister(rightRegister)
		generator.releaseRegister(leftRegister)
		return err
	}
	opCode, ok := binaryOpCode(expression.Operator)
	if !ok {
		// 不支持的二元操作符返回明确错误。
		generator.releaseRegister(rightRegister)
		generator.releaseRegister(leftRegister)
		return fmt.Errorf("codegen unsupported binary operator %q", expression.Operator)
	}
	if err := generator.withSourceLine(expression.Position, func() error {
		// 二元运算运行期错误应归因到操作符所在行，匹配 Lua 5.3 的 lineinfo。
		generator.emitABC(opCode, targetRegister, leftRegister, rightRegister)
		return nil
	}); err != nil {
		generator.releaseRegister(rightRegister)
		generator.releaseRegister(leftRegister)
		return err
	}
	generator.releaseRegister(rightRegister)
	generator.releaseRegister(leftRegister)

	// 二元表达式编译完成。
	return nil
}

// compileBinaryRKTo 使用 RK 操作数编译无副作用二元表达式。
//
// expression 左右两侧必须都是当前 local 或字面量；调用、索引、upvalue、全局读取等可能有副作用
// 或需要运行期查找的表达式会返回 handled=false，由通用路径保持原求值语义。
func (generator *generator) compileBinaryRKTo(expression *parser.BinaryExpression, targetRegister int) (handled bool, err error) {
	opCode, ok := binaryOpCode(expression.Operator)
	if !ok {
		// 非普通二元 opcode 不属于该快路径。
		return false, nil
	}
	if !generator.isSafeRKCandidate(expression.Left) || !generator.isSafeRKCandidate(expression.Right) {
		// 左右任一侧不是无副作用 RK 候选时，不能提前生成半截 LOADK 指令。
		return false, nil
	}
	leftOperand, leftRegister, leftOK, err := generator.safeRKOperand(expression.Left)
	if err != nil || !leftOK {
		// 左侧无法安全转 RK 时交给通用路径。
		return false, err
	}
	rightOperand, rightRegister, rightOK, err := generator.safeRKOperand(expression.Right)
	if err != nil || !rightOK {
		// 右侧无法安全转 RK 时交给通用路径。
		generator.releaseOptionalRegister(leftRegister)
		return false, err
	}
	if err := generator.withSourceLine(expression.Position, func() error {
		// RK 快路径仍把运行期错误归因到二元操作符所在行。
		generator.emitABC(opCode, targetRegister, leftOperand, rightOperand)
		return nil
	}); err != nil {
		generator.releaseOptionalRegister(rightRegister)
		generator.releaseOptionalRegister(leftRegister)
		return true, err
	}
	generator.releaseOptionalRegister(rightRegister)
	generator.releaseOptionalRegister(leftRegister)
	return true, nil
}

// compileBinaryLeftRKRightToTargetTo 编译左侧可 RK、右侧需要求值的普通二元表达式。
//
// targetRegister 不能承载 active local；该路径对齐 Lua 5.3 codegen，先让右侧表达式直接占用目标
// 临时寄存器，再使用 RK 左操作数与目标寄存器生成运算，减少 `local + call()` 场景的额外临时槽。
func (generator *generator) compileBinaryLeftRKRightToTargetTo(expression *parser.BinaryExpression, targetRegister int) (handled bool, err error) {
	opCode, ok := binaryOpCode(expression.Operator)
	if !ok {
		// 非普通二元 opcode 不属于该快路径。
		return false, nil
	}
	leftOperand, leftOK, err := generator.safeInlineRKOperand(expression.Left)
	if err != nil || !leftOK {
		// 左侧无法安全转 RK 时交给通用可复用目标路径。
		return false, err
	}
	if generator.isSafeRKCandidate(expression.Right) {
		// 右侧也能转 RK 时已由 compileBinaryRKTo 处理；错误保持原始编译语义。
		return false, nil
	}
	generator.ensureRegister(targetRegister)
	if err := generator.compileExpressionTo(expression.Right, targetRegister); err != nil {
		// 右侧表达式失败时直接返回，不生成二元运算。
		return true, err
	}
	if expressionIsFixedSingleResultCall(expression.Right) {
		// 固定单返回右侧调用完成后可回收参数槽，避免后续同层表达式被推高寄存器水位。
		generator.releaseCallArgumentsAfterFixedResult(targetRegister, 1)
	}
	if err := generator.withSourceLine(expression.Position, func() error {
		// 左 RK + 右目标寄存器路径保持二元运算错误归因到操作符所在行。
		generator.emitABC(opCode, targetRegister, leftOperand, targetRegister)
		return nil
	}); err != nil {
		return true, err
	}
	return true, nil
}

// compileConcatTo 编译连续字符串拼接表达式。
//
// Lua 5.3 的 `..` 是右结合操作符，但 codegen 会把连续 concat 操作数铺到连续寄存器并生成
// 单条 OP_CONCAT。运行期 OP_CONCAT 负责按右结合语义处理元方法。
func (generator *generator) compileConcatTo(expression *parser.BinaryExpression, targetRegister int) error {
	operands := flattenConcatOperands(expression, nil)
	if len(operands) < 2 {
		// 防御异常 AST；普通二元表达式至少应有两个操作数。
		return fmt.Errorf("codegen invalid concat expression")
	}
	if generator.nextRegister+len(operands) > maxProtoRegisters {
		// CONCAT 操作数必须放入连续寄存器，超过上限时返回编译错误。
		return fmt.Errorf("too many registers")
	}
	startRegister := generator.allocateRegister()
	for operandIndex, operand := range operands {
		currentRegister := startRegister + operandIndex
		if operandIndex > 0 {
			// 第一个寄存器已由 allocateRegister 占用，后续寄存器需要显式纳入水位。
			generator.ensureRegister(currentRegister)
		}
		if err := generator.compileExpressionTo(operand, currentRegister); err != nil {
			// 任一操作数编译失败时释放整个连续临时区间。
			generator.releaseRegistersFrom(startRegister)
			return err
		}
	}
	if err := generator.withSourceLine(expression.Position, func() error {
		// 单条 CONCAT 覆盖完整操作数区间，减少多段拼接的中间字符串和指令数量。
		generator.emitABC(bytecode.OpConcat, targetRegister, startRegister, startRegister+len(operands)-1)
		return nil
	}); err != nil {
		generator.releaseRegistersFrom(startRegister)
		return err
	}
	generator.releaseRegistersFrom(startRegister)
	return nil
}

// flattenConcatOperands 按源码左到右顺序展开连续 `..` 二元表达式。
func flattenConcatOperands(expression parser.Expression, operands []parser.Expression) []parser.Expression {
	binaryExpression, ok := expression.(*parser.BinaryExpression)
	if !ok || binaryExpression.Operator != ".." {
		// 非 concat 节点就是一个实际操作数。
		return append(operands, expression)
	}
	operands = flattenConcatOperands(binaryExpression.Left, operands)
	return flattenConcatOperands(binaryExpression.Right, operands)
}

// compileBinaryWithReusableTargetTo 复用目标寄存器编译普通二元表达式。
//
// 该路径只在 targetRegister 不承载当前 active local 时使用；否则左操作数求值会提前覆盖
// 后续右操作数可能读取的局部变量，破坏 Lua 5.3 左到右求值语义。
func (generator *generator) compileBinaryWithReusableTargetTo(expression *parser.BinaryExpression, targetRegister int) error {
	generator.ensureRegister(targetRegister)
	if err := generator.compileExpressionTo(expression.Left, targetRegister); err != nil {
		// 左操作数失败时不继续生成右操作数，保留原始错误。
		return err
	}
	if expressionIsFixedSingleResultCall(expression.Left) {
		// 左侧固定单返回调用只保留 targetRegister 结果，参数槽不再参与后续右侧求值。
		generator.releaseCallArgumentsAfterFixedResult(targetRegister, 1)
	}
	rightOperand, rightRegister, err := generator.binaryRightOperand(expression.Right)
	if err != nil {
		// 右操作数失败时直接返回，目标寄存器保持已编译的左操作数。
		return err
	}
	opCode, ok := binaryOpCode(expression.Operator)
	if !ok {
		// 不支持的二元操作符返回明确错误。
		generator.releaseOptionalRegister(rightRegister)
		return fmt.Errorf("codegen unsupported binary operator %q", expression.Operator)
	}
	if err := generator.withSourceLine(expression.Position, func() error {
		// 复用目标寄存器路径同样需要把运行期错误归因到二元操作符所在行。
		generator.emitABC(opCode, targetRegister, targetRegister, rightOperand)
		return nil
	}); err != nil {
		generator.releaseOptionalRegister(rightRegister)
		return err
	}
	generator.releaseOptionalRegister(rightRegister)

	// 复用目标寄存器的二元表达式编译完成。
	return nil
}

// binaryRightOperand 编译可复用目标二元表达式的右操作数。
//
// 字面量和当前 local 会直接作为 RK 操作数；其他表达式编译到临时寄存器，并返回该临时寄存器供
// 调用方在发射 opcode 后释放。
func (generator *generator) binaryRightOperand(expression parser.Expression) (operand int, tempRegister int, err error) {
	if operand, register, ok, err := generator.safeRKOperand(expression); err != nil || ok {
		// 安全 RK 操作数不需要额外临时寄存器；错误保持原编译语义。
		return operand, register, err
	}
	rightRegister := generator.allocateRegister()
	if err := generator.compileExpressionTo(expression, rightRegister); err != nil {
		// 右操作数失败时释放临时寄存器并返回。
		generator.releaseRegister(rightRegister)
		return 0, -1, err
	}
	if expressionIsFixedSingleResultCall(expression) {
		// 固定单返回调用的实参槽不再被后续指令读取，可立即回收以贴近 Lua 5.3 codegen 水位。
		generator.releaseCallArgumentsAfterFixedResult(rightRegister, 1)
	}
	return rightRegister, rightRegister, nil
}

// registerHasActiveLocal 判断寄存器当前是否承载可见局部变量。
//
// 若返回 true，表达式编译不能把该寄存器作为中间结果覆盖，否则后续子表达式读取同名 local
// 会看到错误值。
func (generator *generator) registerHasActiveLocal(register int) bool {
	found := false
	generator.forEachLocal(func(_ string, binding localBinding) {
		// 任一可见 local 使用该寄存器时都视为不可复用。
		if binding.register == register {
			found = true
		}
	})
	if found {
		// 命中活动 local 时，调用方不能把该寄存器作为临时目标。
		return true
	}

	// 没有 active local 占用该寄存器。
	return false
}

// compileComparisonTo 编译比较表达式。
//
// targetRegister 接收 boolean 结果；比较操作符必须属于 `== ~= < <= > >=`。
func (generator *generator) compileComparisonTo(expression *parser.BinaryExpression, targetRegister int) error {
	leftRegister := generator.allocateRegister()
	rightRegister := generator.allocateRegister()
	if err := generator.compileExpressionTo(expression.Left, leftRegister); err != nil {
		// 左侧表达式失败时释放比较临时寄存器。
		generator.releaseRegister(rightRegister)
		generator.releaseRegister(leftRegister)
		return err
	}
	if err := generator.compileExpressionTo(expression.Right, rightRegister); err != nil {
		// 右侧表达式失败时释放比较临时寄存器。
		generator.releaseRegister(rightRegister)
		generator.releaseRegister(leftRegister)
		return err
	}
	opCode, expectedTrue, swapOperands := comparisonOpCode(expression.Operator)
	leftOperand := leftRegister
	rightOperand := rightRegister
	if swapOperands {
		// `>` 和 `>=` 通过交换左右操作数复用 Lua 5.3 的 LT/LE 指令。
		leftOperand, rightOperand = rightRegister, leftRegister
	}
	generator.emitABC(opCode, expectedTrue, leftOperand, rightOperand)
	generator.emitABC(bytecode.OpLoadBool, targetRegister, 1, 1)
	generator.emitABC(bytecode.OpLoadBool, targetRegister, 0, 0)
	generator.releaseRegister(rightRegister)
	generator.releaseRegister(leftRegister)

	// 比较表达式编译完成。
	return nil
}

// compileShortCircuitTo 编译 and/or 短路表达式。
//
// 当前实现使用 TEST/JMP/MOVE 形态保留右侧惰性求值，后续跳转回填任务会扩展为公共 patch list。
func (generator *generator) compileShortCircuitTo(expression *parser.BinaryExpression, targetRegister int) error {
	if leftName, ok := expression.Left.(*parser.NameExpression); ok {
		// local 左操作数可用 TESTSET 合并复制和真假测试，对齐 Lua 5.3 官方 codegen。
		if binding, found := generator.lookupLocal(leftName.Name); found && binding.register != targetRegister {
			// TESTSET 条件满足时把左侧 local 复制到目标寄存器，随后 JMP 跳过右侧表达式。
			if expression.Operator == "and" {
				// and 在左侧为 false/nil 时保留左侧结果并跳过右侧。
				generator.emitABC(bytecode.OpTestSet, targetRegister, binding.register, 0)
			} else {
				// or 在左侧为 truthy 时保留左侧结果并跳过右侧。
				generator.emitABC(bytecode.OpTestSet, targetRegister, binding.register, 1)
			}
			jumpPC := generator.emitJump(0)
			if err := generator.compileExpressionTo(expression.Right, targetRegister); err != nil {
				// 右侧表达式失败时返回错误，保持原有编译失败语义。
				return err
			}
			generator.patchJump(jumpPC, len(generator.proto.Code))
			return nil
		}
	}
	if err := generator.compileExpressionTo(expression.Left, targetRegister); err != nil {
		// 左侧表达式决定是否短路，必须先生成到目标寄存器。
		return err
	}
	if expression.Operator == "and" {
		// and 在左侧为 false/nil 时跳过右侧，TEST 的 C=0 表示期望假值。
		generator.emitABC(bytecode.OpTest, targetRegister, 0, 0)
	} else {
		// or 在左侧为 truthy 时跳过右侧，TEST 的 C=1 表示期望真值。
		generator.emitABC(bytecode.OpTest, targetRegister, 0, 1)
	}
	jumpPC := generator.emitJump(0)

	// 未短路时覆盖目标寄存器为右侧表达式结果。
	if err := generator.compileExpressionTo(expression.Right, targetRegister); err != nil {
		// 右侧表达式失败时返回错误。
		return err
	}
	generator.patchJump(jumpPC, len(generator.proto.Code))

	// 短路表达式编译完成。
	return nil
}

// compileTableConstructorTo 编译 table constructor 表达式。
//
// 默认数组字段使用 NEWTABLE 和 SETTABLE 写入 1-based integer key；末尾字段为 vararg 或函数调用
// 时改用开放写入与 SETLIST B=0，让 `{..., f()}` 按 Lua 5.3 语义展开全部返回值。
func (generator *generator) compileTableConstructorTo(expression *parser.TableConstructorExpression, targetRegister int) error {
	generator.emitABC(bytecode.OpNewTable, targetRegister, 0, 0)
	if len(expression.Fields) > 0 {
		// table 数组字段末尾是开放表达式时必须展开为开放列表。
		if isOpenListExpression(expression.Fields[len(expression.Fields)-1]) {
			for fieldIndex, fieldExpression := range expression.Fields {
				// SETLIST 消费 R(A+1)..，因此字段值必须紧跟 table 目标寄存器。
				valueRegister := targetRegister + 1 + fieldIndex
				generator.ensureRegister(valueRegister)
				if fieldIndex == len(expression.Fields)-1 {
					// 末尾开放表达式使用开放写入，SETLIST B=0 后续按 VM openTop 决定真实字段数。
					if err := generator.compileOpenListExpressionTo(fieldExpression, valueRegister); err != nil {
						// 开放表达式编译失败时停止构造。
						return err
					}
					generator.emitABC(bytecode.OpSetList, targetRegister, 0, 1)
					break
				}
				if err := generator.compileExpressionTo(fieldExpression, valueRegister); err != nil {
					// 固定字段编译失败时停止构造，避免生成部分 table 初始化。
					return err
				}
			}
			// trailing vararg 的数组部分已经通过 SETLIST 完成。
			goto recordFields
		}
	}
	for fieldIndex, fieldExpression := range expression.Fields {
		// Lua 数组字段从 1 开始编号。
		valueRegister := generator.allocateRegister()
		if err := generator.compileExpressionTo(fieldExpression, valueRegister); err != nil {
			// 字段表达式编译失败时释放临时寄存器。
			generator.releaseRegister(valueRegister)
			return err
		}
		if expressionIsFixedSingleResultCall(fieldExpression) {
			generator.releaseCallArgumentsAfterFixedResult(valueRegister, 1)
		}
		keyIndex := generator.addConstant(bytecode.IntegerConstant(int64(fieldIndex + 1)))
		keyOperand, keyRegister, err := generator.rkOperandForConstantIndex(keyIndex)
		if err != nil {
			// 数组 key 常量无法编码或加载时释放字段值寄存器。
			generator.releaseRegister(valueRegister)
			return err
		}
		generator.emitABC(bytecode.OpSetTable, targetRegister, keyOperand, valueRegister)
		generator.releaseOptionalRegister(keyRegister)
		generator.releaseRegister(valueRegister)
	}
recordFields:
	for _, recordField := range expression.RecordFields {
		// 记录字段使用字段名字符串作为 table key。
		valueRegister := generator.allocateRegister()
		if err := generator.compileExpressionTo(recordField.Value, valueRegister); err != nil {
			// 字段值表达式编译失败时释放临时寄存器。
			generator.releaseRegister(valueRegister)
			return err
		}
		if expressionIsFixedSingleResultCall(recordField.Value) {
			generator.releaseCallArgumentsAfterFixedResult(valueRegister, 1)
		}
		keyIndex := generator.addConstant(bytecode.StringConstant(recordField.Name))
		keyOperand, keyRegister, err := generator.rkOperandForConstantIndex(keyIndex)
		if err != nil {
			// 记录字段 key 常量无法编码或加载时释放字段值寄存器。
			generator.releaseRegister(valueRegister)
			return err
		}
		generator.emitABC(bytecode.OpSetTable, targetRegister, keyOperand, valueRegister)
		generator.releaseOptionalRegister(keyRegister)
		generator.releaseRegister(valueRegister)
	}
	for _, indexField := range expression.IndexFields {
		// 动态键字段需要分别求值 key 和 value，再用 SETTABLE 写入 table。
		keyRegister := generator.allocateRegister()
		if err := generator.compileExpressionTo(indexField.Key, keyRegister); err != nil {
			// key 表达式失败时释放临时寄存器。
			generator.releaseRegister(keyRegister)
			return err
		}
		if expressionIsFixedSingleResultCall(indexField.Key) {
			generator.releaseCallArgumentsAfterFixedResult(keyRegister, 1)
		}
		valueRegister := generator.allocateRegister()
		if err := generator.compileExpressionTo(indexField.Value, valueRegister); err != nil {
			// value 表达式失败时释放两个临时寄存器。
			generator.releaseRegister(valueRegister)
			generator.releaseRegister(keyRegister)
			return err
		}
		if expressionIsFixedSingleResultCall(indexField.Value) {
			generator.releaseCallArgumentsAfterFixedResult(valueRegister, 1)
		}
		generator.emitABC(bytecode.OpSetTable, targetRegister, keyRegister, valueRegister)
		generator.releaseRegister(valueRegister)
		generator.releaseRegister(keyRegister)
	}

	// table constructor 编译完成。
	return nil
}

// isOpenListExpression 判断表达式在列表尾部是否需要展开多返回值。
//
// Lua 5.3 只有 vararg、普通函数调用和 method call 在表达式列表最后位置保留多返回值。
func isOpenListExpression(expression parser.Expression) bool {
	switch expression.(type) {
	case *parser.VarargExpression, *parser.FunctionCallExpression, *parser.MethodCallExpression:
		// 这些表达式位于列表尾部时应展开为开放列表。
		return true
	default:
		// 其他表达式只产生一个值。
		return false
	}
}

// compileOpenListExpressionTo 编译表达式列表尾部的开放表达式。
//
// targetRegister 是开放列表起始寄存器；编译结果必须设置 VM openTop，供后续 SETLIST B=0 消费。
func (generator *generator) compileOpenListExpressionTo(expression parser.Expression, targetRegister int) error {
	switch typedExpression := expression.(type) {
	case *parser.VarargExpression:
		// vararg B=0 写入全部 vararg 并设置开放栈顶。
		generator.emitABC(bytecode.OpVararg, targetRegister, 0, 0)
		return nil
	case *parser.FunctionCallExpression:
		// CALL C=0 写入被调函数全部返回值并设置开放栈顶。
		return generator.compileFunctionCallTo(typedExpression, targetRegister, -1)
	case *parser.MethodCallExpression:
		// method call 同样通过 CALL C=0 展开全部返回值。
		return generator.compileMethodCallTo(typedExpression, targetRegister, -1)
	default:
		// 调用方应先用 isOpenListExpression 判断，保留防御式错误。
		return fmt.Errorf("codegen unsupported open list expression %T", expression)
	}
}

// allocateRegister 分配一个寄存器。
//
// 返回值是当前函数内的寄存器下标；超过 Lua 5.3 A 字段上限时会在后续范围检查任务中报错。
func (generator *generator) allocateRegister() int {
	// 使用单调递增的 nextRegister 表示栈顶。
	register := generator.nextRegister
	generator.nextRegister++
	if generator.nextRegister > generator.maxRegister {
		// 记录最大寄存器数量，用于 Proto.MaxStackSize。
		generator.maxRegister = generator.nextRegister
	}

	// 返回新分配的寄存器下标。
	return register
}

// ensureRegister 确保指定寄存器纳入当前 Proto 栈大小统计。
//
// targetRegister 可能来自 return 固定目标或长期 local 寄存器，不一定通过 allocateRegister 分配。
func (generator *generator) ensureRegister(targetRegister int) {
	if targetRegister+1 > generator.maxRegister {
		// MaxStackSize 表示需要的寄存器数量，因此使用下标加一更新。
		generator.maxRegister = targetRegister + 1
	}
	if targetRegister >= generator.nextRegister {
		// 直接写入高位寄存器时推进 nextRegister，避免后续临时寄存器覆盖目标。
		generator.nextRegister = targetRegister + 1
	}
}

// releaseRegister 释放最近分配的临时寄存器。
//
// 当前分配器按栈 discipline 工作；释放非栈顶寄存器会被忽略以保护长期 local 寄存器。
func (generator *generator) releaseRegister(register int) {
	if register == generator.nextRegister-1 {
		// 只有栈顶临时寄存器可以安全回退。
		generator.nextRegister--
	}
}

// releaseOptionalRegister 释放可选临时寄存器。
//
// register 小于 0 表示调用方使用了 RK 常量内联路径，没有额外寄存器需要释放。
func (generator *generator) releaseOptionalRegister(register int) {
	if register >= 0 {
		// 仅当 helper 实际分配寄存器时才回退临时寄存器水位。
		generator.releaseRegister(register)
	}
}

// maxStackSize 返回当前函数原型可写入 Proto 的栈大小。
//
// Proto.MaxStackSize 是 uint8；深表达式只被官方测试用于 load 成功性验证时可能产生较高的临时
// 水位，这里饱和到可编码上限。实际 CALL 等连续寄存器需求会在对应生成路径提前报错。
func (generator *generator) maxStackSize() uint8 {
	if generator.maxRegister > maxProtoRegisters {
		// 防止 uint8 回绕污染 Proto 元信息。
		return uint8(maxProtoRegisters)
	}

	// 寄存器数量可被 Proto.MaxStackSize 精确表达。
	return uint8(generator.maxRegister)
}

// releaseRegistersFrom 释放从指定寄存器开始的临时寄存器区间。
//
// startRegister 通常是调用语句的函数寄存器，调用参数位于其后，整个区间可一次回收。
func (generator *generator) releaseRegistersFrom(startRegister int) {
	if startRegister < generator.nextRegister {
		// 只向下回退栈顶，不影响更低位置的长期 local。
		generator.nextRegister = startRegister
	}
}

// releaseCallArgumentsAfterFixedResult 回收固定返回 CALL 的实参寄存器。
//
// targetRegister 是 CALL A 字段，resultCount 是固定返回值数量。Lua 5.3 固定返回 CALL 结束后，
// 函数和实参槽只保留返回区间；但 Go codegen 的 nextRegister 还必须位于所有活跃 local 之后，
// 否则后续临时寄存器会覆盖 numeric for 控制变量或同作用域后续 local。
func (generator *generator) releaseCallArgumentsAfterFixedResult(targetRegister int, resultCount int) {
	releaseFrom := targetRegister + resultCount
	generator.forEachLocal(func(_ string, binding localBinding) {
		if binding.register >= releaseFrom {
			// 活跃 local 位于返回区间之后时，回收水位必须保留在该 local 后面。
			releaseFrom = binding.register + 1
		}
	})
	generator.releaseRegistersFrom(releaseFrom)
}

// defineLocal 登记一个局部变量和调试生命周期。
//
// name 是 Lua 局部变量名；register 是对应寄存器；position 当前保留给后续行号映射扩展。
func (generator *generator) defineLocal(name string, register int, position lexer.Position) {
	// 普通 local 默认不是 const。
	generator.defineLocalWithConst(name, register, position, false)
}

// defineLocalWithConst 登记一个局部变量和调试生命周期，并记录 const 约束。
//
// name 是 Lua 局部变量名；register 是对应寄存器；constLocal 为 true 时后续当前函数内普通赋值会报错。
func (generator *generator) defineLocalWithConst(name string, register int, position lexer.Position, constLocal bool) {
	currentScopeID := generator.currentScopeID()
	if previousBinding, exists := generator.lookupLocal(name); exists && previousBinding.scopeID == currentScopeID {
		// 同一词法作用域内重新声明同名 local 时，旧 local 从新声明生效处开始不可见。
		generator.proto.LocalVars[previousBinding.localVarIndex].EndPC = len(generator.proto.Code)
	}
	localVar := bytecode.LocalVar{Name: name, Register: register, Const: constLocal, StartPC: len(generator.proto.Code), EndPC: len(generator.proto.Code)}
	index := len(generator.proto.LocalVars)
	generator.proto.LocalVars = append(generator.proto.LocalVars, localVar)
	generator.setLocal(name, localBinding{register: register, localVarIndex: index, scopeID: currentScopeID, constLocal: constLocal})
}

// constAssignmentError 构造 const 绑定被重新赋值的编译错误。
func constAssignmentError(name string, position lexer.Position) error {
	// 有效行号用于对齐现有编译错误的 line N 风格。
	if position.Line > 0 {
		// 返回包含 const 名称的错误，方便用户定位被覆盖的绑定。
		return fmt.Errorf("line %d: cannot assign to const binding '%s'", position.Line, name)
	}

	// 缺少行号时仍保留核心错误文本。
	return fmt.Errorf("cannot assign to const binding '%s'", name)
}

// isTopLevelChunkScope 判断当前生成位置是否处于 chunk 顶层 block。
func (generator *generator) isTopLevelChunkScope() bool {
	currentScope := generator.currentScope()
	return generator.parent == nil && currentScope != nil && currentScope.Depth == 0
}

// defineGlobalConst 记录当前 chunk 顶层 const 声明出的只读全局名称。
func (generator *generator) defineGlobalConst(name string, position lexer.Position) {
	root := generator.rootGenerator()
	if root.globalConsts == nil {
		// 仅在首次顶层 const 出现时创建映射，普通 Lua chunk 不承担额外分配。
		root.globalConsts = make(map[string]lexer.Position)
	}
	root.globalConsts[name] = position
}

// lookupGlobalConst 查找当前 chunk 顶层 const 声明出的只读全局名称。
func (generator *generator) lookupGlobalConst(name string) (lexer.Position, bool) {
	root := generator.rootGenerator()
	if root == nil || root.globalConsts == nil {
		// 没有顶层 const 时直接视为未命中。
		return lexer.Position{}, false
	}
	position, ok := root.globalConsts[name]
	return position, ok
}

// rootGenerator 返回当前嵌套函数所属 chunk 的顶层生成器。
func (generator *generator) rootGenerator() *generator {
	current := generator
	for current != nil && current.parent != nil {
		// 沿 parent 链回到 chunk 顶层，顶层 const 对同一 chunk 内嵌函数可见。
		current = current.parent
	}
	return current
}

// currentScopeID 返回当前 parser 作用域编号。
//
// 没有 parser 作用域时返回 -1，调用方可用该值作为保守的函数级作用域标识。
func (generator *generator) currentScopeID() int {
	currentScope := generator.currentScope()
	if currentScope == nil {
		// 缺少作用域信息时使用 -1，避免空指针并保持同一无作用域上下文可比较。
		return -1
	}

	// 返回 parser 语义阶段分配的稳定作用域编号。
	return currentScope.ID
}

// beginScope 保存进入嵌套 block 前的 codegen 可见局部状态。
//
// 返回快照必须传给 endScope；该机制只恢复 codegen 解析名称所需的 locals 与寄存器水位，
// 不回退已经生成的指令、常量或调试局部变量表。
func (generator *generator) beginScope() scopeSnapshot {
	return scopeSnapshot{
		localInlineName:    generator.localInlineName,
		localInlineBinding: generator.localInlineBinding,
		localInlineValid:   generator.localInlineValid,
		locals:             generator.snapshotLocals(),
		localVarCount:      len(generator.proto.LocalVars),
		nextRegister:       generator.nextRegister,
	}
}

// endScope 关闭嵌套 block 新增 local 并恢复外层 codegen 状态。
//
// endPC 是当前 block 结束时的指令位置；本作用域新增的 LocalVars 都会把 EndPC 回填到这里。
func (generator *generator) endScope(scope scopeSnapshot, endPC int) {
	if generator.hasCapturedLocalSince(scope.localVarCount) {
		// 退出包含已捕获新增 local 的 block 时发出 close-only JMP，运行期据此闭合 open upvalue。
		generator.emitAsBx(bytecode.OpJmp, scope.nextRegister+1, 0)
		endPC = len(generator.proto.Code)
	}
	for index := scope.localVarCount; index < len(generator.proto.LocalVars); index++ {
		// 所有本作用域新增局部变量都在当前 block 末尾结束。
		generator.proto.LocalVars[index].EndPC = endPC
	}
	generator.mergeCapturedLocalsIntoSnapshot(&scope)
	generator.restoreLocalSnapshot(scope)
	generator.nextRegister = scope.nextRegister
}

// hasCapturedLocalSince 判断指定 LocalVars 起点之后是否存在被子函数捕获的局部变量。
func (generator *generator) hasCapturedLocalSince(localVarStart int) bool {
	found := false
	generator.forEachLocal(func(_ string, binding localBinding) {
		if binding.localVarIndex >= localVarStart && binding.captured {
			// 只要有新增 local 被捕获，退出作用域就必须关闭对应 open upvalue。
			found = true
		}
	})
	if found {
		// 存在新增 captured local 时必须生成 close-only JMP。
		return true
	}

	// 没有新增 captured local 时可省略 close-only JMP。
	return false
}

// mergeCapturedLocalsIntoSnapshot 将内层 block 期间产生的外层 local 捕获标记合并回快照。
//
// beginScope 会复制 locals 供退出 block 后恢复名称解析；若内层函数捕获了进入 block 前已存在的
// local，resolveUpvalue 会先标记当前 binding。退出 block 时必须把该标记写回快照，否则外层后续
// endScope 会误以为该 local 未被捕获，漏发 OP_JMP close 指令并让下一轮寄存器复用污染旧闭包。
func (generator *generator) mergeCapturedLocalsIntoSnapshot(snapshot *scopeSnapshot) {
	if snapshot == nil || (!snapshot.localInlineValid && len(snapshot.locals) == 0) || generator.localCount() == 0 {
		// 没有外层快照或当前局部表为空时，无需合并捕获标记。
		return
	}
	if snapshot.localInlineValid {
		// inline 快照也可能在内层 block 被子函数捕获，必须把 captured 标记合并回去。
		currentBinding, exists := generator.lookupLocal(snapshot.localInlineName)
		if exists &&
			currentBinding.localVarIndex == snapshot.localInlineBinding.localVarIndex &&
			currentBinding.captured &&
			!snapshot.localInlineBinding.captured {
			// 当前 inline local 已在内层被捕获，恢复外层快照前必须保留 captured 标记。
			snapshot.localInlineBinding.captured = true
		}
	}
	for name, snapshotBinding := range snapshot.locals {
		currentBinding, exists := generator.lookupLocal(name)
		if !exists {
			// 当前作用域中该名称已被完全遮蔽或移除，快照保持原值。
			continue
		}
		if currentBinding.localVarIndex != snapshotBinding.localVarIndex || !currentBinding.captured || snapshotBinding.captured {
			// 同名内层 local 的捕获不能污染外层；已是 captured 时也无需重复写回。
			continue
		}
		snapshotBinding.captured = true
		snapshot.locals[name] = snapshotBinding
	}
}

// closeLocals 将当前函数所有局部变量生命周期结束位置回填为 endPC。
//
// 未被嵌套 block 提前关闭的函数级 local 使用函数结束作为 EndPC。
func (generator *generator) closeLocals(endPC int) {
	generator.forEachLocal(func(_ string, binding localBinding) {
		// 每个局部变量的 EndPC 使用函数最终指令位置。
		generator.proto.LocalVars[binding.localVarIndex].EndPC = endPC
	})
}

// resolveUpvalue 解析并登记当前函数需要捕获的 upvalue。
//
// 返回值 upvalueIndex 是当前 Proto.Upvalues 下标；ok=false 表示外层也找不到该名称。
// position 用于 Lua 5.3 upvalue 上限错误行号。
func (generator *generator) resolveUpvalue(name string, position lexer.Position) (upvalueIndex int, ok bool, err error) {
	if index, exists := generator.upvalues[name]; exists {
		// 已捕获名称直接复用 upvalue 下标。
		return index, true, nil
	}
	if generator.parent == nil {
		// 顶层函数没有外层作用域，无法捕获 upvalue。
		return 0, false, nil
	}
	if binding, exists := generator.parent.lookupLocal(name); exists {
		if len(generator.proto.Upvalues) >= maxProtoRegisters {
			// Lua 5.3 单函数 upvalue 数量超过上限时报告源码行号。
			return 0, true, tooManyUpvaluesError(position)
		}
		// 命中父函数 local 时，登记 InStack upvalue。
		index := len(generator.proto.Upvalues)
		generator.proto.Upvalues = append(generator.proto.Upvalues, bytecode.UpvalueDesc{Name: name, InStack: true, Index: uint8(binding.register), Const: binding.constLocal})
		generator.setUpvalue(name, index)
		binding.captured = true
		generator.parent.setLocal(name, binding)
		return index, true, nil
	}
	parentIndex, exists, err := generator.parent.resolveUpvalue(name, position)
	if err != nil {
		// 外层捕获链路已经命中 upvalue 上限。
		return 0, true, err
	}
	if !exists {
		// 递归外层也找不到该名称。
		return 0, false, nil
	}
	if len(generator.proto.Upvalues) >= maxProtoRegisters {
		// 间接捕获同样受单函数 upvalue 数量上限约束。
		return 0, true, tooManyUpvaluesError(position)
	}
	index := len(generator.proto.Upvalues)
	generator.proto.Upvalues = append(generator.proto.Upvalues, bytecode.UpvalueDesc{Name: name, InStack: false, Index: uint8(parentIndex), Const: generator.parent.proto.Upvalues[parentIndex].Const})
	generator.setUpvalue(name, index)

	// 返回间接捕获的 upvalue 下标。
	return index, true, nil
}

// tooManyUpvaluesError 构造 Lua 5.3 upvalue 上限错误。
//
// position 来自触发捕获的名称表达式；无有效行号时仍保留固定错误片段。
func tooManyUpvaluesError(position lexer.Position) error {
	if position.Line > 0 {
		// 官方 errors.lua 同时匹配 `line N` 和 `too many upvalues`。
		return fmt.Errorf("line %d: too many upvalues", position.Line)
	}

	// 缺少行号时仍返回核心错误文本。
	return fmt.Errorf("too many upvalues")
}

// envUpvalueIndex 返回当前函数 `_ENV` upvalue 下标。
//
// 顶层 Proto 把 `_ENV` 描述为外部注入 upvalue；嵌套函数通过外层 `_ENV` 间接捕获。
func (generator *generator) envUpvalueIndex() int {
	if index, exists := generator.upvalues[envUpvalueName]; exists {
		// 已登记 `_ENV` 时复用下标，避免重复 UpvalueDesc。
		return index
	}
	index := len(generator.proto.Upvalues)
	if generator.parent == nil {
		// 顶层 `_ENV` 由 lua.LoadString 绑定到 State globals，不来自源码局部寄存器。
		generator.proto.Upvalues = append(generator.proto.Upvalues, bytecode.UpvalueDesc{Name: envUpvalueName, InStack: true, Index: 0})
		generator.setUpvalue(envUpvalueName, index)
		return index
	}
	if binding, exists := generator.parent.lookupLocal(envUpvalueName); exists {
		// 父函数显式 local _ENV 时，嵌套函数必须捕获该寄存器作为自身环境。
		generator.proto.Upvalues = append(generator.proto.Upvalues, bytecode.UpvalueDesc{Name: envUpvalueName, InStack: true, Index: uint8(binding.register)})
		generator.setUpvalue(envUpvalueName, index)
		binding.captured = true
		generator.parent.setLocal(envUpvalueName, binding)
		return index
	}
	parentIndex := generator.parent.envUpvalueIndex()
	// 嵌套函数捕获父函数的 `_ENV` upvalue，运行时 CLOSURE 会从父 closure.Upvalues 读取。
	generator.proto.Upvalues = append(generator.proto.Upvalues, bytecode.UpvalueDesc{Name: envUpvalueName, InStack: false, Index: uint8(parentIndex)})
	generator.setUpvalue(envUpvalueName, index)

	// 返回当前函数中的 `_ENV` upvalue 下标。
	return index
}

// emitJump 追加待回填 JMP 指令。
//
// sbx 使用 0 占位；调用方后续必须用 patchJump 写入真实目标。
func (generator *generator) emitJump(a int) int {
	// 先生成 sBx=0 占位跳转。
	return generator.emitAsBx(bytecode.OpJmp, a, 0)
}

// emitJumpFor 追加待回填 for 循环跳转指令。
//
// opCode 必须是 FORPREP 或 FORLOOP；A 字段传入数值 for 的基准寄存器。
func (generator *generator) emitJumpFor(opCode bytecode.OpCode, a int) int {
	// 先生成 sBx=0 占位，由 patchJump 根据目标 pc 回填。
	return generator.emitAsBx(opCode, a, 0)
}

// patchJump 将 jumpPC 处的 JMP 回填到 targetPC。
//
// targetPC 是跳转后要执行的指令位置；sBx 按 Lua pc 已经前移一条的语义计算。
func (generator *generator) patchJump(jumpPC int, targetPC int) {
	offset := targetPC - jumpPC - 1
	original := generator.proto.Code[jumpPC]
	generator.proto.Code[jumpPC] = bytecode.CreateAsBx(original.OpCode(), original.A(), offset)
}

// patchGotoJump 回填 goto 跳转，并在跳出局部作用域时设置 close 起点。
//
// jumpPC 指向 goto 的 JMP 占位；target 是 label 的目标 PC 与作用域水位；sourceNextRegister
// 是 goto 发出时的寄存器水位。若源水位高于目标水位，说明 goto 离开了部分 local 作用域，
// JMP A 必须设为目标水位加一，运行期据此关闭对应 open upvalue。
func (generator *generator) patchGotoJump(jumpPC int, target labelInfo, sourceNextRegister int) {
	offset := target.pc - jumpPC - 1
	closeRegister := 0
	if sourceNextRegister > target.nextRegister {
		// Lua 5.3 JMP 的 A 字段保存待关闭寄存器加一，0 表示不关闭。
		closeRegister = target.nextRegister + 1
	}
	generator.proto.Code[jumpPC] = bytecode.CreateAsBx(bytecode.OpJmp, closeRegister, offset)
}

// patchJumpChecked 校验 sBx 范围后回填跳转目标。
//
// jumpPC 必须指向待回填的 iAsBx 跳转指令；targetPC 是跳转后要执行的指令位置。offset 超出
// Lua 5.3 sBx 可编码范围时返回包含 `too long` 的编译错误，兼容官方 constructs.lua 检查。
func (generator *generator) patchJumpChecked(jumpPC int, targetPC int) error {
	offset := targetPC - jumpPC - 1
	if offset < -bytecode.MaxArgSBx || offset > bytecode.MaxArgSBx {
		// sBx 只有 18 位 excess-K 表达，过长控制结构不能静默截断。
		return fmt.Errorf("control structure too long")
	}

	// 范围合法时复用原有回填逻辑。
	generator.patchJump(jumpPC, targetPC)
	return nil
}

// patchPendingGotos 回填当前函数内所有前向 goto 跳转。
//
// parser 语义分析已保证 label 存在且不会非法跳入内层作用域；这里仅把名称解析为 PC。
func (generator *generator) patchPendingGotos() error {
	for _, pending := range generator.pendingGotos {
		// 每个 goto 都必须能在当前函数 label 表中找到当前作用域可见的目标位置。
		target, exists := generator.resolveGotoLabel(pending)
		if !exists {
			// 未定义 label 正常会被 parser 拦截；保留错误便于直接调用 codegen 时定位。
			return fmt.Errorf("codegen undefined label %q", pending.label)
		}
		generator.patchGotoJump(pending.jumpPC, target, pending.sourceNextRegister)
	}
	generator.pendingGotos = nil

	// 所有 goto 已完成回填。
	return nil
}

// resolveGotoLabel 按 Lua label 可见性解析 goto 的目标。
//
// 解析优先选择同 block label，其次选择最近外层祖先 block label；兄弟或内层 block 的同名 label
// 对当前 goto 不可见，不能被用于回填。
func (generator *generator) resolveGotoLabel(pending pendingGoto) (labelInfo, bool) {
	labels := generator.labelPCs[pending.label]
	if pending.sourceScope == nil {
		// 旧 parser 语义阶段可能没有给函数表达式内 block 标注 scope；若当前函数内同名
		// label 唯一，仍可安全回填。多个同名 label 缺少 scope 时目标不唯一，保持失败。
		return uniqueLabelInfo(labels)
	}
	var best labelInfo
	found := false
	for _, candidate := range labels {
		if !generator.scopeContains(candidate.scope, pending.sourceScope) {
			// label 所在作用域不是 goto 作用域的祖先时不可见。
			continue
		}
		if !found || candidate.scope.Depth > best.scope.Depth {
			// 越深的祖先作用域越接近 goto，优先作为跳转目标。
			best = candidate
			found = true
		}
	}
	return best, found
}

// uniqueLabelInfo 在缺失 scope 信息时返回唯一 label 目标。
//
// labels 为空或包含多个同名目标时返回 false，避免在作用域信息不足时误解析 goto。
func uniqueLabelInfo(labels []labelInfo) (labelInfo, bool) {
	if len(labels) != 1 {
		// 没有目标或目标不唯一时不能安全回填。
		return labelInfo{}, false
	}

	// 唯一目标在当前函数内无歧义。
	return labels[0], true
}

// scopeContains 判断 outer 是否为 inner 的同一作用域或祖先作用域。
//
// codegen 复用 parser 语义阶段的 ScopeInfo 父链，保证同名 label 在不相交 block 中不会互相抢占。
func (generator *generator) scopeContains(outer *parser.ScopeInfo, inner *parser.ScopeInfo) bool {
	if outer == nil || inner == nil {
		// 缺失 scope 信息时无法证明可见，按不可见处理。
		return false
	}
	for current := inner; current != nil; current = generator.scopes[current.ParentID] {
		if current.ID == outer.ID {
			// 在父链上命中 outer，说明 outer label 对 inner goto 可见。
			return true
		}
		if current.ParentID == outer.ID {
			// 子函数 codegen 只索引当前函数内已编译 block；直接父 scope 可能尚未在索引中，
			// 但 ParentID 已足以证明 outer 是 inner 的父作用域。
			return true
		}
		if current.ParentID < 0 {
			// 到达函数顶层仍未命中 outer，说明不是祖先。
			return false
		}
	}

	// 父链索引缺失时按不可见处理。
	return false
}

// beginLoop 开始记录一个循环内部的 break 跳转。
//
// 返回值是循环在 breakJumps 栈中的下标，必须传给 patchLoopBreaks 或 discardLoop。
func (generator *generator) beginLoop() int {
	// 每个循环拥有独立 break/continue 列表，嵌套循环只回填最内层控制流。
	generator.breakJumps = append(generator.breakJumps, nil)
	generator.continueJumps = append(generator.continueJumps, nil)
	generator.loopCloseRegisters = append(generator.loopCloseRegisters, generator.nextRegister+1)
	return len(generator.breakJumps) - 1
}

// discardLoop 丢弃编译失败循环的 break 记录。
//
// loopIndex 必须是 beginLoop 返回的当前最内层循环下标；异常路径不回填 break，避免污染外层循环。
func (generator *generator) discardLoop(loopIndex int) {
	if loopIndex < 0 || loopIndex >= len(generator.breakJumps) {
		// 非法下标只可能来自内部调用错误，保持 no-op 避免二次 panic 掩盖原错误。
		return
	}
	if loopIndex == len(generator.breakJumps)-1 {
		// 正常路径丢弃最内层循环记录。
		generator.breakJumps = generator.breakJumps[:loopIndex]
		generator.continueJumps = generator.continueJumps[:loopIndex]
		generator.loopCloseRegisters = generator.loopCloseRegisters[:loopIndex]
		return
	}

	// 非最内层异常极少发生，保守清理到该层，避免外层 break 列表错位。
	generator.breakJumps = generator.breakJumps[:loopIndex]
	generator.continueJumps = generator.continueJumps[:loopIndex]
	generator.loopCloseRegisters = generator.loopCloseRegisters[:loopIndex]
}

// patchLoopBreaks 回填当前循环内所有 break 跳转。
//
// loopIndex 必须是 beginLoop 返回的当前最内层循环下标；targetPC 是循环后一条指令位置。
func (generator *generator) patchLoopBreaks(loopIndex int, targetPC int) {
	if loopIndex < 0 || loopIndex >= len(generator.breakJumps) {
		// 非法下标表示内部状态异常；没有可回填列表时保持 no-op。
		return
	}
	for _, breakPC := range generator.breakJumps[loopIndex] {
		// 每个 break 都跳到循环结束位置。
		generator.patchJump(breakPC, targetPC)
	}
	generator.breakJumps = generator.breakJumps[:loopIndex]
	generator.continueJumps = generator.continueJumps[:loopIndex]
	generator.loopCloseRegisters = generator.loopCloseRegisters[:loopIndex]
}

// patchLoopContinues 回填当前循环内所有 continue 跳转。
//
// loopIndex 必须是 beginLoop 返回的当前最内层循环下标；targetPC 是最近循环的续迭代入口。
func (generator *generator) patchLoopContinues(loopIndex int, targetPC int) {
	if loopIndex < 0 || loopIndex >= len(generator.continueJumps) {
		// 非法下标表示内部状态异常；没有可回填列表时保持 no-op。
		return
	}
	for _, continuePC := range generator.continueJumps[loopIndex] {
		// 每个 continue 都跳到当前循环的下一轮入口。
		generator.patchJump(continuePC, targetPC)
	}
}

// currentLoopCloseRegister 返回当前最内层循环跳转离开时应关闭的寄存器起点。
func (generator *generator) currentLoopCloseRegister() int {
	if len(generator.loopCloseRegisters) == 0 {
		// 缺失循环状态时保守返回 0，让上层错误路径保持原来的无 close JMP。
		return 0
	}

	// OP_JMP A 字段使用寄存器下标加一；beginLoop 已保存该编码值。
	return generator.loopCloseRegisters[len(generator.loopCloseRegisters)-1]
}

// addConstant 追加或复用 Proto 常量。
//
// 返回值是常量表索引，可直接用于 LOADK 的 Bx 字段。
func (generator *generator) addConstant(constant bytecode.Constant) int {
	if index, ok := generator.constantIndex(constant); ok {
		// 常量已存在时复用索引，保持常量池按原值去重。
		return index
	}
	index := generator.proto.AddConstant(constant)
	generator.recordConstantIndex(constant, index)

	// 返回新增常量索引。
	return index
}

// constantIndex 查询常量表中已有的原值下标。
//
// 返回 ok=false 表示该常量尚未登记；integer 与 number 分属不同索引，兼容 Lua 5.3 常量类型边界。
func (generator *generator) constantIndex(constant bytecode.Constant) (int, bool) {
	switch constant.Kind {
	case bytecode.ConstantNil:
		// nil 不能作为 C Lua table key；Go 侧用专用字段表达同一唯一常量。
		return generator.constants.nilIndex, generator.constants.hasNil
	case bytecode.ConstantBoolean:
		// bool 只有 false/true 两种取值，使用固定数组避免 map 分配。
		boolSlot := 0
		if constant.Bool {
			// true 存放在第二个槽位，false 保持零槽位。
			boolSlot = 1
		}
		return generator.constants.boolIndex[boolSlot], generator.constants.hasBool[boolSlot]
	case bytecode.ConstantInteger:
		// integer 使用 int64 原值索引，避免和 float 同值碰撞。
		if generator.constants.hasInlineInteger && generator.constants.inlineIntegerValue == constant.Integer {
			// 单 integer 常量函数直接命中 inline 槽，避免创建 map。
			return generator.constants.inlineIntegerIndex, true
		}
		index, ok := generator.constants.integers[constant.Integer]
		return index, ok
	case bytecode.ConstantNumber:
		// float number 使用 float64 原值索引，保留 Lua 5.3 number 常量边界。
		index, ok := generator.constants.numbers[constant.Number]
		return index, ok
	case bytecode.ConstantString:
		// string 按字节序列原值索引，不需要额外 quote 转义。
		index, ok := generator.constants.strings[constant.String]
		return index, ok
	default:
		// 未知常量类型不参与复用，避免未来扩展误合并。
		return 0, false
	}
}

// recordConstantIndex 登记新常量的常量表下标。
//
// 调用方必须保证该常量刚刚追加到 Proto.Constants；重复登记会覆盖同值索引并破坏首次出现顺序。
func (generator *generator) recordConstantIndex(constant bytecode.Constant, index int) {
	switch constant.Kind {
	case bytecode.ConstantNil:
		// nil 常量全局唯一。
		generator.constants.hasNil = true
		generator.constants.nilIndex = index
	case bytecode.ConstantBoolean:
		// bool 常量按 false/true 固定槽位登记。
		boolSlot := 0
		if constant.Bool {
			// true 存放在第二个槽位，false 保持零槽位。
			boolSlot = 1
		}
		generator.constants.hasBool[boolSlot] = true
		generator.constants.boolIndex[boolSlot] = index
	case bytecode.ConstantInteger:
		if generator.constants.hasInlineInteger && generator.constants.inlineIntegerValue == constant.Integer {
			// 理论上 addConstant 会先命中 constantIndex；这里保守更新，保持重复登记时下标一致。
			generator.constants.inlineIntegerIndex = index
			return
		}
		if _, exists := generator.constants.integers[constant.Integer]; exists {
			// 已经进入 overflow map 的 integer 保持在 map 中。
			generator.constants.integers[constant.Integer] = index
			return
		}
		if !generator.constants.hasInlineInteger && len(generator.constants.integers) == 0 {
			// 首个 integer 常量进入 inline 槽，覆盖单常量子函数热路径。
			generator.constants.hasInlineInteger = true
			generator.constants.inlineIntegerValue = constant.Integer
			generator.constants.inlineIntegerIndex = index
			return
		}
		if generator.constants.integers == nil {
			// 第二个不同 integer 常量首次出现时才创建 overflow 索引表。
			generator.constants.integers = make(map[int64]int)
		}
		generator.constants.integers[constant.Integer] = index
	case bytecode.ConstantNumber:
		if generator.constants.numbers == nil {
			// float number 常量首次出现时才创建索引表。
			generator.constants.numbers = make(map[float64]int)
		}
		generator.constants.numbers[constant.Number] = index
	case bytecode.ConstantString:
		if generator.constants.strings == nil {
			// string 常量首次出现时才创建索引表。
			generator.constants.strings = make(map[string]int)
		}
		generator.constants.strings[constant.String] = index
	default:
		// 未知常量类型已追加到 Proto，但不建立复用索引。
		return
	}
}

// addNumberConstant 按 lexer 数字分类添加常量。
//
// 十进制/十六进制整数生成 integer 常量，浮点生成 number 常量。
func (generator *generator) addNumberConstant(number lexer.NumberLiteral) int {
	switch number.Kind {
	case lexer.NumberDecimalInteger, lexer.NumberHexInteger:
		// integer 字面量必须保留精确整数语义。
		return generator.addConstant(bytecode.IntegerConstant(number.Integer))
	case lexer.NumberDecimalFloat, lexer.NumberHexFloat:
		// float 字面量使用 Lua number 常量。
		return generator.addConstant(bytecode.NumberConstant(number.Number))
	default:
		// 未知分类按源码文本保底为 string 不合适，因此返回 nil 常量触发后续测试暴露。
		return generator.addConstant(bytecode.NilConstant())
	}
}

// rkOperandForConstantIndex 返回常量索引对应的 RK 操作数。
//
// index 不超过 RK 常量范围时直接编码为常量操作数；超过 RK 范围时分配一个临时寄存器并
// 生成 LOADK/LOADKX，把常量装入寄存器后返回寄存器操作数。返回的 register 小于 0 表示
// 未分配临时寄存器；调用方必须在使用完指令后按栈顺序释放非负 register。
func (generator *generator) rkOperandForConstantIndex(index int) (operand int, register int, err error) {
	if index <= bytecode.MaxIndexRK {
		// 小常量保留 Lua 5.3 RK 内联编码，避免额外 LOADK 指令。
		return bytecode.RKAsK(index), -1, nil
	}
	register = generator.allocateRegister()
	if err := generator.emitLoadConstantIndexTo(index, register); err != nil {
		// 常量无法写入临时寄存器时立即释放，避免污染后续寄存器分配。
		generator.releaseRegister(register)
		return 0, -1, err
	}

	// RK 字段未设置 BitRK 时表示寄存器下标。
	return register, register, nil
}

// emitLoadConstantIndexTo 生成把常量索引写入目标寄存器的指令。
//
// Lua 5.3 对 Bx 范围内常量使用 LOADK；超过 Bx 但仍在 Ax 范围内时使用 LOADKX + EXTRAARG。
// 常量索引超过 Ax 字段时返回错误，调用方应终止 codegen。
func (generator *generator) emitLoadConstantIndexTo(index int, targetRegister int) error {
	if index <= bytecode.MaxArgBx {
		// Bx 可容纳时使用单条 LOADK。
		generator.emitABx(bytecode.OpLoadK, targetRegister, index)
		return nil
	}
	if index <= bytecode.MaxArgAx {
		// 大常量索引用 LOADKX 等待下一条 EXTRAARG 提供 Ax。
		generator.emitABx(bytecode.OpLoadKX, targetRegister, 0)
		generator.emitAx(bytecode.OpExtraArg, index)
		return nil
	}

	// Ax 已经是 Lua 5.3 可编码的最大常量索引。
	return fmt.Errorf("codegen constant index too large: %d", index)
}

// emitABC 追加 iABC 指令。
//
// 参数含义由 opcode 决定；调用方负责传入符合 Lua 5.3 字段宽度的值。
func (generator *generator) emitABC(opCode bytecode.OpCode, a int, b int, c int) int {
	// 使用 bytecode 统一指令构造器，避免本包重复维护位布局。
	return generator.addInstruction(bytecode.CreateABC(opCode, a, b, c))
}

// emitABx 追加 iABx 指令。
//
// 主要用于 LOADK 和 CLOSURE 等使用 Bx 字段的 opcode。
func (generator *generator) emitABx(opCode bytecode.OpCode, a int, bx int) int {
	// 使用 bytecode 统一指令构造器，避免字段编码分叉。
	return generator.addInstruction(bytecode.CreateABx(opCode, a, bx))
}

// emitAx 追加 iAx 指令。
//
// 当前用于 LOADKX 和 SETLIST 等需要 EXTRAARG 扩展参数的场景。
func (generator *generator) emitAx(opCode bytecode.OpCode, ax int) int {
	// 使用 bytecode 统一指令构造器，保持 Ax 位布局与 VM 解码一致。
	return generator.addInstruction(bytecode.CreateAx(opCode, ax))
}

// emitAsBx 追加 iAsBx 指令。
//
// 当前用于短路逻辑生成的小范围 JMP。
func (generator *generator) emitAsBx(opCode bytecode.OpCode, a int, sbx int) int {
	// 使用 bytecode excess-K 编码构造有符号偏移指令。
	return generator.addInstruction(bytecode.CreateAsBx(opCode, a, sbx))
}

// addInstruction 追加指令并同步写入当前源码行号。
//
// 指令和 LineInfo 必须保持一一对应；currentLine 为 0 时写入 0，debug 层会把它视为未知行。
func (generator *generator) addInstruction(instruction bytecode.Instruction) int {
	pc := generator.proto.AddInstruction(instruction)
	generator.proto.LineInfo = append(generator.proto.LineInfo, generator.currentLine)
	return pc
}

// binaryOpCode 返回普通二元操作符对应的 Lua opcode。
//
// ok=false 表示当前 codegen 阶段尚未支持该操作符。
func binaryOpCode(operator string) (bytecode.OpCode, bool) {
	switch operator {
	case "+":
		// 加法映射到 OP_ADD。
		return bytecode.OpAdd, true
	case "-":
		// 减法映射到 OP_SUB。
		return bytecode.OpSub, true
	case "*":
		// 乘法映射到 OP_MUL。
		return bytecode.OpMul, true
	case "%":
		// 取模映射到 OP_MOD。
		return bytecode.OpMod, true
	case "^":
		// 幂运算映射到 OP_POW。
		return bytecode.OpPow, true
	case "/":
		// 浮点除法映射到 OP_DIV。
		return bytecode.OpDiv, true
	case "//":
		// 整除映射到 OP_IDIV。
		return bytecode.OpIDiv, true
	case "&":
		// 位与映射到 OP_BAND。
		return bytecode.OpBAnd, true
	case "|":
		// 位或映射到 OP_BOR。
		return bytecode.OpBOr, true
	case "~":
		// 位异或映射到 OP_BXOR。
		return bytecode.OpBXor, true
	case "<<":
		// 左移映射到 OP_SHL。
		return bytecode.OpShl, true
	case ">>":
		// 右移映射到 OP_SHR。
		return bytecode.OpShr, true
	case "..":
		// 连接当前按普通二元形态生成 OP_CONCAT。
		return bytecode.OpConcat, true
	default:
		return 0, false
	}
}

// isComparisonOperator 判断操作符是否属于 Lua 5.3 比较表达式。
//
// 返回 true 时 codegen 应使用比较测试指令，而不是普通二元算术指令。
func isComparisonOperator(operator string) bool {
	switch operator {
	case "==", "~=", "<", "<=", ">", ">=":
		// 六个 Lua 比较操作符都产生 boolean 表达式结果。
		return true
	default:
		// 其他操作符交由算术、位运算、连接或短路逻辑处理。
		return false
	}
}

// comparisonOpCode 返回比较操作符对应的测试 opcode。
//
// expectedTrue 是 opcode A 字段；swapOperands 为 true 表示通过交换左右操作数复用 LT/LE。
func comparisonOpCode(operator string) (opCode bytecode.OpCode, expectedTrue int, swapOperands bool) {
	switch operator {
	case "==":
		// EQ A=1 表示比较为 false 时跳过下一条，配合 LOADBOOL 生成 true/false。
		return bytecode.OpEq, 1, false
	case "~=":
		// EQ A=0 表示比较为 true 时跳过下一条，从而得到“不等于”的布尔结果。
		return bytecode.OpEq, 0, false
	case "<":
		// LT A=1 直接表达小于为真。
		return bytecode.OpLt, 1, false
	case "<=":
		// LE A=1 直接表达小于等于为真。
		return bytecode.OpLe, 1, false
	case ">":
		// 大于通过交换操作数复用 LT。
		return bytecode.OpLt, 1, true
	case ">=":
		// 大于等于通过交换操作数复用 LE。
		return bytecode.OpLe, 1, true
	default:
		// 调用方应先通过 isComparisonOperator 过滤，默认值只作为防御兜底。
		return 0, 0, false
	}
}

// comparisonConditionOpCode 返回比较条件直译为“真时跳过下一条 JMP”的 opcode 形态。
//
// Lua 5.3 比较指令语义是 `(comparison ~= A) then pc++`；while 需要条件为 true 时跳过
// false JMP，因此 A 字段与布尔物化路径不同。
func comparisonConditionOpCode(operator string) (opCode bytecode.OpCode, skipOnTrueA int, swapOperands bool) {
	switch operator {
	case "==":
		// 相等为真时需要跳过 false JMP，因此 A=0。
		return bytecode.OpEq, 0, false
	case "~=":
		// 不等于为真等价于 EQ 结果为 false，此时 A=1 才会跳过 false JMP。
		return bytecode.OpEq, 1, false
	case "<":
		// 小于为真时 A=0 才会跳过 false JMP。
		return bytecode.OpLt, 0, false
	case "<=":
		// 小于等于为真时 A=0 才会跳过 false JMP。
		return bytecode.OpLe, 0, false
	case ">":
		// 大于通过交换操作数复用 LT。
		return bytecode.OpLt, 0, true
	case ">=":
		// 大于等于通过交换操作数复用 LE。
		return bytecode.OpLe, 0, true
	default:
		// 调用方应先通过 isComparisonOperator 过滤，默认值只作为防御兜底。
		return 0, 0, false
	}
}
