// Package iolib 实现 Lua 5.3 io 标准库的第一阶段能力。
//
// 本包当前注册标准流、file userdata、基础路径文件访问、io.close 和 io.flush。进程管道等
// 更高风险能力会在后续兼容阶段逐步开放。
package iolib

import (
	"bufio"
	"fmt"
	gio "io"
	"os"
	"os/exec"
	goruntime "runtime"
	"strconv"
	"strings"

	"github.com/ZingYao/go-lua-vm/runtime"
	"github.com/ZingYao/go-lua-vm/stdlib/internal/procstatus"
)

// Flusher 表示 file userdata 可选的刷新能力。
//
// Go 标准库没有统一的 Flush 接口，本接口用于适配 bufio.Writer、测试替身和后续自定义
// 文件包装对象。Flush 返回的错误会按 Lua 运行期错误向上传递。
type Flusher interface {
	Flush() error
}

// File 表示 Lua 5.3 file userdata 的纯 Go 承载对象。
//
// name 只用于错误消息和调试展示；reader、writer、flusher、closer 分别承载读写、刷新、
// 关闭能力。standard 为 true 表示 stdin/stdout/stderr，此类句柄不能被 Lua 关闭宿主进程资源。
type File struct {
	// name 保存文件在 Lua 错误消息中的可读名称。
	name string
	// reader 保存后续 file:read/io.read 使用的读取端能力。
	reader gio.Reader
	// writer 保存后续 file:write/io.write 使用的写入端能力。
	writer gio.Writer
	// writeBuffer 保存 setvbuf("full"/"line") 启用后的写缓冲器。
	writeBuffer *bufio.Writer
	// bufferMode 保存当前写缓冲模式，空字符串表示未显式设置。
	bufferMode string
	// flusher 保存 io.flush 或 file:flush 使用的刷新能力。
	flusher Flusher
	// closer 保存 io.close 或 file:close 使用的关闭能力。
	closer gio.Closer
	// seeker 保存 file:seek 使用的定位能力。
	seeker gio.Seeker
	// lineReader 保存按行读取的缓冲器，io.lines 和 file:lines 会复用该读取位置。
	lineReader *bufio.Reader
	// standard 标记该文件是否为宿主标准流，标准流关闭必须保持 no-op。
	standard bool
	// closed 标记当前 file 是否已经关闭。
	closed bool
}

// popenCloser 负责关闭 io.popen 管道并等待宿主 shell 进程退出。
//
// pipe 是 StdoutPipe 或 StdinPipe 返回的管道端；cmd 是已经 Start 的宿主命令。
type popenCloser struct {
	// pipe 保存需要关闭的管道端。
	pipe gio.Closer
	// cmd 保存需要等待的宿主 shell 进程。
	cmd *exec.Cmd
}

// Close 关闭 popen 管道并等待 shell 进程结束。
//
// 返回错误会由 file:close 转换为 Lua error；关闭管道后等待 shell，避免产生僵尸进程。
func (closer *popenCloser) Close() error {
	// 先关闭管道端，让等待进程前的资源状态明确。
	_, err := closer.CloseStatus()
	return err
}

// CloseStatus 关闭 popen 管道并返回 Lua 5.3 pclose 状态三元组。
//
// 正常退出返回 `true, "exit", 0`；非零退出或信号终止按失败三元组返回，不抛 Lua error。
// 只有管道关闭或命令等待本身出现非退出状态错误时才返回 error。
func (closer *popenCloser) CloseStatus() ([]runtime.Value, error) {
	if closer == nil {
		// nil closer 没有资源需要释放。
		return procstatus.ValuesFromRunError(nil)
	}
	if closer.pipe != nil {
		// 管道关闭失败说明底层 IO 状态异常，继续等待后返回该错误。
		if err := closer.pipe.Close(); err != nil {
			// pipe.Close 的错误通常已足够说明资源释放失败。
			return nil, runtime.RaiseError(runtime.StringValue(err.Error()))
		}
	}
	if closer.cmd == nil {
		// 没有命令对象时只需完成管道关闭。
		return procstatus.ValuesFromRunError(nil)
	}
	return procstatus.ValuesFromRunError(closer.cmd.Wait())
}

var (
	// defaultInput 保存当前阶段的默认输入句柄，对应 Lua 启动时的 io.stdin。
	defaultInput = NewStandardFile("stdin", os.Stdin, nil)
	// defaultOutput 保存当前阶段的默认输出句柄，对应 Lua 启动时的 io.stdout。
	defaultOutput = NewStandardFile("stdout", nil, os.Stdout)
	// defaultError 保存当前阶段的默认错误输出句柄，对应 Lua 启动时的 io.stderr。
	defaultError = NewStandardFile("stderr", nil, os.Stderr)
	// defaultInputValue 保存当前默认输入对应的 Lua userdata identity。
	defaultInputValue runtime.Value
	// defaultOutputValue 保存当前默认输出对应的 Lua userdata identity。
	defaultOutputValue runtime.Value
	// defaultErrorValue 保存当前默认错误输出对应的 Lua userdata identity。
	defaultErrorValue runtime.Value
)

const (
	// readNumberMaxTokenLength 限制 `io.read("n")` 单次扫描的数字候选长度。
	//
	// Lua 5.3 底层会使用固定长度缓冲读取数字；超长数字应读取一个前缀后失败，并把剩余内容留给
	// 后续 read，官方 files.lua 依赖该行为验证长数字不会整行吞掉。
	readNumberMaxTokenLength = 200
	// maxLineFormats 限制 io.lines/file:lines 单次 iterator 支持的读取格式数量。
	//
	// Lua 5.3 官方 files.lua 使用 250 个字节读取格式作为合法边界，并要求 251 个格式参数报
	// `too many arguments`，因此这里显式保留该兼容阈值。
	maxLineFormats = 250
)

// NewFile 创建普通 file userdata 承载对象。
//
// name 用于调试和错误消息；reader、writer、flusher、closer 可按能力传入 nil。普通文件的
// Close 会尝试调用 closer，Flush 会尝试调用 flusher。该函数不触发宿主路径访问。
func NewFile(name string, reader gio.Reader, writer gio.Writer, flusher Flusher, closer gio.Closer) *File {
	// 普通 file 直接记录所有能力，生命周期由对应 userdata 或调用方管理。
	file := &File{
		name:    name,
		reader:  reader,
		writer:  writer,
		flusher: flusher,
		closer:  closer,
	}
	if seeker, ok := reader.(gio.Seeker); ok {
		// reader 同时实现 Seek 时记录定位能力。
		file.seeker = seeker
	} else if seeker, ok := writer.(gio.Seeker); ok {
		// writer 同时实现 Seek 时记录定位能力。
		file.seeker = seeker
	}
	return file
}

// NewStandardFile 创建标准流 file 承载对象。
//
// 标准流对应宿主已经打开的 stdin/stdout/stderr，不允许 Lua close 真正关闭底层句柄；
// flush 也保持保守 no-op，避免终端、管道和平台差异导致不稳定行为。
func NewStandardFile(name string, reader gio.Reader, writer gio.Writer) *File {
	// 标准流只记录读写能力，并通过 standard 标记屏蔽关闭和底层同步。
	return &File{
		name:     name,
		reader:   reader,
		writer:   writer,
		standard: true,
	}
}

// Name 返回 file userdata 的调试名称。
//
// 返回值仅用于错误消息和测试断言，不参与 Lua 5.3 可观察路径语义。
func (file *File) Name() string {
	// nil 文件没有名称，返回空字符串保持调用方可安全展示。
	if file == nil {
		return ""
	}

	// 非 nil 文件直接返回创建时记录的名称。
	return file.name
}

// Closed 返回 file userdata 是否已经关闭。
//
// 标准流关闭为 no-op，因此标准流通常保持未关闭；普通文件 Close 成功或重复调用后返回 true。
func (file *File) Closed() bool {
	// nil 文件视为不可用的关闭状态。
	if file == nil {
		return true
	}

	// 返回内部关闭标记。
	return file.closed
}

// Read 从 file userdata 当前位置读取二进制字节。
//
// buffer 由调用方提供且可为空；返回值遵循 io.Reader 语义。已关闭或不可读文件返回
// Lua error；如果 file:read 已经建立缓冲器，则继续复用该缓冲器，避免丢失预读字节。
func (file *File) Read(buffer []byte) (int, error) {
	// 读取前统一校验文件生命周期和读端能力。
	if file == nil || file.closed {
		// 空文件和已关闭文件都不能继续读取。
		return 0, runtime.RaiseError(runtime.StringValue("attempt to use a closed file"))
	}
	if file.reader == nil {
		// 只写 file userdata 没有可用读端。
		return 0, runtime.RaiseError(runtime.StringValue("file is not readable"))
	}
	reader := file.reader
	if file.lineReader != nil {
		// 复用既有行缓冲器，保持与 file:read 共享同一当前位置。
		reader = file.lineReader
	}
	return reader.Read(buffer)
}

// Flush 刷新 file userdata。
//
// 标准流当前保持 no-op；普通文件若提供 Flusher 则调用 Flush，否则按 Lua 兼容方向视为成功。
// 已关闭文件会返回 Lua 错误，由上层包装成 `attempt to use a closed file`。
func (file *File) Flush() error {
	// Flush 先校验 file 是否可用。
	if file == nil {
		// nil 文件通常来自错误 userdata，返回明确 Lua 错误。
		return runtime.RaiseError(runtime.StringValue("attempt to use a closed file"))
	}
	if file.closed {
		// 已关闭文件不能继续刷新。
		return runtime.RaiseError(runtime.StringValue("attempt to use a closed file"))
	}
	if file.standard {
		// 标准流刷新先保持 no-op，避免 Sync 终端或管道造成平台差异。
		return nil
	}
	if file.flusher == nil {
		// 未提供底层刷新能力时仍需刷新 Lua 层 setvbuf 创建的缓冲。
		if file.writeBuffer != nil {
			if err := file.writeBuffer.Flush(); err != nil {
				return runtime.RaiseError(runtime.StringValue(err.Error()))
			}
		}
		return nil
	}
	if file.writeBuffer != nil {
		// Lua 层写缓冲必须先落到底层 writer，再调用底层刷新能力。
		if err := file.writeBuffer.Flush(); err != nil {
			return runtime.RaiseError(runtime.StringValue(err.Error()))
		}
	}

	// 普通文件把底层刷新错误转换为 Lua 运行期错误。
	if err := file.flusher.Flush(); err != nil {
		// Flush 失败时保留底层错误文本，便于嵌入方诊断。
		return runtime.RaiseError(runtime.StringValue(err.Error()))
	}
	return nil
}

// Close 关闭 file userdata。
//
// 标准流关闭为 no-op；普通文件关闭成功后标记 closed。重复关闭保持幂等，避免 State.Close
// finalizer 与显式 io.close 之间重复释放同一资源。
func (file *File) Close() error {
	// Close 先校验 file 是否可用。
	if file == nil {
		// nil 文件没有底层资源，返回明确 Lua 错误。
		return runtime.RaiseError(runtime.StringValue("attempt to use a closed file"))
	}
	if file.standard {
		// 标准流不能被 Lua 真正关闭，否则会影响宿主进程。
		return nil
	}
	if file.closed {
		// 重复关闭保持幂等，State.Close 和显式 close 可安全组合。
		return nil
	}
	if file.writeBuffer != nil {
		// 关闭前必须刷新 Lua 层写缓冲，匹配 Lua 文件 close 会提交缓冲内容的语义。
		if err := file.writeBuffer.Flush(); err != nil {
			return runtime.RaiseError(runtime.StringValue(err.Error()))
		}
	}
	file.closed = true
	if file.closer == nil {
		// 没有底层 closer 时只更新状态即可。
		return nil
	}

	// 普通文件把底层关闭错误转换为 Lua 运行期错误。
	if err := file.closer.Close(); err != nil {
		// Close 失败时保留底层错误文本，便于嵌入方诊断。
		return runtime.RaiseError(runtime.StringValue(err.Error()))
	}
	return nil
}

// NewFileValue 创建可放入 Lua 栈或 table 的 file userdata 值。
//
// state 可为 nil；非 nil 时会注册 userdata finalizer，State.Close 会兜底关闭普通文件。
// 标准流 finalizer 关闭仍为 no-op，不会影响宿主 stdin/stdout/stderr。
func NewFileValue(state *runtime.State, file *File) runtime.Value {
	// userdata 承载 file 指针，并通过 finalizer 兜底释放普通文件。
	userdata := runtime.NewUserdataWithFinalizer(file, func(payload any) error {
		// finalizer 只处理 *File，其他 payload 类型直接忽略。
		filePayload, ok := payload.(*File)
		if !ok {
			// 非 *File 表示构造路径异常；关闭阶段保持兜底不传播。
			return nil
		}
		return filePayload.Close()
	})
	_ = userdata.SetMetatable(newFileMetatable())
	value := userdata.Value()
	if state != nil && !state.IsClosed() {
		// State 存活时注册 userdata，确保 State.Close 具有资源回收路径。
		_ = state.RegisterUserdata(userdata)
	}
	return value
}

// currentFileValue 返回 file 对应的稳定 Lua userdata。
//
// current 若已经包装同一个 *File，则直接复用以保持 rawequal；否则创建新的 userdata。
func currentFileValue(state *runtime.State, file *File, current runtime.Value) runtime.Value {
	// 已有 userdata 指向同一 file 时复用，满足标准库句柄 identity 语义。
	if existingFile, ok := fileFromValue(current); ok && existingFile == file {
		return current
	}

	// 没有可复用 userdata 时创建新包装。
	return NewFileValue(state, file)
}

// newFileMetatable 创建 file userdata 的方法元表。
//
// 返回元表包含 `__index` 方法表，覆盖 Lua 5.3 file 对象第一阶段已实现的 close、flush、
// lines、read、seek、setvbuf 与 write。调用方为每个 userdata 创建独立元表，避免测试或
// 嵌入方修改某个文件元表时污染其他文件对象。
func newFileMetatable() *runtime.Table {
	// 先构造方法表，再挂到元表 __index 字段。
	methods := runtime.NewTable()
	methods.RawSetString("close", runtime.ReferenceValue(runtime.KindGoClosure, runtime.GoResultsFunction(FileClose)))
	methods.RawSetString("flush", runtime.ReferenceValue(runtime.KindGoClosure, runtime.GoResultsFunction(FileFlush)))
	methods.RawSetString("lines", runtime.ReferenceValue(runtime.KindGoClosure, runtime.GoResultsFunction(FileLines)))
	methods.RawSetString("read", runtime.ReferenceValue(runtime.KindGoClosure, runtime.GoResultsFunction(FileRead)))
	methods.RawSetString("seek", runtime.ReferenceValue(runtime.KindGoClosure, runtime.GoResultsFunction(FileSeek)))
	methods.RawSetString("setvbuf", runtime.ReferenceValue(runtime.KindGoClosure, runtime.GoResultsFunction(FileSetVBuf)))
	methods.RawSetString("write", runtime.ReferenceValue(runtime.KindGoClosure, runtime.GoResultsFunction(FileWrite)))

	metatable := runtime.NewTable()
	metatable.RawSetString("__name", runtime.StringValue("FILE*"))
	metatable.RawSetString("__gc", runtime.ReferenceValue(runtime.KindGoClosure, runtime.GoResultsFunction(FileGC)))
	metatable.RawSetString("__tostring", runtime.ReferenceValue(runtime.KindGoClosure, runtime.GoResultsFunction(FileToString)))
	metatable.RawSetString("__index", runtime.ReferenceValue(runtime.KindTable, methods))
	return metatable
}

// Open 将 Lua 5.3 io 标准库注册到 State 全局环境。
//
// state 必须非 nil 且未关闭；成功后全局 `io` 字段指向库表，包含 close、flush、stdin、
// stdout 和 stderr，并开放官方测试套件所需的基础路径文件读写。
func Open(state *runtime.State) error {
	// 注册入口先校验 State 生命周期，避免向关闭后的全局表写入库函数。
	if state == nil {
		// nil State 没有 globals，调用方需要先创建 runtime.State。
		return fmt.Errorf("io library unavailable: %w", runtime.ErrNilState)
	}
	if state.IsClosed() {
		// 已关闭 State 的 globals 已释放，不能继续注册标准库。
		return fmt.Errorf("io library unavailable: %w", runtime.ErrClosedState)
	}

	options := state.Options()
	library := runtime.NewTable()
	// io 库函数以 Go closure 注册，后续 VM CALL 会通过 bridge 调用。
	library.RawSetString("close", runtime.ReferenceValue(runtime.KindGoClosure, runtime.GoResultsFunction(Close)))
	library.RawSetString("flush", runtime.ReferenceValue(runtime.KindGoClosure, runtime.GoResultsFunction(Flush)))
	library.RawSetString("input", runtime.ReferenceValue(runtime.KindGoClosure, runtime.GoResultsFunction(func(args ...runtime.Value) ([]runtime.Value, error) {
		// io.input 只有字符串路径会访问宿主文件系统，file userdata 切换仍允许。
		return InputWithOptions(options, args...)
	})))
	library.RawSetString("lines", runtime.ReferenceValue(runtime.KindGoClosure, runtime.GoResultsFunction(func(args ...runtime.Value) ([]runtime.Value, error) {
		// io.lines 只有字符串路径会访问宿主文件系统，默认输入迭代仍允许。
		return LinesWithOptions(options, args...)
	})))
	library.RawSetString("open", runtime.ReferenceValue(runtime.KindGoClosure, runtime.GoResultsFunction(func(args ...runtime.Value) ([]runtime.Value, error) {
		// io.open 始终访问宿主路径，注册入口按 State 权限控制。
		return OpenFileWithOptions(options, args...)
	})))
	library.RawSetString("output", runtime.ReferenceValue(runtime.KindGoClosure, runtime.GoResultsFunction(func(args ...runtime.Value) ([]runtime.Value, error) {
		// io.output 只有字符串路径会访问宿主文件系统，file userdata 切换仍允许。
		return OutputWithOptions(options, args...)
	})))
	library.RawSetString("popen", runtime.ReferenceValue(runtime.KindGoClosure, runtime.GoResultsFunction(func(args ...runtime.Value) ([]runtime.Value, error) {
		// io.popen 启动宿主进程，默认嵌入模式必须关闭。
		return POpenWithOptions(options, args...)
	})))
	library.RawSetString("read", runtime.ReferenceValue(runtime.KindGoClosure, runtime.GoResultsFunction(Read)))
	library.RawSetString("tmpfile", runtime.ReferenceValue(runtime.KindGoClosure, runtime.GoResultsFunction(func(args ...runtime.Value) ([]runtime.Value, error) {
		// io.tmpfile 创建宿主临时文件，注册入口按 State 权限控制。
		return TmpFileWithOptions(options, args...)
	})))
	library.RawSetString("type", runtime.ReferenceValue(runtime.KindGoClosure, runtime.GoResultsFunction(Type)))
	library.RawSetString("write", runtime.ReferenceValue(runtime.KindGoClosure, runtime.GoResultsFunction(Write)))
	defaultInputValue = currentFileValue(state, defaultInput, defaultInputValue)
	defaultOutputValue = currentFileValue(state, defaultOutput, defaultOutputValue)
	defaultErrorValue = currentFileValue(state, defaultError, defaultErrorValue)
	library.RawSetString("stdin", defaultInputValue)
	library.RawSetString("stdout", defaultOutputValue)
	library.RawSetString("stderr", defaultErrorValue)
	state.SetGlobal("io", runtime.ReferenceValue(runtime.KindTable, library))
	return nil
}

// Read 实现 Lua 5.3 `io.read` 的第一阶段语义。
//
// 当前从默认输入读取；无参数等价于 `*l`，支持 `*l`、`*a` 和正整数 byte count。
// `*n` 数字扫描会在后续更完整的格式读取阶段补齐。
func Read(args ...runtime.Value) ([]runtime.Value, error) {
	// 默认输入必须存在且具备读取能力。
	if defaultInput != nil && defaultInput.closed {
		// Lua 5.3 对当前默认输入关闭后的 io.read 使用专门错误文本。
		return nil, fmt.Errorf(" input file is closed")
	}
	if defaultInput == nil || defaultInput.reader == nil {
		// 没有可读默认输入时返回 Lua 错误。
		return nil, runtime.RaiseError(runtime.StringValue("default input is not readable"))
	}
	if len(args) == 0 {
		// 无参数按 Lua 默认格式读取一行。
		return readOne(defaultInput, runtime.StringValue("*l"), "read")
	}

	results := make([]runtime.Value, 0, len(args))
	for index, format := range args {
		// 逐个读取格式并追加对应结果。
		values, err := readOne(defaultInput, format, "read")
		if err != nil {
			// 任一格式读取失败立即返回错误，已读取结果不再暴露。
			return nil, err
		}
		if len(values) == 0 {
			// 内部读取函数不应返回空结果，保持防御式错误。
			return nil, runtime.RaiseError(runtime.StringValue("io.read internal empty result"))
		}
		results = append(results, values[0])
		if values[0].IsNil() {
			// Lua read 在某个格式 EOF 后停止继续读取后续格式。
			_ = index
			break
		}
	}
	return results, nil
}

// TmpFile 实现 Lua 5.3 `io.tmpfile` 的默认 sandbox 策略。
//
// 临时文件仍属于宿主文件系统写入能力；当前默认策略下返回 Lua 错误，后续接入
// AllowHostFilesystem 与临时目录隔离后再创建真实文件。
func TmpFile(args ...runtime.Value) ([]runtime.Value, error) {
	// io.tmpfile 不接受参数，当前阶段忽略多余参数并创建可自动关闭的宿主临时文件。
	file, err := os.CreateTemp("", "go-lua-vm-io-*")
	if err != nil {
		// 临时文件创建失败时按 Lua error 暴露底层原因。
		return nil, runtime.RaiseError(runtime.StringValue(err.Error()))
	}
	return []runtime.Value{NewFileValue(nil, NewFile(file.Name(), file, file, nil, file))}, nil
}

// TmpFileWithOptions 按 State 文件系统策略执行 Lua 5.3 `io.tmpfile`。
//
// options.AllowHostFilesystem 为 false 时返回 Lua error；授权后才创建宿主临时文件。
func TmpFileWithOptions(options runtime.Options, args ...runtime.Value) ([]runtime.Value, error) {
	// 临时文件属于宿主文件系统写入能力，默认嵌入模式拒绝。
	if !options.AllowHostFilesystem {
		// 禁用策略必须在创建临时文件前生效，避免留下宿主文件副作用。
		return nil, filesystemDisabledError("tmpfile")
	}
	return TmpFile(args...)
}

// Type 实现 Lua 5.3 `io.type`。
//
// file userdata 未关闭时返回 `"file"`，已关闭时返回 `"closed file"`；非 file userdata
// 或其他 Lua 值返回 nil。该函数不触发错误。
func Type(args ...runtime.Value) ([]runtime.Value, error) {
	// io.type 缺失参数时返回 nil。
	if len(args) == 0 || args[0].IsNil() {
		return []runtime.Value{runtime.NilValue()}, nil
	}
	file, ok := fileFromValue(args[0])
	if !ok {
		// 非 file userdata 按 Lua 语义返回 nil。
		return []runtime.Value{runtime.NilValue()}, nil
	}
	if file.closed {
		// 已关闭 file 返回 Lua 约定文本。
		return []runtime.Value{runtime.StringValue("closed file")}, nil
	}
	return []runtime.Value{runtime.StringValue("file")}, nil
}

// Write 实现 Lua 5.3 `io.write` 的第一阶段语义。
//
// 当前写入默认输出；参数必须是 string、integer 或 number。成功时返回当前输出 file，底层写入错误
// 转换为 Lua error。无参数时只校验默认输出可写并返回当前输出 file。
func Write(args ...runtime.Value) ([]runtime.Value, error) {
	argumentTexts := make([]string, 0, len(args))
	for index, value := range args {
		// 每个参数都必须先转换为 Lua io.write 文本；参数错误优先于默认输出句柄状态。
		text, err := writeArgument(value, index+1, "write")
		if err != nil {
			// 参数类型不兼容时立即返回 Lua 参数错误，供调用边界补齐 io.write 名称。
			return nil, err
		}
		argumentTexts = append(argumentTexts, text)
	}

	// 默认输出必须存在且具备写入能力。
	if defaultOutput != nil && defaultOutput.closed {
		// Lua 5.3 对当前默认输出关闭后的 io.write 使用专门错误文本。
		return nil, fmt.Errorf(" output file is closed")
	}
	if defaultOutput == nil || defaultOutput.writer == nil {
		// 没有可写默认输出时返回 Lua 错误。
		return nil, runtime.RaiseError(runtime.StringValue("default output is not writable"))
	}
	for _, text := range argumentTexts {
		// 参数文本已经完成校验，开始按顺序写入默认输出。
		if _, err := defaultOutput.writer.Write([]byte(text)); err != nil {
			// 底层写入失败转换为 Lua 运行期错误。
			return nil, runtime.RaiseError(runtime.StringValue(err.Error()))
		}
	}
	defaultOutputValue = currentFileValue(nil, defaultOutput, defaultOutputValue)
	return []runtime.Value{defaultOutputValue}, nil
}

// Input 实现 Lua 5.3 `io.input` 的基础路径与 file 切换语义。
//
// 无参数时返回当前默认输入 file userdata；传入 file userdata 时切换默认输入并返回该文件；
// 传入 string 文件名时按只读模式打开文件并切换默认输入。
func Input(args ...runtime.Value) ([]runtime.Value, error) {
	// 无参数时返回当前默认输入。
	if len(args) == 0 || args[0].IsNil() {
		defaultInputValue = currentFileValue(nil, defaultInput, defaultInputValue)
		return []runtime.Value{defaultInputValue}, nil
	}
	if args[0].Kind == runtime.KindString {
		// 字符串路径按 Lua 5.3 语义打开为默认输入文件。
		values, err := OpenFile(args[0], runtime.StringValue("r"))
		if err != nil {
			// 打开失败时直接传播 Lua error。
			return nil, err
		}
		file, err := fileArgument(values, 1, "input")
		if err != nil {
			// OpenFile 返回值必须是 file userdata；异常时按内部错误暴露。
			return nil, err
		}
		defaultInput = file
		defaultInputValue = values[0]
		return values, nil
	}

	file, err := fileArgument(args, 1, "input")
	if err != nil {
		// 参数不是 file userdata 时返回 Lua 参数错误。
		return nil, err
	}
	if file.closed {
		// 已关闭文件不能设为默认输入。
		return nil, runtime.RaiseError(runtime.StringValue("attempt to use a closed file"))
	}
	defaultInput = file
	defaultInputValue = args[0]
	return []runtime.Value{args[0]}, nil
}

// InputWithOptions 按 State 文件系统策略执行 Lua 5.3 `io.input`。
//
// 字符串路径需要 AllowHostFilesystem；无参数查询和 file userdata 切换不触发宿主路径访问。
func InputWithOptions(options runtime.Options, args ...runtime.Value) ([]runtime.Value, error) {
	// 字符串路径会打开宿主文件，默认必须拒绝。
	if len(args) > 0 && args[0].Kind == runtime.KindString && !options.AllowHostFilesystem {
		return nil, filesystemDisabledError("input")
	}
	return Input(args...)
}

// Lines 实现 Lua 5.3 `io.lines` 的基础迭代语义。
//
// 无参数时返回一个按行读取当前默认输入的 Go iterator；传入文件名时打开该文件并返回迭代器。
// 返回 iterator 每次返回一行，不包含行尾换行符，EOF 返回 nil；文件名路径在 EOF 时关闭。
func Lines(args ...runtime.Value) ([]runtime.Value, error) {
	// 有文件名参数时打开宿主文件并绑定到本次 iterator。
	if len(args) > 0 && !args[0].IsNil() {
		if args[0].Kind == runtime.KindString {
			// 字符串路径按只读模式打开。
			values, err := OpenFile(args[0], runtime.StringValue("r"))
			if err != nil {
				// 打开失败时返回 Lua error。
				return nil, err
			}
			file, err := fileArgument(values, 1, "lines")
			if err != nil {
				// OpenFile 返回值必须是 file userdata。
				return nil, err
			}
			iterator, err := makeLinesIterator(file, args[1:], true)
			if err != nil {
				// 格式参数错误在创建 iterator 时直接返回。
				return nil, err
			}
			return []runtime.Value{runtime.ReferenceValue(runtime.KindGoClosure, iterator)}, nil
		}
		// Lua 5.3 io.lines 第一参数是可选文件名，非字符串按参数错误处理。
		return nil, badArgument("lines", 1, "string expected")
	}
	if defaultInput == nil || defaultInput.reader == nil {
		// 当前默认输入不可读时返回明确 Lua 错误。
		return nil, runtime.RaiseError(runtime.StringValue("default input is not readable"))
	}

	formats := []runtime.Value(nil)
	if len(args) > 0 && args[0].IsNil() {
		// io.lines(nil, formats...) 使用当前默认输入，但仍保留后续读取格式。
		formats = args[1:]
	}
	iterator, err := makeLinesIterator(defaultInput, formats, false)
	if err != nil {
		// 默认输入无格式参数，不应触发参数错误。
		return nil, err
	}
	return []runtime.Value{runtime.ReferenceValue(runtime.KindGoClosure, iterator)}, nil
}

// LinesWithOptions 按 State 文件系统策略执行 Lua 5.3 `io.lines`。
//
// 字符串路径需要 AllowHostFilesystem；无参数迭代当前默认输入不触发路径访问。
func LinesWithOptions(options runtime.Options, args ...runtime.Value) ([]runtime.Value, error) {
	// 字符串路径会打开宿主文件，默认必须拒绝。
	if len(args) > 0 && args[0].Kind == runtime.KindString {
		values, err := OpenFileWithOptions(options, args[0], runtime.StringValue("r"))
		if err != nil {
			// 权限或参数错误直接传播，保持 io.lines(path) 的 Lua error 语义。
			return nil, err
		}
		file, err := fileArgument(values, 1, "lines")
		if err != nil {
			// 打开失败时把 io.open 的第二返回值提升为 io.lines 错误。
			if len(values) > 1 && values[1].Kind == runtime.KindString {
				return nil, runtime.RaiseError(values[1])
			}
			return nil, err
		}
		iterator, err := makeLinesIterator(file, args[1:], true)
		if err != nil {
			// 格式参数错误在创建 iterator 时直接返回。
			return nil, err
		}
		return []runtime.Value{runtime.ReferenceValue(runtime.KindGoClosure, iterator)}, nil
	}
	return Lines(args...)
}

// OpenFile 实现 Lua 5.3 `io.open` 的基础文件打开语义。
//
// 第一个参数是路径字符串，第二个参数是可选模式字符串；支持 Lua 5.3 常见 r/w/a 及加号模式，
// `b` 标记在 Go 中无差异但会被接受。成功时返回 file userdata。
func OpenFile(args ...runtime.Value) ([]runtime.Value, error) {
	// io.open 的第一个参数必须是路径字符串。
	if len(args) == 0 || args[0].Kind != runtime.KindString {
		// 缺失或非字符串路径按 Lua 参数错误处理。
		return nil, badArgument("open", 1, "string expected")
	}
	if len(args) > 1 && !args[1].IsNil() && args[1].Kind != runtime.KindString {
		// mode 参数如果出现，必须是字符串。
		return nil, badArgument("open", 2, "string expected")
	}
	mode := "r"
	if len(args) > 1 && !args[1].IsNil() {
		// 显式 mode 覆盖默认只读模式。
		mode = args[1].String
	}
	flags, err := openFlags(mode)
	if err != nil {
		// 非法模式按参数错误处理。
		return nil, err
	}
	file, err := os.OpenFile(args[0].String, flags, 0o666)
	if err != nil {
		// 打开失败时返回 nil 加错误文本，匹配 Lua io.open 可检查失败的语义。
		return []runtime.Value{runtime.NilValue(), runtime.StringValue(err.Error()), runtime.IntegerValue(1)}, nil
	}
	readable, writable := openModeCapabilities(mode)
	var reader gio.Reader
	if readable {
		// 只在 mode 允许读取时暴露 reader，避免写-only 文件 read 时落到底层 EBADF。
		reader = file
	}
	var writer gio.Writer
	if writable {
		// 只在 mode 允许写入时暴露 writer，避免读-only 文件 write 时落到底层 EBADF。
		writer = file
	}
	return []runtime.Value{NewFileValue(nil, NewFile(args[0].String, reader, writer, nil, file))}, nil
}

// OpenFileWithOptions 按 State 文件系统策略执行 Lua 5.3 `io.open`。
//
// options.AllowHostFilesystem 为 false 时，合法路径也返回 Lua error，避免默认嵌入模式读取宿主文件。
func OpenFileWithOptions(options runtime.Options, args ...runtime.Value) ([]runtime.Value, error) {
	// 参数错误优先于权限错误，保持 Lua 标准库参数诊断稳定。
	if len(args) == 0 || args[0].Kind != runtime.KindString {
		// 缺失或非字符串路径按 Lua 参数错误处理。
		return nil, badArgument("open", 1, "string expected")
	}
	if len(args) > 1 && !args[1].IsNil() && args[1].Kind != runtime.KindString {
		// mode 参数如果出现，必须是字符串。
		return nil, badArgument("open", 2, "string expected")
	}
	mode := "r"
	if len(args) > 1 && !args[1].IsNil() {
		// 显式 mode 覆盖默认只读模式。
		mode = args[1].String
	}
	if _, err := openFlags(mode); err != nil {
		// 非法模式按参数错误处理，优先于权限策略。
		return nil, err
	}
	readable, writable := openModeCapabilities(mode)
	if readable && !writable {
		// 纯读模式允许优先从 VFS 打开；宿主优先时仅在宿主授权后先尝试宿主。
		if options.PreferHostFilesystem && options.AllowHostFilesystem {
			hostValues, _ := OpenFile(args...)
			if len(hostValues) > 0 && !hostValues[0].IsNil() {
				// 宿主文件命中时直接返回，保持优先级承诺。
				return hostValues, nil
			}
			if virtualValues, ok := openVirtualReadFile(options, args[0].String); ok {
				// 宿主未命中后允许 VFS 兜底。
				return virtualValues, nil
			}
			return hostValues, nil
		}
		if virtualValues, ok := openVirtualReadFile(options, args[0].String); ok {
			// 默认 VFS 优先，命中或宿主禁用时都由 VFS 结果决定。
			return virtualValues, nil
		}
	}
	if !options.AllowHostFilesystem {
		// 默认关闭宿主路径访问，避免脚本读取或写入调用方文件。
		return nil, filesystemDisabledError("open")
	}
	return OpenFile(args...)
}

// openVirtualReadFile 按 VFS 策略打开只读 file userdata。
//
// 返回 ok=false 表示未配置 VFS 或 VFS 未命中且宿主仍可兜底；返回 ok=true 表示调用方应直接使用
// 返回值，其中打开失败会以 io.open 的 nil/message/code 三元组表达。
func openVirtualReadFile(options runtime.Options, filename string) ([]runtime.Value, bool) {
	// 未配置 VFS 时调用方继续按宿主权限处理。
	if options.VirtualFilesystem == nil {
		return nil, false
	}
	file, err := runtime.OpenVirtualFile(options, filename)
	if err == nil {
		// VFS 文件只暴露读取和关闭能力，不提供写入能力。
		return []runtime.Value{NewFileValue(nil, NewFile(filename, file, nil, nil, file))}, true
	}
	if options.AllowHostFilesystem {
		// 宿主授权时，VFS 未命中或路径不适配可以继续交给宿主路径处理。
		return nil, false
	}
	return []runtime.Value{runtime.NilValue(), runtime.StringValue(err.Error()), runtime.IntegerValue(1)}, true
}

// Output 实现 Lua 5.3 `io.output` 的基础路径与 file 切换语义。
//
// 无参数时返回当前默认输出 file userdata；传入 file userdata 时切换默认输出并返回该文件；
// 传入 string 文件名时按写入模式打开文件并切换默认输出。
func Output(args ...runtime.Value) ([]runtime.Value, error) {
	// 无参数时返回当前默认输出。
	if len(args) == 0 || args[0].IsNil() {
		defaultOutputValue = currentFileValue(nil, defaultOutput, defaultOutputValue)
		return []runtime.Value{defaultOutputValue}, nil
	}
	if args[0].Kind == runtime.KindString {
		// 字符串路径按 Lua 5.3 语义打开为默认输出文件。
		values, err := OpenFile(args[0], runtime.StringValue("w"))
		if err != nil {
			// 打开失败时直接传播 Lua error。
			return nil, err
		}
		file, err := fileArgument(values, 1, "output")
		if err != nil {
			// OpenFile 返回 nil 时说明打开失败，按 Lua error 暴露失败原因。
			if len(values) > 1 && values[1].Kind == runtime.KindString {
				// io.output 不能接受失败返回，需要抛错阻止后续写入 nil 文件。
				return nil, runtime.RaiseError(values[1])
			}
			return nil, err
		}
		defaultOutput = file
		defaultOutputValue = values[0]
		return values, nil
	}

	file, err := fileArgument(args, 1, "output")
	if err != nil {
		// 参数不是 file userdata 时返回 Lua 参数错误。
		return nil, err
	}
	if file.closed {
		// 已关闭文件不能设为默认输出。
		return nil, runtime.RaiseError(runtime.StringValue("attempt to use a closed file"))
	}
	defaultOutput = file
	defaultOutputValue = args[0]
	return []runtime.Value{args[0]}, nil
}

// OutputWithOptions 按 State 文件系统策略执行 Lua 5.3 `io.output`。
//
// 字符串路径需要 AllowHostFilesystem；无参数查询和 file userdata 切换不触发宿主路径访问。
func OutputWithOptions(options runtime.Options, args ...runtime.Value) ([]runtime.Value, error) {
	// 字符串路径会打开宿主文件，默认必须拒绝。
	if len(args) > 0 && args[0].Kind == runtime.KindString && !options.AllowHostFilesystem {
		return nil, filesystemDisabledError("output")
	}
	return Output(args...)
}

// POpen 实现 Lua 5.3 `io.popen` 的基础宿主 shell 管道语义。
//
// 第一个参数必须是命令字符串；第二个 mode 可选，支持 `r` 和 `w`。成功返回 file userdata，
// 关闭 file 时会等待 shell 进程结束，兼容官方 main.lua 的后台进程输出读取场景。
func POpen(args ...runtime.Value) ([]runtime.Value, error) {
	// io.popen 的第一个参数必须是命令字符串。
	if len(args) == 0 || args[0].Kind != runtime.KindString {
		// 缺失或非字符串命令按 Lua 参数错误处理。
		return nil, badArgument("popen", 1, "string expected")
	}
	if len(args) > 1 && !args[1].IsNil() && args[1].Kind != runtime.KindString {
		// mode 参数如果出现，必须是字符串。
		return nil, badArgument("popen", 2, "string expected")
	}

	mode := "r"
	if len(args) > 1 && !args[1].IsNil() {
		// 显式 mode 覆盖默认读取模式。
		mode = strings.ReplaceAll(args[1].String, "b", "")
	}
	if mode != "r" && mode != "w" {
		// 当前基础实现只开放 Lua 5.3 常用的单向管道。
		return nil, badArgument("popen", 2, "invalid mode")
	}

	name, commandArgs := popenShellCommand(args[0].String)
	cmd := exec.Command(name, commandArgs...)
	if mode == "r" {
		// r 模式读取命令 stdout。
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			// 创建管道失败说明宿主进程能力不可用。
			return nil, runtime.RaiseError(runtime.StringValue(err.Error()))
		}
		if err := cmd.Start(); err != nil {
			// 启动失败时返回 nil 加错误文本，匹配 io.open 类 API 的可检查失败语义。
			return []runtime.Value{runtime.NilValue(), runtime.StringValue(err.Error())}, nil
		}
		file := NewFile("popen", stdout, nil, nil, &popenCloser{pipe: stdout, cmd: cmd})
		return []runtime.Value{NewFileValue(nil, file)}, nil
	}

	// w 模式写入命令 stdin。
	stdin, err := cmd.StdinPipe()
	if err != nil {
		// 创建 stdin 管道失败按 Lua error 暴露底层原因。
		return nil, runtime.RaiseError(runtime.StringValue(err.Error()))
	}
	if err := cmd.Start(); err != nil {
		// 启动失败时返回 nil 加错误文本，调用方可检查并决定降级。
		return []runtime.Value{runtime.NilValue(), runtime.StringValue(err.Error())}, nil
	}
	file := NewFile("popen", nil, stdin, nil, &popenCloser{pipe: stdin, cmd: cmd})
	return []runtime.Value{NewFileValue(nil, file)}, nil
}

// POpenWithOptions 按 State 宿主进程策略执行 Lua 5.3 `io.popen`。
//
// options.AllowProcess 为 false 时，合法命令也返回 Lua error，避免默认嵌入模式启动宿主进程。
func POpenWithOptions(options runtime.Options, args ...runtime.Value) ([]runtime.Value, error) {
	// 参数错误优先于权限错误，保持 Lua 标准库参数诊断稳定。
	if len(args) == 0 || args[0].Kind != runtime.KindString {
		// 缺失或非字符串命令按 Lua 参数错误处理。
		return nil, badArgument("popen", 1, "string expected")
	}
	if len(args) > 1 && !args[1].IsNil() && args[1].Kind != runtime.KindString {
		// mode 参数如果出现，必须是字符串。
		return nil, badArgument("popen", 2, "string expected")
	}
	if !options.AllowProcess {
		// 默认关闭进程创建，避免脚本绕过宿主安全边界。
		return nil, processDisabledError("popen")
	}
	return POpen(args...)
}

// Close 实现 Lua 5.3 `io.close` 的第一阶段语义。
//
// 第一个参数可选；传入 file userdata 时关闭该文件，未传入时按当前默认输出 stdout 处理。
// 默认输出如果是标准流则关闭保持 no-op；关闭成功按 Lua 5.3 语义返回 true。
func Close(args ...runtime.Value) ([]runtime.Value, error) {
	// 无参数时按默认输出关闭；当前默认输出是标准流，因此不会影响宿主进程。
	if len(args) == 0 || args[0].IsNil() {
		if defaultOutput != nil && defaultOutput.standard {
			// 标准输出不能被 Lua 真正关闭，Lua 5.3 返回空结果供脚本以 falsy 判断。
			return nil, nil
		}
		if err := defaultOutput.Close(); err != nil {
			// 默认输出关闭失败时返回 Lua 错误。
			return nil, err
		}
		return []runtime.Value{runtime.BooleanValue(true)}, nil
	}

	file, err := fileArgument(args, 1, "close")
	if err != nil {
		// 参数不是 file userdata 时返回 Lua 参数错误。
		return nil, err
	}
	if !file.standard && file.closed {
		// Lua 可见的显式重复 close 必须报错；底层 File.Close 仍保持幂等供 GC/finalizer 使用。
		return nil, runtime.RaiseError(runtime.StringValue("attempt to close a closed file"))
	}
	if closer, ok := file.closer.(*popenCloser); ok {
		// popen file:close 等价于 Lua pclose，需要返回进程状态三元组而不是单个 true。
		if file.writeBuffer != nil {
			// w 模式关闭前先把 Lua 层缓冲写入管道。
			if err := file.writeBuffer.Flush(); err != nil {
				return nil, runtime.RaiseError(runtime.StringValue(err.Error()))
			}
		}
		file.closed = true
		return closer.CloseStatus()
	}
	if err := file.Close(); err != nil {
		// 底层关闭失败时直接传播 Lua 错误。
		return nil, err
	}
	if file.standard {
		// 标准流 close 是 no-op，Lua 5.3 返回空结果而不是 true。
		return nil, nil
	}
	return []runtime.Value{runtime.BooleanValue(true)}, nil
}

// Flush 实现 Lua 5.3 `io.flush` 的第一阶段语义。
//
// 第一个参数可选；传入 file userdata 时刷新该文件，未传入时刷新当前默认输出 stdout。
// 默认输出如果是标准流则刷新保持 no-op。
func Flush(args ...runtime.Value) ([]runtime.Value, error) {
	// 无参数时按默认输出刷新。
	if len(args) == 0 || args[0].IsNil() {
		if err := defaultOutput.Flush(); err != nil {
			// 默认输出刷新失败时返回 Lua 错误。
			return nil, err
		}
		return nil, nil
	}

	file, err := fileArgument(args, 1, "flush")
	if err != nil {
		// 参数不是 file userdata 时返回 Lua 参数错误。
		return nil, err
	}
	if err := file.Flush(); err != nil {
		// 底层刷新失败时直接传播 Lua 错误。
		return nil, err
	}
	return nil, nil
}

// FileClose 实现 Lua 5.3 file `:close` 的第一阶段方法入口。
//
// colon 调用会把 file userdata 作为第一个参数传入；本函数复用 Close 的关闭语义。后续
// file metatable 接入后会把该函数挂到 `__index.close`。
func FileClose(args ...runtime.Value) ([]runtime.Value, error) {
	// file:close 必须带 self 参数。
	if len(args) == 0 || args[0].IsNil() {
		// 缺少 self 时按 file 参数错误处理。
		return nil, badArgument("close", 1, "FILE* expected")
	}
	return Close(args...)
}

// FileGC 实现 Lua 5.3 file 元表 `__gc` 方法入口。
//
// 正常 finalizer 调用会把 file userdata 作为第一个参数传入；用户直接调用 `__gc()` 且缺少 self
// 时，官方 errors.lua 要求错误文本包含 `no value`，因此该入口与普通 `file:close` 分开处理。
func FileGC(args ...runtime.Value) ([]runtime.Value, error) {
	// __gc 直接调用缺少 self 时，需要报告 no value 而不是普通 FILE* expected。
	if len(args) == 0 || args[0].IsNil() {
		// 缺少待关闭 file 时按 Lua 参数错误返回，供 pcall 捕获。
		return nil, badArgument("__gc", 1, "FILE* expected, got no value")
	}
	file, err := fileArgument(args, 1, "__gc")
	if err != nil {
		// 非 file userdata 继续复用统一 file 参数错误。
		return nil, err
	}
	if err := file.Close(); err != nil {
		// 底层关闭失败时直接传播 Lua 错误。
		return nil, err
	}
	return nil, nil
}

// FileFlush 实现 Lua 5.3 file `:flush` 的第一阶段方法入口。
//
// colon 调用会把 file userdata 作为第一个参数传入；本函数复用 File.Flush 的刷新语义。
// 后续 file metatable 接入后会把该函数挂到 `__index.flush`。
func FileFlush(args ...runtime.Value) ([]runtime.Value, error) {
	// file:flush 必须带 self 参数。
	file, err := fileArgument(args, 1, "flush")
	if err != nil {
		// self 不是 file userdata 时返回 Lua 参数错误。
		return nil, err
	}
	if err := file.Flush(); err != nil {
		// 底层刷新失败时直接传播 Lua 错误。
		return nil, err
	}
	return []runtime.Value{runtime.BooleanValue(true)}, nil
}

// FileToString 实现 Lua 5.3 file userdata 的 `__tostring` 元方法。
//
// 打开的文件返回以 `file ` 开头的文本；关闭后的文件必须返回固定 `file (closed)`，官方 files.lua
// 会分别检查这两个可见格式。
func FileToString(args ...runtime.Value) ([]runtime.Value, error) {
	// __tostring 必须接收 file userdata self。
	file, err := fileArgument(args, 1, "tostring")
	if err != nil {
		// self 错误按普通 file 参数错误返回。
		return nil, err
	}
	if file.closed {
		// 关闭文件的 tostring 文本固定，便于 Lua 脚本判断资源状态。
		return []runtime.Value{runtime.StringValue("file (closed)")}, nil
	}
	return []runtime.Value{runtime.StringValue("file (" + file.name + ")")}, nil
}

// FileLines 实现 Lua 5.3 file `:lines` 的第一阶段方法入口。
//
// colon 调用会把 file userdata 作为第一个参数传入；返回 iterator 每次从该文件读取一行。
// 支持默认行读取和 Lua 5.3 的多格式读取，EOF 返回 nil 结束迭代。
func FileLines(args ...runtime.Value) ([]runtime.Value, error) {
	// file:lines 必须带 self 参数。
	file, err := fileArgument(args, 1, "lines")
	if err != nil {
		// self 不是 file userdata 时返回 Lua 参数错误。
		return nil, err
	}
	if file.reader == nil {
		// Lua 5.3 的 file:lines 在不可读文件上仍先返回 iterator，错误推迟到 iterator 调用时发生。
		iterator := runtime.GoResultsFunction(func(values ...runtime.Value) ([]runtime.Value, error) {
			// 调用不可读 iterator 时返回可被 pcall 捕获的普通错误文本。
			return nil, fmt.Errorf("file is not readable")
		})
		return []runtime.Value{runtime.ReferenceValue(runtime.KindGoClosure, iterator)}, nil
	}
	iterator, err := makeLinesIterator(file, args[1:], false)
	if err != nil {
		// 格式参数错误在创建 iterator 时直接返回。
		return nil, err
	}
	return []runtime.Value{runtime.ReferenceValue(runtime.KindGoClosure, iterator)}, nil
}

// FileRead 实现 Lua 5.3 file `:read` 的第一阶段方法入口。
//
// colon 调用会把 file userdata 作为第一个参数传入；无格式参数时等价于 `*l`，当前支持
// `*l`、`*a` 和非负整数 byte count，`*n` 后续补齐。
func FileRead(args ...runtime.Value) ([]runtime.Value, error) {
	// file:read 必须带 self 参数。
	file, err := fileArgument(args, 1, "read")
	if err != nil {
		// self 不是 file userdata 时返回 Lua 参数错误。
		return nil, err
	}
	if file.reader == nil {
		// 写-only 文件读取按普通 I/O 失败返回 nil、错误文本和错误码，允许 Lua 侧用 not 判断。
		return []runtime.Value{runtime.NilValue(), runtime.StringValue("file is not readable"), runtime.IntegerValue(1)}, nil
	}
	if len(args) == 1 {
		// 无格式参数时默认读取一行。
		return readOne(file, runtime.StringValue("*l"), "read")
	}

	results := make([]runtime.Value, 0, len(args)-1)
	for index := 1; index < len(args); index++ {
		// 逐个读取格式并追加对应结果。
		values, err := readOne(file, args[index], "read")
		if err != nil {
			// 任一格式读取失败立即返回错误。
			return nil, err
		}
		if len(values) == 0 {
			// 内部读取函数不应返回空结果，保持防御式错误。
			return nil, runtime.RaiseError(runtime.StringValue("file:read internal empty result"))
		}
		results = append(results, values[0])
		if values[0].IsNil() {
			// Lua read 在某个格式 EOF 后停止继续读取后续格式。
			break
		}
	}
	return results, nil
}

// makeLinesIterator 创建 io.lines/file:lines 使用的多格式迭代器。
//
// formats 为空时默认按 `l` 读取一行；非空时每次 iterator 调用按顺序读取全部格式并返回多结果。
// closeOnEOF 为 true 时表示路径型 io.lines，遇到 EOF 后自动关闭文件，并在再次调用时报告已关闭。
func makeLinesIterator(file *File, formats []runtime.Value, closeOnEOF bool) (runtime.GoResultsFunction, error) {
	// 先校验读取端能力，避免 iterator 延迟到首次调用才暴露不可读文件。
	if file == nil || file.reader == nil {
		// 不可读文件无法创建行迭代器。
		return nil, runtime.RaiseError(runtime.StringValue("file is not readable"))
	}
	if len(formats) > maxLineFormats {
		// Lua 5.3 对 lines 的格式参数数量有固定上限。
		return nil, runtime.RaiseError(runtime.StringValue("too many arguments"))
	}
	if len(formats) == 0 {
		// 无格式参数时，默认按不带行尾的行模式读取。
		formats = []runtime.Value{runtime.StringValue("l")}
	}
	exhausted := false
	return runtime.GoResultsFunction(func(values ...runtime.Value) ([]runtime.Value, error) {
		// 路径型 iterator EOF 后再次调用要报告自动关闭状态。
		if exhausted {
			// 已自动关闭的路径型 iterator 不允许继续读取。
			return nil, runtime.RaiseError(runtime.StringValue("file is already closed"))
		}
		results := make([]runtime.Value, 0, len(formats))
		for index, format := range formats {
			// 每个 iterator 调用按固定格式列表顺序读取多个结果。
			values, err := readOne(file, format, "lines")
			if err != nil {
				// 格式错误或读取错误直接传播。
				return nil, err
			}
			if len(values) == 0 {
				// readOne 必须至少返回一个 Lua 值。
				return nil, runtime.RaiseError(runtime.StringValue("lines internal empty result"))
			}
			if values[0].IsNil() && index == 0 {
				// 第一个格式 EOF 表示迭代结束，不返回任何值。
				if closeOnEOF {
					// 路径型 io.lines 在 EOF 时自动关闭底层文件。
					_ = file.Close()
					exhausted = true
				}
				return nil, nil
			}
			results = append(results, values[0])
			if values[0].IsNil() {
				// 后续格式遇到 EOF 时保留 nil 结果并结束本次多格式读取。
				break
			}
		}
		return results, nil
	}), nil
}

// FileSeek 实现 Lua 5.3 file `:seek` 的第一阶段方法入口。
//
// colon 调用会把 file userdata 作为第一个参数传入；whence 可为 `set`、`cur`、`end`，
// offset 默认为 0。成功返回新的文件位置。
func FileSeek(args ...runtime.Value) ([]runtime.Value, error) {
	// file:seek 必须带 self 参数。
	file, err := fileArgument(args, 1, "seek")
	if err != nil {
		// self 不是 file userdata 时返回 Lua 参数错误。
		return nil, err
	}
	if file.closed {
		// 已关闭文件不能定位。
		return nil, runtime.RaiseError(runtime.StringValue("attempt to use a closed file"))
	}
	if file.seeker == nil {
		// 没有 seek 能力的文件按 Lua 5.3 普通 IO 失败返回 falsy、错误文本和数字错误码。
		return []runtime.Value{runtime.BooleanValue(false), runtime.StringValue("file is not seekable"), runtime.IntegerValue(1)}, nil
	}

	whenceName := "cur"
	if len(args) > 1 && !args[1].IsNil() {
		// whence 参数如果出现必须是字符串。
		if args[1].Kind != runtime.KindString {
			// 非字符串 whence 按参数错误处理。
			return nil, badArgument("seek", 2, "string expected")
		}
		whenceName = args[1].String
	}
	offset := int64(0)
	if len(args) > 2 && !args[2].IsNil() {
		// offset 参数如果出现必须是 integer。
		value, ok := args[2].ToInteger()
		if !ok {
			// 非 integer offset 按参数错误处理。
			return nil, badArgument("seek", 3, "integer expected")
		}
		offset = value
	}

	whence, err := seekWhence(whenceName)
	if err != nil {
		// 非法 whence 返回 Lua 参数错误。
		return nil, err
	}
	if whence == gio.SeekCurrent && file.lineReader != nil {
		// bufio.Reader 可能已经从底层文件预读了后续字节；Lua 可见当前位置应扣除尚未消费的缓冲字节。
		offset -= int64(file.lineReader.Buffered())
	}
	position, err := file.seeker.Seek(offset, whence)
	if err != nil {
		// 底层定位失败按 Lua 5.3 普通 IO 失败返回 falsy、错误文本和数字错误码。
		return []runtime.Value{runtime.BooleanValue(false), runtime.StringValue(err.Error()), runtime.IntegerValue(1)}, nil
	}
	if file.lineReader != nil {
		// seek 后清空行缓冲器，避免旧缓冲位置污染后续读取。
		file.lineReader = nil
	}
	return []runtime.Value{runtime.IntegerValue(position)}, nil
}

// FileSetVBuf 实现 Lua 5.3 file `:setvbuf` 的第一阶段方法入口。
//
// 当前 file userdata 尚未引入可变缓冲策略，因此该方法只校验 mode 与 size 并返回 true。
// mode 支持 `no`、`full`、`line`；size 可选且必须是非负 integer。
func FileSetVBuf(args ...runtime.Value) ([]runtime.Value, error) {
	// file:setvbuf 必须带 self 参数。
	file, err := fileArgument(args, 1, "setvbuf")
	if err != nil {
		// self 不是 file userdata 时返回 Lua 参数错误。
		return nil, err
	}
	if file.closed {
		// 已关闭文件不能调整缓冲策略。
		return nil, runtime.RaiseError(runtime.StringValue("attempt to use a closed file"))
	}
	if len(args) < 2 || args[1].Kind != runtime.KindString {
		// mode 是必需字符串参数。
		return nil, badArgument("setvbuf", 2, "string expected")
	}
	if !validBufferMode(args[1].String) {
		// mode 不属于 Lua 5.3 支持集合时返回参数错误。
		return nil, badArgument("setvbuf", 2, "invalid mode")
	}
	if len(args) > 2 && !args[2].IsNil() {
		// size 如果出现必须是非负 integer。
		size, ok := args[2].ToInteger()
		if !ok {
			// 非 integer size 按参数错误处理。
			return nil, badArgument("setvbuf", 3, "integer expected")
		}
		if size < 0 {
			// 负数 size 不合法。
			return nil, badArgument("setvbuf", 3, "size out of range")
		}
	}
	if err := file.setWriteBufferMode(args[1].String, args); err != nil {
		// 缓冲模式切换失败时按 Lua 运行期错误返回。
		return nil, err
	}
	return []runtime.Value{runtime.BooleanValue(true)}, nil
}

// FileWrite 实现 Lua 5.3 file `:write` 的第一阶段方法入口。
//
// colon 调用会把 file userdata 作为第一个参数传入；其余参数必须是 string、integer 或
// number。成功时返回 file userdata 自身，便于链式调用。
func FileWrite(args ...runtime.Value) ([]runtime.Value, error) {
	// file:write 必须带 self 参数。
	file, err := fileArgument(args, 1, "write")
	if err != nil {
		// self 不是 file userdata 时返回 Lua 参数错误。
		return nil, err
	}
	if file.closed {
		// 已关闭文件不能写入。
		return nil, runtime.RaiseError(runtime.StringValue("attempt to use a closed file"))
	}
	if file.writer == nil {
		// 未关闭但缺少 writer 能力时按普通 I/O 失败返回，允许 Lua 脚本用三返回判断。
		return []runtime.Value{runtime.NilValue(), runtime.StringValue("file is not writable"), runtime.IntegerValue(1)}, nil
	}
	for index := 1; index < len(args); index++ {
		// 每个参数都必须可转换为写入文本。
		text, err := writeArgument(args[index], index+1, "write")
		if err != nil {
			// 参数类型不兼容时立即返回 Lua 参数错误。
			return nil, err
		}
		if err := file.writeString(text); err != nil {
			// 底层写入失败转换为 Lua 运行期错误。
			return nil, err
		}
	}
	return []runtime.Value{args[0]}, nil
}

// setWriteBufferMode 切换 file:setvbuf 管理的写缓冲模式。
//
// mode 已由调用方校验为 no/full/line；args 用于读取可选 size。切换模式前会刷新旧缓冲，
// 避免前一模式的内容滞留。没有 writer 能力的文件只记录成功，实际写入仍会由 file:write 拒绝。
func (file *File) setWriteBufferMode(mode string, args []runtime.Value) error {
	if file.writeBuffer != nil {
		// 切换缓冲策略前先提交旧缓冲，避免内容在模式变化时丢失。
		if err := file.writeBuffer.Flush(); err != nil {
			return runtime.RaiseError(runtime.StringValue(err.Error()))
		}
	}
	file.writeBuffer = nil
	file.bufferMode = mode
	if mode == "no" || file.writer == nil {
		// no 模式关闭 Lua 层缓冲；不可写文件不创建无效缓冲器。
		return nil
	}

	size := int64(0)
	if len(args) > 2 && !args[2].IsNil() {
		// size 已通过非负 integer 校验，这里只读取用于设置 bufio 容量。
		size, _ = args[2].ToInteger()
	}
	if size <= 0 {
		// Lua 未指定 size 时使用 Go bufio 默认大小。
		file.writeBuffer = bufio.NewWriter(file.writer)
		return nil
	}
	file.writeBuffer = bufio.NewWriterSize(file.writer, int(size))
	return nil
}

// writeString 按当前缓冲模式写入字符串。
//
// full 模式只写入 Lua 层缓冲；line 模式在写入内容包含换行时刷新；no/未设置模式直接写到底层
// writer。返回错误已经转换为 Lua error，调用方可直接传播。
func (file *File) writeString(text string) error {
	if file.writeBuffer == nil {
		// 未启用 Lua 层缓冲时直接写底层 writer。
		if _, err := file.writer.Write([]byte(text)); err != nil {
			return runtime.RaiseError(runtime.StringValue(err.Error()))
		}
		return nil
	}
	if _, err := file.writeBuffer.WriteString(text); err != nil {
		// 缓冲写入失败保留底层错误文本。
		return runtime.RaiseError(runtime.StringValue(err.Error()))
	}
	if file.bufferMode == "line" && strings.Contains(text, "\n") {
		// line 模式遇到换行提交整段缓冲，使另一 reader 可见完整行。
		if err := file.writeBuffer.Flush(); err != nil {
			return runtime.RaiseError(runtime.StringValue(err.Error()))
		}
	}
	return nil
}

// ReadLine 从 file userdata 读取下一行文本。
//
// 返回行内容不包含 `\n` 或 `\r\n`；读到 EOF 时返回 io.EOF。该方法服务 io.lines 和
// 后续 file:lines，不改变 Lua 可观察的文件对象身份。
func (file *File) ReadLine() (string, error) {
	// 行读取前先校验文件对象。
	if file == nil {
		// nil 文件无法读取。
		return "", runtime.RaiseError(runtime.StringValue("attempt to use a closed file"))
	}
	if file.closed {
		// 已关闭文件不能继续读取。
		return "", runtime.RaiseError(runtime.StringValue("attempt to use a closed file"))
	}
	if file.reader == nil {
		// 没有 reader 能力的文件不可读。
		return "", runtime.RaiseError(runtime.StringValue("file is not readable"))
	}
	if file.lineReader == nil {
		// 首次按行读取时创建缓冲器，并保留后续读取位置。
		file.lineReader = bufio.NewReader(file.reader)
	}

	line, err := file.lineReader.ReadString('\n')
	if err != nil && err != gio.EOF {
		// 非 EOF 错误直接返回给调用方包装。
		return "", err
	}
	if err == gio.EOF && line == "" {
		// 空行且 EOF 表示没有更多内容。
		return "", gio.EOF
	}
	if len(line) > 0 && line[len(line)-1] == '\n' {
		// Lua lines 不返回行尾换行。
		line = line[:len(line)-1]
	}
	if len(line) > 0 && line[len(line)-1] == '\r' {
		// 兼容 CRLF 文本，去掉换行前的回车。
		line = line[:len(line)-1]
	}
	return line, nil
}

// ReadLineWithEnd 从 file userdata 读取下一行文本并保留行尾换行。
//
// 返回行内容包含读取到的 `\n` 或文件原始行尾；读到 EOF 且没有内容时返回 io.EOF。该方法服务
// Lua 5.3 的 `L`/`*L` 读取格式。
func (file *File) ReadLineWithEnd() (string, error) {
	// 行读取前先校验文件对象。
	if file == nil {
		// nil 文件无法读取。
		return "", runtime.RaiseError(runtime.StringValue("attempt to use a closed file"))
	}
	if file.closed {
		// 已关闭文件不能继续读取。
		return "", runtime.RaiseError(runtime.StringValue("attempt to use a closed file"))
	}
	if file.reader == nil {
		// 没有 reader 能力的文件不可读。
		return "", runtime.RaiseError(runtime.StringValue("file is not readable"))
	}
	if file.lineReader == nil {
		// 首次按行读取时创建缓冲器，并保留后续读取位置。
		file.lineReader = bufio.NewReader(file.reader)
	}

	line, err := file.lineReader.ReadString('\n')
	if err != nil && err != gio.EOF {
		// 非 EOF 错误直接返回给调用方包装。
		return "", err
	}
	if err == gio.EOF && line == "" {
		// 空行且 EOF 表示没有更多内容。
		return "", gio.EOF
	}
	return line, nil
}

// ReadAll 从 file userdata 当前读取位置读取剩余全部文本。
//
// 如果之前已经使用 ReadLine 创建缓冲器，则从缓冲器继续读取，避免丢失预读字节。
func (file *File) ReadAll() (string, error) {
	// 读取前先校验文件对象。
	if file == nil {
		// nil 文件无法读取。
		return "", runtime.RaiseError(runtime.StringValue("attempt to use a closed file"))
	}
	if file.closed {
		// 已关闭文件不能继续读取。
		return "", runtime.RaiseError(runtime.StringValue("attempt to use a closed file"))
	}
	if file.reader == nil {
		// 没有 reader 能力的文件不可读。
		return "", runtime.RaiseError(runtime.StringValue("file is not readable"))
	}

	reader := file.reader
	if file.lineReader != nil {
		// 已经存在缓冲读取器时必须复用，保留正确读取位置。
		reader = file.lineReader
	}
	bytes, err := gio.ReadAll(reader)
	if err != nil {
		// ReadAll 的底层错误交给调用方包装。
		return "", err
	}
	return string(bytes), nil
}

// ReadBytes 从 file userdata 当前读取位置读取指定字节数。
//
// count 必须非负；读取到 EOF 且没有任何字节时返回 io.EOF，读取到部分字节时返回已读内容。
func (file *File) ReadBytes(count int64) (string, error) {
	// 字节读取前先校验文件对象。
	if file == nil {
		// nil 文件无法读取。
		return "", runtime.RaiseError(runtime.StringValue("attempt to use a closed file"))
	}
	if file.closed {
		// 已关闭文件不能继续读取。
		return "", runtime.RaiseError(runtime.StringValue("attempt to use a closed file"))
	}
	if file.reader == nil {
		// 没有 reader 能力的文件不可读。
		return "", runtime.RaiseError(runtime.StringValue("file is not readable"))
	}
	if count < 0 {
		// 负数字节数不符合 Lua read count 语义。
		return "", runtime.RaiseError(runtime.StringValue("negative read count"))
	}
	if count == 0 {
		if file.lineReader == nil {
			// 0 字节读取仍需探测 EOF；创建缓冲器后续读取继续复用同一位置。
			file.lineReader = bufio.NewReader(file.reader)
		}
		if _, err := file.lineReader.Peek(1); err == gio.EOF {
			// EOF 上读取 0 字节按 Lua 语义返回 nil。
			return "", gio.EOF
		} else if err != nil {
			// 其他底层错误直接返回。
			return "", err
		}
		// 非 EOF 时读取 0 字节返回空字符串。
		return "", nil
	}

	reader := file.reader
	if file.lineReader != nil {
		// 已经存在缓冲读取器时必须复用，避免跳过预读字节。
		reader = file.lineReader
	}
	buffer := make([]byte, count)
	readCount, err := gio.ReadFull(reader, buffer)
	if err != nil {
		if err == gio.EOF && readCount == 0 {
			// 没有读到任何字节且 EOF，调用方应返回 nil。
			return "", gio.EOF
		}
		if err == gio.ErrUnexpectedEOF {
			// 部分读取仍然返回已读内容，兼容 Lua 对 count 的宽松 EOF 行为。
			return string(buffer[:readCount]), nil
		}
		// 其他错误直接返回。
		return "", err
	}
	return string(buffer), nil
}

// readOne 按单个 Lua io.read 格式读取一个结果。
//
// format 可为 `*l`、`*a` 或非负 integer 字节数；EOF 时返回 Lua nil。
func readOne(file *File, format runtime.Value, functionName string) ([]runtime.Value, error) {
	// 字符串格式按 Lua 5.3 控制码处理。
	if format.Kind == runtime.KindString {
		switch format.String {
		case "*l", "l":
			// 行模式读取下一行。
			line, err := file.ReadLine()
			if err == gio.EOF {
				// EOF 返回 nil。
				return []runtime.Value{runtime.NilValue()}, nil
			}
			if err != nil {
				// 读取错误转换为 Lua 运行期错误。
				return nil, runtime.RaiseError(runtime.StringValue(err.Error()))
			}
			return []runtime.Value{runtime.StringValue(line)}, nil
		case "*L", "L":
			// 保留行尾模式读取下一行，覆盖官方 files.lua 的长数字失败后读剩余行场景。
			line, err := file.ReadLineWithEnd()
			if err == gio.EOF {
				// EOF 返回 nil。
				return []runtime.Value{runtime.NilValue()}, nil
			}
			if err != nil {
				// 读取错误转换为 Lua 运行期错误。
				return nil, runtime.RaiseError(runtime.StringValue(err.Error()))
			}
			return []runtime.Value{runtime.StringValue(line)}, nil
		case "*a", "a", "all", "*all":
			// 全量模式读取剩余内容。
			content, err := file.ReadAll()
			if err != nil {
				// 全量读取错误转换为 Lua 运行期错误。
				return nil, runtime.RaiseError(runtime.StringValue(err.Error()))
			}
			return []runtime.Value{runtime.StringValue(content)}, nil
		case "*n", "n":
			// 数字模式读取一个 Lua 兼容 number token，覆盖官方 files.lua 的整数和十六进制浮点。
			return readNumber(file)
		default:
			// 未知格式按 Lua 参数错误处理。
			return nil, badArgument(functionName, 1, "invalid format")
		}
	}
	count, ok := format.ToInteger()
	if !ok {
		// 非字符串也非 integer count 的格式不合法。
		return nil, badArgument(functionName, 1, "invalid format")
	}
	if count < 0 {
		// 负数字节数不合法。
		return nil, badArgument(functionName, 1, "invalid format")
	}
	text, err := file.ReadBytes(count)
	if err == gio.EOF {
		// EOF 且没有读到任何字节时返回 nil。
		return []runtime.Value{runtime.NilValue()}, nil
	}
	if err != nil {
		// 读取错误转换为 Lua 运行期错误。
		return nil, runtime.RaiseError(runtime.StringValue(err.Error()))
	}
	return []runtime.Value{runtime.StringValue(text)}, nil
}

// readNumber 从 file 当前读取位置扫描一个 Lua 5.3 数字。
//
// 当前实现按空白分隔 token，优先解析 integer，再解析 float；EOF 且没有 token 时返回 nil。
func readNumber(file *File) ([]runtime.Value, error) {
	// 数字读取前先校验文件对象。
	if file == nil {
		// nil 文件无法读取。
		return nil, runtime.RaiseError(runtime.StringValue("attempt to use a closed file"))
	}
	if file.closed {
		// 已关闭文件不能继续读取。
		return nil, runtime.RaiseError(runtime.StringValue("attempt to use a closed file"))
	}
	if file.reader == nil {
		// 没有 reader 能力的文件不可读。
		return nil, runtime.RaiseError(runtime.StringValue("file is not readable"))
	}
	if file.lineReader == nil {
		// 数字扫描需要可 UnreadByte 的缓冲器，首次读取时创建并保存。
		file.lineReader = bufio.NewReader(file.reader)
	}

	token, err := readNumberToken(file.lineReader)
	if err != nil {
		// 底层读取错误直接转换为 Lua 运行期错误。
		return nil, runtime.RaiseError(runtime.StringValue(err.Error()))
	}
	if token == "" {
		// EOF 前没有数字 token 时按 Lua 语义返回 nil。
		return []runtime.Value{runtime.NilValue()}, nil
	}
	if len(token) >= readNumberMaxTokenLength {
		// 达到固定扫描上限表示数字候选过长；Lua 5.3 会返回 nil，并让剩余字节留给后续读取。
		return []runtime.Value{runtime.NilValue()}, nil
	}
	if integer, err := strconv.ParseInt(token, 0, 64); err == nil {
		// 能无损解析为 int64 时优先返回 Lua integer。
		return []runtime.Value{runtime.IntegerValue(integer)}, nil
	}
	if unsignedInteger, ok := parseUnsignedHexInteger(token); ok {
		// Lua 5.3 十六进制 integer 按 lua_Integer 位宽解释，超出有符号上限时按补码落入负数。
		return []runtime.Value{runtime.IntegerValue(int64(unsignedInteger))}, nil
	}
	number, err := strconv.ParseFloat(token, 64)
	if err != nil {
		// token 不是合法数字时返回 nil，保持 io.read("n") 的普通失败语义。
		return []runtime.Value{runtime.NilValue()}, nil
	}
	return []runtime.Value{runtime.NumberValue(number)}, nil
}

// parseUnsignedHexInteger 尝试按 Lua 5.3 十六进制 integer 位模式解析 token。
//
// 返回 ok=false 表示 token 不是无符号十六进制整数，调用方应继续尝试浮点解析。
func parseUnsignedHexInteger(token string) (uint64, bool) {
	// 十六进制浮点包含 p/P，不能走 integer 位模式。
	if strings.ContainsAny(token, "pP.") {
		return 0, false
	}
	signless := strings.TrimPrefix(strings.TrimPrefix(token, "+"), "-")
	if !strings.HasPrefix(signless, "0x") && !strings.HasPrefix(signless, "0X") {
		// 只有 0x/0X 前缀的 token 才需要无符号位模式兜底。
		return 0, false
	}
	parsed, err := strconv.ParseUint(signless[2:], 16, 64)
	if err != nil {
		// 解析失败说明不是可表示的 64 位十六进制整数。
		return 0, false
	}
	if strings.HasPrefix(token, "-") {
		// 显式负号先取反，再按 uint64 位模式返回。
		return uint64(-int64(parsed)), true
	}
	return parsed, true
}

// readNumberToken 读取下一个 Lua 数字候选 token。
//
// reader 必须非 nil；返回空字符串表示 EOF 前没有读取到 token。
func readNumberToken(reader *bufio.Reader) (string, error) {
	// 先跳过前导空白。
	for {
		current, err := reader.ReadByte()
		if err == gio.EOF {
			// EOF 前没有 token。
			return "", nil
		}
		if err != nil {
			// 底层错误交给调用方包装。
			return "", err
		}
		if !isReadNumberSpace(current) {
			// 第一个非空白字节属于 token，需要退回后进入正文读取。
			if unreadErr := reader.UnreadByte(); unreadErr != nil {
				// 退回失败说明缓冲器状态异常。
				return "", unreadErr
			}
			break
		}
	}

	var builder strings.Builder
	current, err := reader.ReadByte()
	if err == gio.EOF {
		// EOF 前没有 token。
		return "", nil
	}
	if err != nil {
		// 底层错误交给调用方包装。
		return "", err
	}
	if current == '+' || current == '-' {
		// 前导符号属于数字候选；若后续不是数字起始，符号自身作为失败 token 被消费。
		builder.WriteByte(current)
		next, readErr := reader.ReadByte()
		if readErr == gio.EOF {
			return builder.String(), nil
		}
		if readErr != nil {
			return "", readErr
		}
		if !isReadNumberStart(next) {
			if unreadErr := reader.UnreadByte(); unreadErr != nil {
				return "", unreadErr
			}
			return builder.String(), nil
		}
		current = next
	}
	if current == '0' {
		// 0x/0X 进入十六进制扫描；否则退回预读字节并走十进制扫描。
		builder.WriteByte(current)
		next, readErr := reader.ReadByte()
		if readErr == gio.EOF {
			return builder.String(), nil
		}
		if readErr != nil {
			return "", readErr
		}
		if next == 'x' || next == 'X' {
			builder.WriteByte(next)
			return readHexNumberToken(reader, &builder)
		}
		if unreadErr := reader.UnreadByte(); unreadErr != nil {
			return "", unreadErr
		}
		return readDecimalNumberToken(reader, &builder, true, true)
	}
	if current >= '1' && current <= '9' {
		// 普通十进制数字从首个数字继续扫描。
		builder.WriteByte(current)
		return readDecimalNumberToken(reader, &builder, true, true)
	}
	if current == '.' {
		// 点开头数字必须跟随十进制数字；否则只消费点并返回失败 token。
		builder.WriteByte(current)
		next, readErr := reader.ReadByte()
		if readErr == gio.EOF {
			return builder.String(), nil
		}
		if readErr != nil {
			return "", readErr
		}
		if !isDecimalDigit(next) {
			if unreadErr := reader.UnreadByte(); unreadErr != nil {
				return "", unreadErr
			}
			return builder.String(), nil
		}
		builder.WriteByte(next)
		return readDecimalNumberToken(reader, &builder, true, true)
	}
	if unreadErr := reader.UnreadByte(); unreadErr != nil {
		// 非数字起始字符不能被数字读取消费。
		return "", unreadErr
	}
	return "", nil
}

// readDecimalNumberToken 从当前十进制 token 后继续读取数字、小数和指数部分。
func readDecimalNumberToken(reader *bufio.Reader, builder *strings.Builder, hasDigit bool, mantissaValid bool) (string, error) {
	for {
		current, err := reader.ReadByte()
		if err == gio.EOF {
			return builder.String(), nil
		}
		if err != nil {
			return "", err
		}
		if isDecimalDigit(current) {
			appended, appendErr := appendReadNumberByte(reader, builder, current)
			if appendErr != nil {
				return "", appendErr
			}
			if !appended {
				return builder.String(), nil
			}
			hasDigit = true
			continue
		}
		if current == '.' {
			// 十进制小数点只允许出现一次；第二个点作为终止字符留给后续读取。
			builderText := builder.String()
			if strings.Contains(builderText, ".") {
				if unreadErr := reader.UnreadByte(); unreadErr != nil {
					return "", unreadErr
				}
				return builder.String(), nil
			}
			builder.WriteByte(current)
			continue
		}
		if (current == 'e' || current == 'E') && mantissaValid && hasDigit {
			// 指数标记一旦出现就属于候选 token；没有指数数字时整个 token 解析失败。
			builder.WriteByte(current)
			next, readErr := reader.ReadByte()
			if readErr == gio.EOF {
				return builder.String(), nil
			}
			if readErr != nil {
				return "", readErr
			}
			if next == '+' || next == '-' {
				builder.WriteByte(next)
				next, readErr = reader.ReadByte()
				if readErr == gio.EOF {
					return builder.String(), nil
				}
				if readErr != nil {
					return "", readErr
				}
			}
			if !isDecimalDigit(next) {
				if unreadErr := reader.UnreadByte(); unreadErr != nil {
					return "", unreadErr
				}
				return builder.String(), nil
			}
			builder.WriteByte(next)
			for {
				exponentByte, exponentErr := reader.ReadByte()
				if exponentErr == gio.EOF {
					return builder.String(), nil
				}
				if exponentErr != nil {
					return "", exponentErr
				}
				if !isDecimalDigit(exponentByte) {
					if unreadErr := reader.UnreadByte(); unreadErr != nil {
						return "", unreadErr
					}
					return builder.String(), nil
				}
				appended, appendErr := appendReadNumberByte(reader, builder, exponentByte)
				if appendErr != nil {
					return "", appendErr
				}
				if !appended {
					return builder.String(), nil
				}
			}
		}
		if unreadErr := reader.UnreadByte(); unreadErr != nil {
			return "", unreadErr
		}
		return builder.String(), nil
	}
}

// readHexNumberToken 从 `0x`/`0X` 后继续读取 Lua 十六进制整数或浮点 token。
func readHexNumberToken(reader *bufio.Reader, builder *strings.Builder) (string, error) {
	hasHexDigit := false
	for {
		current, err := reader.ReadByte()
		if err == gio.EOF {
			return builder.String(), nil
		}
		if err != nil {
			return "", err
		}
		if isHexDigit(current) {
			appended, appendErr := appendReadNumberByte(reader, builder, current)
			if appendErr != nil {
				return "", appendErr
			}
			if !appended {
				return builder.String(), nil
			}
			hasHexDigit = true
			continue
		}
		if current == '.' {
			// 十六进制小数点只允许出现一次；第二个点作为终止字符留给后续读取。
			if strings.Contains(builder.String(), ".") {
				if unreadErr := reader.UnreadByte(); unreadErr != nil {
					return "", unreadErr
				}
				return builder.String(), nil
			}
			builder.WriteByte(current)
			continue
		}
		if (current == 'p' || current == 'P') && hasHexDigit {
			// 十六进制指数必须带十进制数字；缺失时保留已消费的指数标记作为失败 token。
			builder.WriteByte(current)
			next, readErr := reader.ReadByte()
			if readErr == gio.EOF {
				return builder.String(), nil
			}
			if readErr != nil {
				return "", readErr
			}
			if next == '+' || next == '-' {
				builder.WriteByte(next)
				next, readErr = reader.ReadByte()
				if readErr == gio.EOF {
					return builder.String(), nil
				}
				if readErr != nil {
					return "", readErr
				}
			}
			if !isDecimalDigit(next) {
				if unreadErr := reader.UnreadByte(); unreadErr != nil {
					return "", unreadErr
				}
				return builder.String(), nil
			}
			builder.WriteByte(next)
			for {
				exponentByte, exponentErr := reader.ReadByte()
				if exponentErr == gio.EOF {
					return builder.String(), nil
				}
				if exponentErr != nil {
					return "", exponentErr
				}
				if !isDecimalDigit(exponentByte) {
					if unreadErr := reader.UnreadByte(); unreadErr != nil {
						return "", unreadErr
					}
					return builder.String(), nil
				}
				appended, appendErr := appendReadNumberByte(reader, builder, exponentByte)
				if appendErr != nil {
					return "", appendErr
				}
				if !appended {
					return builder.String(), nil
				}
			}
		}
		if unreadErr := reader.UnreadByte(); unreadErr != nil {
			return "", unreadErr
		}
		return builder.String(), nil
	}
}

// appendReadNumberByte 向数字候选追加字节，并在超长时退回当前字节。
func appendReadNumberByte(reader *bufio.Reader, builder *strings.Builder, value byte) (bool, error) {
	if builder.Len() >= readNumberMaxTokenLength {
		// 当前字节尚未属于返回 token，必须退回给后续 read 继续消费。
		if unreadErr := reader.UnreadByte(); unreadErr != nil {
			return false, unreadErr
		}
		return false, nil
	}
	builder.WriteByte(value)
	return true, nil
}

// isReadNumberStart 判断字节是否可作为符号后的数字起始。
func isReadNumberStart(value byte) bool {
	// Lua 数字允许十进制数字、十六进制前缀中的 0 和点开头小数。
	return isDecimalDigit(value) || value == '.'
}

// isDecimalDigit 判断字节是否为 ASCII 十进制数字。
func isDecimalDigit(value byte) bool {
	// 数字读取只处理 Lua 源码数字允许的 ASCII 数字。
	return value >= '0' && value <= '9'
}

// isHexDigit 判断字节是否为 ASCII 十六进制数字。
func isHexDigit(value byte) bool {
	// 十六进制整数和浮点共享该判断。
	return (value >= '0' && value <= '9') || (value >= 'a' && value <= 'f') || (value >= 'A' && value <= 'F')
}

// isReadNumberSpace 判断字节是否是 Lua 文本读取中的空白。
//
// Lua 官方测试的数字文件使用 ASCII 空白；这里覆盖常见空白字符，避免引入 Unicode 读取状态。
func isReadNumberSpace(value byte) bool {
	// ASCII 空白对齐 Lua 文本文件读取的主要场景。
	switch value {
	case ' ', '\n', '\r', '\t', '\v', '\f':
		return true
	default:
		return false
	}
}

// writeArgument 将 io.write 参数转换为输出文本。
//
// 仅支持 Lua 5.3 允许的 string、integer 与 number；其他类型返回参数错误。
func writeArgument(value runtime.Value, position int, functionName string) (string, error) {
	// 根据 Lua 值类型选择写入格式。
	switch value.Kind {
	case runtime.KindString:
		// string 按原始字节写出。
		return value.String, nil
	case runtime.KindInteger:
		// integer 使用十进制文本写出。
		return strconv.FormatInt(value.Integer, 10), nil
	case runtime.KindNumber:
		// float number 使用紧凑格式，接近 Lua tostring 的默认数字展示。
		return strconv.FormatFloat(value.Number, 'g', -1, 64), nil
	default:
		// 其他类型不能由 io.write 直接写出。
		return "", badArgument(functionName, position, "string expected")
	}
}

// fileFromValue 从 Lua 值中提取 file userdata。
//
// 返回 false 表示该值不是由本包创建的 file userdata，不产生 Lua error。
func fileFromValue(value runtime.Value) (*File, bool) {
	// 只有 userdata 可能承载 file。
	if value.Kind != runtime.KindUserdata {
		return nil, false
	}
	userdata, ok := value.Ref.(*runtime.Userdata)
	if !ok {
		// userdata 引用类型错配时按非 file 处理。
		return nil, false
	}
	file, ok := userdata.Data.(*File)
	if !ok || file == nil {
		// payload 不是 *File 时按非 file 处理。
		return nil, false
	}
	return file, true
}

// seekWhence 将 Lua file:seek 的 whence 文本转换为 Go io.Seek* 常量。
//
// whenceName 必须为 `set`、`cur` 或 `end`；非法值返回 Lua 参数错误。
func seekWhence(whenceName string) (int, error) {
	// 根据 Lua whence 文本选择 Go seek 常量。
	switch whenceName {
	case "set":
		// 从文件起点计算偏移。
		return gio.SeekStart, nil
	case "cur":
		// 从当前位置计算偏移。
		return gio.SeekCurrent, nil
	case "end":
		// 从文件末尾计算偏移。
		return gio.SeekEnd, nil
	default:
		// 其他 whence 文本不合法。
		return 0, badArgument("seek", 2, "invalid option")
	}
}

// validBufferMode 判断 file:setvbuf 的 mode 是否被 Lua 5.3 接受。
//
// 当前实现只做策略记录前的参数门禁，不实际改变 Go reader/writer 缓冲行为。
func validBufferMode(mode string) bool {
	// 根据 Lua 5.3 支持的三个 mode 判断。
	switch mode {
	case "no", "full", "line":
		// 支持的 mode 返回 true。
		return true
	default:
		// 未知 mode 返回 false。
		return false
	}
}

// openFlags 将 Lua io.open mode 转换为 Go os.OpenFile flags。
//
// mode 必须是 Lua 5.3 支持的 `r`、`w`、`a` 及可选 `+`/`b` 组合；返回 flags 仅描述打开
// 行为，权限由调用方统一传入 0666。
func openFlags(mode string) (int, error) {
	// 空 mode 不合法，Lua 5.3 要求模式首字符说明打开方向。
	if mode == "" {
		return 0, badArgument("open", 2, "invalid mode")
	}
	primary := string(mode[0])
	tail := mode[1:]
	readWrite := false
	if strings.HasPrefix(tail, "+") {
		// Lua 5.3 只接受 + 出现在主模式字符之后，且必须早于可选 b。
		readWrite = true
		tail = strings.TrimPrefix(tail, "+")
	}
	// b 只能出现在可选 + 之后；Go 文本/二进制无差异，因此只消费该标记。
	tail = strings.TrimPrefix(tail, "b")
	if tail != "" {
		// 剩余字符说明 mode 顺序或内容不符合 Lua 5.3。
		return 0, badArgument("open", 2, "invalid mode")
	}
	switch primary {
	case "r":
		// r/r+ 打开已有文件，r+ 可读写。
		if readWrite {
			// r+ 不创建也不截断。
			return os.O_RDWR, nil
		}
		return os.O_RDONLY, nil
	case "w":
		// w/w+ 创建或截断文件。
		if readWrite {
			// w+ 可读写并截断。
			return os.O_RDWR | os.O_CREATE | os.O_TRUNC, nil
		}
		return os.O_WRONLY | os.O_CREATE | os.O_TRUNC, nil
	case "a":
		// a/a+ 追加写入，必要时创建文件。
		if readWrite {
			// a+ 可读写但写入追加到末尾。
			return os.O_RDWR | os.O_CREATE | os.O_APPEND, nil
		}
		return os.O_WRONLY | os.O_CREATE | os.O_APPEND, nil
	default:
		// 其他模式不属于 Lua 5.3 支持集合。
		return 0, badArgument("open", 2, "invalid mode")
	}
}

// openModeCapabilities 返回 Lua 文件 mode 对应的读写能力。
func openModeCapabilities(mode string) (bool, bool) {
	// `+` 模式总是读写；否则首字符决定单向能力。
	readable := strings.HasPrefix(mode, "r") || strings.Contains(mode, "+")
	writable := strings.HasPrefix(mode, "w") || strings.HasPrefix(mode, "a") || strings.Contains(mode, "+")
	return readable, writable
}

// popenShellCommand 返回 io.popen 使用的宿主 shell 命令行。
//
// command 是 Lua 传入的原始命令文本；返回值可直接传给 exec.Command。
func popenShellCommand(command string) (string, []string) {
	// Windows 平台使用 cmd.exe 的 /C 语义执行命令字符串。
	if goruntime.GOOS == "windows" {
		return "cmd", []string{"/C", command}
	}
	// 非 Windows 平台追加 exit $?，避免 sh 对最后一个外部命令做 exec 优化而把子 shell 信号冒泡。
	return "/bin/sh", []string{"-c", command + "; exit $?"}
}

// fileArgument 提取指定位置的 file userdata。
//
// position 使用 Lua 1-based 参数序号；functionName 用于构造 Lua 标准库参数错误。
// 参数缺失、非 userdata 或 userdata payload 不是 *File 时均返回 Lua 参数错误。
func fileArgument(args []runtime.Value, position int, functionName string) (*File, error) {
	// 参数缺失时无法取得 file。
	if position <= 0 || position > len(args) {
		// Lua 标准库把缺失参数报告为 file expected。
		return nil, badArgument(functionName, position, "FILE* expected")
	}
	value := args[position-1]
	if value.Kind != runtime.KindUserdata {
		// 非 userdata 不可能是 file。
		return nil, badArgument(functionName, position, fmt.Sprintf("FILE* expected, got %s", runtime.LuaErrorTypeName(value)))
	}
	userdata, ok := value.Ref.(*runtime.Userdata)
	if !ok {
		// KindUserdata 但引用类型错配，说明构造路径异常。
		return nil, badArgument(functionName, position, fmt.Sprintf("FILE* expected, got %s", runtime.LuaErrorTypeName(value)))
	}
	file, ok := userdata.Data.(*File)
	if !ok || file == nil {
		// userdata payload 不是 *File 时不能作为 io 文件使用。
		return nil, badArgument(functionName, position, fmt.Sprintf("FILE* expected, got %s", runtime.LuaErrorTypeName(value)))
	}
	return file, nil
}

// badArgument 构造 Lua 标准库参数错误。
//
// functionName 不包含库名前缀；position 使用 Lua 1-based 参数序号；detail 写入括号内的
// 具体约束说明。返回错误可被 errors.Is(err, runtime.ErrLuaError) 识别。
func badArgument(functionName string, position int, detail string) error {
	// 参数错误统一走 Lua error 对象，便于 pcall/xpcall 后续复用。
	return runtime.RaiseError(runtime.StringValue(fmt.Sprintf("bad argument #%d to '%s' (%s)", position, functionName, detail)))
}

// filesystemDisabledError 构造宿主文件系统禁用错误。
//
// functionName 用于说明触发禁用策略的 io 函数；返回值是 Lua error，表示脚本需要宿主显式
// 开启 sandbox 文件系统权限后才能继续。
func filesystemDisabledError(functionName string) error {
	// 文件系统访问默认关闭，错误消息必须稳定便于测试和嵌入方识别。
	return runtime.RaiseError(runtime.StringValue(fmt.Sprintf("io.%s: host filesystem access is disabled", functionName)))
}

// processDisabledError 构造宿主进程创建禁用错误。
//
// functionName 用于说明触发禁用策略的 io 函数；返回值是 Lua error，表示脚本需要宿主显式
// 开启进程权限后才能继续。
func processDisabledError(functionName string) error {
	// 进程创建默认关闭，避免脚本绕过宿主安全边界。
	return runtime.RaiseError(runtime.StringValue(fmt.Sprintf("io.%s: host process access is disabled", functionName)))
}
