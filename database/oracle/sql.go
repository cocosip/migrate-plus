package oracle

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	objectFunction  = "FUNCTION"
	objectProcedure = "PROCEDURE"
)

func splitStatements(script string) ([]string, error) {
	var statements, words []string
	start, hasSQL := 0, false
	terminator := -1
	appendStatement := func(end int) {
		if hasSQL {
			statement := script[start:end]
			// WITH routines contain PL/SQL, but the enclosing statement is SQL.
			if len(words) > 0 && words[0] == "WITH" && terminator >= start {
				statement = script[start:terminator] + script[terminator+1:end]
			}
			statements = append(statements, strings.TrimSpace(statement))
		}
		words, hasSQL = nil, false
		terminator = -1
	}
	for i := 0; i < len(script); {
		if script[i] == '/' && slashLine(script, i) {
			appendStatement(i)
			i++
			start = i
			continue
		}
		if script[i] == ';' {
			if !procedural(words) {
				appendStatement(i)
				start = i + 1
			} else {
				terminator = i
			}
			i++
			continue
		}
		end, word, meaningful, err := sqlToken(script, i)
		if err != nil {
			return nil, err
		}
		hasSQL = hasSQL || meaningful
		if meaningful {
			terminator = -1
		}
		if word != "" && len(words) < 8 {
			words = append(words, word)
		}
		i = end
	}
	appendStatement(len(script))
	return statements, nil
}

func slashLine(script string, i int) bool {
	start := strings.LastIndexByte(script[:i], '\n') + 1
	end := strings.IndexByte(script[i:], '\n')
	if end < 0 {
		end = len(script) - i
	}
	return strings.TrimSpace(script[start:i+end]) == "/"
}

func procedural(words []string) bool {
	if len(words) == 0 {
		return false
	}
	if words[0] == "BEGIN" || words[0] == "DECLARE" {
		return true
	}
	if words[0] == "WITH" && len(words) > 1 {
		return words[1] == objectFunction || words[1] == objectProcedure
	}
	if words[0] != "CREATE" {
		return false
	}
	for _, word := range words[1:] {
		switch word {
		case "OR", "REPLACE", "EDITIONABLE", "NONEDITIONABLE":
			continue
		case objectProcedure, objectFunction, "PACKAGE", "TRIGGER", "TYPE":
			return true
		default:
			return false
		}
	}
	return false
}

// sqlToken skips complete literals/comments so their delimiters remain opaque.
func sqlToken(script string, i int) (end int, word string, meaningful bool, err error) {
	switch {
	case strings.ContainsRune(" \t\r\n\v\f", rune(script[i])):
		return i + 1, "", false, nil
	case strings.HasPrefix(script[i:], "--"):
		end = strings.IndexByte(script[i:], '\n')
		if end < 0 {
			return len(script), "", false, nil
		}
		return i + end + 1, "", false, nil
	case strings.HasPrefix(script[i:], "/*"):
		end = strings.Index(script[i+2:], "*/")
		if end < 0 {
			return 0, "", false, fmt.Errorf("oracle: unclosed comment at byte %d", i)
		}
		return i + end + 4, "", false, nil
	case strings.HasPrefix(script[i:], "<<"):
		end = strings.Index(script[i+2:], ">>")
		if end < 0 {
			return 0, "", false, fmt.Errorf("oracle: unclosed PL/SQL label at byte %d", i)
		}
		return i + end + 4, "", true, nil
	case script[i] == '\'' || script[i] == '"':
		end, err = quotedEnd(script, i)
		return end, "", true, err
	}
	if offset := alternativeQuoteOffset(script[i:]); offset > 0 {
		end, err = alternativeQuoteEnd(script, i+offset)
		return end, "", true, err
	}
	if asciiLetter(script[i]) {
		end = i + 1
		for end < len(script) && identifierByte(script[end]) {
			end++
		}
		return end, strings.ToUpper(script[i:end]), true, nil
	}
	return i + 1, "", true, nil
}

func quotedEnd(script string, i int) (int, error) {
	quote := script[i]
	for j := i + 1; j < len(script); j++ {
		if script[j] != quote {
			continue
		}
		if j+1 < len(script) && script[j+1] == quote {
			j++
			continue
		}
		return j + 1, nil
	}
	return 0, fmt.Errorf("oracle: unclosed quoted value at byte %d", i)
}

func alternativeQuoteOffset(script string) int {
	if len(script) > 2 && (script[0] == 'q' || script[0] == 'Q') && script[1] == '\'' {
		return 2
	}
	if len(script) > 3 && (script[0] == 'n' || script[0] == 'N') && (script[1] == 'q' || script[1] == 'Q') && script[2] == '\'' {
		return 3
	}
	return 0
}

func alternativeQuoteEnd(script string, delimiter int) (int, error) {
	closing, size := utf8.DecodeRuneInString(script[delimiter:])
	if index := strings.IndexRune("[{(<", closing); index >= 0 {
		closing = rune("]})>"[index])
	}
	terminator := string(closing) + "'"
	end := strings.Index(script[delimiter+size:], terminator)
	if end < 0 {
		return 0, fmt.Errorf("oracle: unclosed alternative quote at byte %d", delimiter)
	}
	return delimiter + size + end + len(terminator), nil
}

func asciiLetter(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func identifierByte(b byte) bool {
	return asciiLetter(b) || (b >= '0' && b <= '9') || b == '_' || b == '$' || b == '#'
}
