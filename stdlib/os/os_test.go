package oslib

import (
	"errors"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/ZingYao/go-lua-vm/runtime"
)

// TestOpenRegistersOSLibrary 验证 Open 注册 os 库基础字段。
//
// 该用例覆盖 os.clock、os.date、os.difftime 和 os.execute 的注册形态，确保 VM 后续可通过
// Go closure 调用这些函数。
func TestOpenRegistersOSLibrary(t *testing.T) {
	// 测试先创建独立 State，避免污染其他标准库测试。
	state := runtime.NewState()
	if err := Open(state); err != nil {
		// Open 不应在有效 State 上失败。
		t.Fatalf("Open failed: %v", err)
	}

	osValue := state.GetGlobal("os")
	if osValue.Kind != runtime.KindTable {
		// os 全局必须是 table。
		t.Fatalf("os global kind = %v, want table", osValue.Kind)
	}
	osTable := osValue.Ref.(*runtime.Table)
	for _, name := range []string{"clock", "date", "difftime", "execute", "exit", "getenv", "remove", "rename", "setlocale", "time", "tmpname"} {
		// 本轮注册的 os 函数必须是 Go closure。
		value := osTable.RawGetString(name)
		if value.Kind != runtime.KindGoClosure {
			// 函数字段类型错误会导致后续 VM CALL 失败。
			t.Fatalf("os.%s kind = %v, want Go closure", name, value.Kind)
		}
	}
}

// TestClockReturnsElapsedSeconds 验证 os.clock 返回非负秒数。
//
// 当前纯 Go 实现使用进程单调 elapsed time 近似 Lua CPU time，至少必须保证类型和范围稳定。
func TestClockReturnsElapsedSeconds(t *testing.T) {
	// 调用 os.clock 获取秒数。
	values, err := Clock()
	if err != nil {
		// os.clock 不应失败。
		t.Fatalf("Clock failed: %v", err)
	}
	if len(values) != 1 || values[0].Kind != runtime.KindNumber || values[0].Number < 0 {
		// 返回值必须是非负 number。
		t.Fatalf("Clock result = %#v, want non-negative number", values)
	}
}

// TestDateFormatsAndTable 验证 os.date 的字符串与 table 输出。
//
// 使用固定 Unix 时间戳避免测试依赖真实时间漂移。
func TestDateFormatsAndTable(t *testing.T) {
	// 固定时间戳对应 UTC 1970-01-02 03:04:05。
	values, err := Date(runtime.StringValue("!%Y-%m-%d %H:%M:%S"), runtime.IntegerValue(97445))
	if err != nil {
		// 合法格式和时间戳不应失败。
		t.Fatalf("Date format failed: %v", err)
	}
	if len(values) != 1 || values[0].String != "1970-01-02 03:04:05" {
		// 格式化文本必须稳定。
		t.Fatalf("Date formatted = %#v, want 1970-01-02 03:04:05", values)
	}
	ordinalValues, err := Date(runtime.StringValue("!%w/%j/%Ex/%Oy"), runtime.IntegerValue(97445))
	if err != nil {
		// Lua 5.3 官方 files.lua 需要 %w、%j 以及 POSIX E/O 修饰符可用。
		t.Fatalf("Date ordinal format failed: %v", err)
	}
	if len(ordinalValues) != 1 || ordinalValues[0].String != "5/002/01/02/70/70" {
		// UTC 1970-01-02 是周五且年内第 2 天，%Ex/%Oy 复用 x/y 稳定格式。
		t.Fatalf("Date ordinal formatted = %#v, want 5/002/01/02/70/70", ordinalValues)
	}
	if _, err := Date(runtime.StringValue("%9")); !errors.Is(err, runtime.ErrLuaError) {
		// 非法转换符必须返回 Lua error。
		t.Fatalf("Date invalid conversion error = %v, want Lua error", err)
	}
	if _, err := Date(runtime.StringValue("%E9")); !errors.Is(err, runtime.ErrLuaError) {
		// E/O 修饰符后的未知转换同样非法。
		t.Fatalf("Date invalid modifier error = %v, want Lua error", err)
	}
	if _, err := Date(runtime.StringValue("%Y"), runtime.IntegerValue(1<<60)); err == nil || !strings.Contains(runtime.ErrorObject(err).String, "cannot be represented") {
		// 极端时间戳超出兼容层稳定表示范围时必须返回 cannot be represented。
		t.Fatalf("Date huge timestamp error = %v, want cannot be represented", err)
	}

	tableValues, err := Date(runtime.StringValue("!*t"), runtime.IntegerValue(97445))
	if err != nil {
		// *t 表输出不应失败。
		t.Fatalf("Date table failed: %v", err)
	}
	if len(tableValues) != 1 || tableValues[0].Kind != runtime.KindTable {
		// *t 必须返回 table。
		t.Fatalf("Date table result = %#v, want table", tableValues)
	}
	table := tableValues[0].Ref.(*runtime.Table)
	if table.RawGetString("year").Integer != 1970 || table.RawGetString("month").Integer != 1 || table.RawGetString("day").Integer != 2 {
		// 日期字段必须匹配固定时间戳。
		t.Fatalf("Date table has wrong date fields")
	}
	if table.RawGetString("hour").Integer != 3 || table.RawGetString("min").Integer != 4 || table.RawGetString("sec").Integer != 5 {
		// 时间字段必须匹配固定时间戳。
		t.Fatalf("Date table has wrong time fields")
	}
	if table.RawGetString("wday").Integer != int64(time.Friday)+1 {
		// Lua wday 使用 1=Sunday。
		t.Fatalf("Date table wday = %d", table.RawGetString("wday").Integer)
	}
}

// TestDateSupportsCommonStrftimeDirectives 验证日志解析常用 POSIX strftime 指令。
//
// 固定使用 UTC 时间戳，避免本地时区和宿主 locale 影响断言；覆盖 Lua 5.3 `os.date`
// 通过 C strftime 常见可用的格式子集。
func TestDateSupportsCommonStrftimeDirectives(t *testing.T) {
	// 2014-02-10 16:46:36 UTC 是 Monday，便于同时覆盖星期、月份和 ISO week。
	values, err := Date(runtime.StringValue("!%a|%A|%b|%B|%C|%D|%e|%F|%g|%G|%h|%I|%k|%l|%n|%p|%r|%R|%s|%t|%T|%u|%U|%V|%W|%x|%X|%z|%Z"), runtime.IntegerValue(1392050796))
	if err != nil {
		// 常用 strftime 指令不应被误判为非法转换。
		t.Fatalf("Date common strftime failed: %v", err)
	}
	want := "Mon|Monday|Feb|February|20|02/10/14|10|2014-02-10|14|2014|Feb|04|16| 4|\n|PM|04:46:36 PM|16:46|1392050796|\t|16:46:36|1|06|07|06|02/10/14|16:46:36|+0000|UTC"
	if len(values) != 1 || values[0].Kind != runtime.KindString || values[0].String != want {
		// 输出文本必须稳定匹配 C locale 兼容格式。
		t.Fatalf("Date common strftime = %#v, want %q", values, want)
	}
}

// TestDiffTime 验证 os.difftime 返回秒差。
//
// 两个 Unix 时间戳相减应返回 number 类型的秒数差。
func TestDiffTime(t *testing.T) {
	// 计算 120 - 45 的秒差。
	values, err := DiffTime(runtime.IntegerValue(120), runtime.IntegerValue(45))
	if err != nil {
		// 合法 integer 参数不应失败。
		t.Fatalf("DiffTime failed: %v", err)
	}
	if len(values) != 1 || values[0].Kind != runtime.KindNumber || values[0].Number != 75 {
		// 返回值必须为 75 秒。
		t.Fatalf("DiffTime result = %#v, want 75", values)
	}
}

// TestExecuteRunsHostShell 验证 os.execute 的 shell 查询与命令执行。
//
// 无参数查询 shell 可用性返回 true；传入命令时返回 Lua 5.3 风格的成功或失败状态。
func TestExecuteRunsHostShell(t *testing.T) {
	// 无参数时返回 true 表示 shell 可用。
	values, err := Execute()
	if err != nil {
		// 查询 shell 不应抛错。
		t.Fatalf("Execute query failed: %v", err)
	}
	if len(values) != 1 || values[0].Kind != runtime.KindBoolean || !values[0].Bool {
		// 官方测试入口要求 shell 可用。
		t.Fatalf("Execute query result = %#v, want true", values)
	}
	success, err := Execute(runtime.StringValue("exit 0"))
	if err != nil {
		// 成功命令不应抛 Lua 错误。
		t.Fatalf("Execute success failed: %v", err)
	}
	if len(success) != 3 || success[0].Kind != runtime.KindBoolean || !success[0].Bool || success[2].Integer != 0 {
		// 成功命令必须返回 true, "exit", 0。
		t.Fatalf("Execute success result = %#v, want true exit 0", success)
	}
	failed, err := Execute(runtime.StringValue("exit 7"))
	if err != nil {
		// 非零退出码按返回值表达，不抛 Lua 错误。
		t.Fatalf("Execute failure failed: %v", err)
	}
	if len(failed) != 3 || !failed[0].IsNil() || failed[2].Integer != 7 {
		// 失败命令必须返回 nil, "exit", code。
		t.Fatalf("Execute failure result = %#v, want nil exit 7", failed)
	}
	if goruntime.GOOS != "windows" {
		// Unix-like 平台需要区分信号终止，覆盖官方 files.lua 的 HUP/KILL 用例。
		signaled, err := Execute(runtime.StringValue("kill -s HUP $$"))
		if err != nil {
			// 信号终止不应抛 Lua 错误，应通过三元组返回。
			t.Fatalf("Execute signal failed: %v", err)
		}
		if len(signaled) != 3 || !signaled[0].IsNil() || signaled[1].String != "signal" || signaled[2].Integer != 1 {
			// HUP 信号必须返回 nil, "signal", 1。
			t.Fatalf("Execute signal result = %#v, want nil signal 1", signaled)
		}
		nestedShell, err := Execute(runtime.StringValue("sh -c 'kill -s HUP $$'"))
		if err != nil {
			// 子 shell 被信号终止时，外层 shell 应按 exit 状态返回。
			t.Fatalf("Execute nested shell signal failed: %v", err)
		}
		if len(nestedShell) != 3 || !nestedShell[0].IsNil() || nestedShell[1].String != "exit" || nestedShell[2].Integer <= 0 {
			// 官方 files.lua 只要求嵌套 shell 返回 exit 且状态码为正。
			t.Fatalf("Execute nested shell signal result = %#v, want nil exit positive", nestedShell)
		}
	}
	if _, err := Execute(runtime.IntegerValue(1)); !errors.Is(err, runtime.ErrLuaError) {
		// 非 string 命令必须返回参数错误。
		t.Fatalf("Execute argument error = %v, want Lua error", err)
	}
}

func TestExecuteWithOptionsDeniesExplicitlyDisabledProcess(t *testing.T) {
	options := runtime.Options{}.WithProcessAccess(false)
	if _, err := ExecuteWithOptions(options, runtime.StringValue("exit 0")); !errors.Is(err, runtime.ErrLuaError) {
		t.Fatalf("ExecuteWithOptions disabled error = %v, want Lua error", err)
	}
	values, err := ExecuteWithOptions(options)
	if err != nil || len(values) != 1 || values[0].Kind != runtime.KindBoolean || values[0].Bool {
		t.Fatalf("disabled shell query = %#v, %v; want false", values, err)
	}
}

// TestExitReturnsStructuredLuaError 验证 os.exit 在嵌入模式下不终止宿主进程。
//
// 当前实现通过 Lua error 包装 ExitError，后续 CLI 层可用 errors.As 提取退出码。
func TestExitReturnsStructuredLuaError(t *testing.T) {
	// integer 退出码和 close=false 应写入 ExitError。
	_, err := Exit(runtime.IntegerValue(7), runtime.BooleanValue(false))
	if !errors.Is(err, runtime.ErrLuaError) {
		// os.exit 必须表现为 Lua error。
		t.Fatalf("Exit error = %v, want Lua error", err)
	}
	var exitErr *ExitError
	if !errors.As(err, &exitErr) {
		// 错误链必须能提取 ExitError。
		t.Fatalf("Exit error does not expose ExitError: %v", err)
	}
	if exitErr.Code != 7 || exitErr.Close {
		// 退出码和 close 标记必须保持参数语义。
		t.Fatalf("ExitError = %#v, want code=7 close=false", exitErr)
	}
}

// TestGetEnvPolicyAndFileOperations 验证环境变量策略与基础文件操作。
//
// getenv、remove 和 rename 的注册入口默认禁用宿主访问；显式授权后才执行真实宿主操作。
func TestGetEnvPolicyAndFileOperations(t *testing.T) {
	// getenv 默认禁止读取宿主环境变量。
	if _, err := GetEnvWithOptions(runtime.Options{}, runtime.StringValue("PATH")); !errors.Is(err, runtime.ErrLuaError) {
		// 禁用策略必须使用 Lua error 表达。
		t.Fatalf("GetEnv error = %v, want Lua error", err)
	}
	t.Setenv("GO_LUA_VM_OS_TEST", "enabled")
	envValues, err := GetEnvWithOptions(runtime.Options{AllowEnvironment: true}, runtime.StringValue("GO_LUA_VM_OS_TEST"))
	if err != nil {
		// 显式授权后 os.getenv 应读取宿主环境变量。
		t.Fatalf("GetEnv allowed failed: %v", err)
	}
	if len(envValues) != 1 || envValues[0].String != "enabled" {
		// 返回值必须是宿主环境变量文本。
		t.Fatalf("GetEnv allowed = %#v, want enabled", envValues)
	}

	dir := t.TempDir()
	sourcePath := filepath.Join(dir, "source.tmp")
	targetPath := filepath.Join(dir, "target.tmp")
	if err := os.WriteFile(sourcePath, []byte("x"), 0o666); err != nil {
		// 测试夹具文件必须可创建。
		t.Fatalf("WriteFile failed: %v", err)
	}
	if _, err := RenameWithOptions(runtime.Options{}, runtime.StringValue(sourcePath), runtime.StringValue(targetPath)); !errors.Is(err, runtime.ErrLuaError) {
		// 默认注册策略必须拒绝宿主文件系统写入。
		t.Fatalf("Rename default error = %v, want Lua error", err)
	}
	renamed, err := RenameWithOptions(runtime.Options{AllowHostFilesystem: true}, runtime.StringValue(sourcePath), runtime.StringValue(targetPath))
	if err != nil {
		// 有效路径改名不应失败。
		t.Fatalf("Rename failed: %v", err)
	}
	if len(renamed) != 1 || renamed[0].Kind != runtime.KindBoolean || !renamed[0].Bool {
		// os.rename 成功时返回 true。
		t.Fatalf("Rename result = %#v, want true", renamed)
	}
	missingRename, err := RenameWithOptions(runtime.Options{AllowHostFilesystem: true}, runtime.StringValue(sourcePath), runtime.StringValue(targetPath))
	if err != nil {
		// 改名不存在源路径属于普通失败返回，不应抛 Lua error。
		t.Fatalf("Rename missing failed with error: %v", err)
	}
	if len(missingRename) != 3 || !missingRename[0].IsNil() || missingRename[1].Kind != runtime.KindString || missingRename[2].Kind != runtime.KindInteger {
		// 缺失源路径必须返回 nil、错误文本和数字错误码。
		t.Fatalf("Rename missing result = %#v, want nil message code", missingRename)
	}
	if _, err := RemoveWithOptions(runtime.Options{}, runtime.StringValue(targetPath)); !errors.Is(err, runtime.ErrLuaError) {
		// 默认注册策略必须拒绝宿主文件删除。
		t.Fatalf("Remove default error = %v, want Lua error", err)
	}
	removed, err := RemoveWithOptions(runtime.Options{AllowHostFilesystem: true}, runtime.StringValue(targetPath))
	if err != nil {
		// 有效路径删除不应失败。
		t.Fatalf("Remove failed: %v", err)
	}
	if len(removed) != 1 || removed[0].Kind != runtime.KindBoolean || !removed[0].Bool {
		// os.remove 成功时返回 true。
		t.Fatalf("Remove result = %#v, want true", removed)
	}
	missing, err := RemoveWithOptions(runtime.Options{AllowHostFilesystem: true}, runtime.StringValue(targetPath))
	if err != nil {
		// 删除不存在路径属于 os.remove 普通失败返回，不应抛 Lua error。
		t.Fatalf("Remove missing failed with error: %v", err)
	}
	if len(missing) != 2 || !missing[0].IsNil() || missing[1].Kind != runtime.KindString || missing[1].String == "" {
		// 缺失文件必须返回 nil 和错误文本，便于脚本使用 not os.remove(...) 判断。
		t.Fatalf("Remove missing result = %#v, want nil and message", missing)
	}
}

// TestSetLocalePolicy 验证 os.setlocale 的固定 C locale 策略。
//
// 当前纯 Go 实现不切换进程 locale，只支持查询和设置 C locale。
func TestSetLocalePolicy(t *testing.T) {
	// 无参数查询当前 locale 返回 C。
	values, err := SetLocale()
	if err != nil {
		// 查询 locale 不应失败。
		t.Fatalf("SetLocale query failed: %v", err)
	}
	if len(values) != 1 || values[0].String != cLocaleName {
		// 当前固定 locale 为 C。
		t.Fatalf("SetLocale query = %#v, want C", values)
	}
	values, err = SetLocale(runtime.StringValue(cLocaleName), runtime.StringValue("all"))
	if err != nil {
		// 设置 C locale 不应失败。
		t.Fatalf("SetLocale C failed: %v", err)
	}
	if len(values) != 1 || values[0].String != cLocaleName {
		// 设置 C locale 返回 C。
		t.Fatalf("SetLocale C = %#v, want C", values)
	}
	values, err = SetLocale(runtime.StringValue("en_US.UTF-8"))
	if err != nil {
		// 不支持的 locale 按 Lua 语义返回 nil，不抛错。
		t.Fatalf("SetLocale unsupported failed: %v", err)
	}
	if len(values) != 1 || !values[0].IsNil() {
		// 不支持的 locale 必须返回 nil。
		t.Fatalf("SetLocale unsupported = %#v, want nil", values)
	}
}

// TestTimeReturnsCurrentOrTableTimestamp 验证 os.time 的无参和 table 参数路径。
//
// 无参返回当前 Unix 秒；table 参数按本地时区构造 Unix 秒。
func TestTimeReturnsCurrentOrTableTimestamp(t *testing.T) {
	// 无参时间戳应落在调用前后范围内。
	before := time.Now().Unix()
	values, err := Time()
	after := time.Now().Unix()
	if err != nil {
		// 无参 os.time 不应失败。
		t.Fatalf("Time now failed: %v", err)
	}
	if len(values) != 1 || values[0].Kind != runtime.KindInteger || values[0].Integer < before || values[0].Integer > after {
		// 当前时间戳必须在调用窗口内。
		t.Fatalf("Time now = %#v, want between %d and %d", values, before, after)
	}

	table := runtime.NewTable()
	table.RawSetString("year", runtime.IntegerValue(1970))
	table.RawSetString("month", runtime.IntegerValue(1))
	table.RawSetString("day", runtime.IntegerValue(2))
	table.RawSetString("hour", runtime.IntegerValue(3))
	table.RawSetString("min", runtime.IntegerValue(4))
	table.RawSetString("sec", runtime.IntegerValue(5))
	tableValue := runtime.ReferenceValue(runtime.KindTable, table)
	values, err = Time(tableValue)
	if err != nil {
		// 完整 date table 不应失败。
		t.Fatalf("Time table failed: %v", err)
	}
	want := time.Date(1970, time.January, 2, 3, 4, 5, 0, time.Local).Unix()
	if len(values) != 1 || values[0].Integer != want {
		// table 参数必须按本地时区转换。
		t.Fatalf("Time table = %#v, want %d", values, want)
	}
	stringFieldTable := runtime.NewTable()
	stringFieldTable.RawSetString("year", runtime.StringValue("1970"))
	stringFieldTable.RawSetString("month", runtime.StringValue("1"))
	stringFieldTable.RawSetString("day", runtime.StringValue("2"))
	stringFieldTable.RawSetString("hour", runtime.StringValue("3"))
	stringFieldTable.RawSetString("min", runtime.StringValue("4"))
	stringFieldTable.RawSetString("sec", runtime.StringValue("5"))
	stringFieldValues, err := Time(runtime.ReferenceValue(runtime.KindTable, stringFieldTable))
	if err != nil {
		// Lua 5.3 C 标准库通过 luaL_checkinteger 读取字段，numeric string 应可转换。
		t.Fatalf("Time numeric string table failed: %v", err)
	}
	if len(stringFieldValues) != 1 || stringFieldValues[0].Integer != want {
		// numeric string 字段应与 integer 字段构造同一个时间戳。
		t.Fatalf("Time numeric string table = %#v, want %d", stringFieldValues, want)
	}
	if stringFieldTable.RawGetString("year").Kind != runtime.KindInteger ||
		stringFieldTable.RawGetString("sec").Kind != runtime.KindInteger {
		// os.time(table) 成功后仍要把归一化 integer 字段写回原 table。
		t.Fatalf("Time numeric string table was not normalized")
	}
	normalizingTable := runtime.NewTable()
	normalizingTable.RawSetString("year", runtime.IntegerValue(2005))
	normalizingTable.RawSetString("month", runtime.IntegerValue(1))
	normalizingTable.RawSetString("day", runtime.IntegerValue(1))
	normalizingTable.RawSetString("hour", runtime.IntegerValue(1))
	normalizingTable.RawSetString("min", runtime.IntegerValue(0))
	normalizingTable.RawSetString("sec", runtime.IntegerValue(-3602))
	if _, err := Time(runtime.ReferenceValue(runtime.KindTable, normalizingTable)); err != nil {
		// 越界秒数字段应由 Go time.Date 归一化，不应直接失败。
		t.Fatalf("Time normalizing table failed: %v", err)
	}
	if normalizingTable.RawGetString("year").Integer != 2004 ||
		normalizingTable.RawGetString("month").Integer != 12 ||
		normalizingTable.RawGetString("day").Integer != 31 ||
		normalizingTable.RawGetString("hour").Integer != 23 ||
		normalizingTable.RawGetString("min").Integer != 59 ||
		normalizingTable.RawGetString("sec").Integer != 58 ||
		normalizingTable.RawGetString("yday").Integer != 366 {
		// os.time(table) 必须按 Lua 5.3 行为把归一化后的字段写回原 table。
		t.Fatalf("Time normalized table fields are wrong")
	}
	table.RawSetString("year", runtime.IntegerValue(-1))
	if _, err := Time(tableValue); err == nil || !strings.Contains(runtime.ErrorObject(err).String, "out-of-bound") {
		// 负年份越界必须返回 out-of-bound。
		t.Fatalf("Time negative year error = %v, want out-of-bound", err)
	}
	table.RawSetString("year", runtime.IntegerValue(1<<60))
	if _, err := Time(tableValue); err == nil || !strings.Contains(runtime.ErrorObject(err).String, "cannot be represented") {
		// 超大年份必须返回 cannot be represented。
		t.Fatalf("Time huge year error = %v, want cannot be represented", err)
	}
}

// TestTmpNameReturnsTemporaryPath 验证 os.tmpname 返回可用临时路径。
//
// 返回路径应为字符串，且创建出的占位文件可被 os.remove 清理。
func TestTmpNameReturnsTemporaryPath(t *testing.T) {
	// os.tmpname 返回一个宿主临时路径。
	values, err := TmpName()
	if err != nil {
		// 生成临时路径不应失败。
		t.Fatalf("TmpName failed: %v", err)
	}
	if len(values) != 1 || values[0].Kind != runtime.KindString || values[0].String == "" {
		// 返回值必须是非空字符串。
		t.Fatalf("TmpName result = %#v, want non-empty string", values)
	}
	if _, err := os.Stat(values[0].String); err != nil {
		// 占位文件应存在，确保路径唯一且官方清理脚本可删除。
		t.Fatalf("TmpName path stat failed: %v", err)
	}
	removed, err := Remove(values[0])
	if err != nil {
		// tmpname 返回路径必须可通过 os.remove 清理。
		t.Fatalf("Remove tmpname path failed: %v", err)
	}
	if len(removed) != 1 || removed[0].Kind != runtime.KindBoolean || !removed[0].Bool {
		// 删除 tmpname 占位文件应返回 true。
		t.Fatalf("Remove tmpname result = %#v, want true", removed)
	}
}
