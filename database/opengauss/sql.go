package opengauss

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// validateScript checks protocol boundaries; openGauss still parses and executes the unchanged file.
func validateScript(script string, caps capabilities) error {
	if !utf8.ValidString(script) {
		return fmt.Errorf("opengauss: SQL must be valid UTF8")
	}
	if strings.IndexByte(script, 0) >= 0 {
		return fmt.Errorf("opengauss: SQL contains a NUL byte")
	}
	var words []string
	for i := 0; i < len(script); {
		end, word, err := scriptToken(script, i, caps)
		if err != nil {
			return err
		}
		i = end
		if word == ";" {
			words = nil
			continue
		}
		if word == "" {
			continue
		}
		if len(words) == 0 {
			switch word {
			case "COPY", "DELIMITER", "SOURCE", "SPOOL", "PROMPT", "DECLARE", "/", "\\":
				return fmt.Errorf("opengauss: COPY, client commands and unquoted procedural blocks are unsupported")
			}
		} else if words[0] == "BEGIN" && len(words) == 1 && word != "WORK" && word != "TRANSACTION" && word != "ISOLATION" && word != "READ" {
			return fmt.Errorf("opengauss: use a quoted native body for procedural blocks")
		}
		if word == "\\" {
			return fmt.Errorf("opengauss: client backslash commands are unsupported")
		}
		if wordsContainRoutine(words) && (word == "BEGIN" || word == "DECLARE") {
			return fmt.Errorf("opengauss: routine bodies must be quoted or dollar-quoted")
		}
		if len(words) < 4 {
			words = append(words, word)
		}
	}
	return nil
}

func wordsContainRoutine(words []string) bool {
	if len(words) == 0 || words[0] != "CREATE" {
		return false
	}
	for _, word := range words[:min(len(words), 4)] {
		switch word {
		case "FUNCTION", "PROCEDURE", "PACKAGE", "TRIGGER":
			return true
		}
	}
	return false
}

func scriptToken(s string, i int, caps capabilities) (int, string, error) {
	switch {
	case strings.ContainsRune(" \t\r\n\f\v", rune(s[i])):
		return i + 1, "", nil
	case strings.HasPrefix(s[i:], "--"):
		if end := strings.IndexAny(s[i:], "\r\n"); end >= 0 {
			return i + end + 1, "", nil
		}
		return len(s), "", nil
	case strings.HasPrefix(s[i:], "/*"):
		return commentEnd(s, i)
	case caps.mode == "A" && (s[i] == 'q' || s[i] == 'Q') && i+2 < len(s) && s[i+1] == '\'':
		return alternativeQuoteEnd(s, i)
	case s[i] == '\'' || s[i] == '"':
		escaped := s[i] == '\'' && (!caps.standardStrings || (i > 0 && (s[i-1] == 'e' || s[i-1] == 'E') && (i < 2 || !identifierByte(s[i-2]))))
		return quoteEnd(s, i, escaped)
	case s[i] == '$':
		if end, ok := dollarQuoteEnd(s, i); ok {
			return end, "", nil
		}
		return 0, "", fmt.Errorf("opengauss: invalid or unterminated dollar quote")
	case s[i] == '`':
		return 0, "", fmt.Errorf("opengauss: backtick quoting is unsupported; use native quoted identifiers")
	case identifierByte(s[i]):
		end := i + 1
		for end < len(s) && identifierByte(s[end]) {
			end++
		}
		return end, strings.ToUpper(s[i:end]), nil
	default:
		return i + 1, s[i : i+1], nil
	}
}

func commentEnd(s string, start int) (int, string, error) {
	depth := 1
	for i := start + 2; i < len(s); i++ {
		if strings.HasPrefix(s[i:], "/*") {
			depth++
			i++
		} else if strings.HasPrefix(s[i:], "*/") {
			depth--
			i++
			if depth == 0 {
				return i + 1, "", nil
			}
		}
	}
	return 0, "", fmt.Errorf("opengauss: unterminated SQL comment")
}

func quoteEnd(s string, start int, escaped bool) (int, string, error) {
	for i := start + 1; i < len(s); i++ {
		if escaped && s[i] == '\\' {
			i++
			continue
		}
		if s[i] == s[start] {
			if i+1 < len(s) && s[i+1] == s[start] {
				i++
				continue
			}
			if escaped {
				if next := continuedQuote(s, i+1); next >= 0 {
					i = next
					continue
				}
			}
			return i + 1, "", nil
		}
	}
	return 0, "", fmt.Errorf("opengauss: unterminated SQL quote")
}

func continuedQuote(s string, start int) int {
	newline := false
	for i := start; i < len(s); {
		switch {
		case strings.ContainsRune(" \t\r\n\f\v", rune(s[i])):
			newline = newline || s[i] == '\r' || s[i] == '\n'
			i++
		case strings.HasPrefix(s[i:], "--"):
			end := strings.IndexAny(s[i:], "\r\n")
			if end < 0 {
				return -1
			}
			i += end
		default:
			if newline && s[i] == '\'' {
				return i
			}
			return -1
		}
	}
	return -1
}

func alternativeQuoteEnd(s string, start int) (int, string, error) {
	delimiter := s[start+2]
	switch delimiter {
	case '[':
		delimiter = ']'
	case '(':
		delimiter = ')'
	case '{':
		delimiter = '}'
	case '<':
		delimiter = '>'
	}
	if end := strings.Index(s[start+3:], string([]byte{delimiter, '\''})); end >= 0 {
		return start + 3 + end + 2, "", nil
	}
	return 0, "", fmt.Errorf("opengauss: unterminated alternative quote")
}

func dollarQuoteEnd(s string, start int) (int, bool) {
	end := start + 1
	for end < len(s) && (s[end] == '_' || s[end] >= 'a' && s[end] <= 'z' || s[end] >= 'A' && s[end] <= 'Z' || end > start+1 && s[end] >= '0' && s[end] <= '9') {
		end++
	}
	if end >= len(s) || s[end] != '$' {
		return 0, false
	}
	tag := s[start : end+1]
	if closeAt := strings.Index(s[end+1:], tag); closeAt >= 0 {
		return end + 1 + closeAt + len(tag), true
	}
	return 0, false
}

func identifierByte(b byte) bool {
	return b == '_' || b == '$' || b >= 128 || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9'
}
