package iolib

import (
	"errors"
	gio "io"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"

	"github.com/ZingYao/go-lua-vm/runtime"
)

// countingFlushCloser 记录测试 file 的 flush 与 close 调用次数。
type countingFlushCloser struct {
	// flushCount 记录 Flush 被调用的次数。
	flushCount int
	// closeCount 记录 Close 被调用的次数。
	closeCount int
	// flushErr 是 Flush 需要返回的测试错误。
	flushErr error
	// closeErr 是 Close 需要返回的测试错误。
	closeErr error
}

// restoreDefaultFiles 还原包级默认输入输出，避免测试之间互相污染。
//
// 返回函数必须通过 defer 调用；该 helper 只用于测试包内状态复原，不改变正式库行为。
func restoreDefaultFiles(input *File, output *File, stderr *File) func() {
	// 捕获旧值并返回恢复闭包。
	return func() {
		// 恢复默认输入、输出和错误流。
		defaultInput = input
		defaultOutput = output
		defaultError = stderr
	}
}

// Flush 实现测试刷新接口。
//
// 每次调用都会递增 flushCount，并返回预设错误，用于验证 io.flush 的成功和失败路径。
func (recorder *countingFlushCloser) Flush() error {
	// 测试替身记录刷新调用次数。
	recorder.flushCount++
	return recorder.flushErr
}

// Close 实现测试关闭接口。
//
// 每次调用都会递增 closeCount，并返回预设错误，用于验证 io.close 的成功和失败路径。
func (recorder *countingFlushCloser) Close() error {
	// 测试替身记录关闭调用次数。
	recorder.closeCount++
	return recorder.closeErr
}

// TestOpenRegistersIOLibraryAndStandardFiles 验证 Open 注册 io 库基础字段。
//
// 该用例覆盖标准流 file userdata、io.close 与 io.flush 的注册形态，确保后续 VM CALL 可以
// 通过 Go closure 调用这些函数。
func TestOpenRegistersIOLibraryAndStandardFiles(t *testing.T) {
	// 测试先创建独立 State，避免污染其他标准库测试。
	state := runtime.NewState()
	if err := Open(state); err != nil {
		// Open 不应在有效 State 上失败。
		t.Fatalf("Open failed: %v", err)
	}

	ioValue := state.GetGlobal("io")
	if ioValue.Kind != runtime.KindTable {
		// io 全局必须是 table。
		t.Fatalf("io global kind = %v, want table", ioValue.Kind)
	}
	ioTable := ioValue.Ref.(*runtime.Table)
	for _, name := range []string{"close", "flush", "input", "lines", "open", "output", "popen", "read", "tmpfile", "type", "write"} {
		// close/flush 必须注册为 Go closure。
		value := ioTable.RawGetString(name)
		if value.Kind != runtime.KindGoClosure {
			// 函数字段类型错误会导致后续 VM CALL 失败。
			t.Fatalf("io.%s kind = %v, want Go closure", name, value.Kind)
		}
	}
	for _, name := range []string{"stdin", "stdout", "stderr"} {
		// 标准流必须以 userdata 暴露。
		value := ioTable.RawGetString(name)
		if value.Kind != runtime.KindUserdata {
			// 标准流不是 userdata 会破坏 file API 统一入口。
			t.Fatalf("io.%s kind = %v, want userdata", name, value.Kind)
		}
		file, err := fileArgument([]runtime.Value{value}, 1, name)
		if err != nil {
			// 注册出的 userdata 必须能被 fileArgument 识别。
			t.Fatalf("io.%s is not file userdata: %v", name, err)
		}
		if file.Name() != name {
			// 文件名称用于错误消息和调试输出，应与字段名一致。
			t.Fatalf("io.%s file name = %q", name, file.Name())
		}
		userdata := value.Ref.(*runtime.Userdata)
		metatable := userdata.GetMetatable()
		if metatable == nil {
			// file userdata 必须带方法元表，支持 file:write 等冒号调用。
			t.Fatalf("io.%s userdata metatable is nil", name)
		}
		nameValue := metatable.RawGetString("__name")
		if nameValue.Kind != runtime.KindString || nameValue.String != "FILE*" {
			// file userdata 错误消息依赖 __name 展示官方 FILE* 类型名。
			t.Fatalf("io.%s __name = %v, want FILE*", name, nameValue.DebugString())
		}
		gcValue := metatable.RawGetString("__gc")
		if gcValue.Kind != runtime.KindGoClosure {
			// 官方 errors.lua 会直接调用 getmetatable(io.stdin).__gc()。
			t.Fatalf("io.%s __gc kind = %v, want Go closure", name, gcValue.Kind)
		}
		indexValue := metatable.RawGetString("__index")
		if indexValue.Kind != runtime.KindTable {
			// __index 必须是方法表。
			t.Fatalf("io.%s __index kind = %v, want table", name, indexValue.Kind)
		}
		methods := indexValue.Ref.(*runtime.Table)
		if writeMethod := methods.RawGetString("write"); writeMethod.Kind != runtime.KindGoClosure {
			// file:write 是官方测试最早触达的 file 方法，必须可由 SELF 读取。
			t.Fatalf("io.%s write method kind = %v, want Go closure", name, writeMethod.Kind)
		}
	}
}

// TestFileArgumentReportsMetatableName 验证 file 参数错误展示元表 `__name`。
//
// 参数必须是 Lua 值；非 file 且带 `__name` 的 table 应在错误文本中显示用户类型名，兼容
// Lua 5.3 errors.lua 的 named object 断言。
func TestFileArgumentReportsMetatableName(t *testing.T) {
	// 构造带 __name 的 table，模拟 setmetatable({}, {__name="My Type"})。
	table := runtime.NewTable()
	metatable := runtime.NewTable()
	metatable.RawSetString("__name", runtime.StringValue("My Type"))
	table.SetMetatable(metatable)

	_, err := fileArgument([]runtime.Value{runtime.ReferenceValue(runtime.KindTable, table)}, 1, "input")
	if err == nil {
		// 非 file 参数必须返回 Lua 参数错误。
		t.Fatalf("fileArgument with named table succeeded")
	}
	if !errors.Is(err, runtime.ErrLuaError) || !strings.Contains(runtime.ErrorObject(err).String, "FILE* expected, got My Type") {
		// 错误文本必须包含 got My Type，不能退回 table。
		t.Fatalf("fileArgument error = %v", err)
	}
}

// TestFileUserdataCloseAndFlush 验证普通 file userdata 的刷新与关闭路径。
//
// 该用例覆盖显式传入 file 的 io.flush/io.close，并确认重复 close 按 Lua 5.3 报错且不重复释放底层资源。
func TestFileUserdataCloseAndFlush(t *testing.T) {
	// 测试替身同时实现 Flusher 与 Closer。
	recorder := &countingFlushCloser{}
	file := NewFile("test-file", nil, nil, recorder, recorder)
	value := NewFileValue(nil, file)

	if _, err := Flush(value); err != nil {
		// 有效 file flush 不应失败。
		t.Fatalf("Flush failed: %v", err)
	}
	if recorder.flushCount != 1 {
		// flush 必须调用底层 Flusher 一次。
		t.Fatalf("flush count = %d, want 1", recorder.flushCount)
	}
	closeResult, err := Close(value)
	if err != nil {
		// 有效 file close 不应失败。
		t.Fatalf("Close failed: %v", err)
	}
	if len(closeResult) != 1 || closeResult[0].Kind != runtime.KindBoolean || !closeResult[0].Bool {
		// io.close 成功时必须返回 true，满足 assert(io.close()) 的官方套件路径。
		t.Fatalf("Close result = %#v, want true", closeResult)
	}
	if !file.Closed() {
		// close 成功后 file 必须标记为 closed。
		t.Fatalf("file should be closed")
	}
	if recorder.closeCount != 1 {
		// close 必须调用底层 Closer 一次。
		t.Fatalf("close count = %d, want 1", recorder.closeCount)
	}
	secondCloseResult, err := Close(value)
	if !errors.Is(err, runtime.ErrLuaError) {
		// Lua 可见重复 close 必须抛出 closed file 错误。
		t.Fatalf("second Close error = %v, want Lua error", err)
	}
	if secondCloseResult != nil || !strings.Contains(runtime.ErrorObject(err).String, "closed file") {
		// 错误文本需要包含 closed file，满足官方 files.lua 的 checkerr 断言。
		t.Fatalf("second Close result = %#v, error = %q", secondCloseResult, runtime.ErrorObject(err).String)
	}
	if recorder.closeCount != 1 {
		// 重复 close 不应再次关闭底层资源。
		t.Fatalf("close count after second close = %d, want 1", recorder.closeCount)
	}
}

// TestFileReadStreamsFromCurrentPosition 验证 File.Read 复用 file:read 的缓冲位置。
//
// 测试先按行读取再通过 io.Reader 读取剩余字节，并覆盖已关闭、只写文件的
// Lua error 语义；Read 不应 seek 或主动关闭文件。
func TestFileReadStreamsFromCurrentPosition(t *testing.T) {
	// 行读取会预读底层 reader，后续 Read 必须从同一缓冲器继续。
	file := NewFile("stream", strings.NewReader("first\nsecond"), nil, nil, nil)
	line, err := file.ReadLine()
	if err != nil {
		// 首行读取不应失败。
		t.Fatalf("ReadLine failed: %v", err)
	}
	if line != "first" {
		// ReadLine 不包含行尾。
		t.Fatalf("ReadLine = %q, want first", line)
	}
	remaining, err := gio.ReadAll(file)
	if err != nil {
		// 流式读取应消费预读缓冲中的剩余内容。
		t.Fatalf("Read remaining failed: %v", err)
	}
	if string(remaining) != "second" {
		// 跳过预读缓冲会导致此处错误返回 EOF。
		t.Fatalf("remaining = %q, want second", remaining)
	}
	if file.Closed() {
		// Read 只读取内容，文件生命周期仍归调用方。
		t.Fatalf("Read should not close file")
	}

	closed := NewFile("closed", strings.NewReader("x"), nil, nil, nil)
	if err := closed.Close(); err != nil {
		// 测试需要先进入关闭状态。
		t.Fatalf("Close failed: %v", err)
	}
	if _, err := closed.Read(make([]byte, 1)); !errors.Is(err, runtime.ErrLuaError) {
		// 已关闭文件必须保留可被 pcall 捕获的 Lua error 语义。
		t.Fatalf("closed Read error = %v, want Lua error", err)
	}

	writeOnly := NewFile("write-only", nil, &strings.Builder{}, nil, nil)
	if _, err := writeOnly.Read(make([]byte, 1)); !errors.Is(err, runtime.ErrLuaError) {
		// 只写文件没有 reader 能力。
		t.Fatalf("write-only Read error = %v, want Lua error", err)
	}
}

// TestCloseAndFlushArgumentErrors 验证 io.close/io.flush 的参数错误。
//
// 非 file userdata 必须返回 Lua 参数错误，并可通过 runtime.ErrLuaError 分类识别。
func TestCloseAndFlushArgumentErrors(t *testing.T) {
	// 字符串不能作为 file 参数。
	if _, err := Close(runtime.StringValue("not-file")); !errors.Is(err, runtime.ErrLuaError) {
		// close 参数错误必须是 Lua error。
		t.Fatalf("Close error = %v, want Lua error", err)
	}
	if _, err := Flush(runtime.StringValue("not-file")); !errors.Is(err, runtime.ErrLuaError) {
		// flush 参数错误必须是 Lua error。
		t.Fatalf("Flush error = %v, want Lua error", err)
	}
}

// TestStandardFileIdentityAndClose 验证标准流 userdata 的切换返回值与 close 语义。
//
// Lua 5.3 要求 io.input(io.stdin) 返回传入的同一 userdata；标准流 close 不关闭宿主资源并返回
// falsy 空结果，供官方 files.lua 使用 `not io.close(io.stdin)` 判断。
func TestStandardFileIdentityAndClose(t *testing.T) {
	// 测试会修改包级默认输入输出，结束时必须恢复。
	defer restoreDefaultFiles(defaultInput, defaultOutput, defaultError)()

	stdinValue := NewFileValue(nil, defaultInput)
	inputResult, err := Input(stdinValue)
	if err != nil {
		// 切换到标准输入不应失败。
		t.Fatalf("Input stdin failed: %v", err)
	}
	if len(inputResult) != 1 || !inputResult[0].RawEqual(stdinValue) {
		// 返回值必须保持传入 userdata identity，满足 io.input(io.stdin) == io.stdin。
		t.Fatalf("Input stdin result = %#v, want original userdata", inputResult)
	}
	if currentInput, err := Input(); err != nil || len(currentInput) != 1 || !currentInput[0].RawEqual(stdinValue) {
		// 无参数查询必须返回同一默认输入 userdata。
		t.Fatalf("Input query = %#v err=%v, want stdin userdata", currentInput, err)
	}

	stdoutValue := NewFileValue(nil, defaultOutput)
	outputResult, err := Output(stdoutValue)
	if err != nil {
		// 切换到标准输出不应失败。
		t.Fatalf("Output stdout failed: %v", err)
	}
	if len(outputResult) != 1 || !outputResult[0].RawEqual(stdoutValue) {
		// 返回值必须保持传入 userdata identity，满足 io.output(io.stdout) == io.stdout。
		t.Fatalf("Output stdout result = %#v, want original userdata", outputResult)
	}
	if currentOutput, err := Output(); err != nil || len(currentOutput) != 1 || !currentOutput[0].RawEqual(stdoutValue) {
		// 无参数查询必须返回同一默认输出 userdata。
		t.Fatalf("Output query = %#v err=%v, want stdout userdata", currentOutput, err)
	}

	closeResult, err := Close(stdinValue)
	if err != nil {
		// 关闭标准输入应保持 no-op，不应抛 Lua 错误。
		t.Fatalf("Close stdin failed: %v", err)
	}
	if len(closeResult) != 0 {
		// 标准流 close 返回空结果，Lua 表达式中等价 falsy。
		t.Fatalf("Close stdin result = %#v, want empty result", closeResult)
	}
}

// TestFileOperationErrorsBecomeLuaErrors 验证底层 IO 错误会转换为 Lua 错误。
//
// 这保证嵌入方可以通过 errors.Is 识别 Lua 运行期错误，同时保留底层错误文本。
func TestFileOperationErrorsBecomeLuaErrors(t *testing.T) {
	// 构造底层刷新与关闭错误。
	recorder := &countingFlushCloser{
		flushErr: errors.New("flush failed"),
		closeErr: errors.New("close failed"),
	}
	flushFile := NewFile("flush-file", nil, nil, recorder, nil)
	flushValue := NewFileValue(nil, flushFile)
	if _, err := Flush(flushValue); !errors.Is(err, runtime.ErrLuaError) {
		// flush 底层错误必须包装为 Lua error。
		t.Fatalf("Flush error = %v, want Lua error", err)
	}

	closeFile := NewFile("close-file", nil, nil, nil, recorder)
	closeValue := NewFileValue(nil, closeFile)
	if _, err := Close(closeValue); !errors.Is(err, runtime.ErrLuaError) {
		// close 底层错误必须包装为 Lua error。
		t.Fatalf("Close error = %v, want Lua error", err)
	}
}

// TestReadReadsDefaultInput 验证 io.read 从默认输入按格式读取。
//
// 该用例覆盖默认行读取、指定字节读取和全量读取，避免依赖真实 stdin。
func TestReadReadsDefaultInput(t *testing.T) {
	// 测试会修改包级默认输入，结束时必须恢复。
	defer restoreDefaultFiles(defaultInput, defaultOutput, defaultError)()

	defaultInput = NewFile("input", strings.NewReader("first\nsecond\nrest"), nil, nil, nil)
	first, err := Read()
	if err != nil {
		// 默认 io.read 行读取不应失败。
		t.Fatalf("Read default failed: %v", err)
	}
	if len(first) != 1 || first[0].String != "first" {
		// 默认格式必须读取第一行且不含换行符。
		t.Fatalf("first read = %#v, want first", first)
	}
	bytes, err := Read(runtime.IntegerValue(3))
	if err != nil {
		// count 格式读取不应失败。
		t.Fatalf("Read count failed: %v", err)
	}
	if len(bytes) != 1 || bytes[0].String != "sec" {
		// count 格式必须读取指定字节数。
		t.Fatalf("byte read = %#v, want sec", bytes)
	}
	rest, err := Read(runtime.StringValue("*a"))
	if err != nil {
		// 全量读取不应失败。
		t.Fatalf("Read all failed: %v", err)
	}
	if len(rest) != 1 || rest[0].String != "ond\nrest" {
		// 全量读取必须从当前缓冲位置继续。
		t.Fatalf("rest read = %#v, want remaining content", rest)
	}
}

// TestWriteWritesDefaultOutput 验证 io.write 写入默认输出。
//
// 该用例覆盖 string、integer 与 number 参数，以及成功返回当前输出 file 的 Lua 语义。
func TestWriteWritesDefaultOutput(t *testing.T) {
	// 测试会修改包级默认输出，结束时必须恢复。
	defer restoreDefaultFiles(defaultInput, defaultOutput, defaultError)()

	var builder strings.Builder
	defaultOutput = NewFile("output", nil, &builder, nil, nil)
	result, err := Write(runtime.StringValue("x="), runtime.IntegerValue(42), runtime.StringValue(","), runtime.NumberValue(1.5))
	if err != nil {
		// 可写默认输出不应失败。
		t.Fatalf("Write failed: %v", err)
	}
	if len(result) != 1 {
		// io.write 成功时应返回当前输出 file。
		t.Fatalf("Write result = %#v, want one file", result)
	}
	if file, err := fileArgument(result, 1, "write"); err != nil || file != defaultOutput {
		// 返回的 file 必须是当前默认输出，支持 io.write(...):seek() 链式调用。
		t.Fatalf("Write result = %#v file=%v err=%v, want default output", result, file, err)
	}
	if builder.String() != "x=42,1.5" {
		// 输出文本必须按参数顺序拼接。
		t.Fatalf("written text = %q", builder.String())
	}
}

// TestTypeReportsFileState 验证 io.type 识别 file 与 closed file。
//
// 非 file 值返回 nil，file 关闭前后分别返回 Lua 5.3 约定文本。
func TestTypeReportsFileState(t *testing.T) {
	// 普通字符串不是 file。
	notFile, err := Type(runtime.StringValue("x"))
	if err != nil {
		// io.type 不应对非 file 抛错。
		t.Fatalf("Type non-file failed: %v", err)
	}
	if len(notFile) != 1 || !notFile[0].IsNil() {
		// 非 file 必须返回 nil。
		t.Fatalf("non-file type = %#v, want nil", notFile)
	}

	file := NewFile("file", nil, nil, nil, nil)
	value := NewFileValue(nil, file)
	openType, err := Type(value)
	if err != nil {
		// 未关闭 file 应被识别。
		t.Fatalf("Type file failed: %v", err)
	}
	if len(openType) != 1 || openType[0].String != "file" {
		// 未关闭 file 返回 "file"。
		t.Fatalf("open type = %#v, want file", openType)
	}
	fileCloseResult, err := FileClose(value)
	if err != nil {
		// file:close 方法入口应可关闭普通 file。
		t.Fatalf("FileClose failed: %v", err)
	}
	if len(fileCloseResult) != 1 || fileCloseResult[0].Kind != runtime.KindBoolean || !fileCloseResult[0].Bool {
		// file:close 复用 io.close，成功时也必须返回 true。
		t.Fatalf("FileClose result = %#v, want true", fileCloseResult)
	}
	closedType, err := Type(value)
	if err != nil {
		// 已关闭 file 仍应可被 io.type 识别。
		t.Fatalf("Type closed file failed: %v", err)
	}
	if len(closedType) != 1 || closedType[0].String != "closed file" {
		// 已关闭 file 返回 "closed file"。
		t.Fatalf("closed type = %#v, want closed file", closedType)
	}
}

// TestTmpFilePolicy 验证 io.tmpfile 默认禁用宿主临时文件。
//
// 临时文件仍属于文件系统写入能力，当前 sandbox 默认策略必须拒绝。
func TestTmpFilePolicy(t *testing.T) {
	// io.tmpfile 默认返回 Lua error。
	if _, err := TmpFileWithOptions(runtime.Options{}); !errors.Is(err, runtime.ErrLuaError) {
		// 禁用策略必须使用 Lua error 表达。
		t.Fatalf("TmpFile error = %v, want Lua error", err)
	}
	values, err := TmpFileWithOptions(runtime.Options{AllowHostFilesystem: true})
	if err != nil {
		// 显式授权后应创建宿主临时文件。
		t.Fatalf("TmpFile allowed failed: %v", err)
	}
	file, err := fileArgument(values, 1, "tmpfile")
	if err != nil {
		// io.tmpfile 成功时必须返回 file userdata。
		t.Fatalf("TmpFile allowed result is not file: %v", err)
	}
	if err := file.Close(); err != nil {
		// 测试结束必须关闭临时文件。
		t.Fatalf("TmpFile close failed: %v", err)
	}
	if name := file.Name(); name != "" {
		// 临时文件路径由本测试创建，关闭后清理宿主临时文件。
		_ = os.Remove(name)
	}
}

// TestFileMethodsFlushLinesReadSeekAndSetVBuf 验证本轮新增 file 方法入口。
//
// 该用例覆盖 file:flush、file:lines、file:read、file:seek 和 file:setvbuf 的基础成功路径。
func TestFileMethodsFlushLinesReadSeekAndSetVBuf(t *testing.T) {
	// flush 使用测试替身确认底层 Flush 被调用。
	recorder := &countingFlushCloser{}
	flushFile := NewFile("flush-file", nil, nil, recorder, nil)
	if result, err := FileFlush(NewFileValue(nil, flushFile)); err != nil {
		// file:flush 不应在有效 file 上失败。
		t.Fatalf("FileFlush failed: %v", err)
	} else if len(result) != 1 || result[0].Kind != runtime.KindBoolean || !result[0].Bool {
		// file:flush 成功时返回 true。
		t.Fatalf("FileFlush result = %#v, want true", result)
	}
	if recorder.flushCount != 1 {
		// file:flush 必须调用底层 Flusher 一次。
		t.Fatalf("flush count = %d, want 1", recorder.flushCount)
	}

	linesFile := NewFile("lines-file", strings.NewReader("alpha\nbeta\n"), nil, nil, nil)
	linesValue := NewFileValue(nil, linesFile)
	lineIteratorValues, err := FileLines(linesValue)
	if err != nil {
		// file:lines 不应在可读 file 上失败。
		t.Fatalf("FileLines failed: %v", err)
	}
	iterator := lineIteratorValues[0].Ref.(runtime.GoResultsFunction)
	for index, want := range []string{"alpha", "beta"} {
		// iterator 每次读取下一行。
		result, err := iterator()
		if err != nil {
			// 有效行读取不应失败。
			t.Fatalf("file line %d failed: %v", index, err)
		}
		if len(result) != 1 || result[0].String != want {
			// 返回行不包含换行符。
			t.Fatalf("file line %d = %#v, want %q", index, result, want)
		}
	}

	path := filepath.Join(t.TempDir(), "lines-path.txt")
	if err := os.WriteFile(path, []byte("alpha\nbeta\n"), 0o600); err != nil {
		// 路径型 io.lines 测试需要真实文件。
		t.Fatalf("write lines path failed: %v", err)
	}
	pathIteratorValues, err := Lines(runtime.StringValue(path))
	if err != nil {
		// io.lines(path) 应能打开文件并返回迭代器。
		t.Fatalf("Lines(path) failed: %v", err)
	}
	pathIterator := pathIteratorValues[0].Ref.(runtime.GoResultsFunction)
	for range []string{"alpha", "beta"} {
		// 先耗尽路径型迭代器。
		if _, err := pathIterator(); err != nil {
			t.Fatalf("path iterator read failed: %v", err)
		}
	}
	if result, err := pathIterator(); err != nil || result != nil {
		// 第一次 EOF 返回 nil 结束迭代。
		t.Fatalf("path iterator EOF = %#v, err=%v", result, err)
	}
	if _, err := pathIterator(); err == nil || !strings.Contains(runtime.ErrorObject(err).String, "file is already closed") {
		// EOF 后再次调用同一 iterator 必须返回官方期望的已关闭错误文本。
		t.Fatalf("path iterator closed error = %v", err)
	}

	multiFormatInput := strings.Repeat("a", maxLineFormats) + "\n"
	multiFormatFile := NewFile("multi-lines-file", strings.NewReader(multiFormatInput), nil, nil, nil)
	multiFormatArgs := []runtime.Value{NewFileValue(nil, multiFormatFile)}
	for index := 0; index < maxLineFormats; index++ {
		// 每个数字格式读取一个字节，覆盖官方 files.lua 的 250 格式参数边界。
		multiFormatArgs = append(multiFormatArgs, runtime.IntegerValue(1))
	}
	multiFormatValues, err := FileLines(multiFormatArgs...)
	if err != nil {
		// 250 个格式参数仍应允许创建 iterator。
		t.Fatalf("FileLines 250 formats failed: %v", err)
	}
	multiFormatIterator := multiFormatValues[0].Ref.(runtime.GoResultsFunction)
	multiFormatResult, err := multiFormatIterator()
	if err != nil {
		// 多格式 iterator 首次读取不应失败。
		t.Fatalf("multi-format iterator failed: %v", err)
	}
	if len(multiFormatResult) != maxLineFormats {
		// 每个格式参数都应对应一个返回值。
		t.Fatalf("multi-format result len = %d, want %d", len(multiFormatResult), maxLineFormats)
	}
	for index, value := range multiFormatResult {
		// 每个字节格式都应读到一个 a。
		if value.String != "a" {
			t.Fatalf("multi-format result %d = %#v, want a", index, value)
		}
	}
	tooManyFormats := append([]runtime.Value{}, multiFormatArgs...)
	tooManyFormats = append(tooManyFormats, runtime.IntegerValue(1))
	if _, err := FileLines(tooManyFormats...); err == nil || !strings.Contains(runtime.ErrorObject(err).String, "too many arguments") {
		// 251 个格式参数必须按 Lua 5.3 返回 too many arguments。
		t.Fatalf("FileLines too many formats error = %v", err)
	}
	writeOnlyLinesValues, err := FileLines(NewFileValue(nil, NewFile("write-only-lines", nil, &strings.Builder{}, nil, nil)))
	if err != nil {
		// 不可读 file:lines 应先返回 iterator，错误延迟到 iterator 调用。
		t.Fatalf("FileLines write-only creation failed: %v", err)
	}
	writeOnlyIterator := writeOnlyLinesValues[0].Ref.(runtime.GoResultsFunction)
	if _, err := writeOnlyIterator(); err == nil || !strings.Contains(err.Error(), "file is not readable") {
		// iterator 调用时才报告不可读，便于 Lua 侧 pcall 捕获。
		t.Fatalf("FileLines write-only iterator error = %v", err)
	}

	readFile := NewFile("read-file", strings.NewReader("one\ntwo\nthree"), nil, nil, nil)
	readValue := NewFileValue(nil, readFile)
	readResult, err := FileRead(readValue, runtime.StringValue("*l"), runtime.IntegerValue(2), runtime.StringValue("*a"))
	if err != nil {
		// file:read 支持行、字节数和全量读取。
		t.Fatalf("FileRead failed: %v", err)
	}
	if len(readResult) != 3 || readResult[0].String != "one" || readResult[1].String != "tw" || readResult[2].String != "o\nthree" {
		// file:read 必须按格式顺序返回多个结果。
		t.Fatalf("FileRead result = %#v", readResult)
	}
	readAllAliasFile := NewFile("read-all-alias-file", strings.NewReader("buffered"), nil, nil, nil)
	readAllAliasResult, err := FileRead(NewFileValue(nil, readAllAliasFile), runtime.StringValue("all"))
	if err != nil {
		// file:read("all") 是 Lua 5.3 兼容别名，等价于读取剩余全部内容。
		t.Fatalf("FileRead all alias failed: %v", err)
	}
	if len(readAllAliasResult) != 1 || readAllAliasResult[0].String != "buffered" {
		// all 别名必须返回剩余全部内容。
		t.Fatalf("FileRead all alias result = %#v", readAllAliasResult)
	}
	lineWithEndFile := NewFile("line-with-end-file", strings.NewReader("alpha\nbeta"), nil, nil, nil)
	lineWithEndValue := NewFileValue(nil, lineWithEndFile)
	lineWithEndResult, err := FileRead(lineWithEndValue, runtime.StringValue("L"), runtime.StringValue("*L"), runtime.StringValue("L"))
	if err != nil {
		// file:read("L") 支持保留行尾换行。
		t.Fatalf("FileRead line with end failed: %v", err)
	}
	if len(lineWithEndResult) != 3 || lineWithEndResult[0].String != "alpha\n" || lineWithEndResult[1].String != "beta" || !lineWithEndResult[2].IsNil() {
		// L/*L 必须保留行尾；EOF 且没有内容时返回 nil。
		t.Fatalf("FileRead line with end result = %#v", lineWithEndResult)
	}
	seekBufferedReader := strings.NewReader("one\ntwo\nthree\n")
	seekBufferedFile := NewFile("seek-buffered-file", seekBufferedReader, nil, nil, nil)
	seekBufferedValue := NewFileValue(nil, seekBufferedFile)
	if _, err := FileRead(seekBufferedValue); err != nil {
		// 先按行读取，触发 bufio.Reader 预读。
		t.Fatalf("FileRead buffered first line failed: %v", err)
	}
	seekBufferedPosition, err := FileSeek(seekBufferedValue)
	if err != nil {
		// 行读取后的当前位置查询不应失败。
		t.Fatalf("FileSeek buffered current failed: %v", err)
	}
	if len(seekBufferedPosition) != 1 || seekBufferedPosition[0].Integer != 4 {
		// Lua 可见位置应在第一行之后，而不是底层 Reader 预读后的 EOF。
		t.Fatalf("FileSeek buffered current = %#v, want 4", seekBufferedPosition)
	}
	if _, err := FileSeek(seekBufferedValue, runtime.StringValue("set"), seekBufferedPosition[0]); err != nil {
		// 回到同一逻辑位置后应继续读取第二行。
		t.Fatalf("FileSeek buffered set failed: %v", err)
	}
	seekBufferedLine, err := FileRead(seekBufferedValue)
	if err != nil {
		// seek 后继续按行读取不应失败。
		t.Fatalf("FileRead buffered after seek failed: %v", err)
	}
	if len(seekBufferedLine) != 1 || seekBufferedLine[0].String != "two" {
		// seek 后应从第二行开始读取。
		t.Fatalf("FileRead buffered after seek = %#v, want two", seekBufferedLine)
	}

	numberFile := NewFile("number-file", strings.NewReader("9223372036854775807\n0xABCp-3\n-0xABCp-3\n0x8000000000000001\n"), nil, nil, nil)
	numberValue := NewFileValue(nil, numberFile)
	numberResult, err := FileRead(numberValue, runtime.StringValue("n"), runtime.StringValue("*n"), runtime.StringValue("n"), runtime.StringValue("n"))
	if err != nil {
		// 数字读取不应在合法整数和十六进制浮点上失败。
		t.Fatalf("FileRead numbers failed: %v", err)
	}
	if len(numberResult) != 4 || numberResult[0].Kind != runtime.KindInteger || numberResult[0].Integer != 9223372036854775807 || numberResult[1].Kind != runtime.KindNumber || numberResult[2].Kind != runtime.KindNumber || numberResult[3].Kind != runtime.KindInteger || numberResult[3].Integer != -9223372036854775807 {
		// 数字读取必须保留整数优先语义，并支持十六进制浮点。
		t.Fatalf("FileRead numbers = %#v, want integer, hex floats, and wrapped hex integer", numberResult)
	}

	terminationFile := NewFile("number-termination-file", strings.NewReader("-12.3-\t-0xffff+  .3|5.E-3X  +234e+13E 0xDEADBEEFDEADBEEFx\n0x1.13Ap+3e\n.e+\t0.e;\t--;  0xX;\n"), nil, nil, nil)
	terminationValue := NewFileValue(nil, terminationFile)
	terminationCases := []struct {
		wantNumber runtime.Value
		readCount  int64
		wantSuffix string
	}{
		{wantNumber: runtime.NumberValue(-12.3), readCount: 1, wantSuffix: "-"},
		{wantNumber: runtime.IntegerValue(-0xffff), readCount: 2, wantSuffix: "+ "},
		{wantNumber: runtime.NumberValue(0.3), readCount: 1, wantSuffix: "|"},
		{wantNumber: runtime.NumberValue(5e-3), readCount: 1, wantSuffix: "X"},
		{wantNumber: runtime.NumberValue(234e13), readCount: 1, wantSuffix: "E"},
		{wantNumber: runtime.IntegerValue(-2401053088876216593), readCount: 2, wantSuffix: "x\n"},
		{wantNumber: runtime.NumberValue(0x1.13ap3), readCount: 1, wantSuffix: "e"},
	}
	for caseIndex, testCase := range terminationCases {
		// 数字读取应只消费合法数字前缀，后续终止字节必须留给下一次读取。
		numberValues, err := FileRead(terminationValue, runtime.StringValue("n"))
		if err != nil {
			t.Fatalf("FileRead termination number %d failed: %v", caseIndex, err)
		}
		if len(numberValues) != 1 || !numberValues[0].RawEqual(testCase.wantNumber) {
			t.Fatalf("FileRead termination number %d = %#v, want %#v", caseIndex, numberValues, testCase.wantNumber)
		}
		suffixValues, err := FileRead(terminationValue, runtime.IntegerValue(testCase.readCount))
		if err != nil {
			t.Fatalf("FileRead termination suffix %d failed: %v", caseIndex, err)
		}
		if len(suffixValues) != 1 || suffixValues[0].String != testCase.wantSuffix {
			t.Fatalf("FileRead termination suffix %d = %#v, want %q", caseIndex, suffixValues, testCase.wantSuffix)
		}
	}
	invalidCases := []struct {
		readCount  int64
		wantSuffix string
	}{
		{readCount: 2, wantSuffix: "e+"},
		{readCount: 1, wantSuffix: ";"},
		{readCount: 2, wantSuffix: "-;"},
		{readCount: 1, wantSuffix: "X"},
		{readCount: 1, wantSuffix: ";"},
	}
	for caseIndex, testCase := range invalidCases {
		// 非法数字候选返回 nil，并按官方 files.lua 规则只消费已扫描的候选前缀。
		numberValues, err := FileRead(terminationValue, runtime.StringValue("n"))
		if err != nil {
			t.Fatalf("FileRead invalid number %d failed: %v", caseIndex, err)
		}
		if len(numberValues) != 1 || !numberValues[0].IsNil() {
			t.Fatalf("FileRead invalid number %d = %#v, want nil", caseIndex, numberValues)
		}
		suffixValues, err := FileRead(terminationValue, runtime.IntegerValue(testCase.readCount))
		if err != nil {
			t.Fatalf("FileRead invalid suffix %d failed: %v", caseIndex, err)
		}
		if len(suffixValues) != 1 || suffixValues[0].String != testCase.wantSuffix {
			t.Fatalf("FileRead invalid suffix %d = %#v, want %q", caseIndex, suffixValues, testCase.wantSuffix)
		}
	}
	longNumberInput := "1234" + strings.Repeat("0", 1000) + "\n"
	longNumberFile := NewFile("long-number-file", strings.NewReader(longNumberInput), nil, nil, nil)
	longNumberValue := NewFileValue(nil, longNumberFile)
	longNumberResult, err := FileRead(longNumberValue, runtime.StringValue("n"))
	if err != nil {
		// 超长数字读取不应抛 Go error。
		t.Fatalf("FileRead long number failed: %v", err)
	}
	if len(longNumberResult) != 1 || !longNumberResult[0].IsNil() {
		// 超长数字按 Lua 5.3 固定缓冲行为返回 nil。
		t.Fatalf("FileRead long number = %#v, want nil", longNumberResult)
	}
	longNumberRemainder, err := FileRead(longNumberValue, runtime.StringValue("L"))
	if err != nil {
		// 超长数字失败后剩余 0 和换行必须还能被读取。
		t.Fatalf("FileRead long number remainder failed: %v", err)
	}
	if len(longNumberRemainder) != 1 || !strings.HasSuffix(longNumberRemainder[0].String, "\n") || strings.Trim(longNumberRemainder[0].String, "0\n") != "" {
		// 剩余行应只包含未被数字扫描吞掉的 0 和换行。
		t.Fatalf("FileRead long number remainder = %#v", longNumberRemainder)
	}

	seekReader := strings.NewReader("abcdef")
	seekFile := NewFile("seek-file", seekReader, nil, nil, nil)
	seekValue := NewFileValue(nil, seekFile)
	seekResult, err := FileSeek(seekValue, runtime.StringValue("set"), runtime.IntegerValue(3))
	if err != nil {
		// file:seek set 模式不应失败。
		t.Fatalf("FileSeek failed: %v", err)
	}
	if len(seekResult) != 1 || seekResult[0].Integer != 3 {
		// file:seek 返回新的文件位置。
		t.Fatalf("FileSeek result = %#v, want 3", seekResult)
	}
	readAfterSeek, err := FileRead(seekValue, runtime.IntegerValue(2))
	if err != nil {
		// seek 后继续读取不应失败。
		t.Fatalf("FileRead after seek failed: %v", err)
	}
	if len(readAfterSeek) != 1 || readAfterSeek[0].String != "de" {
		// seek 后读取应从新位置开始。
		t.Fatalf("read after seek = %#v, want de", readAfterSeek)
	}
	openText, err := runtime.ToString(seekValue)
	if err != nil {
		// file userdata tostring 不应失败。
		t.Fatalf("file tostring failed: %v", err)
	}
	if !strings.HasPrefix(openText.String, "file ") {
		// 打开的 file tostring 必须使用 Lua 5.3 的 file 前缀。
		t.Fatalf("open file tostring = %q", openText.String)
	}

	if result, err := FileSetVBuf(seekValue, runtime.StringValue("line"), runtime.IntegerValue(1024)); err != nil {
		// 当前 setvbuf 只校验参数并返回成功。
		t.Fatalf("FileSetVBuf failed: %v", err)
	} else if len(result) != 1 || result[0].Kind != runtime.KindBoolean || !result[0].Bool {
		// file:setvbuf 成功时返回 true。
		t.Fatalf("FileSetVBuf result = %#v, want true", result)
	}
	bufferPath := filepath.Join(t.TempDir(), "buffered.txt")
	writerFile, err := os.OpenFile(bufferPath, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o600)
	if err != nil {
		// 写缓冲测试需要真实文件，以便另一个 reader 观察 close 前后的可见性。
		t.Fatalf("open buffered writer failed: %v", err)
	}
	readerFile, err := os.Open(bufferPath)
	if err != nil {
		// 独立 reader 用于验证 full 缓冲不会提前写入底层文件。
		t.Fatalf("open buffered reader failed: %v", err)
	}
	writerValue := NewFileValue(nil, NewFile(bufferPath, writerFile, writerFile, nil, writerFile))
	readerValue := NewFileValue(nil, NewFile(bufferPath, readerFile, nil, nil, readerFile))
	if _, err := FileSetVBuf(writerValue, runtime.StringValue("full"), runtime.IntegerValue(2000)); err != nil {
		// full 缓冲模式必须能启用。
		t.Fatalf("FileSetVBuf full failed: %v", err)
	}
	if _, err := FileWrite(writerValue, runtime.StringValue("x")); err != nil {
		// full 缓冲模式下写入应先进入 Lua 层缓冲。
		t.Fatalf("FileWrite buffered failed: %v", err)
	}
	beforeClose, err := FileRead(readerValue, runtime.StringValue("all"))
	if err != nil {
		// close 前 reader 读取不应报错。
		t.Fatalf("FileRead before buffered close failed: %v", err)
	}
	if len(beforeClose) != 1 || beforeClose[0].String != "" {
		// full 缓冲 close 前不应让另一个 reader 看到内容。
		t.Fatalf("FileRead before buffered close = %#v, want empty string", beforeClose)
	}
	if _, err := FileClose(writerValue); err != nil {
		// close 必须刷新 full 缓冲。
		t.Fatalf("FileClose buffered failed: %v", err)
	}
	if _, err := readerFile.Seek(0, 0); err != nil {
		// reader 需要回到文件开头重新读取 close 后内容。
		t.Fatalf("seek buffered reader failed: %v", err)
	}
	readerFileObject, _ := fileArgument([]runtime.Value{readerValue}, 1, "read")
	readerFileObject.lineReader = nil
	afterClose, err := FileRead(readerValue, runtime.StringValue("all"))
	if err != nil {
		// close 后 reader 读取不应报错。
		t.Fatalf("FileRead after buffered close failed: %v", err)
	}
	if len(afterClose) != 1 || afterClose[0].String != "x" {
		// full 缓冲 close 后必须提交到底层文件。
		t.Fatalf("FileRead after buffered close = %#v, want x", afterClose)
	}
	_, _ = FileClose(readerValue)
	if _, err := FileClose(seekValue); err != nil {
		// 关闭普通 file 应成功。
		t.Fatalf("FileClose for tostring failed: %v", err)
	}
	closedText, err := runtime.ToString(seekValue)
	if err != nil {
		// 关闭 file tostring 不应失败。
		t.Fatalf("closed file tostring failed: %v", err)
	}
	if closedText.String != "file (closed)" {
		// 关闭 file tostring 必须返回固定文本。
		t.Fatalf("closed file tostring = %q", closedText.String)
	}
}

// TestFileWriteWritesTargetFile 验证 file:write 写入目标 file。
//
// file:write 成功时应返回 self，并按参数顺序写出 string、integer 与 number。
func TestFileWriteWritesTargetFile(t *testing.T) {
	// 使用 strings.Builder 作为目标 writer，避免真实文件系统依赖。
	var builder strings.Builder
	file := NewFile("write-file", nil, &builder, nil, nil)
	value := NewFileValue(nil, file)
	result, err := FileWrite(value, runtime.StringValue("v="), runtime.IntegerValue(7), runtime.StringValue(","), runtime.NumberValue(2.5))
	if err != nil {
		// 有效 file:write 不应失败。
		t.Fatalf("FileWrite failed: %v", err)
	}
	if len(result) != 1 || result[0].Ref != value.Ref {
		// 成功时返回 self，便于 Lua 链式调用。
		t.Fatalf("FileWrite result = %#v, want self", result)
	}
	if builder.String() != "v=7,2.5" {
		// 写入内容必须按参数顺序拼接。
		t.Fatalf("FileWrite text = %q, want v=7,2.5", builder.String())
	}
	readOnlyValue := NewFileValue(nil, NewFile("read-only-file", strings.NewReader("x"), nil, nil, nil))
	readOnlyResult, err := FileWrite(readOnlyValue, runtime.StringValue("x"))
	if err != nil {
		// 可读不可写文件的写入失败应通过返回值表达，不应抛出 Lua error。
		t.Fatalf("FileWrite read-only failed with error: %v", err)
	}
	if len(readOnlyResult) != 3 || !readOnlyResult[0].IsNil() || readOnlyResult[1].Kind != runtime.KindString || readOnlyResult[2].Kind != runtime.KindInteger {
		// Lua I/O 普通失败应返回 nil、错误文本和数字错误码。
		t.Fatalf("FileWrite read-only result = %#v", readOnlyResult)
	}
}

// TestFileMethodErrors 验证 file 方法的关键错误路径。
//
// 非 file self 和非法 setvbuf mode 必须返回 Lua error；不可 seek 文件按 Lua IO 普通失败返回。
func TestFileMethodErrors(t *testing.T) {
	// 非 file self 会触发参数错误。
	if _, err := FileFlush(runtime.StringValue("bad")); !errors.Is(err, runtime.ErrLuaError) {
		// file:flush self 错误必须是 Lua error。
		t.Fatalf("FileFlush error = %v, want Lua error", err)
	}
	notSeekable := NewFile("not-seekable", strings.NewReader("abc"), nil, nil, nil)
	notSeekable.seeker = nil
	seekValues, err := FileSeek(NewFileValue(nil, notSeekable))
	if err != nil {
		// 不可 seek 文件不是参数错误，应按普通 IO 失败返回。
		t.Fatalf("FileSeek not seekable failed with error: %v", err)
	}
	if len(seekValues) != 3 || seekValues[0].Kind != runtime.KindBoolean || seekValues[0].Bool || seekValues[1].Kind != runtime.KindString || seekValues[2].Kind != runtime.KindInteger {
		// 不可 seek 需要返回 falsy、错误文本和数字错误码。
		t.Fatalf("FileSeek not seekable = %#v, want false message code", seekValues)
	}
	if _, err := FileSetVBuf(NewFileValue(nil, notSeekable), runtime.StringValue("bad")); !errors.Is(err, runtime.ErrLuaError) {
		// 非法 setvbuf mode 必须返回 Lua error。
		t.Fatalf("FileSetVBuf error = %v, want Lua error", err)
	}
}

// TestInputAndOutputSwitchDefaultFiles 验证 io.input/io.output 可切换默认 file。
//
// 该用例覆盖 file userdata 形式的默认输入输出切换，并确认无参数调用返回当前句柄。
func TestInputAndOutputSwitchDefaultFiles(t *testing.T) {
	// 测试会修改包级默认句柄，结束时必须恢复。
	defer restoreDefaultFiles(defaultInput, defaultOutput, defaultError)()

	inputFile := NewFile("input", strings.NewReader("a\n"), nil, nil, nil)
	inputValue := NewFileValue(nil, inputFile)
	inputResult, err := Input(inputValue)
	if err != nil {
		// file userdata 作为输入句柄应被接受。
		t.Fatalf("Input failed: %v", err)
	}
	if len(inputResult) != 1 {
		// io.input 必须返回当前输入句柄。
		t.Fatalf("Input result count = %d, want 1", len(inputResult))
	}
	currentInput, err := fileArgument(inputResult, 1, "input")
	if err != nil {
		// 返回值必须仍是 file userdata。
		t.Fatalf("Input result is not file: %v", err)
	}
	if currentInput != inputFile {
		// 默认输入必须切换到传入文件。
		t.Fatalf("default input was not switched")
	}

	outputFile := NewFile("output", nil, &strings.Builder{}, nil, nil)
	outputValue := NewFileValue(nil, outputFile)
	outputResult, err := Output(outputValue)
	if err != nil {
		// file userdata 作为输出句柄应被接受。
		t.Fatalf("Output failed: %v", err)
	}
	currentOutput, err := fileArgument(outputResult, 1, "output")
	if err != nil {
		// 返回值必须仍是 file userdata。
		t.Fatalf("Output result is not file: %v", err)
	}
	if currentOutput != outputFile {
		// 默认输出必须切换到传入文件。
		t.Fatalf("default output was not switched")
	}
}

// TestReadWriteClosedDefaultFileText 验证默认输入输出关闭后的 Lua 5.3 错误文本。
//
// 官方 files.lua 使用 checkerr 匹配 ` input file is closed` 与 ` output file is closed`，
// 因此默认句柄入口需要区别于普通 file:read/file:write 的 closed file 文本。
func TestReadWriteClosedDefaultFileText(t *testing.T) {
	// 测试会修改包级默认句柄，结束时必须恢复。
	defer restoreDefaultFiles(defaultInput, defaultOutput, defaultError)()

	defaultInput = NewFile("input", strings.NewReader("x"), nil, nil, nil)
	_ = defaultInput.Close()
	if _, err := Read(); err == nil || !strings.Contains(err.Error(), " input file is closed") {
		// 关闭默认输入后 io.read 必须返回官方可匹配文本。
		t.Fatalf("Read closed default input error = %v", err)
	}

	defaultOutput = NewFile("output", nil, &strings.Builder{}, nil, nil)
	_ = defaultOutput.Close()
	if _, err := Write(runtime.StringValue("x")); err == nil || !strings.Contains(err.Error(), " output file is closed") {
		// 关闭默认输出后 io.write 必须返回官方可匹配文本。
		t.Fatalf("Write closed default output error = %v", err)
	}
	_, err := Write(runtime.ReferenceValue(runtime.KindTable, runtime.NewTable()))
	if err == nil || !strings.Contains(runtime.ErrorObject(err).String, "bad argument #1 to 'write'") {
		// 参数类型错误优先于关闭输出句柄，便于 Lua 调用边界把函数名改写为 io.write。
		t.Fatalf("Write invalid argument with closed output error = %v", err)
	}
}

// TestLinesReadsDefaultInput 验证 io.lines 从当前默认输入逐行读取。
//
// 该用例使用 strings.Reader 避免读取真实 stdin，并确认返回行不包含换行符。
func TestLinesReadsDefaultInput(t *testing.T) {
	// 测试会修改包级默认输入，结束时必须恢复。
	defer restoreDefaultFiles(defaultInput, defaultOutput, defaultError)()

	defaultInput = NewFile("input", strings.NewReader("first\nsecond\r\nthird"), nil, nil, nil)
	values, err := Lines()
	if err != nil {
		// 默认输入可读时 io.lines 应返回 iterator。
		t.Fatalf("Lines failed: %v", err)
	}
	if len(values) != 1 || values[0].Kind != runtime.KindGoClosure {
		// io.lines 必须返回 Go closure 形式的 iterator。
		t.Fatalf("Lines result = %#v, want iterator", values)
	}
	iterator := values[0].Ref.(runtime.GoResultsFunction)
	for index, want := range []string{"first", "second", "third"} {
		// 每次调用 iterator 读取下一行。
		result, err := iterator()
		if err != nil {
			// iterator 读取有效内容不应失败。
			t.Fatalf("iterator call %d failed: %v", index, err)
		}
		if len(result) != 1 || result[0].String != want {
			// 行内容必须去掉行尾换行符。
			t.Fatalf("line %d = %#v, want %q", index, result, want)
		}
	}
	result, err := iterator()
	if err != nil {
		// EOF 结束不应作为错误返回。
		t.Fatalf("iterator EOF failed: %v", err)
	}
	if len(result) != 0 {
		// EOF 时 iterator 应返回 nil 结果。
		t.Fatalf("EOF result = %#v, want empty", result)
	}

	defaultInput = NewFile("input-lines-L", strings.NewReader("\nline\nother"), nil, nil, nil)
	lineWithEndValues, err := Lines(runtime.NilValue(), runtime.StringValue("L"))
	if err != nil {
		// io.lines(nil, "L") 应使用默认输入并保留格式参数。
		t.Fatalf("Lines nil L failed: %v", err)
	}
	lineWithEndIterator := lineWithEndValues[0].Ref.(runtime.GoResultsFunction)
	var builder strings.Builder
	for {
		// 持续读取直到 EOF。
		values, err := lineWithEndIterator()
		if err != nil {
			t.Fatalf("Lines nil L iterator failed: %v", err)
		}
		if len(values) == 0 {
			break
		}
		builder.WriteString(values[0].String)
	}
	if builder.String() != "\nline\nother" {
		// L 格式必须保留行尾换行。
		t.Fatalf("Lines nil L text = %q", builder.String())
	}
}

// TestFilesystemAccessAndProcessPolicy 验证基础文件访问与 popen 进程管道。
//
// 该用例覆盖 io.open/input/output/lines 的路径形式，并确认 io.popen 可读取 shell 输出。
func TestFilesystemAccessAndProcessPolicy(t *testing.T) {
	// 测试会修改包级默认句柄，结束时必须恢复。
	defer restoreDefaultFiles(defaultInput, defaultOutput, defaultError)()

	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "input.txt")
	outputPath := filepath.Join(dir, "output.txt")
	if err := os.WriteFile(sourcePath, []byte("alpha\nbeta\n"), 0o666); err != nil {
		// 测试输入文件必须可创建。
		t.Fatalf("WriteFile input failed: %v", err)
	}
	if _, err := OpenFile(runtime.StringValue(sourcePath), runtime.StringValue("rb+")); !errors.Is(err, runtime.ErrLuaError) {
		// Lua 5.3 拒绝 rb+ 顺序，只接受 r+b。
		t.Fatalf("OpenFile rb+ error = %v, want Lua error", err)
	}
	if values, err := OpenFile(runtime.StringValue(sourcePath), runtime.StringValue("r+b")); err != nil {
		// r+b 是 Lua 5.3 合法模式。
		t.Fatalf("OpenFile r+b failed: %v", err)
	} else if file, err := fileArgument(values, 1, "open"); err != nil {
		// 合法模式必须返回 file userdata。
		t.Fatalf("OpenFile r+b result is not file: %v", err)
	} else {
		// 测试打开的文件需要及时关闭。
		_ = file.Close()
	}

	openValues, err := OpenFile(runtime.StringValue(sourcePath), runtime.StringValue("r"))
	if err != nil {
		// io.open 读取已有文件不应失败。
		t.Fatalf("OpenFile read failed: %v", err)
	}
	openFile, err := fileArgument(openValues, 1, "open")
	if err != nil {
		// io.open 成功时必须返回 file userdata。
		t.Fatalf("OpenFile result is not file: %v", err)
	}
	defer openFile.Close()
	firstLine, err := FileRead(NewFileValue(nil, openFile), runtime.StringValue("l"))
	if err != nil {
		// 打开的文件应可读取。
		t.Fatalf("FileRead opened file failed: %v", err)
	}
	if len(firstLine) != 1 || firstLine[0].String != "alpha" {
		// 读取内容必须来自宿主文件。
		t.Fatalf("OpenFile first line = %#v, want alpha", firstLine)
	}

	if _, err := Input(runtime.StringValue(sourcePath)); err != nil {
		// io.input 字符串路径应切换默认输入。
		t.Fatalf("Input string failed: %v", err)
	}
	readValues, err := Read(runtime.StringValue("a"))
	if err != nil {
		// 默认输入切换后应可全量读取。
		t.Fatalf("Read default input failed: %v", err)
	}
	if len(readValues) != 1 || readValues[0].String != "alpha\nbeta\n" {
		// io.read("a") 必须读取文件全部内容。
		t.Fatalf("Read default input = %#v, want file content", readValues)
	}
	if _, err := Close(NewFileValue(nil, defaultInput)); err != nil {
		// Windows 不允许删除仍被打开的临时文件，读取后必须显式关闭默认输入。
		t.Fatalf("Close default input failed: %v", err)
	}

	if _, err := Output(runtime.StringValue(outputPath)); err != nil {
		// io.output 字符串路径应切换默认输出。
		t.Fatalf("Output string failed: %v", err)
	}
	if _, err := Write(runtime.StringValue("written")); err != nil {
		// 默认输出切换后应可写入文件。
		t.Fatalf("Write default output failed: %v", err)
	}
	closeOutputResult, err := Close()
	if err != nil {
		// 关闭默认输出应刷新并关闭文件。
		t.Fatalf("Close default output failed: %v", err)
	}
	if len(closeOutputResult) != 1 || closeOutputResult[0].Kind != runtime.KindBoolean || !closeOutputResult[0].Bool {
		// 关闭默认输出成功时返回 true，兼容 assert(io.close())。
		t.Fatalf("Close default output result = %#v, want true", closeOutputResult)
	}
	if bytes, err := os.ReadFile(outputPath); err != nil || string(bytes) != "written" {
		// 输出文件内容必须匹配写入文本。
		t.Fatalf("output file = %q err=%v, want written", string(bytes), err)
	}

	lineValues, err := Lines(runtime.StringValue(sourcePath))
	if err != nil {
		// io.lines(filename) 应返回独立 iterator。
		t.Fatalf("Lines string failed: %v", err)
	}
	iterator := lineValues[0].Ref.(runtime.GoResultsFunction)
	for index, want := range []string{"alpha", "beta"} {
		// iterator 每次读取文件下一行。
		result, err := iterator()
		if err != nil {
			// 行读取不应失败。
			t.Fatalf("Lines iterator %d failed: %v", index, err)
		}
		if len(result) != 1 || result[0].String != want {
			// 返回内容必须去除换行。
			t.Fatalf("Lines iterator %d = %#v, want %q", index, result, want)
		}
	}
	if result, err := iterator(); err != nil || len(result) != 0 {
		// 路径型 iterator 必须在 EOF 调用时自动关闭文件，避免 Windows 临时文件删除失败。
		t.Fatalf("Lines iterator EOF = %#v err=%v, want empty", result, err)
	}
	popenValues, err := POpen(runtime.StringValue("printf 'demo\\n'"), runtime.StringValue("r"))
	if err != nil {
		// io.popen 读模式应可启动宿主 shell 并返回 file。
		t.Fatalf("POpen read failed: %v", err)
	}
	popenFile, err := fileArgument(popenValues, 1, "popen")
	if err != nil {
		// popen 成功时必须返回 file userdata。
		t.Fatalf("POpen result is not file: %v", err)
	}
	popenLine, err := FileRead(NewFileValue(nil, popenFile), runtime.StringValue("l"))
	if err != nil {
		// popen 输出应可按行读取。
		t.Fatalf("POpen read line failed: %v", err)
	}
	if len(popenLine) != 1 || popenLine[0].String != "demo" {
		// shell 输出必须去掉行尾换行。
		t.Fatalf("POpen line = %#v, want demo", popenLine)
	}
	popenClose, err := Close(NewFileValue(nil, popenFile))
	if err != nil {
		// popen close 应等待 shell 正常退出。
		t.Fatalf("POpen close failed: %v", err)
	}
	if len(popenClose) != 3 || popenClose[0].Kind != runtime.KindBoolean || !popenClose[0].Bool || popenClose[1].String != "exit" || popenClose[2].Integer != 0 {
		// popen close 成功时返回 Lua 5.3 pclose 三元组。
		t.Fatalf("POpen close = %#v, want true exit 0", popenClose)
	}
	popenFailureValues, err := POpen(runtime.StringValue("exit 7"), runtime.StringValue("r"))
	if err != nil {
		// 非零退出命令仍应成功创建 popen file。
		t.Fatalf("POpen failure command failed: %v", err)
	}
	popenFailureFile, err := fileArgument(popenFailureValues, 1, "popen")
	if err != nil {
		// popen 失败命令仍必须返回 file userdata。
		t.Fatalf("POpen failure result is not file: %v", err)
	}
	popenFailureClose, err := Close(NewFileValue(nil, popenFailureFile))
	if err != nil {
		// 非零退出必须通过返回值表达，不应抛 Lua error。
		t.Fatalf("POpen failure close failed: %v", err)
	}
	if len(popenFailureClose) != 3 || !popenFailureClose[0].IsNil() || popenFailureClose[1].String != "exit" || popenFailureClose[2].Integer != 7 {
		// popen 非零退出必须返回 nil, "exit", code。
		t.Fatalf("POpen failure close = %#v, want nil exit 7", popenFailureClose)
	}
	if goruntime.GOOS != "windows" {
		// Unix-like 平台需要区分信号终止，覆盖官方 files.lua 的 HUP/KILL 用例。
		popenSignalValues, err := POpen(runtime.StringValue("kill -s HUP $$"), runtime.StringValue("r"))
		if err != nil {
			// 信号终止命令仍应成功创建 popen file。
			t.Fatalf("POpen signal command failed: %v", err)
		}
		popenSignalFile, err := fileArgument(popenSignalValues, 1, "popen")
		if err != nil {
			// popen 信号命令仍必须返回 file userdata。
			t.Fatalf("POpen signal result is not file: %v", err)
		}
		popenSignalClose, err := Close(NewFileValue(nil, popenSignalFile))
		if err != nil {
			// 信号终止必须通过返回值表达，不应抛 Lua error。
			t.Fatalf("POpen signal close failed: %v", err)
		}
		if len(popenSignalClose) != 3 || !popenSignalClose[0].IsNil() || popenSignalClose[1].String != "signal" || popenSignalClose[2].Integer != 1 {
			// HUP 信号必须返回 nil, "signal", 1。
			t.Fatalf("POpen signal close = %#v, want nil signal 1", popenSignalClose)
		}
		popenNestedValues, err := POpen(runtime.StringValue("sh -c 'kill -s HUP $$'"), runtime.StringValue("r"))
		if err != nil {
			// 嵌套 shell 信号命令仍应成功创建 popen file。
			t.Fatalf("POpen nested shell command failed: %v", err)
		}
		popenNestedFile, err := fileArgument(popenNestedValues, 1, "popen")
		if err != nil {
			// popen 嵌套 shell 命令仍必须返回 file userdata。
			t.Fatalf("POpen nested shell result is not file: %v", err)
		}
		popenNestedClose, err := Close(NewFileValue(nil, popenNestedFile))
		if err != nil {
			// 嵌套 shell 信号终止必须通过返回值表达。
			t.Fatalf("POpen nested shell close failed: %v", err)
		}
		if len(popenNestedClose) != 3 || !popenNestedClose[0].IsNil() || popenNestedClose[1].String != "exit" || popenNestedClose[2].Integer <= 0 {
			// 官方 files.lua 只要求嵌套 shell 返回 exit 且状态码为正。
			t.Fatalf("POpen nested shell close = %#v, want nil exit positive", popenNestedClose)
		}
	}
}

func TestPOpenWithOptionsDeniesExplicitlyDisabledProcess(t *testing.T) {
	options := runtime.Options{}.WithProcessAccess(false)
	if _, err := POpenWithOptions(options, runtime.StringValue("exit 0"), runtime.StringValue("r")); !errors.Is(err, runtime.ErrLuaError) {
		t.Fatalf("POpenWithOptions disabled error = %v, want Lua error", err)
	}
}
