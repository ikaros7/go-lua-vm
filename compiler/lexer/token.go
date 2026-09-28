package lexer

import (
	"errors"
	"fmt"
	"strings"
)

// TokenKind 表示 Lua 5.3 词法 token 类型。
//
// Kind 只表达词法分类；语法阶段会结合 Text 和 Literal 字段解释具体语义。
type TokenKind string

const (
	// TokenEOF 表示源码已经读取到结尾。
	TokenEOF TokenKind = "eof"
	// TokenIllegal 表示遇到无法识别或无法合法解析的源码片段。
	TokenIllegal TokenKind = "illegal"
	// TokenIdentifier 表示普通标识符。
	TokenIdentifier TokenKind = "identifier"
	// TokenKeyword 表示 Lua 5.3 保留关键字。
	TokenKeyword TokenKind = "keyword"
	// TokenNumber 表示数字字面量。
	TokenNumber TokenKind = "number"
	// TokenString 表示字符串字面量。
	TokenString TokenKind = "string"
	// TokenOperator 表示操作符或分隔符。
	TokenOperator TokenKind = "operator"
)

// Token 表示 Lua 5.3 lexer 输出的一个 token。
//
// Text 保存源码原文或规范化文本；Literal 保存字符串字面量解码后的内容；Number 保存数字扫描结果；
// Err 保存非法 token 的错误原因。
type Token struct {
	// Kind 保存 token 词法类型。
	Kind TokenKind
	// Text 保存 token 文本。
	Text string
	// Literal 保存字符串字面量解码后的内容。
	Literal string
	// Number 保存数字字面量结果。
	Number NumberLiteral
	// Position 保存 token 起始位置。
	Position Position
	// Err 保存非法 token 的错误原因。
	Err error
}

// NextToken 扫描并返回下一个 Lua 5.3 token。
//
// 本方法会跳过空白和注释；扫描错误会以 TokenIllegal 返回，不会 panic。
func (lexer *Lexer) NextToken() Token {
	// 每次取 token 前先跳过 Lua 可忽略内容。
	lexer.SkipIgnored()
	startPosition := lexer.source.Position()
	if lexer.source.offset >= len(lexer.source.input) {
		// EOF 是合法 token，供 parser 判断输入结束。
		return Token{Kind: TokenEOF, Text: "<eof>", Position: startPosition}
	}
	firstByte := lexer.source.input[lexer.source.offset]
	if lexer.canStartStringToken(firstByte) {
		// 只有长括号或短字符串引号才可能进入字符串扫描，普通 identifier/number/operator 直接跳过试探。
		if token, ok := lexer.scanStringToken(startPosition); ok {
			// 字符串扫描成功或出错都已经形成完整 token。
			return token
		}
	}
	if lexer.canStartNumberToken(firstByte) {
		// 数字扫描成功或出错都已经形成完整 token。
		if token, ok := lexer.scanNumberToken(); ok {
			return token
		}
	}
	if isIdentifierStartByte(firstByte) || firstByte >= 0x80 && lexer.canStartUnicodeIdentifier() {
		// 标识符和关键字共用标识符扫描路径。
		if token, ok := lexer.scanIdentifierOrKeywordToken(); ok {
			return token
		}
	}
	if token, ok := lexer.scanOperatorToken(startPosition); ok {
		// 操作符按最长匹配优先返回。
		return token
	}

	// 当前字符无法识别，消费一个 rune 以避免调用方死循环。
	illegalRune, position, _ := lexer.source.Next()
	return Token{
		Kind:     TokenIllegal,
		Text:     string(illegalRune),
		Position: position,
		Err:      fmt.Errorf("illegal character %q", illegalRune),
	}
}

func (lexer *Lexer) canStartUnicodeIdentifier() bool {
	firstRune, ok := lexer.source.Peek()
	return ok && isIdentifierStart(firstRune)
}

// canStartStringToken 判断当前位置是否可能开启 Lua 字符串 token。
//
// 长字符串只能以 `[` 开始，短字符串只能以单引号或双引号开始；该 guard 只跳过不可能命中的试探，
// 真正的长括号层级、短字符串转义和错误语义仍由 scanStringToken 内部完成。
func (lexer *Lexer) canStartStringToken(firstByte byte) bool {
	switch firstByte {
	case '[', '\'', '"':
		// 三类 ASCII 起始字符覆盖 Lua 5.3 的长字符串和短字符串。
		return true
	default:
		// 其他字符不可能被字符串扫描消费。
		return false
	}
}

// canStartNumberToken 判断当前位置是否可能开启 Lua 数字 token。
//
// Lua 5.3 数字可以从 ASCII 数字开始，也可以是 `.` 后接数字的十进制浮点；单独 `.`、`..` 和 `...`
// 必须继续交给 operator 扫描，不能被数字路径吞掉。
func (lexer *Lexer) canStartNumberToken(firstByte byte) bool {
	if firstByte >= '0' && firstByte <= '9' {
		// 普通十进制和 0x/0X 十六进制数字都以 ASCII 数字开头。
		return true
	}
	if firstByte != '.' {
		// 其他字符不能开启数字 token。
		return false
	}
	nextOffset := lexer.source.offset + 1
	if nextOffset >= len(lexer.source.input) {
		// 单独点号必须保留给 operator。
		return false
	}
	nextByte := lexer.source.input[nextOffset]
	if nextByte >= '0' && nextByte <= '9' {
		// `.5` 这类前导点十进制浮点必须保留数字语义。
		return true
	}

	// 点号后不是数字时交给最长 operator 匹配。
	return false
}

// scanStringToken 尝试扫描字符串 token。
//
// startPosition 是 token 起始位置；返回 ok=false 表示当前位置不是字符串。
func (lexer *Lexer) scanStringToken(startPosition Position) (Token, bool) {
	longText, longPosition, ok, err := lexer.ScanLongString()
	if ok {
		if err != nil {
			// 长字符串开始后出现错误时，返回非法 token 保留错误原因。
			return Token{Kind: TokenIllegal, Text: lexer.illegalStringTokenText(longPosition, err), Position: longPosition, Err: err}, true
		}
		return Token{Kind: TokenString, Text: longText, Literal: longText, Position: longPosition}, true
	}

	shortText, shortPosition, ok, err := lexer.ScanShortString()
	if ok {
		if err != nil {
			// 短字符串开始后出现错误时，返回非法 token 保留错误原因。
			return Token{Kind: TokenIllegal, Text: lexer.illegalStringTokenText(shortPosition, err), Position: shortPosition, Err: err}, true
		}
		return Token{Kind: TokenString, Text: shortText, Literal: shortText, Position: shortPosition}, true
	}

	// 当前不是任何字符串起始字符。
	return Token{Position: startPosition}, false
}

// illegalStringTokenText 返回非法字符串 token 的 Lua 风格 near 文本。
//
// 未终止字符串在 Lua 5.3 中报告 near <eof>；其他 escape 错误需要保留源码片段，供 parser
// 组装 `near '...'` 错误消息并让官方 literals.lua 能匹配具体非法序列。
func (lexer *Lexer) illegalStringTokenText(startPosition Position, err error) string {
	if errors.Is(err, ErrUnterminatedLongString) || errors.Is(err, ErrUnterminatedShortString) {
		// 未终止字符串统一按 EOF 报告，避免把整段残缺源码塞进错误消息。
		return "<eof>"
	}
	endOffset := lexer.source.Position().Offset
	if endOffset < startPosition.Offset {
		// 防御异常回退状态，避免切片越界。
		return ""
	}
	if endOffset > len(lexer.source.input) {
		// 防御非法 offset，保证错误路径也不会 panic。
		endOffset = len(lexer.source.input)
	}
	if errors.Is(err, ErrInvalidEscape) && strings.Contains(err.Error(), "decimal") && endOffset < len(lexer.source.input) {
		// Lua scanner 对十进制转义越界会展示到闭合引号；其他非法 escape 不预览后续字符。
		startQuote := lexer.source.input[startPosition.Offset]
		if lexer.source.input[endOffset] == startQuote {
			// 只预览同类闭合引号，不消费输入状态，后续 parser 仍会按非法 token 停止。
			endOffset++
		}
	}
	if errors.Is(err, ErrInvalidEscape) && strings.Contains(err.Error(), "unicode") && endOffset > startPosition.Offset {
		if lexer.source.input[endOffset-1] == '}' {
			// Unicode code point 过大时官方 near 片段停在右大括号之前。
			endOffset--
		}
	}

	// 返回从字符串起始分隔符到失败位置的原始源码片段。
	return lexer.source.input[startPosition.Offset:endOffset]
}

// scanNumberToken 尝试扫描数字 token。
//
// 返回 ok=false 表示当前位置不是数字起始字符。
func (lexer *Lexer) scanNumberToken() (Token, bool) {
	numberLiteral, ok, err := lexer.ScanNumber()
	if !ok {
		// 当前不是数字起始字符。
		return Token{}, false
	}
	if err != nil {
		// 数字起始后解析失败，返回非法 token。
		return Token{Kind: TokenIllegal, Text: numberLiteral.Text, Number: numberLiteral, Position: numberLiteral.Position, Err: err}, true
	}

	// 数字扫描成功，Number 字段保存解析结果。
	return Token{Kind: TokenNumber, Text: numberLiteral.Text, Number: numberLiteral, Position: numberLiteral.Position}, true
}

// scanIdentifierOrKeywordToken 尝试扫描标识符或关键字 token。
//
// 返回 ok=false 表示当前位置不是标识符起始字符。
func (lexer *Lexer) scanIdentifierOrKeywordToken() (Token, bool) {
	identifierText, position, ok := lexer.ScanIdentifier()
	if !ok {
		// 当前不是标识符起始字符。
		return Token{}, false
	}
	if isLuaKeyword(identifierText) {
		// Lua 保留字在词法层标记为 keyword。
		return Token{Kind: TokenKeyword, Text: identifierText, Position: position}, true
	}

	// 非关键字标记为普通 identifier。
	return Token{Kind: TokenIdentifier, Text: identifierText, Position: position}, true
}

// scanOperatorToken 尝试扫描 Lua 5.3 操作符或分隔符 token。
//
// startPosition 是操作符起始位置；返回 ok=false 表示当前位置没有合法操作符。
func (lexer *Lexer) scanOperatorToken(startPosition Position) (Token, bool) {
	operatorText, ok := lexer.scanOperatorText()
	if !ok {
		// 没有匹配到任何 Lua 5.3 操作符。
		return Token{Position: startPosition}, false
	}

	// 操作符按最长匹配结果返回，Position 使用调用方已保存的起始位置。
	return Token{Kind: TokenOperator, Text: operatorText, Position: startPosition}, true
}

// scanOperatorText 按首字节扫描 Lua 5.3 操作符文本。
//
// 所有 Lua 5.3 操作符都是 ASCII 且不跨行；这里直接按源码字节做最长匹配，避免每个操作符 token 都遍历
// 完整操作符列表。消费仍通过 Source.Next 维护行列和 Offset 语义。
func (lexer *Lexer) scanOperatorText() (string, bool) {
	offset := lexer.source.offset
	if offset >= len(lexer.source.input) {
		// EOF 不能形成操作符。
		return "", false
	}

	switch lexer.source.input[offset] {
	case '.':
		if lexer.hasOperatorPrefix(offset, "...") {
			// vararg 操作符必须优先于连接和点号。
			return lexer.consumeOperator("..."), true
		}
		if lexer.hasOperatorPrefix(offset, "..") {
			// 连接操作符优先于单点。
			return lexer.consumeOperator(".."), true
		}
		return lexer.consumeOperator("."), true
	case '/':
		if lexer.hasOperatorPrefix(offset, "//") {
			// 整除操作符优先于除法。
			return lexer.consumeOperator("//"), true
		}
		return lexer.consumeOperator("/"), true
	case '<':
		if lexer.hasOperatorPrefix(offset, "<<") {
			// 左移操作符优先于小于号。
			return lexer.consumeOperator("<<"), true
		}
		if lexer.hasOperatorPrefix(offset, "<=") {
			// 小于等于优先于小于号。
			return lexer.consumeOperator("<="), true
		}
		return lexer.consumeOperator("<"), true
	case '>':
		if lexer.hasOperatorPrefix(offset, ">>") {
			// 右移操作符优先于大于号。
			return lexer.consumeOperator(">>"), true
		}
		if lexer.hasOperatorPrefix(offset, ">=") {
			// 大于等于优先于大于号。
			return lexer.consumeOperator(">="), true
		}
		return lexer.consumeOperator(">"), true
	case '=':
		if lexer.hasOperatorPrefix(offset, "==") {
			// 相等比较优先于赋值等号。
			return lexer.consumeOperator("=="), true
		}
		return lexer.consumeOperator("="), true
	case '~':
		if lexer.hasOperatorPrefix(offset, "~=") {
			// 不等比较优先于按位非。
			return lexer.consumeOperator("~="), true
		}
		return lexer.consumeOperator("~"), true
	case ':':
		if lexer.hasOperatorPrefix(offset, "::") {
			// label 分隔符优先于单冒号。
			return lexer.consumeOperator("::"), true
		}
		return lexer.consumeOperator(":"), true
	case '+', '-', '*', '%', '^', '#', '&', '|', '(', ')', '{', '}', '[', ']', ';', ',':
		// 这些都是单字节操作符或分隔符。
		return lexer.consumeOperator(lexer.source.input[offset : offset+1]), true
	default:
		// 非 ASCII 操作符起始字符交给非法 token 路径消费。
		return "", false
	}
}

// hasOperatorPrefix 判断当前 offset 是否具有指定 ASCII 操作符前缀。
//
// 调用方保证 operatorText 是 Lua 5.3 ASCII 操作符；本 helper 不修改 Source 状态。
func (lexer *Lexer) hasOperatorPrefix(offset int, operatorText string) bool {
	// 使用 HasPrefix 直接比较源码字节，避免多次 UTF-8 解码。
	return strings.HasPrefix(lexer.source.input[offset:], operatorText)
}

// consumeOperator 消费已经匹配成功的 ASCII 操作符文本。
//
// 操作符不包含换行，但仍通过 Source.Next 推进，确保列号、Offset 和后续 Mark/Reset 语义一致。
func (lexer *Lexer) consumeOperator(operatorText string) string {
	for consumed := 0; consumed < len(operatorText); consumed++ {
		// 已经按源码字节确认操作符存在，Next 必须成功；失败时也不额外构造错误路径。
		lexer.source.Next()
	}
	return operatorText
}

// isLuaKeyword 判断文本是否是 Lua 5.3 保留关键字。
//
// 关键字表对齐 Lua 5.3 手册的 reserved words。
func isLuaKeyword(text string) bool {
	switch text {
	case "and", "break", "do", "else", "elseif", "end", "false", "for", "function", "goto", "if", "in", "local", "nil", "not", "or", "repeat", "return", "then", "true", "until", "while":
		// 命中保留字表时按 keyword token 输出。
		return true
	default:
		// 其他标识符文本不是关键字。
		return false
	}
}
