package lexer

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestSourceReadRuneTracksUTF8ByteOffsets 验证 Source 按 UTF-8 读取并维护字节偏移。
//
// Lua 字符串长度后续按字节计算，因此 lexer 必须在 rune 行列之外保留字节 Offset。
func TestSourceReadRuneTracksUTF8ByteOffsets(t *testing.T) {
	source := NewSource("a界\nb")

	firstRune, firstPosition, ok := source.Next()
	if !ok {
		// 第一个字符必须可读，否则输入流初始化失败。
		t.Fatalf("first rune should be readable")
	}
	if firstRune != 'a' || firstPosition.Line != 1 || firstPosition.Column != 1 || firstPosition.Offset != 0 {
		// ASCII 字符应占 1 字节，初始行列应为 1:1。
		t.Fatalf("unexpected first rune=%q position=%+v", firstRune, firstPosition)
	}

	secondRune, secondPosition, ok := source.Next()
	if !ok {
		// 第二个 UTF-8 字符必须可读。
		t.Fatalf("second rune should be readable")
	}
	if secondRune != '界' || secondPosition.Line != 1 || secondPosition.Column != 2 || secondPosition.Offset != 1 {
		// 非 ASCII rune 列号只增加 1，但 Offset 必须按 UTF-8 字节前进。
		t.Fatalf("unexpected second rune=%q position=%+v", secondRune, secondPosition)
	}

	newlineRune, newlinePosition, ok := source.Next()
	if !ok {
		// 换行字符必须可读并推进到下一行。
		t.Fatalf("newline should be readable")
	}
	if newlineRune != '\n' || newlinePosition.Line != 1 || newlinePosition.Column != 3 || newlinePosition.Offset != 4 {
		// `界` 占 3 字节，所以换行 Offset 应为 4。
		t.Fatalf("unexpected newline rune=%q position=%+v", newlineRune, newlinePosition)
	}

	currentPosition := source.Position()
	if currentPosition.Line != 2 || currentPosition.Column != 1 || currentPosition.Offset != 5 {
		// 换行后下一次读取位置应回到下一行第一列。
		t.Fatalf("unexpected current position=%+v", currentPosition)
	}
}

// TestSourcePeekKeepsASCIIAndUTF8Positions 验证预读 ASCII 快路径不改变 UTF-8 位置语义。
func TestSourcePeekKeepsASCIIAndUTF8Positions(t *testing.T) {
	source := NewSource("ab界")

	firstRune, ok := source.Peek()
	if !ok || firstRune != 'a' {
		// 初始 ASCII 预读必须返回首字符且不能到达 EOF。
		t.Fatalf("unexpected first peek rune=%q ok=%v", firstRune, ok)
	}
	secondRune, ok := source.PeekOffset(1)
	if !ok || secondRune != 'b' {
		// runeOffset=1 仍在 ASCII 快路径内，必须按单字符偏移。
		t.Fatalf("unexpected second peek rune=%q ok=%v", secondRune, ok)
	}
	thirdRune, ok := source.PeekOffset(2)
	if !ok || thirdRune != '界' {
		// ASCII 后的 UTF-8 rune 必须按 rune 偏移返回，而不是按字节误切。
		t.Fatalf("unexpected third peek rune=%q ok=%v", thirdRune, ok)
	}
	if position := source.Position(); position.Line != 1 || position.Column != 1 || position.Offset != 0 {
		// 所有 Peek 路径都不能推进 Source 位置。
		t.Fatalf("peek should not advance position, got %+v", position)
	}
}

// TestSourceSkipsInitialUTF8BOM 验证源码开头 UTF-8 BOM 会被 Lua 5.3 兼容路径忽略。
func TestSourceSkipsInitialUTF8BOM(t *testing.T) {
	source := NewSource("\xef\xbb\xbfreturn")
	nextRune, position, ok := source.Next()
	if !ok {
		// BOM 后仍有源码，读取不应到达 EOF。
		t.Fatalf("Next after BOM returned EOF")
	}
	if nextRune != 'r' {
		// 首个可见字符必须是 BOM 后的源码字符。
		t.Fatalf("first rune after BOM = %q, want r", nextRune)
	}
	if position.Line != 1 || position.Column != 1 || position.Offset != 3 {
		// BOM 不占 Lua 可见列，但字节 Offset 仍反映原始输入位置。
		t.Fatalf("position after BOM = %+v", position)
	}
}

// TestSourceReadRuneNormalizesCRLF 验证 Source 按 Lua 5.3 语义归一换行。
//
// CR 和 CRLF 都必须对上层表现为单个 '\n'，并且 CRLF 只能推进一行，避免长字符串和
// debug line info 在 Windows 换行输入下多算一行。
func TestSourceReadRuneNormalizesCRLF(t *testing.T) {
	source := NewSource("a\r\nb\rc\n\rd")

	firstRune, _, ok := source.Next()
	if !ok || firstRune != 'a' {
		// 首字符用于把读取位置推进到 CRLF 前。
		t.Fatalf("unexpected first rune=%q ok=%v", firstRune, ok)
	}
	newlineRune, newlinePosition, ok := source.Next()
	if !ok {
		// CRLF 必须作为一个可读换行返回。
		t.Fatalf("expected CRLF newline")
	}
	if newlineRune != '\n' || newlinePosition.Line != 1 || newlinePosition.Column != 2 || newlinePosition.Offset != 1 {
		// CRLF 的读取位置指向 CR，返回值统一归一为 LF。
		t.Fatalf("unexpected CRLF rune=%q position=%+v", newlineRune, newlinePosition)
	}
	positionAfterCRLF := source.Position()
	if positionAfterCRLF.Line != 2 || positionAfterCRLF.Column != 1 || positionAfterCRLF.Offset != 3 {
		// CRLF 消费两个字节但只推进一行。
		t.Fatalf("unexpected position after CRLF=%+v", positionAfterCRLF)
	}
	source.Next()
	crRune, crPosition, ok := source.Next()
	if !ok || crRune != '\n' || crPosition.Line != 2 || crPosition.Column != 2 {
		// 单独 CR 也必须归一为 LF 并推进一行。
		t.Fatalf("unexpected CR rune=%q position=%+v ok=%v", crRune, crPosition, ok)
	}
	source.Next()
	lfcrRune, lfcrPosition, ok := source.Next()
	if !ok || lfcrRune != '\n' || lfcrPosition.Line != 3 || lfcrPosition.Column != 2 {
		// LFCR 也属于 Lua 5.3 的单个换行序列。
		t.Fatalf("unexpected LFCR rune=%q position=%+v ok=%v", lfcrRune, lfcrPosition, ok)
	}
	positionAfterLFCR := source.Position()
	if positionAfterLFCR.Line != 4 || positionAfterLFCR.Column != 1 || positionAfterLFCR.Offset != 8 {
		// LFCR 消费两个字节但只推进一行。
		t.Fatalf("unexpected position after LFCR=%+v", positionAfterLFCR)
	}
}

// TestLexerSkipIgnoredSkipsWhitespaceAndShortComments 验证空白和短注释会被跳过。
//
// 该用例覆盖当前阶段已实现的可忽略内容，长注释不在本轮语义范围内。
func TestLexerSkipIgnoredSkipsWhitespaceAndShortComments(t *testing.T) {
	lexer := New(" \t-- comment\n  value")

	lexer.SkipIgnored()
	nextRune, ok := lexer.PeekRune()
	if !ok {
		// 跳过可忽略内容后应保留后续源码字符。
		t.Fatalf("expected next rune after ignored input")
	}
	if nextRune != 'v' {
		// 短注释和空白都应被消费，当前位置应落在 value 的首字符。
		t.Fatalf("unexpected next rune=%q", nextRune)
	}

	position := lexer.Position()
	if position.Line != 2 || position.Column != 3 {
		// 第二行前两个空格被跳过后，value 应从第 3 列开始。
		t.Fatalf("unexpected position after skip=%+v", position)
	}
}

// TestLexerSkipsInitialShebang 验证 Lua CLI 首行 shebang 会被跳过并保留行号。
//
// 官方测试套件入口 all.lua 以 shebang 开头；lexer 必须只跳过文件首行的 shebang，
// 不能把普通 `#` 长度操作符当作注释处理。
func TestLexerSkipsInitialShebang(t *testing.T) {
	lexer := New(StripInitialShebang("#!../lua\nlocal value = 1"))

	token := lexer.NextToken()
	if token.Kind != TokenKeyword || token.Text != "local" {
		// shebang 后第一条有效 token 必须是第二行的 local。
		t.Fatalf("unexpected token after shebang: %#v", token)
	}
	if token.Position.Line != 2 || token.Position.Column != 1 {
		// 跳过 shebang 后仍需保留原始源码行号，便于错误定位。
		t.Fatalf("unexpected token position after shebang: %+v", token.Position)
	}

	bomLexer := New(StripInitialShebang("\xef\xbb\xbf# comment\nreturn 1"))
	token = bomLexer.NextToken()
	if token.Kind != TokenKeyword || token.Text != "return" {
		// BOM 后的首行 # comment 也应按文件 shebang/comment 兼容路径跳过。
		t.Fatalf("unexpected token after BOM shebang: %#v", token)
	}
	if token.Position.Line != 2 || token.Position.Column != 1 {
		// BOM 不占可见列，shebang 占位行仍应让 return 位于第二行。
		t.Fatalf("unexpected token position after BOM shebang: %+v", token.Position)
	}

	hashLexer := New("local n = #items")
	for {
		// 扫描到 # 操作符，确认非首行 shebang 不会被 strip。
		token = hashLexer.NextToken()
		if token.Kind == TokenEOF || token.Kind == TokenIllegal {
			// 未看到 # 说明长度操作符被错误跳过或扫描失败。
			t.Fatalf("hash operator was not preserved, token=%#v", token)
		}
		if token.Text == "#" {
			// 找到 # 操作符即完成验证。
			break
		}
	}

	plainHashLexer := New("#=1")
	token = plainHashLexer.NextToken()
	if token.Kind != TokenOperator || token.Text != "#" {
		// load(string) 或普通 lexer 输入中的首行 # 仍应保留给 parser 判定为非法语句。
		t.Fatalf("plain leading hash was stripped: %#v", token)
	}
}

// TestLexerSkipIgnoredKeepsMinusOperator 验证单个减号不会被误识别为短注释。
//
// Lua 短注释必须以 `--` 开头，单个 `-` 后续会由操作符 token 任务处理。
func TestLexerSkipIgnoredKeepsMinusOperator(t *testing.T) {
	lexer := New("  - value")

	lexer.SkipIgnored()
	nextRune, ok := lexer.PeekRune()
	if !ok {
		// 空白后还有减号，不能到达 EOF。
		t.Fatalf("expected minus operator after whitespace")
	}
	if nextRune != '-' {
		// 单个减号不是短注释，必须保留给后续 token 扫描。
		t.Fatalf("unexpected next rune=%q", nextRune)
	}
}

// TestLexerSkipIgnoredConsumesCommentToEOF 验证短注释可以延伸到 EOF。
//
// Lua 允许文件以短注释结尾；跳过后 PeekRune 应报告 EOF。
func TestLexerSkipIgnoredConsumesCommentToEOF(t *testing.T) {
	lexer := New("-- final comment")

	lexer.SkipIgnored()
	_, ok := lexer.PeekRune()
	if ok {
		// 注释延伸到 EOF 时，所有输入都应被消费。
		t.Fatalf("expected EOF after final short comment")
	}
}

// TestLexerSkipIgnoredSkipsLongComment 验证长注释会被 SkipIgnored 跳过。
//
// Lua 长注释使用 `--[=*[...]=*]` 形式，等号数量必须在开闭分隔符中匹配。
func TestLexerSkipIgnoredSkipsLongComment(t *testing.T) {
	lexer := New("--[=[\ncomment\n]=]value")

	lexer.SkipIgnored()
	nextRune, ok := lexer.PeekRune()
	if !ok {
		// 长注释后还有 value，不能到达 EOF。
		t.Fatalf("expected value after long comment")
	}
	if nextRune != 'v' {
		// 长注释整体应被丢弃，当前位置应落在 value。
		t.Fatalf("unexpected next rune=%q", nextRune)
	}
}

// TestLexerScanLongString 验证 Lua 长字符串内容扫描。
//
// 长字符串会去掉开闭分隔符，并忽略开头紧跟的首个换行。
func TestLexerScanLongString(t *testing.T) {
	lexer := New("[=[\nhello\n]=]")

	text, position, ok, err := lexer.ScanLongString()
	if err != nil {
		// 合法长字符串不应返回错误。
		t.Fatalf("scan long string failed: %v", err)
	}
	if !ok {
		// 输入以长括号开头，必须识别为长字符串。
		t.Fatalf("expected long string")
	}
	if text != "hello\n" {
		// 初始换行应被忽略，正文换行应保留。
		t.Fatalf("unexpected long string text=%q", text)
	}
	if position.Line != 1 || position.Column != 1 {
		// 返回位置应指向长字符串起始分隔符。
		t.Fatalf("unexpected long string position=%+v", position)
	}
}

// TestLexerScanLongStringNormalizesNewlines 验证长字符串按 Lua 5.3 归一化换行。
//
// 官方 literals.lua 期望长字符串中的 CR、CRLF 和 LFCR 内容都变为 '\n'，且混合换行只算一行。
func TestLexerScanLongStringNormalizesNewlines(t *testing.T) {
	lexer := New("[[\ralo\n\ralo\r\n]]")

	text, _, ok, err := lexer.ScanLongString()
	if err != nil {
		// 合法长字符串不应返回错误。
		t.Fatalf("scan long string failed: %v", err)
	}
	if !ok {
		// 输入以长括号开头，必须识别为长字符串。
		t.Fatalf("expected long string")
	}
	if text != "alo\nalo\n" {
		// CR、CRLF 与 LFCR 都必须统一写入 LF。
		t.Fatalf("unexpected normalized long string text=%q", text)
	}
	if lexer.Position().Line != 4 {
		// 初始 CR、LFCR、CRLF 共三次换行，关闭分隔符后应位于第 4 行。
		t.Fatalf("unexpected lexer position=%+v", lexer.Position())
	}
}

// TestLexerScanShortStringQuotes 验证单双引号短字符串都可扫描。
//
// 两种引号都属于 Lua 5.3 短字符串语法，闭合引号必须与起始引号一致。
func TestLexerScanShortStringQuotes(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		input string
		want  string
	}{
		{name: "single", input: "'hello'", want: "hello"},
		{name: "double", input: "\"world\"", want: "world"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			lexer := New(testCase.input)

			text, _, ok, err := lexer.ScanShortString()
			if err != nil {
				// 合法短字符串不应返回错误。
				t.Fatalf("scan short string failed: %v", err)
			}
			if !ok {
				// 当前输入以引号开头，必须识别为短字符串。
				t.Fatalf("expected short string")
			}
			if text != testCase.want {
				// 字符串内容必须去掉外层引号。
				t.Fatalf("unexpected text=%q want=%q", text, testCase.want)
			}
		})
	}
}

// TestLexerScanShortStringEscapes 验证 Lua 5.3 短字符串转义序列。
//
// 该用例覆盖常用字符转义、十进制字节、十六进制字节、Unicode code point 和 `\z` 空白吞噬。
func TestLexerScanShortStringEscapes(t *testing.T) {
	lexer := New("'a\\n\\65\\x42\\u{43}\\z  \n\tD'")

	text, _, ok, err := lexer.ScanShortString()
	if err != nil {
		// 合法转义序列不应返回错误。
		t.Fatalf("scan escaped string failed: %v", err)
	}
	if !ok {
		// 输入以单引号开头，必须识别为短字符串。
		t.Fatalf("expected escaped short string")
	}
	if text != "a\nABCD" {
		// 转义结果必须按 Lua 5.3 字节/Unicode 语义展开。
		t.Fatalf("unexpected escaped text=%q", text)
	}
}

// TestLexerScanStringPreservesInvalidUTF8Bytes 验证字符串字面量保留 Lua 原始高位字节。
//
// Lua 5.3 字符串是任意字节序列；官方 pm.lua 含 Latin-1 单字节 0xe1，词法扫描不能把它
// 转换成 UTF-8 RuneError 的三字节表示。
func TestLexerScanStringPreservesInvalidUTF8Bytes(t *testing.T) {
	shortLexer := New("'\xe1'")
	shortText, _, shortOK, shortErr := shortLexer.ScanShortString()
	if shortErr != nil {
		// 合法短字符串中的高位原始字节不应报错。
		t.Fatalf("scan short high byte failed: %v", shortErr)
	}
	if !shortOK || shortText != "\xe1" || len(shortText) != 1 {
		// 短字符串必须保留原始单字节内容。
		t.Fatalf("short high byte mismatch: text=%q len=%d ok=%v", shortText, len(shortText), shortOK)
	}

	longLexer := New("[[\xe1]]")
	longText, _, longOK, longErr := longLexer.ScanLongString()
	if longErr != nil {
		// 合法长字符串中的高位原始字节不应报错。
		t.Fatalf("scan long high byte failed: %v", longErr)
	}
	if !longOK || longText != "\xe1" || len(longText) != 1 {
		// 长字符串必须保留原始单字节内容。
		t.Fatalf("long high byte mismatch: text=%q len=%d ok=%v", longText, len(longText), longOK)
	}
}

// TestLexerScanShortStringRejectsInvalidEscape 验证非法转义会返回错误。
//
// Lua 5.3 不接受未知反斜杠转义，后续完整 token 错误会复用该错误语义。
func TestLexerScanShortStringRejectsInvalidEscape(t *testing.T) {
	lexer := New("'\\q'")

	_, _, ok, err := lexer.ScanShortString()
	if !ok {
		// 输入以单引号开头，必须进入短字符串扫描。
		t.Fatalf("expected short string")
	}
	if err == nil {
		// 未知转义必须报错。
		t.Fatalf("expected invalid escape error")
	}
}

// TestLexerScanNumberKinds 验证 Lua 5.3 数字字面量的四种基础形态。
//
// 该用例覆盖十进制整数、十进制浮点、十六进制整数和十六进制浮点。
func TestLexerScanNumberKinds(t *testing.T) {
	for _, testCase := range []struct {
		name        string
		input       string
		wantText    string
		wantKind    NumberKind
		wantInteger int64
		wantNumber  float64
	}{
		{name: "decimal integer", input: "123 next", wantText: "123", wantKind: NumberDecimalInteger, wantInteger: 123},
		{name: "decimal integer overflow becomes float", input: "10000000000000000000000 next", wantText: "10000000000000000000000", wantKind: NumberDecimalFloat, wantNumber: 1e22},
		{name: "decimal float", input: "12.5e1 next", wantText: "12.5e1", wantKind: NumberDecimalFloat, wantNumber: 125},
		{name: "leading dot decimal float", input: ".0 next", wantText: ".0", wantKind: NumberDecimalFloat, wantNumber: 0},
		{name: "leading dot decimal exponent", input: ".2e2 next", wantText: ".2e2", wantKind: NumberDecimalFloat, wantNumber: 20},
		{name: "hex integer", input: "0x2a next", wantText: "0x2a", wantKind: NumberHexInteger, wantInteger: 42},
		{name: "hex integer uint64 wrap", input: "0x8000000000000000 next", wantText: "0x8000000000000000", wantKind: NumberHexInteger, wantInteger: -9223372036854775808},
		{name: "hex integer long wrap", input: "0x13121110090807060504030201 next", wantText: "0x13121110090807060504030201", wantKind: NumberHexInteger, wantInteger: 0x0807060504030201},
		{name: "hex float", input: "0x1.8p1 next", wantText: "0x1.8p1", wantKind: NumberHexFloat, wantNumber: 3},
		{name: "hex float without exponent", input: "0xF0.0 next", wantText: "0xF0.0", wantKind: NumberHexFloat, wantNumber: 240},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			lexer := New(testCase.input)

			numberLiteral, ok, err := lexer.ScanNumber()
			if err != nil {
				// 合法数字字面量不应返回错误。
				t.Fatalf("scan number failed: %v", err)
			}
			if !ok {
				// 输入以数字开头，必须识别出数字字面量。
				t.Fatalf("expected number")
			}
			if numberLiteral.Text != testCase.wantText || numberLiteral.Kind != testCase.wantKind {
				// 文本和分类必须与输入形态一致。
				t.Fatalf("unexpected number=%+v", numberLiteral)
			}
			if testCase.wantKind == NumberDecimalInteger || testCase.wantKind == NumberHexInteger {
				// 整数分类必须写入 Integer 字段。
				if numberLiteral.Integer != testCase.wantInteger {
					t.Fatalf("unexpected integer=%d want=%d", numberLiteral.Integer, testCase.wantInteger)
				}
			} else {
				// 浮点分类必须写入 Number 字段。
				if numberLiteral.Number != testCase.wantNumber {
					t.Fatalf("unexpected number=%v want=%v", numberLiteral.Number, testCase.wantNumber)
				}
			}
		})
	}
}

// TestLexerScanNumberKeepsConcatDots 验证十进制整数不会吞掉连接操作符。
//
// Lua 中 `1..x` 应扫描为数字 `1` 后跟 `..`，不能扫描成浮点 `1.`。
func TestLexerScanNumberKeepsConcatDots(t *testing.T) {
	lexer := New("1..x")

	numberLiteral, ok, err := lexer.ScanNumber()
	if err != nil {
		// 合法整数前缀不应返回错误。
		t.Fatalf("scan number failed: %v", err)
	}
	if !ok {
		// 输入以数字开头，必须扫描出数字。
		t.Fatalf("expected number")
	}
	if numberLiteral.Text != "1" || numberLiteral.Kind != NumberDecimalInteger {
		// 数字扫描必须在连接操作符前停止。
		t.Fatalf("unexpected number literal=%+v", numberLiteral)
	}

	nextRune, ok := lexer.PeekRune()
	if !ok {
		// 连接操作符还未被消费，不能到达 EOF。
		t.Fatalf("expected concat operator after number")
	}
	if nextRune != '.' {
		// 下一个字符必须仍是连接操作符的第一个点。
		t.Fatalf("unexpected next rune=%q", nextRune)
	}
}

// TestLexerScanNumberRejectsInvalidExponent 验证非法指数会返回数字错误。
//
// 指数标记后必须至少有一位十进制数字。
func TestLexerScanNumberRejectsInvalidExponent(t *testing.T) {
	lexer := New("12e+")

	_, ok, err := lexer.ScanNumber()
	if !ok {
		// 输入以数字开头，必须进入数字扫描。
		t.Fatalf("expected number scan")
	}
	if err == nil {
		// 缺少指数数字必须报错。
		t.Fatalf("expected invalid number error")
	}
}

// TestLexerScanIdentifier 验证 Lua 标识符扫描。
//
// 当前阶段标识符只处理 ASCII 字母、数字和下划线，关键字分类由后续任务处理。
func TestLexerScanIdentifier(t *testing.T) {
	lexer := New("_name123 +")

	identifier, position, ok := lexer.ScanIdentifier()
	if !ok {
		// 输入以下划线开头，必须识别为标识符。
		t.Fatalf("expected identifier")
	}
	if identifier != "_name123" {
		// 标识符必须包含后续 ASCII 字母和数字。
		t.Fatalf("unexpected identifier=%q", identifier)
	}
	if position.Line != 1 || position.Column != 1 {
		// 起始位置应指向标识符第一个字符。
		t.Fatalf("unexpected identifier position=%+v", position)
	}

	nextRune, ok := lexer.PeekRune()
	if !ok {
		// 标识符后仍有空格和操作符，不能到达 EOF。
		t.Fatalf("expected remaining input")
	}
	if nextRune != ' ' {
		// 标识符扫描必须在第一个非标识符字符处停止。
		t.Fatalf("unexpected next rune=%q", nextRune)
	}
}

// TestLexerScanIdentifierAcceptsUnicode 验证宿主扩展可直接使用中文标识符。
func TestLexerScanIdentifierAcceptsUnicode(t *testing.T) {
	lexer := New("abc界_面2 +")

	identifier, position, ok := lexer.ScanIdentifier()
	if !ok {
		// 输入以 ASCII 字母开头，必须先识别前缀标识符。
		t.Fatalf("expected identifier")
	}
	if identifier != "abc界_面2" {
		t.Fatalf("unexpected identifier=%q", identifier)
	}
	if position.Line != 1 || position.Column != 1 || position.Offset != 0 {
		// 起始位置应指向标识符第一个字符。
		t.Fatalf("unexpected identifier position=%+v", position)
	}

	currentPosition := lexer.source.Position()
	if currentPosition.Line != 1 || currentPosition.Column != 8 || currentPosition.Offset != len("abc界_面2") {
		t.Fatalf("unexpected current position=%+v", currentPosition)
	}

	nextRune, ok := lexer.PeekRune()
	if !ok {
		t.Fatalf("expected remaining input")
	}
	if nextRune != ' ' {
		t.Fatalf("unexpected next rune=%q", nextRune)
	}

	token := lexer.NextToken()
	if token.Kind != TokenOperator || token.Text != "+" {
		t.Fatalf("unexpected token after identifier: kind=%s text=%q err=%v", token.Kind, token.Text, token.Err)
	}
}

func TestLexerNextTokenStartsWithUnicodeIdentifier(t *testing.T) {
	lexer := New("登录状态 = true")
	token := lexer.NextToken()
	if token.Kind != TokenIdentifier || token.Text != "登录状态" {
		t.Fatalf("unexpected unicode identifier: kind=%s text=%q err=%v", token.Kind, token.Text, token.Err)
	}
}

// TestLexerNextTokenRecognizesKeywordOperatorEOFAndIllegal 验证完整 token 流的关键分类。
//
// 该用例覆盖关键字、标识符、操作符、EOF 和非法字符，确保 lexer 可以被 parser 基础接入。
func TestLexerNextTokenRecognizesKeywordOperatorEOFAndIllegal(t *testing.T) {
	lexer := New("if x ~= 1 @")

	expectedTokens := []struct {
		kind TokenKind
		text string
	}{
		{kind: TokenKeyword, text: "if"},
		{kind: TokenIdentifier, text: "x"},
		{kind: TokenOperator, text: "~="},
		{kind: TokenNumber, text: "1"},
		{kind: TokenIllegal, text: "@"},
		{kind: TokenEOF, text: "<eof>"},
	}

	for tokenIndex, expectedToken := range expectedTokens {
		// 按顺序读取 token，任何一个分类错误都说明 token 流不可用于 parser。
		token := lexer.NextToken()
		if token.Kind != expectedToken.kind || token.Text != expectedToken.text {
			t.Fatalf("token %d got kind=%s text=%q want kind=%s text=%q", tokenIndex, token.Kind, token.Text, expectedToken.kind, expectedToken.text)
		}
		if token.Kind == TokenIllegal && token.Err == nil {
			// 非法 token 必须携带错误原因，便于后续错误报告。
			t.Fatalf("illegal token should carry error")
		}
	}
}

// TestLexerNextTokenGuardsKeepStringsCommentsAndBrackets 验证 token 起始 guard 不改变字符串、注释和括号语义。
func TestLexerNextTokenGuardsKeepStringsCommentsAndBrackets(t *testing.T) {
	lexer := New("-- short comment\n[=[long]=] --[[hidden]] \"short\" [ ]")

	expectedTokens := []struct {
		kind    TokenKind
		text    string
		literal string
	}{
		{kind: TokenString, text: "long", literal: "long"},
		{kind: TokenString, text: "short", literal: "short"},
		{kind: TokenOperator, text: "["},
		{kind: TokenOperator, text: "]"},
		{kind: TokenEOF, text: "<eof>"},
	}
	for tokenIndex, expectedToken := range expectedTokens {
		// 逐个读取 token，确保短注释和长注释被跳过，长/短字符串和普通方括号仍按原类型输出。
		token := lexer.NextToken()
		if token.Kind != expectedToken.kind || token.Text != expectedToken.text || token.Literal != expectedToken.literal {
			t.Fatalf("token %d got kind=%s text=%q literal=%q want kind=%s text=%q literal=%q", tokenIndex, token.Kind, token.Text, token.Literal, expectedToken.kind, expectedToken.text, expectedToken.literal)
		}
	}
}

// TestLexerNextTokenByteDispatchKeepsNumbersIdentifiersAndDots 验证首字节分派保留数字、标识符和点号操作符边界。
func TestLexerNextTokenByteDispatchKeepsNumbersIdentifiersAndDots(t *testing.T) {
	lexer := New(".5 .. . name 12")

	expectedTokens := []struct {
		kind TokenKind
		text string
	}{
		{kind: TokenNumber, text: ".5"},
		{kind: TokenOperator, text: ".."},
		{kind: TokenOperator, text: "."},
		{kind: TokenIdentifier, text: "name"},
		{kind: TokenNumber, text: "12"},
		{kind: TokenEOF, text: "<eof>"},
	}
	for tokenIndex, expectedToken := range expectedTokens {
		// 前导点数字必须走 number，普通点号仍必须走 operator，identifier 不应受数字路径影响。
		token := lexer.NextToken()
		if token.Kind != expectedToken.kind || token.Text != expectedToken.text {
			// token 种类或文本不一致时立即失败，避免后续 token 偏移掩盖首个边界错误。
			t.Fatalf("token %d got kind=%s text=%q want kind=%s text=%q", tokenIndex, token.Kind, token.Text, expectedToken.kind, expectedToken.text)
		}
	}
}

// TestLexerOperatorLongestMatch 验证 Lua 5.3 操作符按最长匹配输出。
//
// 该用例覆盖多字符操作符与同前缀单字符操作符，防止快速扫描路径把 `...`、`//`、`::` 等拆错。
func TestLexerOperatorLongestMatch(t *testing.T) {
	lexer := New("... .. . // / << <= < >> >= > == = ~= ~ :: : + - * % ^ # & | ( ) { } [ ] ; ,")

	expectedOperators := []string{
		"...", "..", ".", "//", "/", "<<", "<=", "<", ">>", ">=", ">", "==", "=", "~=", "~", "::", ":",
		"+", "-", "*", "%", "^", "#", "&", "|", "(", ")", "{", "}", "[", "]", ";", ",",
	}
	for operatorIndex, expectedOperator := range expectedOperators {
		// 每个 token 都必须保持 operator 类型和最长匹配文本。
		token := lexer.NextToken()
		if token.Kind != TokenOperator || token.Text != expectedOperator {
			t.Fatalf("operator %d got kind=%s text=%q want %q", operatorIndex, token.Kind, token.Text, expectedOperator)
		}
	}
	if token := lexer.NextToken(); token.Kind != TokenEOF {
		// 操作符序列消费完后必须到达 EOF。
		t.Fatalf("expected EOF after operators, got %#v", token)
	}
}

// TestLexerTokenGolden 验证一段 Lua 代码的 token 序列稳定输出。
//
// golden 覆盖关键字、标识符、十六进制浮点、比较、return、字符串和连接操作符。
func TestLexerTokenGolden(t *testing.T) {
	lexer := New("local x = 0x1.8p1\nif x >= 3 then return 'ok' .. \"!\" end")

	var tokenLines []string
	for {
		// 持续读取 token 直到 EOF，输出稳定的 kind:text@line:column 形式。
		token := lexer.NextToken()
		tokenLines = append(tokenLines, lexerGoldenLine(token))
		if token.Kind == TokenEOF {
			// EOF 已经纳入 golden，读取完成后退出循环。
			break
		}
		if token.Kind == TokenIllegal {
			// golden 用例中不允许非法 token。
			t.Fatalf("unexpected illegal token: %v", token.Err)
		}
	}

	got := strings.Join(tokenLines, "\n")
	goldenPath := filepath.Join("..", "..", "tests", "golden", "lexer_tokens.golden")
	expectedBytes, err := os.ReadFile(goldenPath)
	if err != nil {
		// golden 文件缺失意味着测试资产不完整。
		t.Fatalf("read golden failed: %v", err)
	}
	expected := strings.TrimSpace(strings.ReplaceAll(string(expectedBytes), "\r\n", "\n"))
	if got != expected {
		// token 序列必须与 golden 完全一致。
		t.Fatalf("golden mismatch:\n got:\n%s\nwant:\n%s", got, expected)
	}
}

// lexerGoldenLine 返回 token golden 的单行稳定文本。
//
// 字符串 token 使用 Literal 内容，其他 token 使用 Text 内容。
func lexerGoldenLine(token Token) string {
	// 默认展示 token 的 Text 字段。
	tokenText := token.Text
	if token.Kind == TokenString {
		// 字符串 token 展示解码后的 Literal，便于验证转义和长字符串行为。
		tokenText = token.Literal
	}

	// 输出格式固定为 kind:text@line:column。
	return string(token.Kind) + ":" + tokenText + "@" + positionText(token.Position)
}

// positionText 返回 token 位置的稳定展示。
//
// 只输出行列，不输出字节偏移，避免 UTF-8 字节长度影响普通 golden 阅读。
func positionText(position Position) string {
	// 使用简短十进制行列格式。
	return intToString(position.Line) + ":" + intToString(position.Column)
}

// intToString 返回整数的十进制文本。
//
// 测试 helper 独立封装，避免 golden 输出处散落格式化细节。
func intToString(value int) string {
	// strconv.Itoa 返回稳定十进制文本。
	return strconv.Itoa(value)
}
