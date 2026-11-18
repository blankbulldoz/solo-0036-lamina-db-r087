package syntax

import (
	"fmt"
	"strings"
	"unicode"
)

type token struct {
	text     string
	literal  bool
	position int
}

func lex(sql string) ([]token, error) {
	if len(sql) > 65536 {
		return nil, fmt.Errorf("SQL exceeds 64KiB")
	}
	out := []token{}
	for i := 0; i < len(sql); {
		if unicode.IsSpace(rune(sql[i])) {
			i++
			continue
		}
		start := i
		c := sql[i]
		if c == '\'' {
			i++
			var value strings.Builder
			closed := false
			for i < len(sql) {
				if sql[i] == '\'' {
					if i+1 < len(sql) && sql[i+1] == '\'' {
						value.WriteByte('\'')
						i += 2
						continue
					}
					i++
					closed = true
					break
				}
				value.WriteByte(sql[i])
				i++
			}
			if !closed {
				return nil, fmt.Errorf("unterminated string at byte %d", start)
			}
			out = append(out, token{value.String(), true, start})
			continue
		}
		if strings.ContainsRune(",()*;", rune(c)) {
			out = append(out, token{string(c), false, start})
			i++
			continue
		}
		if strings.ContainsRune("<>=!", rune(c)) {
			i++
			if i < len(sql) && sql[i] == '=' {
				i++
			}
			if c == '<' && i < len(sql) && sql[i] == '>' {
				i++
			}
			out = append(out, token{sql[start:i], false, start})
			continue
		}
		if c == '-' || c == '+' || c >= '0' && c <= '9' {
			i++
			digits := 0
			if c >= '0' && c <= '9' {
				digits++
			}
			for i < len(sql) && sql[i] >= '0' && sql[i] <= '9' {
				i++
				digits++
			}
			if i < len(sql) && sql[i] == '.' {
				if digits == 0 {
					return nil, fmt.Errorf("decimal point requires digits at byte %d", start)
				}
				i++
				frac := 0
				for i < len(sql) && sql[i] >= '0' && sql[i] <= '9' {
					i++
					frac++
				}
				if frac == 0 {
					return nil, fmt.Errorf("decimal point requires a following digit at byte %d", start)
				}
			}
			if digits == 0 {
				return nil, fmt.Errorf("sign requires a digit at byte %d", start)
			}
			out = append(out, token{sql[start:i], false, start})
			continue
		}
		if c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' {
			i++
			for i < len(sql) {
				x := sql[i]
				if !(x == '_' || x >= 'a' && x <= 'z' || x >= 'A' && x <= 'Z' || x >= '0' && x <= '9') {
					break
				}
				i++
			}
			out = append(out, token{strings.ToLower(sql[start:i]), false, start})
			continue
		}
		return nil, fmt.Errorf("unsupported character at byte %d", i)
	}
	if len(out) > 4096 {
		return nil, fmt.Errorf("too many SQL tokens")
	}
	return out, nil
}
