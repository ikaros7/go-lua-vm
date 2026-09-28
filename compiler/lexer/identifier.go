package lexer

import "unicode"

// ScanIdentifier 尝试读取当前位置的 Lua 标识符。
//
// Lua 5.3 itself commonly uses locale-dependent identifiers. This lexer also
// accepts Unicode letters so hosts such as LazyElf can use Chinese names.
func (lexer *Lexer) ScanIdentifier() (string, Position, bool) {
	startPosition := lexer.source.Position()
	input := lexer.source.input
	startOffset := lexer.source.offset
	if startOffset >= len(input) {
		// EOF 不能形成标识符。
		return "", startPosition, false
	}
	firstRune, ok := lexer.source.Peek()
	if !ok || !isIdentifierStart(firstRune) {
		// 非标识符起始字符不能被消费。
		return "", startPosition, false
	}

	for {
		nextRune, available := lexer.source.Peek()
		if !available || !isIdentifierPart(nextRune) {
			break
		}
		lexer.source.Next()
	}

	// 返回标识符文本和起始位置。
	return input[startOffset:lexer.source.offset], startPosition, true
}

// isIdentifierStartByte 判断 byte 是否可以作为 Lua 标识符首字符。
//
// 当前标识符语义限定 ASCII，调用方可用该函数避开 UTF-8 解码。
func isIdentifierStartByte(value byte) bool {
	switch {
	case value == '_':
		// 下划线可以作为标识符首字符。
		return true
	case value >= 'a' && value <= 'z':
		// 小写 ASCII 字母可以作为标识符首字符。
		return true
	case value >= 'A' && value <= 'Z':
		// 大写 ASCII 字母可以作为标识符首字符。
		return true
	default:
		// 其他 byte 不能作为当前阶段标识符首字符。
		return false
	}
}

// isIdentifierPartByte 判断 byte 是否可以作为 Lua 标识符非首字符。
//
// 标识符后续字符允许 ASCII 字母、数字和下划线。
func isIdentifierPartByte(value byte) bool {
	if isIdentifierStartByte(value) {
		// 首字符集合也全部允许出现在后续位置。
		return true
	}
	if value >= '0' && value <= '9' {
		// ASCII 数字可以出现在标识符非首位置。
		return true
	}

	// 其他 byte 不能作为标识符组成部分。
	return false
}

// isIdentifierStart 判断 rune 是否可以作为 Lua 标识符首字符。
//
// ASCII keeps the common fast test while Unicode letters provide the host
// extension used by existing Chinese automation scripts.
func isIdentifierStart(value rune) bool {
	if value >= 0 && value <= 0x7f {
		return isIdentifierStartByte(byte(value))
	}
	return unicode.IsLetter(value)
}

// isIdentifierPart 判断 rune 是否可以作为 Lua 标识符非首字符。
//
// 标识符后续字符允许字母、数字、下划线及 Unicode 组合标记。
func isIdentifierPart(value rune) bool {
	if value >= 0 && value <= 0x7f {
		return isIdentifierPartByte(byte(value))
	}
	return unicode.IsLetter(value) || unicode.IsDigit(value) ||
		unicode.In(value, unicode.Mn, unicode.Mc, unicode.Pc)
}
