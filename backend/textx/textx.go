// Package textx는 민원 글을 다룰 때 쓰는 공백·숫자 규칙을 둔다. 전각 공백과 전각 숫자도 공백과 숫자로 본다.
package textx

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// Space는 정규식 문자 클래스 안에 넣는 공백 문자들이다.
	Space = `\t\n\v\f\r \x{1c}-\x{1f}\x{85}\p{Z}`
	// Digit은 정규식의 숫자다.
	Digit = `\p{Nd}`
)

func isSpace(r rune) bool { return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f) }

// Squeeze는 모든 공백을 뺀다.
func Squeeze(text string) string {
	return strings.Map(func(r rune) rune {
		if isSpace(r) {
			return -1
		}
		return r
	}, text)
}

// Len은 글자 수다.
func Len(text string) int { return utf8.RuneCountInString(text) }

// Atoi는 숫자로만 된 글의 값이다. 전각 숫자도 읽는다.
func Atoi(text string) int {
	value := 0
	for _, r := range text {
		zero := r
		for unicode.Is(unicode.Nd, zero-1) {
			zero--
		}
		value = value*10 + int(r-zero)%10
	}
	return value
}
