package codegen

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ZingYao/go-lua-vm/bytecode"
	"github.com/ZingYao/go-lua-vm/compiler/parser"
	"github.com/ZingYao/go-lua-vm/extensions"
)

// TestCompileChunkDeduplicatesConstantsAndRegisters 验证常量去重、寄存器分配和临时寄存器释放。
//
// 输入包含重复常量和二元表达式，用于覆盖当前 codegen 最小闭环。
func TestCompileChunkDeduplicatesConstantsAndRegisters(t *testing.T) {
	chunk := parseChunkForCodegenTest(t, "local a = 1 local b = 1 local c = a + b return c")

	proto, err := CompileChunk(chunk, "test")
	if err != nil {
		// 合法最小 Lua 子集不应编译失败。
		t.Fatalf("compile chunk failed: %v", err)
	}
	if len(proto.Constants) != 1 {
		// 两个字面量 1 必须复用同一个常量表索引。
		t.Fatalf("unexpected constants=%+v", proto.Constants)
	}
	if proto.Constants[0].Kind != bytecode.ConstantInteger || proto.Constants[0].Integer != 1 {
		// 数字 1 应按 Lua 5.3 integer 常量保存。
		t.Fatalf("unexpected constant=%+v", proto.Constants[0])
	}
	if proto.MaxStackSize != 3 {
		// 三个 local 已覆盖 a+b 的 RK 操作数，不再需要额外二元表达式临时寄存器。
		t.Fatalf("unexpected max stack=%d", proto.MaxStackSize)
	}
	if proto.Code[0].OpCode() != bytecode.OpLoadK || proto.Code[1].OpCode() != bytecode.OpLoadK {
		// local a/b 初始化应通过 LOADK 加载同一个常量。
		t.Fatalf("expected first two instructions to be LOADK")
	}
	if proto.Code[0].Bx() != proto.Code[1].Bx() {
		// 重复常量的 LOADK Bx 必须相同。
		t.Fatalf("constant index should be deduplicated: %d != %d", proto.Code[0].Bx(), proto.Code[1].Bx())
	}
	if !containsOpCode(proto, bytecode.OpAdd) {
		// a + b 必须生成 ADD 指令。
		t.Fatalf("expected ADD instruction")
	}
	if proto.Code[len(proto.Code)-1].OpCode() != bytecode.OpReturn {
		// 显式 return 应生成 RETURN 作为最后一条指令。
		t.Fatalf("expected final RETURN")
	}
}

// TestCompileChunkKeepsDistinctFloatConstants 验证浮点常量按 Lua 5.3 原值去重。
//
// Lua 5.3 的 addk 对 number 使用 TValue 原值作为 key；相近但不相等的浮点数不能被格式化文本误合并。
func TestCompileChunkKeepsDistinctFloatConstants(t *testing.T) {
	chunk := parseChunkForCodegenTest(t, "local a = 1.0000001 local b = 1.0000002 return a, b")

	proto, err := CompileChunk(chunk, "test")
	if err != nil {
		// 合法浮点字面量不应编译失败。
		t.Fatalf("compile chunk failed: %v", err)
	}
	if len(proto.Constants) != 2 {
		// 两个浮点原值不同，必须保留两个常量表槽位。
		t.Fatalf("unexpected constants=%+v", proto.Constants)
	}
	if proto.Constants[0].Kind != bytecode.ConstantNumber || proto.Constants[1].Kind != bytecode.ConstantNumber {
		// 两个字面量都含小数部分，应按 Lua number 常量保存。
		t.Fatalf("unexpected constant kinds=%+v", proto.Constants)
	}
	if proto.Constants[0].Number == proto.Constants[1].Number {
		// 测试夹具必须覆盖两个不同的浮点值。
		t.Fatalf("fixture constants should be distinct: %+v", proto.Constants)
	}
}

// TestCompileChunkReleasesFixedCallArguments 验证固定单返回 CALL 后回收参数槽。
//
// Lua 5.3 官方 codegen 会在 `x = x + f(i) + g(i)` 中让第二个调用复用前一个调用的参数槽；
// 回收时仍必须保留 numeric for 的控制变量寄存器，避免后续临时值覆盖循环状态。
func TestCompileChunkReleasesFixedCallArguments(t *testing.T) {
	chunk := parseChunkForCodegenTest(t, `
local function f(i) return i end
local function g(i) return i end
local x = 0
for i = 1, 10 do
  x = x + f(i) + g(i)
end
return x
`)

	proto, err := CompileChunk(chunk, "call-register-reuse")
	if err != nil {
		// 合法函数调用链不应编译失败。
		t.Fatalf("compile call-register-reuse failed: %v", err)
	}
	if proto.MaxStackSize != 10 {
		// 官方 Lua 5.3 该形态使用 10 个栈槽；多一个槽说明 CALL 实参水位没有及时回收。
		t.Fatalf("unexpected max stack=%d", proto.MaxStackSize)
	}
	if !hasMove(proto, 8, 1) {
		// 第二个调用应从 R8 放置函数 g，而不是因为前一调用残留参数水位推到 R9。
		t.Fatalf("expected second call to reuse register 8")
	}
}

func TestCompileChunkReleasesCallsInLargeTableConstructor(t *testing.T) {
	var source strings.Builder
	source.WriteString("local function value(v) return v end\nlocal mapping = {\n")
	for index := 0; index < 256; index++ {
		fmt.Fprintf(&source, "[value(%d)] = value(%d),\n", index, 255-index)
	}
	source.WriteString("}\nreturn mapping\n")

	chunk := parseChunkForCodegenTest(t, source.String())
	proto, err := CompileChunk(chunk, "large-table-calls")
	if err != nil {
		t.Fatalf("compile large table failed: %v", err)
	}
	if proto.MaxStackSize > 8 {
		t.Fatalf("table calls leaked temporary registers: max stack=%d", proto.MaxStackSize)
	}
}

// TestCompileChunkReleasesOpenArgumentCallAfterFixedResult 验证开放实参固定单返回 CALL 后回收参数槽。
//
// `string.len(string.sub(...))` 会生成外层 CALL B=0/C=2；CALL 完成后仍只有一个固定返回值，
// 参数槽必须释放给后续 math 嵌套调用复用，否则混合标准库脚本会比 Lua 5.3 多占寄存器。
func TestCompileChunkReleasesOpenArgumentCallAfterFixedResult(t *testing.T) {
	chunk := parseChunkForCodegenTest(t, `
local i = 1
local acc = 0
local text = "abcdef0123456789"
while i <= 10 do
  local s, e = string.find(text, "cd", 1, true)
  acc = acc + (s or 0) + (e or 0)
  acc = acc + string.len(string.sub(text, 2, 8))
  acc = acc + math.floor(math.sin(i) * 1000)
  i = i + 1
end
return acc
`)

	proto, err := CompileChunk(chunk, "open-arg-fixed-result")
	if err != nil {
		// 合法标准库混合脚本不应编译失败。
		t.Fatalf("compile open-arg-fixed-result failed: %v", err)
	}
	if proto.MaxStackSize != 10 {
		// 官方 Lua 5.3 该形态使用 10 个栈槽；更高说明 B=0 外层 CALL 后没有释放参数槽。
		t.Fatalf("unexpected max stack=%d", proto.MaxStackSize)
	}
	if !hasCall(proto, 5, 0, 2) {
		// string.len(string.sub(...)) 应保留 CALL B=0/C=2 语义，只在 CALL 后回收水位。
		t.Fatalf("expected outer string.len CALL B=0 C=2")
	}
	if !hasGetTabUpAtOrAfter(proto, 5, 30) {
		// 后续 math.floor 应复用 R5，而不是被上一行开放实参调用推到更高寄存器。
		t.Fatalf("expected later math call to reuse R5")
	}
}

// TestCompileChunkShortCircuit 验证 and/or 短路表达式生成 TESTSET/JMP 形态。
//
// 当前阶段只验证指令结构，运行时真假语义由 VM 指令测试覆盖。
func TestCompileChunkShortCircuit(t *testing.T) {
	chunk := parseChunkForCodegenTest(t, "local a = true local b = false local c = a and b local d = a or b local e = a and (b or a)")

	proto, err := CompileChunk(chunk, "short")
	if err != nil {
		// 合法短路表达式不应编译失败。
		t.Fatalf("compile short circuit failed: %v", err)
	}
	testCount := countOpCode(proto, bytecode.OpTest)
	testSetCount := countOpCode(proto, bytecode.OpTestSet)
	jumpCount := countOpCode(proto, bytecode.OpJmp)
	if testCount != 0 || testSetCount != 4 || jumpCount != 4 {
		// local 左操作数短路节点应使用 TESTSET/JMP，避免 MOVE+TEST 额外指令。
		t.Fatalf("unexpected short circuit shape: test=%d testset=%d jump=%d", testCount, testSetCount, jumpCount)
	}
	if !hasTestSetWithC(proto, 0) || !hasTestSetWithC(proto, 1) {
		// and 使用 C=0，or 使用 C=1，二者都应出现。
		t.Fatalf("expected TESTSET instructions for both and/or")
	}
	if !hasJumpBeyondOne(proto) {
		// 嵌套短路右侧包含多条指令，至少一个 jump 应由回填得到大于 1 的偏移。
		t.Fatalf("expected patched jump beyond one instruction")
	}
}

// TestCompileChunkShortCircuitLocalNameUsesTestSet 验证 local 左操作数短路对齐 Lua 5.3 TESTSET。
func TestCompileChunkShortCircuitLocalNameUsesTestSet(t *testing.T) {
	chunk := parseChunkForCodegenTest(t, "local a = true local b = false local c = a or 0 local d = b and 1 return c, d")

	proto, err := CompileChunk(chunk, "short-testset")
	if err != nil {
		// 合法短路表达式不应编译失败。
		t.Fatalf("compile short-testset failed: %v", err)
	}
	if countOpCode(proto, bytecode.OpTestSet) != 2 || countOpCode(proto, bytecode.OpTest) != 0 {
		// 两个 local 左操作数短路表达式应各生成一条 TESTSET，不再生成 MOVE+TEST。
		t.Fatalf("unexpected testset shape: testset=%d test=%d", countOpCode(proto, bytecode.OpTestSet), countOpCode(proto, bytecode.OpTest))
	}
	if hasMove(proto, 2, 0) || hasMove(proto, 3, 1) {
		// 官方 Lua 5.3 该形态不需要先 MOVE 左侧 local 到目标寄存器。
		t.Fatalf("unexpected MOVE before short-circuit TESTSET")
	}
	if len(proto.Code) != 11 {
		// 本项目显式 return 后不补不可达默认 RETURN；主体指令应比官方 Lua 5.3 少一条。
		t.Fatalf("unexpected instruction count=%d code=%v", len(proto.Code), proto.Code)
	}
}

// TestCompileChunkIfStatement 验证 if/elseif/else 语句生成 TEST/JMP 分支形态。
//
// 当前测试只校验 codegen 支持和基础跳转结构，运行时分支执行由 VM 集成路径继续覆盖。
func TestCompileChunkIfStatement(t *testing.T) {
	chunk := parseChunkForCodegenTest(t, "local a = true local b = false local r = 0 if a then r = 1 elseif b then r = 2 else r = 3 end return r")

	proto, err := CompileChunk(chunk, "if")
	if err != nil {
		// 合法 if/elseif/else 语句不应编译失败。
		t.Fatalf("compile if statement failed: %v", err)
	}
	if countOpCode(proto, bytecode.OpTest) != 2 {
		// if 和 elseif 各自应生成一个 TEST。
		t.Fatalf("expected two TEST instructions")
	}
	if countOpCode(proto, bytecode.OpJmp) < 4 {
		// 每个条件失败路径和分支结束路径都需要 JMP。
		t.Fatalf("expected branch JMP instructions")
	}
	if !hasTestWithC(proto, 0) {
		// 条件分支使用 TEST C=0 搭配 JMP 表示 false/nil 走下一分支。
		t.Fatalf("expected TEST C=0 for if statement")
	}
}

// TestCompileChunkIfLocalConditionReusesRegister 验证 if local 条件直接复用寄存器。
//
// Lua 5.3 官方 codegen 对 `if a then` 直接 TEST local 寄存器；不需要先 MOVE 到临时槽。
func TestCompileChunkIfLocalConditionReusesRegister(t *testing.T) {
	chunk := parseChunkForCodegenTest(t, "local a = true local out = 0 if a then out = 1 end return out")

	proto, err := CompileChunk(chunk, "if-local-condition")
	if err != nil {
		// 合法 if local 条件不应编译失败。
		t.Fatalf("compile if-local-condition failed: %v", err)
	}
	testInstruction, ok := firstInstruction(proto, bytecode.OpTest)
	if !ok {
		// if 条件必须生成 TEST 指令。
		t.Fatalf("expected TEST instruction")
	}
	if testInstruction.A() != 0 {
		// local a 位于 R0，TEST 应直接读取它而不是临时寄存器。
		t.Fatalf("TEST should use local register R0, got R%d", testInstruction.A())
	}
	if hasMove(proto, 2, 0) {
		// 旧形态会把 a MOVE 到第一个临时寄存器 R2 后再 TEST。
		t.Fatalf("unexpected MOVE from local condition into temporary register")
	}
	if proto.MaxStackSize != 2 {
		// 条件和 then 内简单赋值都不需要额外临时寄存器，应与官方 Lua 5.3 同为两个栈槽。
		t.Fatalf("unexpected max stack=%d", proto.MaxStackSize)
	}
	loadOne := false
	for _, instruction := range proto.Code {
		if instruction.OpCode() == bytecode.OpLoadK && instruction.A() == 1 {
			// then 分支中的 out = 1 应直接加载到 out 的寄存器 R1。
			loadOne = true
		}
	}
	if !loadOne {
		// 没有直接 LOADK 到 R1 说明仍走了临时寄存器再 MOVE 的通用赋值路径。
		t.Fatalf("expected direct LOADK into local out")
	}
	if len(proto.Code) != 6 {
		// 单分支 if 后没有后续分支，then 结束后应自然落到 return，不需要零距离 JMP。
		t.Fatalf("unexpected instruction count=%d code=%v", len(proto.Code), proto.Code)
	}
}

// TestCompileChunkIfComparisonConditionDirect 验证 if 比较条件直译为测试跳转。
//
// Lua 5.3 官方 codegen 对安全有序比较条件直接生成 LT/LE 加失败 JMP；不需要先物化 boolean。
func TestCompileChunkIfComparisonConditionDirect(t *testing.T) {
	chunk := parseChunkForCodegenTest(t, "local i = 1 local out = 0 if i < 3 then out = i end return out")

	proto, err := CompileChunk(chunk, "if-comparison-direct")
	if err != nil {
		// 合法 if 比较条件不应编译失败。
		t.Fatalf("compile if comparison failed: %v", err)
	}
	if countOpCode(proto, bytecode.OpLt) != 1 {
		// i < 3 应直译为一条 LT 测试指令。
		t.Fatalf("expected one LT instruction")
	}
	if countOpCode(proto, bytecode.OpTest) != 0 || countOpCode(proto, bytecode.OpLoadBool) != 0 {
		// if 比较条件不应先写入 boolean 临时值。
		t.Fatalf("unexpected boolean materialization: test=%d loadbool=%d", countOpCode(proto, bytecode.OpTest), countOpCode(proto, bytecode.OpLoadBool))
	}
	if countOpCode(proto, bytecode.OpJmp) < 1 {
		// 条件失败路径必须保留一条跳过 then 分支的 JMP。
		t.Fatalf("expected false branch JMP instruction")
	}
}

// TestCompileChunkWhileStatement 验证 while 语句生成 TEST 和回跳 JMP。
//
// 当前测试覆盖官方 main.lua 早期使用的条件循环形态，运行时跳转语义由 VM JMP/TEST 测试承担。
func TestCompileChunkWhileStatement(t *testing.T) {
	chunk := parseChunkForCodegenTest(t, "local i = 1 while i < 3 do i = i + 1 end return i")

	proto, err := CompileChunk(chunk, "while")
	if err != nil {
		// 合法 while 语句不应编译失败。
		t.Fatalf("compile while statement failed: %v", err)
	}
	if countOpCode(proto, bytecode.OpLt) != 1 {
		// 安全比较条件应直译为 Lua 5.3 的 LT 测试指令。
		t.Fatalf("expected one LT instruction")
	}
	if countOpCode(proto, bytecode.OpTest) != 0 {
		// while 比较条件不应先物化为 boolean 再 TEST。
		t.Fatalf("unexpected TEST instruction count=%d", countOpCode(proto, bytecode.OpTest))
	}
	if countOpCode(proto, bytecode.OpLoadBool) != 0 {
		// while 比较条件不应生成 LOADBOOL true/false 序列。
		t.Fatalf("unexpected LOADBOOL instruction count=%d", countOpCode(proto, bytecode.OpLoadBool))
	}
	if countOpCode(proto, bytecode.OpJmp) < 2 {
		// while 至少需要一个条件失败跳出 JMP 和一个循环尾部回跳 JMP。
		t.Fatalf("expected while JMP instructions")
	}
	if !hasBackwardJump(proto) {
		// 循环尾部必须存在负向跳转回条件起点。
		t.Fatalf("expected backward JMP for while loop")
	}

	breakChunk := parseChunkForCodegenTest(t, "while true do break end return 1")
	breakProto, err := CompileChunk(breakChunk, "while-break")
	if err != nil {
		// 循环内 break 应能编译为待回填跳转。
		t.Fatalf("compile while break failed: %v", err)
	}
	if !hasForwardJump(breakProto) {
		// break 必须生成跳出循环的正向 JMP。
		t.Fatalf("expected forward JMP for break")
	}
}

// TestCompileChunkRepeatComparisonConditionDirect 验证 repeat-until 比较条件直译为测试回跳。
//
// repeat 的 until 条件可以访问循环体 local；直译比较条件时仍必须避免 LOADBOOL/TEST 临时值。
func TestCompileChunkRepeatComparisonConditionDirect(t *testing.T) {
	chunk := parseChunkForCodegenTest(t, "local i = 0 repeat i = i + 1 until i >= 10 return i")

	proto, err := CompileChunk(chunk, "repeat-comparison-direct")
	if err != nil {
		// 合法 repeat 比较条件不应编译失败。
		t.Fatalf("compile repeat comparison failed: %v", err)
	}
	if countOpCode(proto, bytecode.OpLe) != 1 {
		// i >= 10 应交换操作数后复用 LE 测试指令。
		t.Fatalf("expected one LE instruction")
	}
	if countOpCode(proto, bytecode.OpTest) != 0 || countOpCode(proto, bytecode.OpLoadBool) != 0 {
		// repeat 比较条件不应先写入 boolean 临时值。
		t.Fatalf("unexpected boolean materialization: test=%d loadbool=%d", countOpCode(proto, bytecode.OpTest), countOpCode(proto, bytecode.OpLoadBool))
	}
	if !hasBackwardJump(proto) {
		// until 条件失败时仍必须回跳到 repeat 体起点。
		t.Fatalf("expected backward JMP for repeat")
	}
}

// TestCompileChunkContinueAndSwitch 验证扩展 continue/switch 生成现有跳转与比较指令。
//
// continue 应降级为 JMP；switch 多值 case 应生成 EQ/JMP 匹配检查，不新增 VM opcode。
func TestCompileChunkContinueAndSwitch(t *testing.T) {
	if !extensions.Compiled().Has(extensions.SyntaxContinue | extensions.SyntaxSwitch) {
		// 当前构建未编译控制流扩展时跳过正向 codegen 用例。
		t.Skip("control-flow syntax extensions are not compiled")
	}
	chunk := parseChunkForCodegenTest(t, "local i = 0 local out = 0 while i < 5 do i = i + 1 if i == 2 then continue end switch i do case 1, 3 out = out + i default out = out + 10 end end return out")

	proto, err := CompileChunk(chunk, "continue-switch")
	if err != nil {
		// 合法 continue/switch 语句不应编译失败。
		t.Fatalf("compile continue/switch failed: %v", err)
	}
	if countOpCode(proto, bytecode.OpEq) < 3 {
		// if 条件和 switch 的两个 case 值都应生成 EQ 检查。
		t.Fatalf("expected EQ instructions for if and switch")
	}
	if countOpCode(proto, bytecode.OpJmp) < 5 {
		// while、continue、if 和 switch 分支都依赖 JMP 回填。
		t.Fatalf("expected JMP instructions for continue/switch")
	}
	if !hasBackwardJump(proto) {
		// while 循环尾部仍必须有回跳。
		t.Fatalf("expected backward JMP for continue loop")
	}
}

// TestCompileChunkRejectsTooLongControlStructure 验证超长控制结构返回编译错误。
//
// Lua 5.3 官方 constructs.lua 会用巨大 while 体检查 `too long` 错误；sBx 超出编码范围时
// codegen 必须拒绝，而不是截断跳转偏移生成错误字节码。
func TestCompileChunkRejectsTooLongControlStructure(t *testing.T) {
	source := "while true do " + strings.Repeat("a = a + 1\n", 1<<18) + "end"
	chunk := parseChunkForCodegenTest(t, source)

	_, err := CompileChunk(chunk, "too-long.lua")
	if err == nil || !strings.Contains(err.Error(), "too long") {
		// 超长 while 必须以包含 too long 的编译错误结束。
		t.Fatalf("CompileChunk too-long error = %v, want contains too long", err)
	}
}

// TestCompileChunkGotoLabel 验证 goto 和 label 会生成并回填函数内 JMP。
//
// Lua 5.3 的 label 不产生运行时效果；goto 必须跳到对应 label 的下一条真实指令。
func TestCompileChunkGotoLabel(t *testing.T) {
	chunk := parseChunkForCodegenTest(t, "goto ok value = 2 ::ok:: return 1")

	proto, err := CompileChunk(chunk, "goto")
	if err != nil {
		// parser 已完成 goto 合法性校验，codegen 不应拒绝合法前向 goto。
		t.Fatalf("compile goto failed: %v", err)
	}
	if !hasForwardJump(proto) {
		// 前向 goto 必须生成正向 JMP，跳过 label 前的赋值语句。
		t.Fatalf("expected forward JMP for goto")
	}
}

// TestCompileChunkGotoFromIfBranchToOuterConsecutiveLabels 验证 if 分支内 goto 可跳到外层连续 label。
//
// 官方 code.lua 的 if-goto optimizations 小节会在 if/elseif/else 子 block 内跳到外层函数
// block 的 `::l1:: ::l2:: ::l3::` 连续 label；codegen 必须沿 parser scope 父链解析目标。
func TestCompileChunkGotoFromIfBranchToOuterConsecutiveLabels(t *testing.T) {
	chunk := parseChunkForCodegenTest(t, `
return function (a, b, c, d, e)
  if a == b then goto l1
  elseif a == c then goto l2
  elseif a == d then goto l2
  else
    if a == e then goto l3 else goto l3 end
  end
  ::l1:: ::l2:: ::l3:: ::l4::
end
`)

	proto, err := CompileChunk(chunk, "if-goto-labels")
	if err != nil {
		// 分支内 goto 跳到外层连续 label 是 Lua 5.3 合法写法，不能报 undefined label。
		t.Fatalf("compile if branch goto labels failed: %v", err)
	}
	if len(proto.Protos) != 1 || countOpCode(proto.Protos[0], bytecode.OpJmp) < 4 {
		// 子函数中多个 goto 分支都应生成并回填 JMP。
		t.Fatalf("expected child proto with patched goto jumps")
	}
}

// TestCompileChunkComparisonExpression 验证比较表达式生成测试指令和布尔结果。
//
// Lua 比较 opcode 不直接写 boolean，codegen 必须用 EQ/LT/LE 搭配 LOADBOOL 合成表达式值。
func TestCompileChunkComparisonExpression(t *testing.T) {
	chunk := parseChunkForCodegenTest(t, "local a = 1 local b = 2 local c = a ~= b local d = b >= a return c")

	proto, err := CompileChunk(chunk, "compare")
	if err != nil {
		// 合法比较表达式不应编译失败。
		t.Fatalf("compile comparison failed: %v", err)
	}
	if !containsOpCode(proto, bytecode.OpEq) || !containsOpCode(proto, bytecode.OpLe) {
		// ~= 使用 EQ A=0，>= 通过交换操作数复用 LE。
		t.Fatalf("expected EQ and LE instructions")
	}
	if countOpCode(proto, bytecode.OpLoadBool) != 4 {
		// 两个比较表达式各需要 true/false 两条 LOADBOOL。
		t.Fatalf("unexpected LOADBOOL count=%d", countOpCode(proto, bytecode.OpLoadBool))
	}
}

// TestCompileChunkGlobalNamesUseEnv 验证未知名称通过 `_ENV` 读写。
//
// Lua 5.3 默认把未声明名称解析为 `_ENV[name]`，顶层 Proto 必须登记 `_ENV` upvalue。
func TestCompileChunkGlobalNamesUseEnv(t *testing.T) {
	chunk := parseChunkForCodegenTest(t, "answer = _VERSION return answer")

	proto, err := CompileChunk(chunk, "globals")
	if err != nil {
		// 全局名称读写应能正常编译为 `_ENV` 访问。
		t.Fatalf("compile globals failed: %v", err)
	}
	if len(proto.Upvalues) != 1 || proto.Upvalues[0].Name != "_ENV" {
		// 顶层全局访问必须声明一个 `_ENV` upvalue，供宿主 State 注入 globals。
		t.Fatalf("unexpected top-level upvalues=%+v", proto.Upvalues)
	}
	if !containsOpCode(proto, bytecode.OpGetTabUp) || !containsOpCode(proto, bytecode.OpSetTabUp) {
		// 读取 `_VERSION` 与写入 answer 分别需要 GETTABUP 和 SETTABUP。
		t.Fatalf("expected GETTABUP and SETTABUP instructions")
	}
	if !hasStringConstant(proto, "_VERSION") || !hasStringConstant(proto, "answer") {
		// 全局 key 必须以字符串常量形式进入当前 Proto 常量池。
		t.Fatalf("missing global key constants=%+v", proto.Constants)
	}
}

// TestCompileChunkGlobalSafeAssignmentUsesSourceRegister 验证安全全局赋值直接使用 RHS 寄存器。
//
// Lua 5.3 官方 codegen 对 `global = local` 会直接把 local 寄存器作为 SETTABUP value 操作数。
func TestCompileChunkGlobalSafeAssignmentUsesSourceRegister(t *testing.T) {
	chunk := parseChunkForCodegenTest(t, "local i = 1 answer = i return answer")

	proto, err := CompileChunk(chunk, "global-safe-assign")
	if err != nil {
		// 合法全局赋值不应编译失败。
		t.Fatalf("compile global safe assignment failed: %v", err)
	}
	foundDirectSet := false
	for _, instruction := range proto.Code {
		if instruction.OpCode() == bytecode.OpSetTabUp && instruction.C() == 0 {
			// local i 位于 R0，SETTABUP 的 value 操作数应直接使用 R0。
			foundDirectSet = true
		}
	}
	if !foundDirectSet {
		// 未找到直接 SETTABUP 说明仍走了临时 MOVE 路径。
		t.Fatalf("expected SETTABUP to use source register R0 directly")
	}
	if hasMove(proto, 1, 0) {
		// 旧形态会先 MOVE R1 R0，再用 R1 写入全局表。
		t.Fatalf("unexpected MOVE before global assignment")
	}
}

// TestCompileChunkNestedGlobalCapturesEnv 验证嵌套函数读取全局时捕获 `_ENV`。
//
// 子函数中的全局名不能直接读取顶层 globals，必须通过父 closure 的 `_ENV` upvalue 间接捕获。
func TestCompileChunkNestedGlobalCapturesEnv(t *testing.T) {
	chunk := parseChunkForCodegenTest(t, "local function f() return _VERSION end")

	proto, err := CompileChunk(chunk, "nested-global")
	if err != nil {
		// 嵌套函数读取全局应可编译。
		t.Fatalf("compile nested global failed: %v", err)
	}
	if len(proto.Upvalues) != 1 || proto.Upvalues[0].Name != "_ENV" {
		// 父 Proto 需要声明 `_ENV`，供运行期创建顶层 closure 时绑定。
		t.Fatalf("unexpected parent upvalues=%+v", proto.Upvalues)
	}
	child := proto.Protos[0]
	if len(child.Upvalues) != 1 || child.Upvalues[0].Name != "_ENV" || child.Upvalues[0].InStack || child.Upvalues[0].Index != 0 {
		// 子 Proto 应捕获父 closure 的第 0 个 upvalue，而不是捕获寄存器。
		t.Fatalf("unexpected child upvalues=%+v", child.Upvalues)
	}
	if !containsOpCode(child, bytecode.OpGetTabUp) {
		// 子函数读取 `_VERSION` 必须生成 GETTABUP。
		t.Fatalf("expected child GETTABUP")
	}
}

// TestCompileChunkLocalEnvReceivesGlobalAssignment 验证 local `_ENV` 会接收未声明名称写入。
//
// Lua 5.3 的 `function xuxu()` 等价于 `_ENV.xuxu = function()`；当前作用域声明 local `_ENV`
// 后必须生成 SETTABLE 写入该寄存器，而不是 SETTABUP 写入顶层 globals。
func TestCompileChunkLocalEnvReceivesGlobalAssignment(t *testing.T) {
	chunk := parseChunkForCodegenTest(t, "local _ENV = {} function xuxu() return 1 end")

	proto, err := CompileChunk(chunk, "local-env-assign")
	if err != nil {
		// local _ENV 下的函数声明必须可编译。
		t.Fatalf("compile local env assignment failed: %v", err)
	}
	if !containsOpCode(proto, bytecode.OpSetTable) {
		// function xuxu 应写入 local _ENV table。
		t.Fatalf("expected SETTABLE for local _ENV assignment")
	}
	if containsOpCode(proto, bytecode.OpSetTabUp) {
		// 当前作用域已有 local _ENV，不应再写顶层 _ENV upvalue。
		t.Fatalf("unexpected SETTABUP for local _ENV assignment")
	}
}

// TestCompileChunkNestedFunctionCapturesLocalEnv 验证嵌套函数捕获父级 local `_ENV`。
//
// preload loader 中常见 `local _ENV = {...}; function xuxu() ... end`，内部函数读取全局名时
// 必须捕获父级 local `_ENV` 寄存器，才能访问模块表字段。
func TestCompileChunkNestedFunctionCapturesLocalEnv(t *testing.T) {
	chunk := parseChunkForCodegenTest(t, "local _ENV = {value = 7} local function f() return value end")

	proto, err := CompileChunk(chunk, "nested-local-env")
	if err != nil {
		// 嵌套函数读取 local _ENV 字段必须可编译。
		t.Fatalf("compile nested local env failed: %v", err)
	}
	if len(proto.Protos) != 1 {
		// f 应生成一个子 Proto。
		t.Fatalf("unexpected child proto count=%d", len(proto.Protos))
	}
	child := proto.Protos[0]
	if len(child.Upvalues) != 1 || child.Upvalues[0].Name != "_ENV" || !child.Upvalues[0].InStack {
		// 子 Proto 应直接捕获父级 local _ENV 寄存器。
		t.Fatalf("unexpected child upvalues=%+v", child.Upvalues)
	}
}

// TestCompileChunkNestedFunctionDebugLines 验证子 Proto 记录函数定义范围和指令行号。
//
// debug.getinfo(function, "SL") 依赖 LineDefined、LastLineDefined 和 LineInfo；这些字段
// 必须由 parser 与 codegen 从源码位置稳定写入。
func TestCompileChunkNestedFunctionDebugLines(t *testing.T) {
	chunk := parseChunkForCodegenTest(t, "\nlocal function f()\n  local value = 1\n  value = value + 1\nend")

	proto, err := CompileChunk(chunk, "function-lines")
	if err != nil {
		// 多行 local function 样例必须可编译。
		t.Fatalf("compile function debug lines failed: %v", err)
	}
	if len(proto.Protos) != 1 {
		// f 应生成一个子 Proto。
		t.Fatalf("unexpected child proto count=%d", len(proto.Protos))
	}
	child := proto.Protos[0]
	if child.LineDefined != 2 || child.LastLineDefined != 5 {
		// 子 Proto 必须保留 function 起始行和 end 结束行。
		t.Fatalf("unexpected child line range=%d,%d", child.LineDefined, child.LastLineDefined)
	}
	if !hasLineInfo(child, 3) || !hasLineInfo(child, 4) || !hasLineInfo(child, 5) {
		// 函数体内语句和隐式关闭 RETURN 对应源码行必须进入 LineInfo。
		t.Fatalf("unexpected child line info=%v", child.LineInfo)
	}
}

// TestCompileReturnExpressionUsesExpressionLine 验证多行 return 表达式使用自身行号。
//
// Lua 5.3 的 `\z` 会吞掉换行后的空白但仍推进源码行号；官方 literals.lua 依赖同一 return
// 列表中后续函数调用的 currentline 指向换行后的表达式行。
func TestCompileReturnExpressionUsesExpressionLine(t *testing.T) {
	chunk := parseChunkForCodegenTest(t, "return 'abc\\z  \n   efg', require\"debug\".getinfo(1).currentline")

	proto, err := CompileChunk(chunk, "return-expression-lines")
	if err != nil {
		// 多行 return 表达式样例必须可编译。
		t.Fatalf("compile return expression lines failed: %v", err)
	}
	if !hasCallLineInfo(proto, 2) {
		// require/getinfo 调用位于换行后的表达式，至少一个 CALL 必须标注为第 2 行。
		t.Fatalf("expected CALL lineinfo on line 2, code=%v lineinfo=%v", proto.Code, proto.LineInfo)
	}
}

// TestCompileChunkLocalDebugInfo 验证局部变量 debug info 的 StartPC 和 EndPC。
//
// LocalVars 后续会供 debug 库读取，本轮先保证最小生命周期信息稳定写入 Proto。
func TestCompileChunkLocalDebugInfo(t *testing.T) {
	chunk := parseChunkForCodegenTest(t, "local a = 1 local b = 2 return a")

	proto, err := CompileChunk(chunk, "locals")
	if err != nil {
		// 合法 local 生命周期样例不应编译失败。
		t.Fatalf("compile locals failed: %v", err)
	}
	if len(proto.LocalVars) != 2 {
		// 两个 local 声明都必须写入调试局部变量表。
		t.Fatalf("unexpected local vars=%+v", proto.LocalVars)
	}
	if proto.LocalVars[0].Name != "a" || proto.LocalVars[1].Name != "b" {
		// LocalVars 必须保留局部变量名称。
		t.Fatalf("unexpected local names=%+v", proto.LocalVars)
	}
	if proto.LocalVars[0].EndPC != len(proto.Code) || proto.LocalVars[1].EndPC != len(proto.Code) {
		// 当前最小生命周期模型把 local 保持到函数结尾。
		t.Fatalf("unexpected local end pc=%+v code=%d", proto.LocalVars, len(proto.Code))
	}
}

// TestCompileChunkClosesSameScopeShadowedLocal 验证同作用域重名 local 会关闭旧调试生命周期。
func TestCompileChunkClosesSameScopeShadowedLocal(t *testing.T) {
	chunk := parseChunkForCodegenTest(t, "local a = 1 local b = 2 local a = 3 return a + b")

	proto, err := CompileChunk(chunk, "shadow")
	if err != nil {
		// 合法同名 local 遮蔽样例不应编译失败。
		t.Fatalf("compile shadowed local failed: %v", err)
	}
	if len(proto.LocalVars) != 3 {
		// 三个声明都必须保留独立 LocVar，便于 dump/load 后按生命周期还原寄存器。
		t.Fatalf("unexpected local vars=%+v", proto.LocalVars)
	}
	if proto.LocalVars[0].Name != "a" || proto.LocalVars[0].EndPC != proto.LocalVars[2].StartPC {
		// 同作用域第二个 a 生效时，第一个 a 已不可见，EndPC 必须落在新声明 StartPC。
		t.Fatalf("shadowed local lifetime mismatch: %+v", proto.LocalVars)
	}
	if proto.LocalVars[1].Name != "b" || proto.LocalVars[1].EndPC != len(proto.Code) {
		// 未被遮蔽的 b 仍应活到 chunk 结束。
		t.Fatalf("neighbor local lifetime mismatch: %+v", proto.LocalVars)
	}
}

// TestGeneratorLocalInlineUsesOverflowOnlyAfterSecondBinding 验证单 local 优先使用 inline 槽。
func TestGeneratorLocalInlineUsesOverflowOnlyAfterSecondBinding(t *testing.T) {
	generator := newGenerator("inline-local")
	firstBinding := localBinding{register: 0, localVarIndex: 0, scopeID: 1}
	generator.setLocal("x", firstBinding)
	if !generator.localInlineValid || generator.localInlineName != "x" || generator.localInlineBinding != firstBinding {
		// 第一个 local 必须进入 inline 槽，避免单参数函数创建 overflow map。
		t.Fatalf("unexpected inline local state: valid=%v name=%q binding=%+v", generator.localInlineValid, generator.localInlineName, generator.localInlineBinding)
	}
	if generator.locals != nil {
		// 单 local 场景不应创建 overflow map，这是本轮优化的核心收益点。
		t.Fatalf("single local should not allocate overflow map: %+v", generator.locals)
	}
	if binding, ok := generator.lookupLocal("x"); !ok || binding != firstBinding {
		// helper 查找必须能读取 inline 槽。
		t.Fatalf("lookup inline local = %+v, %v", binding, ok)
	}

	secondBinding := localBinding{register: 1, localVarIndex: 1, scopeID: 1}
	generator.setLocal("y", secondBinding)
	if generator.localCount() != 2 {
		// inline 槽和 overflow map 都必须计入可见 local 数量。
		t.Fatalf("local count=%d", generator.localCount())
	}
	if binding, ok := generator.lookupLocal("y"); !ok || binding != secondBinding {
		// 第二个不同名称 local 必须进入 overflow map 并仍可通过 helper 查找。
		t.Fatalf("lookup overflow local = %+v, %v", binding, ok)
	}
	if len(generator.locals) != 1 {
		// overflow map 只应保存第二个及之后的不同名称 local。
		t.Fatalf("unexpected overflow locals=%+v", generator.locals)
	}
}

// TestGeneratorUsesInlineScopeStack 验证 codegen 首个 parser 作用域复用内嵌栈槽。
//
// 普通函数通常只有一个顶层 block；嵌套 block 仍必须能按普通切片语义扩展和弹出。
func TestGeneratorUsesInlineScopeStack(t *testing.T) {
	generator := newGenerator("inline-scope-stack")
	firstBlock := &parser.Block{Scope: &parser.ScopeInfo{ID: 1}}
	secondBlock := &parser.Block{Scope: &parser.ScopeInfo{ID: 2}}

	generator.pushScope(firstBlock)
	if len(generator.scopeStack) != 1 || generator.scopeStack[0] != firstBlock.Scope {
		// 首个作用域必须进入栈顶。
		t.Fatalf("unexpected first scope stack=%+v", generator.scopeStack)
	}
	if &generator.scopeStack[0] != &generator.inlineScopeStack[0] {
		// 首个作用域应复用生成器内嵌槽。
		t.Fatalf("first scope should use inline slot")
	}

	generator.pushScope(secondBlock)
	if len(generator.scopeStack) != 2 || generator.scopeStack[1] != secondBlock.Scope {
		// 第二个作用域必须按普通切片语义扩展。
		t.Fatalf("unexpected second scope stack=%+v", generator.scopeStack)
	}
	generator.popScope()
	generator.popScope()
	if len(generator.scopeStack) != 0 {
		// 两次 pop 后应恢复空栈。
		t.Fatalf("scope stack should be empty after pop, got %+v", generator.scopeStack)
	}
}

// TestGeneratorIntegerConstantInlineUsesOverflowOnlyAfterSecondValue 验证单 integer 常量优先使用 inline 槽。
func TestGeneratorIntegerConstantInlineUsesOverflowOnlyAfterSecondValue(t *testing.T) {
	generator := newGenerator("inline-integer-constant")
	firstIndex := generator.addConstant(bytecode.IntegerConstant(7))
	if firstIndex != 0 {
		// 首个常量必须进入常量表 0 号槽位。
		t.Fatalf("first constant index=%d", firstIndex)
	}
	if !generator.constants.hasInlineInteger || generator.constants.inlineIntegerValue != 7 || generator.constants.inlineIntegerIndex != firstIndex {
		// 单 integer 常量应登记在 inline 槽，避免创建 overflow map。
		t.Fatalf("unexpected inline integer state: %+v", generator.constants)
	}
	if generator.constants.integers != nil {
		// 单 integer 常量不应创建 map，这是本轮优化的核心收益点。
		t.Fatalf("single integer constant should not allocate overflow map: %+v", generator.constants.integers)
	}

	reusedIndex := generator.addConstant(bytecode.IntegerConstant(7))
	if reusedIndex != firstIndex || len(generator.proto.Constants) != 1 {
		// 重复 integer 常量必须命中 inline 槽并复用原常量表下标。
		t.Fatalf("reused index=%d first=%d constants=%+v", reusedIndex, firstIndex, generator.proto.Constants)
	}

	secondIndex := generator.addConstant(bytecode.IntegerConstant(9))
	if secondIndex != 1 {
		// 第二个不同 integer 常量应追加到常量表。
		t.Fatalf("second constant index=%d", secondIndex)
	}
	if len(generator.constants.integers) != 1 || generator.constants.integers[9] != secondIndex {
		// 第二个不同 integer 才进入 overflow map。
		t.Fatalf("unexpected overflow integer map=%+v", generator.constants.integers)
	}
	if reusedSecondIndex := generator.addConstant(bytecode.IntegerConstant(9)); reusedSecondIndex != secondIndex || len(generator.proto.Constants) != 2 {
		// overflow 中的重复 integer 仍必须复用原下标。
		t.Fatalf("reused second index=%d second=%d constants=%+v", reusedSecondIndex, secondIndex, generator.proto.Constants)
	}
}

// TestNewGeneratorPreallocatesCodeAndLineInfo 验证 codegen Proto 只预留短表容量。
func TestNewGeneratorPreallocatesCodeAndLineInfo(t *testing.T) {
	generator := newGenerator("prealloc-code")
	if len(generator.proto.Code) != 0 || len(generator.proto.LineInfo) != 0 {
		// 容量预留不能提前写入指令或行号，否则会改变字节码输出。
		t.Fatalf("new generator should start with empty code/lineinfo: code=%d line=%d", len(generator.proto.Code), len(generator.proto.LineInfo))
	}
	if cap(generator.proto.Code) < initialCodeCapacity || cap(generator.proto.LineInfo) < initialCodeCapacity {
		// 小函数至少应能容纳常见的两条指令，减少首次 append 扩容。
		t.Fatalf("unexpected code/lineinfo capacity: code=%d line=%d", cap(generator.proto.Code), cap(generator.proto.LineInfo))
	}
	if len(generator.proto.Constants) != 0 || cap(generator.proto.Constants) < initialConstantCapacity {
		// codegen 的单常量热路径应具备 opt-in 容量，但不能提前写入可见常量。
		t.Fatalf("unexpected constants capacity: len=%d cap=%d", len(generator.proto.Constants), cap(generator.proto.Constants))
	}
	if len(generator.proto.LocalVars) != 0 || cap(generator.proto.LocalVars) < initialLocalVarCapacity {
		// codegen 的单局部变量热路径应具备 opt-in 容量，但不能提前写入可见 LocalVar。
		t.Fatalf("unexpected local vars capacity: len=%d cap=%d", len(generator.proto.LocalVars), cap(generator.proto.LocalVars))
	}

	generator.currentLine = 12
	pc := generator.emitABC(bytecode.OpReturn, 0, 1, 0)
	if pc != 0 || len(generator.proto.Code) != 1 || len(generator.proto.LineInfo) != 1 || generator.proto.LineInfo[0] != 12 {
		// addInstruction 仍必须同步追加真实指令和对应源码行号。
		t.Fatalf("unexpected emitted instruction state: pc=%d code=%+v line=%+v", pc, generator.proto.Code, generator.proto.LineInfo)
	}

	generator.currentLine = 13
	secondPC := generator.emitABC(bytecode.OpMove, 1, 0, 0)
	generator.currentLine = 14
	thirdPC := generator.emitABC(bytecode.OpReturn, 0, 1, 0)
	if secondPC != 1 || thirdPC != 2 || len(generator.proto.Code) != 3 || len(generator.proto.LineInfo) != 3 {
		// 第三条指令可能触发短槽后的普通扩容，但 pc、指令数和行号数必须保持同步。
		t.Fatalf("unexpected post-growth state: second=%d third=%d code=%+v line=%+v", secondPC, thirdPC, generator.proto.Code, generator.proto.LineInfo)
	}
	if generator.proto.LineInfo[0] != 12 || generator.proto.LineInfo[1] != 13 || generator.proto.LineInfo[2] != 14 {
		// 扩容后行号表顺序仍必须和 emit 顺序一致。
		t.Fatalf("line info changed after growth: %+v", generator.proto.LineInfo)
	}

	firstConstant := bytecode.IntegerConstant(1)
	secondConstant := bytecode.IntegerConstant(2)
	if firstIndex := generator.proto.AddConstant(firstConstant); firstIndex != 0 {
		// codegen opt-in 后第一项常量仍从 0 号槽位开始。
		t.Fatalf("first constant index=%d", firstIndex)
	}
	if secondIndex := generator.proto.AddConstant(secondConstant); secondIndex != 1 {
		// 第二项常量触发扩容时仍必须保持常量表下标语义。
		t.Fatalf("second constant index=%d", secondIndex)
	}
	if generator.proto.Constants[0] != firstConstant || generator.proto.Constants[1] != secondConstant {
		// 常量表扩容后顺序必须保持。
		t.Fatalf("constant order changed after growth: %+v", generator.proto.Constants)
	}

	firstLocal := bytecode.LocalVar{Name: "a", Register: 0, StartPC: 0, EndPC: 1}
	secondLocal := bytecode.LocalVar{Name: "b", Register: 1, StartPC: 1, EndPC: 2}
	generator.proto.LocalVars = append(generator.proto.LocalVars, firstLocal)
	generator.proto.LocalVars = append(generator.proto.LocalVars, secondLocal)
	if generator.proto.LocalVars[0] != firstLocal || generator.proto.LocalVars[1] != secondLocal {
		// LocalVars 扩容后顺序必须保持，debug local 生命周期才能稳定输出。
		t.Fatalf("local var order changed after growth: %+v", generator.proto.LocalVars)
	}
}

// TestCompileChunkPreallocatesDirectFunctionBlock 验证当前 block 的直接函数声明会预留短表容量。
//
// 容量预留只能减少 append 扩容成本，不能改变指令、常量、子函数数量、顺序或无子函数 Proto 的 nil 观察语义。
func TestCompileChunkPreallocatesDirectFunctionBlock(t *testing.T) {
	plainChunk := parseChunkForCodegenTest(t, "local x = 1 return x")
	plainProto, err := CompileChunk(plainChunk, "no-child-proto")
	if err != nil {
		// 普通无子函数 chunk 不应编译失败。
		t.Fatalf("compile plain chunk failed: %v", err)
	}
	if plainProto.Protos != nil {
		// 没有子函数时必须保持 nil，避免给普通 Proto 引入额外分配。
		t.Fatalf("plain proto should keep nil children: %+v", plainProto.Protos)
	}

	functionChunk := parseChunkForCodegenTest(t, "function f() return 1 end local function g() return 2 end")
	stats := directFunctionBlockStatsFor(functionChunk.Block)
	if stats.childCount != 2 || stats.instructionCapacity != 4 || stats.nameConstantCapacity != 1 {
		// 当前 block 中两个直接函数声明都应被计入容量，普通 function 额外预留名称常量。
		t.Fatalf("unexpected direct function stats=%+v", stats)
	}
	functionProto, err := CompileChunk(functionChunk, "child-protos")
	if err != nil {
		// 合法函数声明不应编译失败。
		t.Fatalf("compile function chunk failed: %v", err)
	}
	if len(functionProto.Protos) != 2 || cap(functionProto.Protos) < 2 {
		// 子 Proto 长度和容量必须覆盖两个直接函数声明。
		t.Fatalf("unexpected child protos len/cap=%d/%d", len(functionProto.Protos), cap(functionProto.Protos))
	}
	if cap(functionProto.Code) < stats.instructionCapacity || cap(functionProto.LineInfo) < stats.instructionCapacity {
		// 指令和行号容量必须覆盖直接函数声明的保守预估。
		t.Fatalf("unexpected code capacity: code=%d line=%d stats=%+v", cap(functionProto.Code), cap(functionProto.LineInfo), stats)
	}
	if cap(functionProto.Constants) < stats.nameConstantCapacity {
		// 普通 function 名称常量容量必须覆盖直接函数声明预估。
		t.Fatalf("unexpected constants capacity: cap=%d stats=%+v", cap(functionProto.Constants), stats)
	}
	if functionProto.Protos[0].Constants[0] != bytecode.IntegerConstant(1) || functionProto.Protos[1].Constants[0] != bytecode.IntegerConstant(2) {
		// 预留容量不得改变子 Proto 追加顺序。
		t.Fatalf("child proto order changed: %+v", functionProto.Protos)
	}

	arenaGenerator := newGenerator("child-arena")
	arenaGenerator.prepareDirectFunctionBlockCapacity(functionChunk.Block)
	if arenaGenerator.constants.strings == nil {
		// 普通 function 名称常量索引应按直接函数统计预留，避免大量全局函数名触发 map 连续扩容。
		t.Fatalf("expected preallocated string constant index")
	}
	if len(arenaGenerator.childProtoArena) != stats.childCount {
		// 直接子函数 Proto arena 应按统计数量精确分配。
		t.Fatalf("unexpected child proto arena length=%d stats=%+v", len(arenaGenerator.childProtoArena), stats)
	}
	firstChild := arenaGenerator.borrowChildProto("first-child")
	secondChild := arenaGenerator.borrowChildProto("second-child")
	if firstChild != &arenaGenerator.childProtoArena[0] || secondChild != &arenaGenerator.childProtoArena[1] {
		// 预估命中时子 Proto 必须来自父 generator 的连续 arena。
		t.Fatalf("child protos should use arena: first=%p second=%p arena=%p/%p", firstChild, secondChild, &arenaGenerator.childProtoArena[0], &arenaGenerator.childProtoArena[1])
	}
	if firstChild.Source != "first-child" || secondChild.Source != "second-child" {
		// 借用 arena 后仍必须重置并写入各自 source。
		t.Fatalf("child proto source mismatch: first=%q second=%q", firstChild.Source, secondChild.Source)
	}
	if cap(firstChild.Code) < initialCodeCapacity || cap(firstChild.Constants) < initialConstantCapacity || cap(firstChild.LocalVars) < initialLocalVarCapacity {
		// arena 中的 Proto 也必须具备 codegen 短表容量。
		t.Fatalf("arena child proto was not prepared: code=%d constants=%d locals=%d", cap(firstChild.Code), cap(firstChild.Constants), cap(firstChild.LocalVars))
	}
	fallbackChild := arenaGenerator.borrowChildProto("fallback-child")
	if fallbackChild == &arenaGenerator.childProtoArena[0] || fallbackChild == &arenaGenerator.childProtoArena[1] {
		// 超出预估时必须回退独立 Proto，避免覆盖已借出的子 Proto。
		t.Fatalf("fallback child reused arena slot: %p", fallbackChild)
	}

	generatorArena := newGenerator("child-generator-arena")
	generatorArena.prepareDirectFunctionBlockCapacity(functionChunk.Block)
	firstGenerator := generatorArena.borrowChildGenerator("first-generator")
	firstGeneratorProto := firstGenerator.proto
	secondGenerator := generatorArena.borrowChildGenerator("second-generator")
	secondGeneratorProto := secondGenerator.proto
	if firstGenerator != generatorArena.childGeneratorScratch || secondGenerator != firstGenerator {
		// 直接子函数 generator 只在编译期顺序使用，应复用父 generator 的 scratch 槽。
		t.Fatalf("child generators should reuse scratch: first=%p second=%p scratch=%p", firstGenerator, secondGenerator, generatorArena.childGeneratorScratch)
	}
	if secondGenerator.parent != generatorArena {
		// 借用 generator 仍必须指回父 generator，保持 upvalue 捕获语义。
		t.Fatalf("child generator parent mismatch: second=%p parent=%p", secondGenerator.parent, generatorArena)
	}
	if firstGeneratorProto != &generatorArena.childProtoArena[0] || secondGeneratorProto != &generatorArena.childProtoArena[1] {
		// generator arena 和 Proto arena 必须按同一顺序配对，保持 CLOSURE Bx 顺序。
		t.Fatalf("child generator proto mismatch: first=%p second=%p arena=%p/%p", firstGeneratorProto, secondGeneratorProto, &generatorArena.childProtoArena[0], &generatorArena.childProtoArena[1])
	}
	fallbackGenerator := generatorArena.borrowChildGenerator("fallback-generator")
	if fallbackGenerator != generatorArena.childGeneratorScratch {
		// 超出 Proto 预估时仍可复用 generator scratch，因为 generator 不进入最终 Proto。
		t.Fatalf("fallback generator should reuse scratch: %p", fallbackGenerator)
	}
	if fallbackGenerator.proto == &generatorArena.childProtoArena[0] || fallbackGenerator.proto == &generatorArena.childProtoArena[1] {
		// 超出 Proto 预估时必须回退独立 Proto，避免覆盖已借出的子 Proto。
		t.Fatalf("fallback generator reused proto arena slot: %p", fallbackGenerator.proto)
	}

	nestedChunk := parseChunkForCodegenTest(t, "do function nested() end end")
	if nestedStats := directFunctionBlockStatsFor(nestedChunk.Block); nestedStats.childCount != 0 || nestedStats.instructionCapacity != 0 || nestedStats.nameConstantCapacity != 0 {
		// helper 只统计当前 block 的直接函数声明，不递归进入嵌套 block。
		t.Fatalf("nested function should not be counted as direct child: %+v", nestedStats)
	}
}

// TestCompileNestedShadowRestoresInlineLocal 验证内层遮蔽退出后恢复外层 inline local。
func TestCompileNestedShadowRestoresInlineLocal(t *testing.T) {
	chunk := parseChunkForCodegenTest(t, "local x = 1 do local x = 2 end return x")

	proto, err := CompileChunk(chunk, "inline-shadow")
	if err != nil {
		// 合法内层遮蔽样例不应编译失败。
		t.Fatalf("compile inline shadow failed: %v", err)
	}
	if len(proto.LocalVars) != 2 {
		// 外层和内层 x 都必须保留独立调试局部变量生命周期。
		t.Fatalf("unexpected local vars=%+v", proto.LocalVars)
	}
	if proto.LocalVars[0].Name != "x" || proto.LocalVars[1].Name != "x" {
		// 同名遮蔽不能丢失任一 LocalVar 名称。
		t.Fatalf("unexpected local names=%+v", proto.LocalVars)
	}
	returnInstruction := proto.Code[len(proto.Code)-1]
	if returnInstruction.OpCode() != bytecode.OpReturn || returnInstruction.A() != 0 {
		// block 结束后 return x 必须读取外层 R0，而不是内层遮蔽 local 的 R1。
		t.Fatalf("return should read outer inline local R0: code=%v locals=%+v", proto.Code, proto.LocalVars)
	}
}

// TestCompileChunkNestedFunctionCapturesUpvalue 验证嵌套函数 Proto 和 upvalue 捕获。
//
// local function 内读取外层 local 时，子 Proto 必须声明 InStack upvalue 并生成 GETUPVAL。
func TestCompileChunkNestedFunctionCapturesUpvalue(t *testing.T) {
	chunk := parseChunkForCodegenTest(t, "local x = 1 local function f() return x end")

	proto, err := CompileChunk(chunk, "nested")
	if err != nil {
		// 合法嵌套函数样例不应编译失败。
		t.Fatalf("compile nested function failed: %v", err)
	}
	if len(proto.Protos) != 1 {
		// local function f 应生成一个子 Proto。
		t.Fatalf("unexpected child proto count=%d", len(proto.Protos))
	}
	if !containsOpCode(proto, bytecode.OpClosure) {
		// 外层函数必须通过 CLOSURE 创建子闭包。
		t.Fatalf("expected CLOSURE instruction")
	}
	child := proto.Protos[0]
	if len(child.Upvalues) != 1 {
		// 子函数读取 x 应捕获一个 upvalue。
		t.Fatalf("unexpected upvalues=%+v", child.Upvalues)
	}
	if child.Upvalues[0].Name != "x" || !child.Upvalues[0].InStack || child.Upvalues[0].Index != 0 {
		// x 来自父函数 R0，应登记为 InStack upvalue。
		t.Fatalf("unexpected upvalue desc=%+v", child.Upvalues[0])
	}
	if !containsOpCode(child, bytecode.OpGetUpval) {
		// return x 编译时应通过 GETUPVAL 读取 x。
		t.Fatalf("expected GETUPVAL in child")
	}
}

// TestCompileChunkTableConstructor 验证数组风格 table constructor codegen。
//
// 当前 table constructor 使用 NEWTABLE 和 SETTABLE 写入 1-based integer key。
func TestCompileChunkTableConstructor(t *testing.T) {
	chunk := parseChunkForCodegenTest(t, "local a = 7 local t = {a, 2}")

	proto, err := CompileChunk(chunk, "table")
	if err != nil {
		// 当前支持的数组风格 table constructor 不应编译失败。
		t.Fatalf("compile table constructor failed: %v", err)
	}
	if !containsOpCode(proto, bytecode.OpNewTable) {
		// table constructor 必须先生成 NEWTABLE。
		t.Fatalf("expected NEWTABLE instruction")
	}
	if countOpCode(proto, bytecode.OpSetTable) != 2 {
		// 两个数组字段必须各生成一次 SETTABLE。
		t.Fatalf("expected two SETTABLE instructions")
	}
	if !hasIntegerConstant(proto, 1) || !hasIntegerConstant(proto, 2) || !hasIntegerConstant(proto, 7) {
		// key 1/2 和字段字面量 7 都应进入常量表。
		t.Fatalf("missing expected integer constants=%+v", proto.Constants)
	}
}

// TestCompileChunkFieldAndIndexAssignment 验证字段与索引赋值左值 codegen。
//
// 官方测试入口会使用 `_G._ARG = arg`；方括号索引同步覆盖通用 SETTABLE 左值。
func TestCompileChunkFieldAndIndexAssignment(t *testing.T) {
	chunk := parseChunkForCodegenTest(t, "_G._ARG = arg local t = {} t[\"x\"] = 1")

	proto, err := CompileChunk(chunk, "field-assign")
	if err != nil {
		// 字段赋值和索引赋值都应可编译。
		t.Fatalf("compile field/index assignment failed: %v", err)
	}
	if countOpCode(proto, bytecode.OpSetTable) != 2 {
		// `_G._ARG` 和 `t["x"]` 各自应生成一个 SETTABLE。
		t.Fatalf("expected two SETTABLE instructions")
	}
	if !hasStringConstant(proto, "_G") || !hasStringConstant(proto, "_ARG") || !hasStringConstant(proto, "arg") {
		// 全局接收者、字段名和右值全局名都应进入常量池。
		t.Fatalf("missing assignment constants=%+v", proto.Constants)
	}
}

// TestCompileChunkFieldAccessFallsBackWhenConstantExceedsRK 验证字段常量超过 RK 上限时降级为寄存器 key。
//
// 官方 api.lua 会先制造大量常量，再访问字段；Lua 5.3 允许这种场景通过 LOADK+寄存器 RK 编码完成。
func TestCompileChunkFieldAccessFallsBackWhenConstantExceedsRK(t *testing.T) {
	var source strings.Builder
	source.WriteString("local t = {}\n")
	for index := 0; index <= bytecode.MaxIndexRK+4; index++ {
		// 每条索引赋值都制造一个不同字符串常量，稳定推高后续字段名的常量索引。
		fmt.Fprintf(&source, "t[%q] = %d\n", fmt.Sprintf("seed_%03d", index), index)
	}
	source.WriteString("t.overflow = 2\nreturn t.overflow\n")

	chunk := parseChunkForCodegenTest(t, source.String())
	proto, err := CompileChunk(chunk, "rk-overflow")
	if err != nil {
		// 字段名常量超过 RK 上限时不应再直接报错。
		t.Fatalf("compile high constant field access failed: %v", err)
	}
	if !containsOpCode(proto, bytecode.OpLoadK) {
		// 降级路径必须先把高位常量加载进临时寄存器。
		t.Fatalf("expected LOADK fallback for high constant index")
	}
	if !containsGetTableWithRegisterKey(proto) {
		// return t.overflow 应使用寄存器 key 读取字段，避免 RK 常量索引溢出。
		t.Fatalf("expected GETTABLE with register key for high constant index")
	}
}

// TestCompileChunkFunctionCall 验证普通函数调用表达式和调用语句 codegen。
//
// local function 生成 closure 后，调用语句应使用 CALL 且 C=1 表示丢弃返回值。
func TestCompileChunkFunctionCall(t *testing.T) {
	chunk := parseChunkForCodegenTest(t, "local function f(a) return a end f(1)")

	proto, err := CompileChunk(chunk, "call")
	if err != nil {
		// 当前普通函数调用语句应可编译。
		t.Fatalf("compile function call failed: %v", err)
	}
	callInstruction, ok := firstInstruction(proto, bytecode.OpCall)
	if !ok {
		// 调用语句必须生成 CALL。
		t.Fatalf("expected CALL instruction")
	}
	if callInstruction.B() != 2 || callInstruction.C() != 1 {
		// 一个参数 B=2，丢弃返回值 C=1。
		t.Fatalf("unexpected CALL fields: B=%d C=%d", callInstruction.B(), callInstruction.C())
	}
}

// TestCompileCallExpandsTrailingVararg 验证调用实参末尾 vararg 会展开为开放参数。
//
// 官方测试 main.lua 的 RUN helper 会执行 `string.format(p, ...)`；最后一个实参为 `...` 时，
// codegen 必须生成 VARARG B=0 与 CALL B=0，让 VM 按开放栈顶传入全部 vararg。
func TestCompileCallExpandsTrailingVararg(t *testing.T) {
	chunk := parseChunkForCodegenTest(t, "local function f(...) string.format('%s %s', ...) end")

	proto, err := CompileChunk(chunk, "call-vararg")
	if err != nil {
		// vararg 作为调用末尾实参的样例必须可编译。
		t.Fatalf("compile vararg call failed: %v", err)
	}
	child := proto.Protos[0]
	varargInstruction, ok := firstInstruction(child, bytecode.OpVararg)
	if !ok || varargInstruction.B() != 0 {
		// 末尾 vararg 必须使用 B=0 表示开放写入，而不是固定单值。
		t.Fatalf("unexpected VARARG instruction: %v ok=%v", varargInstruction, ok)
	}
	callInstruction, ok := firstInstruction(child, bytecode.OpCall)
	if !ok || callInstruction.B() != 0 {
		// 消费开放 vararg 的 CALL 必须使用 B=0。
		t.Fatalf("unexpected CALL instruction: %v ok=%v", callInstruction, ok)
	}
}

// TestCompileLocalInitializerReadsOuterName 验证 local 初始化表达式读取外层同名变量。
//
// Lua 5.3 规定 `local arg = arg or _ARG` 的 RHS 不应看到正在声明的新局部变量；官方测试
// main.lua 依赖该语义把全局 arg 复制到局部 arg。
func TestCompileLocalInitializerReadsOuterName(t *testing.T) {
	chunk := parseChunkForCodegenTest(t, "local arg = arg or _ARG")

	proto, err := CompileChunk(chunk, "local-init")
	if err != nil {
		// 同名 local 初始化样例必须可编译。
		t.Fatalf("compile local initializer failed: %v", err)
	}
	firstInstruction := proto.Code[0]
	if firstInstruction.OpCode() != bytecode.OpGetTabUp {
		// RHS 的 arg 必须按全局读取生成 GETTABUP，而不是读取未初始化局部寄存器。
		t.Fatalf("first instruction = %v, want GETTABUP", firstInstruction.OpCode())
	}
	if !hasStringConstant(proto, "arg") || !hasStringConstant(proto, "_ARG") {
		// 两个全局候选名都必须进入常量池。
		t.Fatalf("missing arg constants=%+v", proto.Constants)
	}
}

// TestCompileLocalAssignmentUsesTemporaryRegister 验证局部赋值 RHS 可读取旧值。
//
// 官方测试 main.lua 的 `p = string.gsub(p, ...)` 要求 RHS 中的 p 仍是赋值前的字符串；codegen
// 必须先把调用结果写入临时寄存器，再 MOVE 回目标局部，避免覆盖参数读取。
func TestCompileLocalAssignmentUsesTemporaryRegister(t *testing.T) {
	chunk := parseChunkForCodegenTest(t, "local p = 'lua -v' p = string.gsub(p, 'lua', 'glua', 1)")

	proto, err := CompileChunk(chunk, "local-assign-temp")
	if err != nil {
		// 局部自引用赋值样例必须可编译。
		t.Fatalf("compile local assignment failed: %v", err)
	}
	callInstruction, ok := firstInstruction(proto, bytecode.OpCall)
	if !ok {
		// string.gsub 调用必须生成 CALL。
		t.Fatalf("expected CALL instruction")
	}
	if callInstruction.A() == 0 {
		// CALL 不能以目标局部 p 的寄存器作为函数寄存器，否则会覆盖 RHS 旧值。
		t.Fatalf("CALL uses target register A=0")
	}
	if !hasMove(proto, 0, callInstruction.A()) {
		// 调用完成后必须把临时结果移动回局部 p。
		t.Fatalf("missing MOVE from call register %d back to local p", callInstruction.A())
	}
}

// TestCompileUnaryCallOperandReusesTargetRegister 验证一元表达式复用非 local 目标寄存器。
//
// `#string.reverse(s)` 中函数调用结果可以直接写入 LEN 的目标寄存器，避免额外占用一个临时
// 寄存器；目标寄存器若是活跃 local 时仍由专门保护路径处理。
func TestCompileUnaryCallOperandReusesTargetRegister(t *testing.T) {
	chunk := parseChunkForCodegenTest(t, "local s = 'abcDefGHI123' local sum = 0 for i = 1, 10 do sum = sum + #string.reverse(s) end")

	proto, err := CompileChunk(chunk, "unary-call-target")
	if err != nil {
		// 一元调用操作数样例必须可编译。
		t.Fatalf("compile unary call operand failed: %v", err)
	}
	callInstruction, ok := firstInstruction(proto, bytecode.OpCall)
	if !ok {
		// string.reverse 调用必须生成 CALL。
		t.Fatalf("expected CALL instruction")
	}
	lenInstruction, ok := firstInstruction(proto, bytecode.OpLen)
	if !ok {
		// `#` 运算必须生成 LEN。
		t.Fatalf("expected LEN instruction")
	}
	if callInstruction.A() != lenInstruction.A() || lenInstruction.B() != callInstruction.A() {
		// CALL 结果应直接落在 LEN 的源/目标寄存器上，避免额外临时寄存器。
		t.Fatalf("CALL/LEN register mismatch: call=%#v len=%#v", callInstruction, lenInstruction)
	}
	if proto.MaxStackSize != 8 {
		// 对齐 Lua 5.3 官方 codegen，该样例不应再需要第 9 个寄存器。
		t.Fatalf("unexpected max stack=%d", proto.MaxStackSize)
	}
}

// TestCompileUnaryLocalOperandUsesSourceRegister 验证一元 local 操作数直接使用源寄存器。
//
// `#s` 是 stdlib_math_string benchmark 的热循环形态；Lua 5.3 官方 codegen 直接生成
// `LEN target, s`，不需要先把 s MOVE 到 target 再取长度。
func TestCompileUnaryLocalOperandUsesSourceRegister(t *testing.T) {
	chunk := parseChunkForCodegenTest(t, "local sum = 0 local s = 'abcdefghijklmnopqrstuvwxyz' for i = 1, 10 do sum = sum + string.byte(s, (i % #s) + 1) end")

	proto, err := CompileChunk(chunk, "unary-local-source")
	if err != nil {
		// benchmark 片段必须可编译。
		t.Fatalf("compile unary local operand failed: %v", err)
	}
	var lenInstruction bytecode.Instruction
	lenFound := false
	for _, instruction := range proto.Code {
		// 查找热循环中的 LEN 指令。
		if instruction.OpCode() == bytecode.OpLen {
			// 记录第一条 LEN，样例中只会生成一条。
			lenInstruction = instruction
			lenFound = true
			break
		}
	}
	if !lenFound {
		// `#s` 必须生成 LEN。
		t.Fatalf("expected LEN instruction")
	}
	if lenInstruction.B() != 1 {
		// local s 是第二个声明的 local，对应寄存器 R1；LEN 应直接读取该寄存器。
		t.Fatalf("LEN should read local s register R1 directly: %#v", lenInstruction)
	}
	if hasMove(proto, lenInstruction.A(), 1) {
		// 旧形态会先 MOVE s 到 LEN 目标寄存器。
		t.Fatalf("unexpected MOVE before LEN local operand: code=%v", proto.Code)
	}
}

// TestCompileSelfArithmeticAssignmentWritesDirectly 验证 local 自算术赋值直接写回目标寄存器。
//
// 数值 for 热循环中的 `acc = acc + i` 不应生成“复制 acc 到临时、计算临时、MOVE 回 acc”的
// 通用赋值序列；安全右操作数为 local 名称时可以直接生成 `ADD acc, acc, temp`。
func TestCompileSelfArithmeticAssignmentWritesDirectly(t *testing.T) {
	chunk := parseChunkForCodegenTest(t, "local acc = 0 for i = 1, 10 do acc = acc + i end")

	proto, err := CompileChunk(chunk, "self-arith-assign")
	if err != nil {
		// 自算术赋值样例必须可编译。
		t.Fatalf("compile self arithmetic assignment failed: %v", err)
	}
	foundDirectAdd := false
	for _, instruction := range proto.Code {
		// 目标 local acc 使用 R0；优化后 ADD 直接写回 R0 并以 R0 作为左操作数。
		if instruction.OpCode() == bytecode.OpAdd && instruction.A() == 0 && instruction.B() == 0 && !bytecode.IsK(instruction.C()) {
			foundDirectAdd = true
		}
		if instruction.OpCode() == bytecode.OpMove && instruction.A() == 0 && instruction.B() != 0 {
			// 旧通用赋值路径会把临时结果 MOVE 回 acc，优化后不应存在该写回。
			t.Fatalf("unexpected MOVE back to acc from R%d", instruction.B())
		}
	}
	if !foundDirectAdd {
		// 必须存在直接写回 acc 的 ADD。
		t.Fatalf("missing direct ADD into acc; code=%v", proto.Code)
	}
}

// TestCompileSelfArithmeticPureBinaryRightAvoidsLeftMove 验证自二元赋值的纯二元右侧不会复制左值。
//
// `sum = sum + ((i * 3 - 7) // 2) % 97` 是 arith_mix_loop 热路径。官方 Lua 5.3 会把右侧
// 纯算术树编入一个临时寄存器，再直接 `ADD sum, sum, temp`，不需要先 MOVE sum 到临时槽。
func TestCompileSelfArithmeticPureBinaryRightAvoidsLeftMove(t *testing.T) {
	chunk := parseChunkForCodegenTest(t, `
local sum = 0
for i = 1, 10 do
  sum = sum + ((i * 3 - 7) // 2) % 97
end
return sum
`)

	proto, err := CompileChunk(chunk, "self-arith-pure-binary-right")
	if err != nil {
		// 自二元纯算术右侧样例必须可编译。
		t.Fatalf("compile self arithmetic pure binary right failed: %v", err)
	}
	if proto.MaxStackSize != 6 {
		// 官方 Lua 5.3 该热循环只需要 6 个 slot；7 个 slot 表示仍有左值临时 MOVE。
		t.Fatalf("unexpected max stack=%d code=%v", proto.MaxStackSize, proto.Code)
	}

	hasDirectAdd := false
	for _, instruction := range proto.Code {
		switch instruction.OpCode() {
		case bytecode.OpMove:
			if instruction.A() == 5 && instruction.B() == 0 {
				// 旧形态会先把 sum 从 R0 复制到 R5，再用 R5 执行 ADD。
				t.Fatalf("unexpected MOVE of sum before pure binary RHS; code=%v", proto.Code)
			}
		case bytecode.OpAdd:
			if instruction.A() == 0 && instruction.B() == 0 && instruction.C() == 5 {
				// sum 位于 R0，右侧纯算术结果位于 R5，最终 ADD 应直接写回 R0。
				hasDirectAdd = true
			}
		}
	}
	if !hasDirectAdd {
		// 必须存在直接写回 sum 的 ADD。
		t.Fatalf("missing direct ADD for pure binary RHS; code=%v", proto.Code)
	}
}

// TestCompileSelfBinaryChainUsesAccumulator 验证自二元链在最终写回前使用临时累加器。
//
// `sum = sum + i * 3 - 7` 对齐官方 Lua 5.3 codegen：第一层 ADD 写入临时寄存器，第二层 SUB
// 才写回 sum，避免 SUB 失败时提前污染目标 local。
func TestCompileSelfBinaryChainUsesAccumulator(t *testing.T) {
	chunk := parseChunkForCodegenTest(t, `
local sum = 0
for i = 1, 10 do
  sum = sum + i * 3 - 7
end
return sum
`)

	proto, err := CompileChunk(chunk, "self-binary-chain-accumulator")
	if err != nil {
		// 自二元链样例必须可编译。
		t.Fatalf("compile self binary chain failed: %v", err)
	}
	if proto.MaxStackSize != 6 {
		// 官方 Lua 5.3 该形态使用一个表达式临时寄存器，总 stack 为 6。
		t.Fatalf("unexpected max stack=%d code=%v", proto.MaxStackSize, proto.Code)
	}

	foundAccumulatorAdd := false
	foundFinalSub := false
	for _, instruction := range proto.Code {
		switch instruction.OpCode() {
		case bytecode.OpAdd:
			if instruction.A() == 0 {
				// 中间 ADD 不能直接写回 sum，否则后续 SUB 报错会污染目标 local。
				t.Fatalf("unexpected ADD writes target before final operator: code=%v", proto.Code)
			}
			if instruction.A() == 5 && instruction.B() == 0 && instruction.C() == 5 {
				// 第一层 ADD 使用 R5 作为临时累加器。
				foundAccumulatorAdd = true
			}
		case bytecode.OpSub:
			if instruction.A() == 0 && instruction.B() == 5 && bytecode.IsK(instruction.C()) {
				// 最后一层 SUB 才写回 sum。
				foundFinalSub = true
			}
		}
	}
	if !foundAccumulatorAdd || !foundFinalSub {
		// 两层运算必须分别体现临时累加器和最终写回。
		t.Fatalf("missing accumulator shape: add=%v sub=%v code=%v", foundAccumulatorAdd, foundFinalSub, proto.Code)
	}
}

// TestCompileSelfBinaryCallChainReusesAccumulator 验证自二元调用链复用累加器寄存器。
//
// `sum = sum + call() + call()` 需要保持最终写回前不覆盖 sum，同时第一层 `sum + call()`
// 可让调用结果直接占用累加器，避免额外临时寄存器并对齐 Lua 5.3 的寄存器布局。
func TestCompileSelfBinaryCallChainReusesAccumulator(t *testing.T) {
	chunk := parseChunkForCodegenTest(t, "local sum = 0 local s = 'abcdef' for i = 1, 10 do sum = sum + math.sin(i) + math.floor(i / 3) + string.len(s) end")

	proto, err := CompileChunk(chunk, "self-binary-call-chain")
	if err != nil {
		// 自二元调用链样例必须可编译。
		t.Fatalf("compile self binary call chain failed: %v", err)
	}
	if proto.MaxStackSize != 9 {
		// 固定单返回调用回收实参槽后，当前样例可复用到 9 个寄存器。
		t.Fatalf("unexpected max stack=%d", proto.MaxStackSize)
	}
	for instructionIndex, instruction := range proto.Code {
		if instruction.OpCode() != bytecode.OpCall {
			// 只关心第一条标准库调用。
			continue
		}
		if instruction.A() != 6 {
			// 第一层 math.sin 调用应直接写入累加器 R6。
			t.Fatalf("first CALL should use accumulator R6, got R%d", instruction.A())
		}
		if instructionIndex+1 >= len(proto.Code) {
			// CALL 后缺少 ADD 表示测试样例或 codegen 形态异常。
			t.Fatalf("missing ADD after first CALL")
		}
		addInstruction := proto.Code[instructionIndex+1]
		if addInstruction.OpCode() != bytecode.OpAdd || addInstruction.A() != 6 || addInstruction.B() != 0 || addInstruction.C() != 6 {
			// 第一层应生成 `ADD R6, R0, R6`，避免旧形态的 `ADD R6, R0, R7`。
			t.Fatalf("unexpected ADD after first CALL: %s", addInstruction.OpCode().Name())
		}
		return
	}
	t.Fatalf("missing CALL instruction")
}

// TestCompileTableReadWriteUsesDirectRegisters 验证 table 热循环复用 local/for 寄存器。
//
// 对齐 Lua 5.3 C codegen 的 `t[i] = i` 与 `acc = acc + t[i]` 形态，避免通用赋值路径为
// table、key、value 和 acc 生成额外临时 MOVE。
func TestCompileTableReadWriteUsesDirectRegisters(t *testing.T) {
	chunk := parseChunkForCodegenTest(t, "local t = {} for i = 1, 10 do t[i] = i end local acc = 0 for i = 1, 10 do acc = acc + t[i] end")

	proto, err := CompileChunk(chunk, "table-read-write")
	if err != nil {
		// table 读写样例必须可编译。
		t.Fatalf("compile table read write failed: %v", err)
	}
	hasDirectSetTable := false
	hasDirectGetTable := false
	hasDirectAdd := false
	for _, instruction := range proto.Code {
		switch instruction.OpCode() {
		case bytecode.OpSetTable:
			if instruction.A() == 0 && instruction.B() == 4 && instruction.C() == 4 {
				// 第一段 for 的外部变量 i 位于 R4，table t 位于 R0。
				hasDirectSetTable = true
			}
		case bytecode.OpGetTable:
			if instruction.B() == 0 && instruction.C() == 5 {
				// 第二段 for 的外部变量 i 位于 R5，table t 位于 R0。
				hasDirectGetTable = true
			}
		case bytecode.OpAdd:
			if instruction.A() == 1 && instruction.B() == 1 {
				// acc 位于 R1，优化后 ADD 直接写回 acc。
				hasDirectAdd = true
			}
		case bytecode.OpMove:
			if instruction.A() == 1 && instruction.B() != 0 {
				// 旧通用赋值路径会把临时加法结果 MOVE 回 acc。
				t.Fatalf("unexpected MOVE back to acc from R%d", instruction.B())
			}
		}
	}
	if !hasDirectSetTable {
		// 写入循环必须直接复用 t/i/i。
		t.Fatalf("missing direct SETTABLE t[i]=i; code=%v", proto.Code)
	}
	if !hasDirectGetTable || !hasDirectAdd {
		// 读取累加循环必须直接 GETTABLE 后 ADD 回 acc。
		t.Fatalf("missing direct GETTABLE/ADD; get=%v add=%v code=%v", hasDirectGetTable, hasDirectAdd, proto.Code)
	}
}

// TestCompileFieldReadWriteUsesDirectRegisters 验证字段读写热循环复用 local receiver。
//
// 对齐 Lua 5.3 C codegen 的 `t.a = t.a + 1` 与 `sum = sum + t.a` 形态，避免为 receiver 或
// 累加器生成额外 MOVE。
func TestCompileFieldReadWriteUsesDirectRegisters(t *testing.T) {
	chunk := parseChunkForCodegenTest(t, `
local t = {a = 1}
local sum = 0
for i = 1, 10 do
  t.a = t.a + 1
  sum = sum + t.a
end
return sum
`)

	proto, err := CompileChunk(chunk, "field-read-write")
	if err != nil {
		// 字段读写样例必须可编译。
		t.Fatalf("compile field read write failed: %v", err)
	}
	if proto.MaxStackSize != 7 {
		// 官方 Lua 5.3 该形态使用 7 个槽；8 个槽说明仍有 receiver 临时 MOVE。
		t.Fatalf("unexpected max stack=%d code=%v", proto.MaxStackSize, proto.Code)
	}
	hasFieldUpdate := false
	hasFieldAccumulation := false
	for instructionIndex, instruction := range proto.Code {
		switch instruction.OpCode() {
		case bytecode.OpGetTable:
			if instruction.A() == 6 && instruction.B() == 0 && bytecode.IsK(instruction.C()) {
				if instructionIndex+2 < len(proto.Code) && proto.Code[instructionIndex+1].OpCode() == bytecode.OpAdd && proto.Code[instructionIndex+2].OpCode() == bytecode.OpSetTable {
					// t.a 自增应生成 GETTABLE R6, t, "a"; ADD R6; SETTABLE t, "a", R6。
					hasFieldUpdate = true
				}
				if instructionIndex+1 < len(proto.Code) && proto.Code[instructionIndex+1].OpCode() == bytecode.OpAdd && proto.Code[instructionIndex+1].A() == 1 {
					// sum 自累加应直接把 t.a 读入 R6，再 ADD 回 sum。
					hasFieldAccumulation = true
				}
			}
		case bytecode.OpMove:
			if instruction.A() == 6 || instruction.A() == 7 {
				// 旧形态会把 t 或 sum 复制到 R6/R7 后再 GETTABLE/ADD。
				t.Fatalf("unexpected hot-loop MOVE: %v code=%v", instruction, proto.Code)
			}
		}
	}
	if !hasFieldUpdate || !hasFieldAccumulation {
		// 字段自增和字段累加两个热路径都必须命中。
		t.Fatalf("missing direct field paths: update=%v accumulation=%v code=%v", hasFieldUpdate, hasFieldAccumulation, proto.Code)
	}
}

// TestCompileSelfBinaryGlobalRightAvoidsLeftMove 验证自二元赋值的全局右操作数对齐官方字节码。
//
// 官方 Lua 5.3 对 `sum = sum + x` 会先把全局 `x` 读取到临时寄存器，再直接生成
// `ADD sum, sum, temp`；不需要先把 `sum` MOVE 到临时寄存器。
func TestCompileSelfBinaryGlobalRightAvoidsLeftMove(t *testing.T) {
	chunk := parseChunkForCodegenTest(t, "x = 1 local i = 1 local sum = 0 while i <= 10 do sum = sum + x i = i + 1 end")

	proto, err := CompileChunk(chunk, "self-binary-global-right")
	if err != nil {
		// 全局右操作数自二元赋值样例必须可编译。
		t.Fatalf("compile self binary global right failed: %v", err)
	}
	for _, instruction := range proto.Code {
		if instruction.OpCode() == bytecode.OpMove && instruction.A() == 2 && instruction.B() == 1 {
			// 旧形态会把 sum 从 R1 复制到 R2，再用 R2 作为 ADD 左操作数。
			t.Fatalf("unexpected MOVE of sum before global right operand; code=%v", proto.Code)
		}
	}
	hasGlobalReadToTemp := false
	hasDirectAdd := false
	for _, instruction := range proto.Code {
		switch instruction.OpCode() {
		case bytecode.OpGetTabUp:
			if instruction.A() == 2 {
				// 右侧全局 x 应直接读入第一个临时寄存器 R2。
				hasGlobalReadToTemp = true
			}
		case bytecode.OpAdd:
			if instruction.A() == 1 && instruction.B() == 1 && instruction.C() == 2 {
				// sum 位于 R1，官方形态直接以 R1 作为左操作数并写回 R1。
				hasDirectAdd = true
			}
		}
	}
	if !hasGlobalReadToTemp || !hasDirectAdd {
		// 缺失任一关键指令都说明仍未对齐官方 Lua 的寄存器布局。
		t.Fatalf("missing direct global-right ADD; get=%v add=%v code=%v", hasGlobalReadToTemp, hasDirectAdd, proto.Code)
	}
}

// TestCompileSafeBinaryReturnUsesRKOperands 验证单返回值安全二元表达式直接复用参数寄存器。
//
// Lua 5.3 C codegen 对 `return a + b` 生成 `ADD temp, a, b; RETURN temp`，不需要先把 a/b
// MOVE 到额外临时寄存器。该形态能降低函数调用热循环中 leaf callee 的指令数。
func TestCompileSafeBinaryReturnUsesRKOperands(t *testing.T) {
	chunk := parseChunkForCodegenTest(t, "local function add(a, b) return a + b end return add(1, 2)")

	proto, err := CompileChunk(chunk, "safe-binary-return")
	if err != nil {
		// 函数调用样例必须可编译。
		t.Fatalf("compile safe binary return failed: %v", err)
	}
	if len(proto.Protos) != 1 {
		// 测试样例应只生成 add 一个子函数。
		t.Fatalf("unexpected child proto count: %d", len(proto.Protos))
	}
	child := proto.Protos[0]
	foundDirectAdd := false
	for _, instruction := range child.Code {
		switch instruction.OpCode() {
		case bytecode.OpAdd:
			if instruction.B() == 0 && instruction.C() == 1 {
				// a/b 参数分别位于 R0/R1，优化后 ADD 直接读取参数寄存器。
				foundDirectAdd = true
			}
		case bytecode.OpMove:
			if instruction.A() == 2 || instruction.A() == 3 {
				// 旧通用 return 路径会把 a/b 移到临时寄存器再 ADD。
				t.Fatalf("unexpected argument MOVE in child proto: %v", child.Code)
			}
		}
	}
	if !foundDirectAdd {
		// 子函数必须包含直接读取参数寄存器的 ADD。
		t.Fatalf("missing direct ADD for return a + b; code=%v", child.Code)
	}
}

// TestCompileSafeBinaryReturnReadsUpvalueDirectly 验证二元 return 快路径复用参数和 upvalue 寄存器。
//
// `return x + a` 中 a 为 upvalue 时，Lua 5.3 C codegen 会先 GETUPVAL 到结果寄存器，再用
// 参数 x 作为左操作数执行 ADD；不需要额外 MOVE x。
func TestCompileSafeBinaryReturnReadsUpvalueDirectly(t *testing.T) {
	chunk := parseChunkForCodegenTest(t, "local a = 1 local function f(x) return x + a end return f(2)")

	proto, err := CompileChunk(chunk, "safe-binary-return-upvalue")
	if err != nil {
		// upvalue 二元 return 样例必须可编译。
		t.Fatalf("compile safe binary return upvalue failed: %v", err)
	}
	if len(proto.Protos) != 1 {
		// 测试样例应只生成 f 一个子函数。
		t.Fatalf("unexpected child proto count: %d", len(proto.Protos))
	}
	child := proto.Protos[0]
	hasGetUpvalueToResult := false
	hasDirectAdd := false
	for _, instruction := range child.Code {
		switch instruction.OpCode() {
		case bytecode.OpGetUpval:
			if instruction.A() == 1 {
				// 结果寄存器 R1 直接承载 upvalue a。
				hasGetUpvalueToResult = true
			}
		case bytecode.OpAdd:
			if instruction.A() == 1 && instruction.B() == 0 && instruction.C() == 1 {
				// ADD 直接读取参数 x 和刚载入的 upvalue a。
				hasDirectAdd = true
			}
		case bytecode.OpMove:
			if instruction.A() == 1 {
				// 旧通用 return 路径会先把 x MOVE 到结果寄存器。
				t.Fatalf("unexpected MOVE into result register: %v", child.Code)
			}
		}
	}
	if !hasGetUpvalueToResult || !hasDirectAdd {
		// 子函数必须先 GETUPVAL 到结果寄存器，再直接 ADD 参数和 upvalue。
		t.Fatalf("missing direct upvalue binary return; get=%v add=%v code=%v", hasGetUpvalueToResult, hasDirectAdd, child.Code)
	}
}

// TestCompileSingleLocalReturnUsesSourceRegister 验证单 local 返回直接使用源寄存器。
//
// `return x` 不需要 MOVE 到临时寄存器；Lua 5.3 C codegen 直接生成 `RETURN x, 1`。
func TestCompileSingleLocalReturnUsesSourceRegister(t *testing.T) {
	chunk := parseChunkForCodegenTest(t, "local function id(x) return x end return id(1)")

	proto, err := CompileChunk(chunk, "single-local-return")
	if err != nil {
		// 单 local return 样例必须可编译。
		t.Fatalf("compile single local return failed: %v", err)
	}
	if len(proto.Protos) != 1 {
		// 测试样例应只生成 id 一个子函数。
		t.Fatalf("unexpected child proto count: %d", len(proto.Protos))
	}
	child := proto.Protos[0]
	if len(child.Code) == 0 || child.Code[0].OpCode() != bytecode.OpReturn || child.Code[0].A() != 0 || child.Code[0].B() != 2 {
		// 参数 x 位于 R0，应直接作为单返回值起点。
		t.Fatalf("missing direct RETURN R0; code=%v", child.Code)
	}
	for _, instruction := range child.Code {
		if instruction.OpCode() == bytecode.OpMove {
			// 旧通用 return 路径会 MOVE x 到临时寄存器。
			t.Fatalf("unexpected MOVE in single local return: %v", child.Code)
		}
	}
}

// TestCompileBinaryReturnReusesLeftCallArgumentSlots 验证 return 二元表达式会回收左侧调用实参槽。
//
// `return fib(x - 1) + fib(x - 2)` 的左侧 CALL 固定返回一个值；CALL 后 R2 参数槽可被右侧
// 调用复用，从而对齐 Lua 5.3 官方的 R1/R2 双调用结果形态。
func TestCompileBinaryReturnReusesLeftCallArgumentSlots(t *testing.T) {
	chunk := parseChunkForCodegenTest(t, "local function fib(x) if x < 2 then return x end return fib(x - 1) + fib(x - 2) end return fib(4)")

	proto, err := CompileChunk(chunk, "binary-return-left-call")
	if err != nil {
		// 递归二元返回样例必须可编译。
		t.Fatalf("compile binary return left call failed: %v", err)
	}
	if len(proto.Protos) != 1 {
		// 测试样例应只生成 fib 一个子函数。
		t.Fatalf("unexpected child proto count: %d", len(proto.Protos))
	}
	child := proto.Protos[0]
	if child.MaxStackSize != 4 {
		// 官方 Lua 5.3 在该形态下只需要 R0 参数、R1/R2 调用结果和 R3 参数槽。
		t.Fatalf("unexpected child max stack=%d code=%v", child.MaxStackSize, child.Code)
	}
	if !hasCall(child, 1, 2, 2) || !hasCall(child, 2, 2, 2) {
		// 左右两次递归调用应分别复用 R1 和 R2 作为函数/结果寄存器。
		t.Fatalf("expected CALL R1 and CALL R2; code=%v", child.Code)
	}
	hasDirectAdd := false
	for _, instruction := range child.Code {
		if instruction.OpCode() == bytecode.OpAdd && instruction.A() == 1 && instruction.B() == 1 && instruction.C() == 2 {
			// 两个调用结果应直接在 R1/R2 合成返回值。
			hasDirectAdd = true
		}
		if instruction.OpCode() == bytecode.OpGetUpval && instruction.A() == 3 {
			// 旧形态右侧调用会从 R3 开始，导致 MaxStackSize 被推高到 5。
			t.Fatalf("unexpected right call at R3; code=%v", child.Code)
		}
	}
	if !hasDirectAdd {
		// 没有直接 ADD 说明右侧调用结果仍没有复用左侧参数槽。
		t.Fatalf("missing direct ADD R1 R1 R2; code=%v", child.Code)
	}
}

// TestCompileCapturedBlockLocalKeepsCloseJump 验证被闭包捕获的 block local 仍生成 close-only JMP。
//
// 未捕获 local 的作用域退出可以省略零距离 close-only JMP；一旦内层函数捕获 block local，
// 退出 block 时必须保留 A>0 的 JMP，以便运行期关闭 open upvalue。
func TestCompileCapturedBlockLocalKeepsCloseJump(t *testing.T) {
	chunk := parseChunkForCodegenTest(t, "do local x = 1 local function f() return x end end")

	proto, err := CompileChunk(chunk, "captured-block-local")
	if err != nil {
		// 捕获 block local 的样例必须可编译。
		t.Fatalf("compile captured block local failed: %v", err)
	}
	hasCloseOnlyJump := false
	for _, instruction := range proto.Code {
		if instruction.OpCode() == bytecode.OpJmp && instruction.A() > 0 && instruction.SBx() == 0 {
			// A>0 表示从 A-1 起关闭 open upvalue；sBx=0 表示只关闭不跳转。
			hasCloseOnlyJump = true
		}
	}
	if !hasCloseOnlyJump {
		// 捕获 block local 时不能省略 close-only JMP。
		t.Fatalf("missing close-only JMP for captured block local; code=%v", proto.Code)
	}
}

// TestCompileNestedBlockCaptureMergesInlineCapturedLocal 验证嵌套 block 捕获会合并 inline captured 标记。
func TestCompileNestedBlockCaptureMergesInlineCapturedLocal(t *testing.T) {
	chunk := parseChunkForCodegenTest(t, "do local x = 1 do local function f() return x end end end")

	proto, err := CompileChunk(chunk, "nested-inline-capture")
	if err != nil {
		// 捕获外层 block inline local 的样例必须可编译。
		t.Fatalf("compile nested inline capture failed: %v", err)
	}
	hasCloseOnlyJump := false
	for _, instruction := range proto.Code {
		if instruction.OpCode() == bytecode.OpJmp && instruction.A() > 0 && instruction.SBx() == 0 {
			// 内层 block 捕获外层 x 后，外层 block 退出时仍必须关闭 open upvalue。
			hasCloseOnlyJump = true
		}
	}
	if !hasCloseOnlyJump {
		// snapshot 合并遗漏 captured 标记会导致这里缺少 close-only JMP。
		t.Fatalf("missing close-only JMP for nested inline capture; code=%v locals=%+v", proto.Code, proto.LocalVars)
	}
}

// TestCompileAssignmentExpandsLastCallResults 验证普通赋值会展开最后一个调用的多返回值。
//
// 官方 gc.lua 的 `s, i = string.gsub(...)` 依赖 CALL C 字段请求两个返回值；否则第二个左值
// 会被错误补 nil，导致替换次数断言失败。
func TestCompileAssignmentExpandsLastCallResults(t *testing.T) {
	chunk := parseChunkForCodegenTest(t, "s, i = string.gsub('1234', '(%d%d%d%d)', '')")

	proto, err := CompileChunk(chunk, "assign-call-results")
	if err != nil {
		// 普通多返回赋值样例必须可编译。
		t.Fatalf("compile assignment call results failed: %v", err)
	}
	callInstruction, ok := firstInstruction(proto, bytecode.OpCall)
	if !ok {
		// string.gsub 调用必须生成 CALL。
		t.Fatalf("expected CALL instruction")
	}
	if callInstruction.C() != 3 {
		// C=3 表示请求两个返回值，供 s 和 i 两个左值写回。
		t.Fatalf("CALL C=%d, want 3", callInstruction.C())
	}
}

// TestCompileUpvalueAssignmentUsesSetupVal 验证闭包内赋值会写回外层 upvalue。
//
// 官方 all.lua 的 showmem 会更新外层 local max；子函数赋值必须生成 SETUPVAL，而不是把
// 名称当作全局 `_ENV.max` 写入。
func TestCompileUpvalueAssignmentUsesSetupVal(t *testing.T) {
	chunk := parseChunkForCodegenTest(t, "local max = 0 local function f() max = 1 end f()")

	proto, err := CompileChunk(chunk, "upvalue-assign")
	if err != nil {
		// upvalue 赋值样例必须可编译。
		t.Fatalf("compile upvalue assignment failed: %v", err)
	}
	if len(proto.Protos) != 1 {
		// f 应生成一个子 Proto。
		t.Fatalf("unexpected child proto count=%d", len(proto.Protos))
	}
	child := proto.Protos[0]
	if len(child.Upvalues) != 1 || child.Upvalues[0].Name != "max" || !child.Upvalues[0].InStack {
		// 子函数必须捕获外层 local max。
		t.Fatalf("unexpected child upvalues=%+v", child.Upvalues)
	}
	if !containsOpCode(child, bytecode.OpSetupVal) {
		// max = 1 必须写回 upvalue。
		t.Fatalf("expected SETUPVAL instruction")
	}
}

// TestCompileAssignmentTargetUpvaluePrecedesRightHandUpvalue 验证赋值左值 upvalue 先于右值登记。
//
// 官方 calls.lua 会对 dump/load 后的闭包调用 debug.setupvalue；`a = 10 + b` 必须先把
// 左值 a 作为 upvalue 登记，再编译 RHS 中的 b，才能让 upvalue 顺序暴露为 a、b。
func TestCompileAssignmentTargetUpvaluePrecedesRightHandUpvalue(t *testing.T) {
	chunk := parseChunkForCodegenTest(t, `local a, b = 20, 30
return function (x)
  if x == "set" then
    a = 10 + b
    b = b + 1
  else
    return a
  end
end`)

	proto, err := CompileChunk(chunk, "upvalue-order")
	if err != nil {
		// upvalue 顺序样例必须可编译。
		t.Fatalf("compile upvalue order failed: %v", err)
	}
	if len(proto.Protos) != 1 {
		// return function 应生成一个子 Proto。
		t.Fatalf("unexpected child proto count=%d", len(proto.Protos))
	}
	child := proto.Protos[0]
	if len(child.Upvalues) != 2 {
		// 子函数应直接捕获 a 和 b 两个外层局部变量。
		t.Fatalf("unexpected child upvalue count=%d values=%+v", len(child.Upvalues), child.Upvalues)
	}
	if child.Upvalues[0].Name != "a" || child.Upvalues[1].Name != "b" {
		// debug.setupvalue 依赖左值 a 先于 RHS b 枚举。
		t.Fatalf("unexpected child upvalue order=%+v", child.Upvalues)
	}
	if !hasUpvalueInstruction(child, bytecode.OpSetupVal, 0) || !hasUpvalueInstruction(child, bytecode.OpGetUpval, 1) {
		// a = 10 + b 需要 SETUPVAL 写回 a，并通过 GETUPVAL 读取 b；重排后索引必须同步。
		t.Fatalf("missing remapped upvalue instructions code=%+v", child.Code)
	}
}

// TestCompileManyUpvalueSumStaysWithinRKRegisterLimit 验证长 upvalue 累加不生成 RK 溢出寄存器。
//
// 官方 calls.lua 会构造 200 个 upvalue 的求和函数；ADD 的 B/C 字段与 RK 编码共享 9 位，
// 寄存器下标必须低于 BitRK，否则运行期会把寄存器误判为常量。
func TestCompileManyUpvalueSumStaysWithinRKRegisterLimit(t *testing.T) {
	sourceParts := []string{"local a1"}
	for index := 2; index <= 200; index++ {
		// 构造 200 个 local 名称，匹配官方 calls.lua 的压力用例。
		sourceParts = append(sourceParts, fmt.Sprintf(", a%d", index))
	}
	sourceParts = append(sourceParts, " = 1")
	for index := 2; index <= 200; index++ {
		// 每个 local 初始化为对应整数。
		sourceParts = append(sourceParts, fmt.Sprintf(", %d", index))
	}
	sourceParts = append(sourceParts, "; return function () return a1")
	for index := 2; index <= 200; index++ {
		// 子函数返回所有 upvalue 的加法链。
		sourceParts = append(sourceParts, fmt.Sprintf(" + a%d", index))
	}
	sourceParts = append(sourceParts, " end")
	chunk := parseChunkForCodegenTest(t, strings.Join(sourceParts, ""))

	proto, err := CompileChunk(chunk, "many-upvalues")
	if err != nil {
		// 大量 upvalue 求和样例必须可编译。
		t.Fatalf("compile many upvalues failed: %v", err)
	}
	if len(proto.Protos) != 1 {
		// return function 应生成一个子 Proto。
		t.Fatalf("unexpected child proto count=%d", len(proto.Protos))
	}
	child := proto.Protos[0]
	if int(child.MaxStackSize) >= bytecode.BitRK {
		// MaxStackSize 达到 BitRK 会让 ADD B/C 字段中的寄存器被解释为常量。
		t.Fatalf("child max stack too high=%d", child.MaxStackSize)
	}
	for _, instruction := range child.Code {
		// 所有 ADD 的 B/C 寄存器操作数必须低于 RK 常量标记位。
		if instruction.OpCode() == bytecode.OpAdd && (instruction.B() >= bytecode.BitRK || instruction.C() >= bytecode.BitRK) {
			t.Fatalf("ADD uses RK-overflow register: %#v", instruction)
		}
	}
}

// TestCompileChunkMethodCall 验证冒号调用和点号字段访问 codegen。
//
// `io.stderr:write("x")` 形态需要先通过 `_ENV` 读取 io，再 GETTABLE stderr，最后 SELF/CALL。
func TestCompileChunkMethodCall(t *testing.T) {
	chunk := parseChunkForCodegenTest(t, "io.stderr:write(\"x\")")

	proto, err := CompileChunk(chunk, "method")
	if err != nil {
		// 官方测试入口常见的 method call 语句应可编译。
		t.Fatalf("compile method call failed: %v", err)
	}
	if !containsOpCode(proto, bytecode.OpGetTabUp) || !containsOpCode(proto, bytecode.OpGetTable) {
		// 全局 io 读取和 stderr 字段读取必须分别生成 GETTABUP/GETTABLE。
		t.Fatalf("expected global and field access instructions")
	}
	selfInstruction, ok := firstInstruction(proto, bytecode.OpSelf)
	if !ok {
		// 冒号调用必须生成 SELF。
		t.Fatalf("expected SELF instruction")
	}
	callInstruction, ok := firstInstruction(proto, bytecode.OpCall)
	if !ok || callInstruction.B() != 3 || callInstruction.C() != 1 {
		// 一个显式参数加隐式 self，所以 CALL B=3；语句调用丢弃返回值 C=1。
		t.Fatalf("unexpected method CALL: self=%v call=%v ok=%v", selfInstruction, callInstruction, ok)
	}
	if !hasStringConstant(proto, "io") || !hasStringConstant(proto, "stderr") || !hasStringConstant(proto, "write") {
		// 全局名、字段名和方法名都应进入常量池。
		t.Fatalf("missing method constants=%+v", proto.Constants)
	}
}

// TestCompileChunkTailCall 验证 return f(...) 会生成 TAILCALL。
//
// 子函数 g 返回外层 f 调用时，应通过 upvalue 读取 f 并生成尾调用。
func TestCompileChunkTailCall(t *testing.T) {
	chunk := parseChunkForCodegenTest(t, "local function f(a) return a end local function g(x) return f(x) end")

	proto, err := CompileChunk(chunk, "tail")
	if err != nil {
		// 尾调用样例不应编译失败。
		t.Fatalf("compile tail call failed: %v", err)
	}
	if len(proto.Protos) != 2 {
		// f 和 g 应各自生成一个子 Proto。
		t.Fatalf("unexpected child proto count=%d", len(proto.Protos))
	}
	gProto := proto.Protos[1]
	tailInstruction, ok := firstInstruction(gProto, bytecode.OpTailCall)
	if !ok {
		// g 的 return f(x) 必须生成 TAILCALL。
		t.Fatalf("expected TAILCALL instruction")
	}
	if tailInstruction.B() != 2 || tailInstruction.C() != 0 {
		// 一个参数 B=2，开放返回 C=0。
		t.Fatalf("unexpected TAILCALL fields: B=%d C=%d", tailInstruction.B(), tailInstruction.C())
	}
	if tailInstruction.A() == 0 {
		// 参数 x 位于 R0，尾调用函数寄存器不能覆盖它，否则实参会读到被调函数本身。
		t.Fatalf("TAILCALL should not use argument register A=0")
	}
}

// TestCompileChunkMethodTailCall 验证 return self:f(...) 会生成 TAILCALL。
//
// 官方 calls.lua 的 `a:deep(30000)` 依赖 method tail call 不增长调用深度；SELF 后必须使用
// TAILCALL，并把隐式 self 计入 B 字段。
func TestCompileChunkMethodTailCall(t *testing.T) {
	chunk := parseChunkForCodegenTest(t, "function a:deep(n) if n > 0 then return self:deep(n - 1) else return 101 end end")

	proto, err := CompileChunk(chunk, "method-tail")
	if err != nil {
		// method 尾调用样例不应编译失败。
		t.Fatalf("compile method tail call failed: %v", err)
	}
	if len(proto.Protos) != 1 {
		// a:deep 函数体应生成一个子 Proto。
		t.Fatalf("unexpected child proto count=%d", len(proto.Protos))
	}
	child := proto.Protos[0]
	if !containsOpCode(child, bytecode.OpSelf) {
		// 冒号调用仍必须生成 SELF，以准备 method 和 self。
		t.Fatalf("expected SELF instruction")
	}
	tailInstruction, ok := firstInstruction(child, bytecode.OpTailCall)
	if !ok {
		// return self:deep(...) 必须生成 TAILCALL。
		t.Fatalf("expected method TAILCALL instruction")
	}
	if tailInstruction.B() != 3 || tailInstruction.C() != 0 {
		// 一个显式参数加隐式 self，所以 TAILCALL B=3；C=0 表示尾调用开放返回。
		t.Fatalf("unexpected method TAILCALL fields: B=%d C=%d", tailInstruction.B(), tailInstruction.C())
	}
}

// TestCompileChunkVarargAndReturn 验证 vararg 表达式和显式 return codegen。
//
// 显式 return 后不应再追加默认 RETURN。
func TestCompileChunkVarargAndReturn(t *testing.T) {
	chunk := parseChunkForCodegenTest(t, "local function f(...) return ... end")

	proto, err := CompileChunk(chunk, "vararg")
	if err != nil {
		// vararg 函数应可编译。
		t.Fatalf("compile vararg failed: %v", err)
	}
	if !proto.IsVararg {
		// 顶层 chunk 也必须按 vararg 函数编译，CLI 脚本参数通过 `...` 传入。
		t.Fatalf("top-level proto should be vararg")
	}
	child := proto.Protos[0]
	if !child.IsVararg {
		// 函数体必须保留 vararg 标记。
		t.Fatalf("child proto should be vararg")
	}
	if !containsOpCode(child, bytecode.OpVararg) {
		// return ... 应先生成 VARARG。
		t.Fatalf("expected VARARG instruction")
	}
	if countOpCode(child, bytecode.OpReturn) != 1 {
		// 显式 return 后不能再补默认 return。
		t.Fatalf("expected exactly one RETURN")
	}
	returnInstruction, _ := firstInstruction(child, bytecode.OpReturn)
	if returnInstruction.B() != 0 {
		// return ... 必须使用开放返回，RETURN B=0 让 VM 按运行期 vararg 数量返回。
		t.Fatalf("unexpected RETURN B=%d", returnInstruction.B())
	}

	localChunk := parseChunkForCodegenTest(t, "local a, b, c = ...")
	localProto, err := CompileChunk(localChunk, "local-vararg")
	if err != nil {
		// 顶层 local vararg 初始化应可编译。
		t.Fatalf("compile local vararg failed: %v", err)
	}
	varargInstruction, ok := firstInstruction(localProto, bytecode.OpVararg)
	if !ok || varargInstruction.B() != 4 {
		// local a,b,c = ... 应固定展开 3 个值，VARARG B=4。
		t.Fatalf("local vararg instruction = %#v ok=%v", varargInstruction, ok)
	}

	tableChunk := parseChunkForCodegenTest(t, "local t = {...}")
	tableProto, err := CompileChunk(tableChunk, "table-vararg")
	if err != nil {
		// table constructor 末尾 vararg 应可编译。
		t.Fatalf("compile table vararg failed: %v", err)
	}
	tableVarargInstruction, ok := firstInstruction(tableProto, bytecode.OpVararg)
	if !ok || tableVarargInstruction.B() != 0 {
		// `{...}` 必须使用开放 VARARG，才能把全部脚本参数写入数组字段。
		t.Fatalf("table vararg instruction = %#v ok=%v", tableVarargInstruction, ok)
	}
	tableSetListInstruction, ok := firstInstruction(tableProto, bytecode.OpSetList)
	if !ok || tableSetListInstruction.B() != 0 {
		// trailing vararg 的 table constructor 必须使用开放 SETLIST。
		t.Fatalf("table setlist instruction = %#v ok=%v", tableSetListInstruction, ok)
	}
}

// TestCompileChunkNumericFor 验证 numeric for 生成 FORPREP/FORLOOP。
//
// 循环变量在 body 中作为 local 可读，FORLOOP 应回跳到循环体起点。
func TestCompileChunkNumericFor(t *testing.T) {
	chunk := parseChunkForCodegenTest(t, "for i = 1, 3, 1 do local x = i end")

	proto, err := CompileChunk(chunk, "for")
	if err != nil {
		// numeric for 子集应可编译。
		t.Fatalf("compile numeric for failed: %v", err)
	}
	forPrep, ok := firstInstruction(proto, bytecode.OpForPrep)
	if !ok {
		// numeric for 必须生成 FORPREP。
		t.Fatalf("expected FORPREP instruction")
	}
	forLoop, ok := firstInstruction(proto, bytecode.OpForLoop)
	if !ok {
		// numeric for 必须生成 FORLOOP。
		t.Fatalf("expected FORLOOP instruction")
	}
	if forPrep.A() != forLoop.A() {
		// FORPREP 和 FORLOOP 必须共享同一个基准寄存器。
		t.Fatalf("for base register mismatch: prep=%d loop=%d", forPrep.A(), forLoop.A())
	}
	if forPrep.SBx() <= 0 {
		// FORPREP 必须先跳到 FORLOOP，让 FORLOOP 初始化外部循环变量后再进入 body。
		t.Fatalf("expected FORPREP to jump forward to FORLOOP, got %d", forPrep.SBx())
	}
	if forLoop.SBx() >= 0 {
		// FORLOOP 必须向后跳回循环体。
		t.Fatalf("expected backward FORLOOP jump, got %d", forLoop.SBx())
	}
}

// TestCompileChunkGenericFor 验证 generic for 生成 TFORCALL/TFORLOOP。
//
// 当前最小实现覆盖迭代器三元组、迭代变量寄存器和循环回跳结构。
func TestCompileChunkGenericFor(t *testing.T) {
	chunk := parseChunkForCodegenTest(t, "local iter local state local ctrl for k, v in iter, state, ctrl do local x = k end")

	proto, err := CompileChunk(chunk, "generic")
	if err != nil {
		// generic for 子集应可编译。
		t.Fatalf("compile generic for failed: %v", err)
	}
	tforCall, ok := firstInstruction(proto, bytecode.OpTForCall)
	if !ok {
		// generic for 必须生成 TFORCALL。
		t.Fatalf("expected TFORCALL instruction")
	}
	tforLoop, ok := firstInstruction(proto, bytecode.OpTForLoop)
	if !ok {
		// generic for 必须生成 TFORLOOP。
		t.Fatalf("expected TFORLOOP instruction")
	}
	if tforCall.C() != 2 {
		// 两个迭代变量要求 TFORCALL 返回两个值。
		t.Fatalf("unexpected TFORCALL result count=%d", tforCall.C())
	}
	if tforLoop.A() != tforCall.A()+2 {
		// TFORLOOP A 应指向控制变量寄存器。
		t.Fatalf("unexpected TFORLOOP A=%d call A=%d", tforLoop.A(), tforCall.A())
	}
	if tforLoop.SBx() >= 0 {
		// TFORLOOP 必须向后回跳到循环体。
		t.Fatalf("expected backward TFORLOOP jump, got %d", tforLoop.SBx())
	}
	if !hasLocalVar(proto, "k") || !hasLocalVar(proto, "v") {
		// 迭代变量必须登记到 local debug info。
		t.Fatalf("missing iterator locals=%+v", proto.LocalVars)
	}
}

// TestCompileGenericForExpandsIteratorCall 验证泛型 for 会展开最后一个迭代调用三元组。
//
// `for k,v in pairs(a)` 依赖 pairs 返回 iterator/state/control；CALL 必须请求三个返回值，
// 否则 TFORCALL 会把 nil 当作 table 状态传给 next。
func TestCompileGenericForExpandsIteratorCall(t *testing.T) {
	chunk := parseChunkForCodegenTest(t, "local a = {}; for k, v in pairs(a) do local x = v end")

	proto, err := CompileChunk(chunk, "generic-pairs")
	if err != nil {
		// pairs 泛型 for 样例必须可编译。
		t.Fatalf("compile generic pairs failed: %v", err)
	}
	callInstruction, ok := firstInstruction(proto, bytecode.OpCall)
	if !ok {
		// pairs(a) 必须生成 CALL。
		t.Fatalf("expected CALL for pairs")
	}
	if callInstruction.C() != 4 {
		// C=4 表示请求三个返回值：iterator/state/control。
		t.Fatalf("pairs CALL C=%d, want 4", callInstruction.C())
	}
}

// TestCompileChunkDisassemblyGolden 验证 codegen 关键样例反汇编输出稳定。
//
// 该 golden 用于对齐当前项目的 luac 风格关键字段：opcode、常量、locals、upvalues 和子 Proto。
func TestCompileChunkDisassemblyGolden(t *testing.T) {
	chunk := parseChunkForCodegenTest(t, "local a = 1 local function f(x) return x + a end for i = 1, 2 do a = a + i end")

	proto, err := CompileChunk(chunk, "sample")
	if err != nil {
		// golden 样例必须能完成 codegen。
		t.Fatalf("compile disassembly sample failed: %v", err)
	}
	got := bytecode.DisassembleProto(proto)
	goldenPath := filepath.Join("..", "..", "tests", "golden", "codegen_disassemble.golden")
	expectedBytes, err := os.ReadFile(goldenPath)
	if err != nil {
		// golden 文件缺失表示测试资产不完整。
		t.Fatalf("read codegen golden failed: %v", err)
	}
	expectedText := strings.ReplaceAll(string(expectedBytes), "\r\n", "\n")
	if got != expectedText {
		// 反汇编输出必须保持稳定，便于后续与官方 Lua 样例逐步对齐。
		t.Fatalf("codegen disassembly golden mismatch:\n got:\n%s\nwant:\n%s", got, expectedText)
	}
}

// parseChunkForCodegenTest 解析测试用 Lua 源码。
//
// 该 helper 将 parser 失败视为测试夹具错误，便于测试专注 codegen 行为。
func parseChunkForCodegenTest(t *testing.T, input string) *parser.Chunk {
	t.Helper()
	chunk, err := parser.New(input).ParseChunk()
	if err != nil {
		// 测试输入必须先通过 parser。
		t.Fatalf("parse test chunk failed: %v", err)
	}

	// 返回可供 codegen 使用的 AST。
	return chunk
}

// containsOpCode 判断 Proto 中是否包含指定 opcode。
//
// 该 helper 用于测试关键指令是否生成，避免断言完整指令序列过早绑定实现细节。
func containsOpCode(proto *bytecode.Proto, opCode bytecode.OpCode) bool {
	for _, instruction := range proto.Code {
		// 任一指令 opcode 匹配即可返回 true。
		if instruction.OpCode() == opCode {
			return true
		}
	}

	// 遍历后没有找到目标 opcode。
	return false
}

// containsGetTableWithRegisterKey 判断 Proto 中是否存在使用寄存器 key 的 GETTABLE。
//
// RK 参数未设置 BitRK 时表示寄存器下标，可用于确认高位常量 key 已降级到临时寄存器。
func containsGetTableWithRegisterKey(proto *bytecode.Proto) bool {
	for _, instruction := range proto.Code {
		// 只检查 GETTABLE 的 C 操作数。
		if instruction.OpCode() == bytecode.OpGetTable && !bytecode.IsK(instruction.C()) {
			return true
		}
	}

	// 没有找到寄存器 key 形式的 GETTABLE。
	return false
}

// countOpCode 统计 Proto 中指定 opcode 数量。
//
// 用于验证短路逻辑这类有明确指令形态的 codegen 输出。
func countOpCode(proto *bytecode.Proto, opCode bytecode.OpCode) int {
	count := 0
	for _, instruction := range proto.Code {
		// 匹配目标 opcode 时累加。
		if instruction.OpCode() == opCode {
			count++
		}
	}

	// 返回匹配数量。
	return count
}

// hasTestWithC 判断 Proto 中是否存在指定 C 字段的 TEST 指令。
//
// and/or 短路分别依赖 C=0 和 C=1 区分真假期望。
func hasTestWithC(proto *bytecode.Proto, c int) bool {
	for _, instruction := range proto.Code {
		// 只检查 TEST 指令的 C 字段。
		if instruction.OpCode() == bytecode.OpTest && instruction.C() == c {
			return true
		}
	}

	// 没有找到匹配 TEST 指令。
	return false
}

// hasTestSetWithC 判断 Proto 中是否存在指定 C 字段的 TESTSET 指令。
//
// local 左操作数短路表达式使用 TESTSET 合并复制和测试，and/or 分别依赖 C=0/C=1。
func hasTestSetWithC(proto *bytecode.Proto, c int) bool {
	for _, instruction := range proto.Code {
		// 只检查 TESTSET 指令的 C 字段。
		if instruction.OpCode() == bytecode.OpTestSet && instruction.C() == c {
			return true
		}
	}

	// 没有找到匹配 TESTSET 指令。
	return false
}

// hasCall 判断 Proto 中是否存在指定 A/B/C 字段的 CALL 指令。
func hasCall(proto *bytecode.Proto, a int, b int, c int) bool {
	for _, instruction := range proto.Code {
		// CALL 指令字段精确匹配时返回 true。
		if instruction.OpCode() == bytecode.OpCall && instruction.A() == a && instruction.B() == b && instruction.C() == c {
			return true
		}
	}

	// 没有找到匹配 CALL 指令。
	return false
}

// hasGetTabUpAtOrAfter 判断指定 PC 之后是否存在写入目标寄存器的 GETTABUP。
func hasGetTabUpAtOrAfter(proto *bytecode.Proto, targetRegister int, startPC int) bool {
	for instructionIndex, instruction := range proto.Code {
		// 只检查指定 PC 之后的 GETTABUP 写入。
		if instructionIndex >= startPC && instruction.OpCode() == bytecode.OpGetTabUp && instruction.A() == targetRegister {
			return true
		}
	}

	// 没有找到匹配 GETTABUP 指令。
	return false
}

// hasJumpBeyondOne 判断 Proto 中是否存在 sBx 大于 1 的 JMP。
//
// 该 helper 用于确认跳转回填不是固定写死的小偏移。
func hasJumpBeyondOne(proto *bytecode.Proto) bool {
	for _, instruction := range proto.Code {
		// 只检查 JMP 指令的有符号偏移。
		if instruction.OpCode() == bytecode.OpJmp && instruction.SBx() > 1 {
			return true
		}
	}

	// 没有找到大于 1 的跳转偏移。
	return false
}

// hasBackwardJump 判断 Proto 中是否存在负向 JMP。
//
// while/repeat 这类循环需要回跳到循环头，负向 sBx 是最小结构特征。
func hasBackwardJump(proto *bytecode.Proto) bool {
	for _, instruction := range proto.Code {
		// 只检查 JMP 指令的有符号偏移。
		if instruction.OpCode() == bytecode.OpJmp && instruction.SBx() < 0 {
			return true
		}
	}

	// 没有找到负向跳转偏移。
	return false
}

// hasForwardJump 判断 Proto 中是否存在正向 JMP。
//
// break 和条件失败路径都依赖正向跳转离开当前控制流区域。
func hasForwardJump(proto *bytecode.Proto) bool {
	for _, instruction := range proto.Code {
		// 只检查 JMP 指令的有符号偏移。
		if instruction.OpCode() == bytecode.OpJmp && instruction.SBx() > 0 {
			return true
		}
	}

	// 没有找到正向跳转偏移。
	return false
}

// hasIntegerConstant 判断 Proto 常量表是否包含指定 integer。
//
// table constructor 测试用它确认 key 和字段字面量都已写入常量表。
func hasIntegerConstant(proto *bytecode.Proto, value int64) bool {
	for _, constant := range proto.Constants {
		// 只比较 integer 常量。
		if constant.Kind == bytecode.ConstantInteger && constant.Integer == value {
			return true
		}
	}

	// 常量表中没有目标 integer。
	return false
}

// hasStringConstant 判断 Proto 常量表是否包含指定 string。
//
// 全局 `_ENV` 访问测试用它确认全局 key 已进入常量池。
func hasStringConstant(proto *bytecode.Proto, value string) bool {
	for _, constant := range proto.Constants {
		// 只比较 string 常量。
		if constant.Kind == bytecode.ConstantString && constant.String == value {
			return true
		}
	}

	// 常量表中没有目标 string。
	return false
}

// hasLineInfo 判断 Proto 行号表是否包含指定源码行。
//
// 入参 proto 必须非 nil；返回 true 表示至少一条指令归属该源码行。
func hasLineInfo(proto *bytecode.Proto, line int) bool {
	for _, lineInfo := range proto.LineInfo {
		// 行号匹配时即可确认 debug 活跃行数据可用。
		if lineInfo == line {
			// 找到目标行后提前返回，避免继续扫描无意义条目。
			return true
		}
	}

	// 没有任何指令映射到目标行。
	return false
}

// hasCallLineInfo 判断 Proto 是否存在指定行号的 CALL 指令。
//
// return 表达式行号测试需要确认函数调用本身被标注到表达式行，而不是只存在普通 LOADK 行号。
func hasCallLineInfo(proto *bytecode.Proto, line int) bool {
	for pc, instruction := range proto.Code {
		// 只检查普通 CALL；TAILCALL 由尾调用测试单独覆盖。
		if instruction.OpCode() != bytecode.OpCall {
			continue
		}
		if pc < len(proto.LineInfo) && proto.LineInfo[pc] == line {
			// CALL 指令对应行号命中目标行。
			return true
		}
	}

	// 没有目标行号的 CALL 指令。
	return false
}

// hasLocalVar 判断 Proto 是否登记了指定局部变量名。
//
// generic for 测试用它确认迭代变量进入 debug info。
func hasLocalVar(proto *bytecode.Proto, name string) bool {
	for _, localVar := range proto.LocalVars {
		// 名称匹配即可认为已登记。
		if localVar.Name == name {
			return true
		}
	}

	// 没有找到目标局部变量名。
	return false
}

// disassemblyContainsAll 判断反汇编文本是否包含所有片段。
//
// 保留该 helper 供后续官方样例扩展时做局部断言。
func disassemblyContainsAll(text string, fragments ...string) bool {
	for _, fragment := range fragments {
		// 任一片段缺失都返回 false。
		if !strings.Contains(text, fragment) {
			return false
		}
	}

	// 所有片段均存在。
	return true
}

// hasMove 判断 Proto 中是否包含指定源和目标寄存器的 MOVE。
func hasMove(proto *bytecode.Proto, targetRegister int, sourceRegister int) bool {
	for _, instruction := range proto.Code {
		// MOVE 指令的 A 是目标寄存器，B 是源寄存器。
		if instruction.OpCode() == bytecode.OpMove && instruction.A() == targetRegister && instruction.B() == sourceRegister {
			// 找到匹配 MOVE 后立即返回。
			return true
		}
	}

	// 没有找到匹配 MOVE。
	return false
}

// hasUpvalueInstruction 判断 Proto 是否包含引用指定 upvalue 下标的指令。
//
// opCode 只用于 GETUPVAL、SETUPVAL、GETTABUP 和 SETTABUP 这类携带 upvalue 下标的
// ABC 指令；返回 true 表示重排后的索引已经被对应指令使用。
func hasUpvalueInstruction(proto *bytecode.Proto, opCode bytecode.OpCode, upvalueIndex int) bool {
	for _, instruction := range proto.Code {
		// 非目标 opcode 不参与匹配，避免把其他字段误判为 upvalue 下标。
		if instruction.OpCode() != opCode {
			continue
		}
		switch opCode {
		case bytecode.OpSetTabUp:
			// SETTABUP 的 A 字段保存 upvalue 下标。
			if instruction.A() == upvalueIndex {
				return true
			}
		default:
			// GETUPVAL、SETUPVAL 和 GETTABUP 的 B 字段保存 upvalue 下标。
			if instruction.B() == upvalueIndex {
				return true
			}
		}
	}

	// 没有找到引用指定 upvalue 下标的目标指令。
	return false
}

// firstInstruction 返回 Proto 中第一条指定 opcode 指令。
//
// ok=false 表示没有找到目标 opcode。
func firstInstruction(proto *bytecode.Proto, opCode bytecode.OpCode) (bytecode.Instruction, bool) {
	for _, instruction := range proto.Code {
		// 找到目标 opcode 后立即返回。
		if instruction.OpCode() == opCode {
			return instruction, true
		}
	}

	// 未找到目标 opcode。
	return 0, false
}
