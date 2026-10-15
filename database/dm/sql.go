package dm

import (
	"fmt"
	"strings"
)

// splitStatements frames DM SQL; server compatibility modes are not other dialects.
func splitStatements(script string, caps capabilities) ([]string, error) {
	var statements []string
	var words []string
	start, hasSQL, terminated, hasEnd := 0, false, false, false
	appendStatement := func(end int) error {
		if hasSQL {
			if err := validateEnvelope(words, terminated, hasEnd); err != nil {
				return err
			}
			statements = append(statements, strings.TrimSpace(script[start:end]))
		}
		words, hasSQL, terminated, hasEnd = nil, false, false, false
		return nil
	}
	for i := 0; i < len(script); {
		if script[i] == '/' && slashLine(script, i) {
			if err := appendStatement(i); err != nil {
				return nil, err
			}
			i++
			start = i
			continue
		}
		if script[i] == ';' {
			terminated = true
			if !procedural(words) {
				if err := appendStatement(i); err != nil {
					return nil, err
				}
				start = i + 1
			}
			i++
			continue
		}
		end, word, meaningful, err := sqlToken(script, i, caps.backslash)
		if err != nil {
			return nil, err
		}
		if meaningful {
			if !hasSQL && (script[i] == '\\' || script[i] == '@') {
				return nil, fmt.Errorf("dm: client commands are not migration SQL")
			}
			hasSQL, terminated = true, false
		}
		if word != "" {
			hasEnd = hasEnd || word == "END"
			if len(words) < 8 {
				words = append(words, word)
			}
		}
		i = end
	}
	if err := appendStatement(len(script)); err != nil {
		return nil, err
	}
	return statements, nil
}

func validateEnvelope(words []string, terminated, hasEnd bool) error {
	if len(words) == 0 {
		return nil
	}
	switch words[0] {
	case "DELIMITER", "SOURCE", "PROMPT", "SPOOL", "EXIT", "QUIT", "WHENEVER", "CONNECT", "ACCEPT", "DEFINE", "UNDEFINE", "VARIABLE", "PRINT", "COPY":
		return fmt.Errorf("dm: unsupported client command or COPY framing")
	case "SET":
		if len(words) > 1 && words[1] != "TRANSACTION" && words[1] != "CONSTRAINTS" {
			return fmt.Errorf("dm: SET directives and schema changes are not supported in migration scripts")
		}
	case "WITH":
		if len(words) > 1 && (words[1] == objectFunction || words[1] == objectProcedure) {
			return fmt.Errorf("dm: inline WITH routines are not supported")
		}
	case objectBegin:
		if len(words) > 1 && (words[1] == "TRANSACTION" || words[1] == "WORK") {
			return fmt.Errorf("dm: use native DM procedural blocks, not BEGIN transaction framing")
		}
	}
	if procedural(words) && (!terminated || ((words[0] == objectBegin || words[0] == "DECLARE") && !hasEnd)) {
		return fmt.Errorf("dm: incomplete procedural envelope; terminate the block with END; and a slash line")
	}
	return nil
}

func procedural(words []string) bool {
	if len(words) == 0 {
		return false
	}
	if words[0] == objectBegin || words[0] == "DECLARE" {
		return true
	}
	if words[0] != "CREATE" {
		return false
	}
	for _, word := range words[1:] {
		switch word {
		case "OR", "REPLACE":
			continue
		case objectProcedure, objectFunction, "PACKAGE", "TRIGGER", objectType, objectClass:
			return true
		default:
			return false
		}
	}
	return false
}

func slashLine(script string, i int) bool {
	start := strings.LastIndexByte(script[:i], '\n') + 1
	end := strings.IndexByte(script[i:], '\n')
	if end < 0 {
		end = len(script) - i
	}
	return strings.TrimSpace(script[start:i+end]) == "/"
}

func sqlToken(script string, i int, backslash bool) (end int, word string, meaningful bool, err error) {
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
		if end < 0 || strings.Contains(script[i+2:i+2+end], "/*") {
			return 0, "", false, fmt.Errorf("dm: unclosed or nested comment at byte %d", i)
		}
		return i + end + 4, "", false, nil
	case strings.HasPrefix(script[i:], "<<"):
		end = strings.Index(script[i+2:], ">>")
		if end < 0 {
			return 0, "", false, fmt.Errorf("dm: unclosed procedural label at byte %d", i)
		}
		return i + end + 4, "", true, nil
	case script[i] == '\'' || script[i] == '"':
		end, err = quotedEnd(script, i, backslash && script[i] == '\'')
		return end, "", true, err
	case script[i] == '`' || alternativeQuote(script[i:]) || dollarQuote(script[i:]):
		return 0, "", false, fmt.Errorf("dm: vendor-specific quoting is not supported at byte %d; use native DM quotes", i)
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

func quotedEnd(script string, i int, backslash bool) (int, error) {
	quote := script[i]
	for j := i + 1; j < len(script); j++ {
		if backslash && script[j] == '\\' {
			j++
			continue
		}
		if script[j] == quote {
			if j+1 < len(script) && script[j+1] == quote {
				j++
				continue
			}
			return j + 1, nil
		}
	}
	return 0, fmt.Errorf("dm: unclosed quoted value at byte %d", i)
}

func alternativeQuote(s string) bool {
	return len(s) > 1 && (s[0] == 'q' || s[0] == 'Q') && s[1] == '\'' ||
		len(s) > 2 && (s[0] == 'n' || s[0] == 'N') && (s[1] == 'q' || s[1] == 'Q') && s[2] == '\''
}

func dollarQuote(s string) bool {
	if s[0] != '$' {
		return false
	}
	for i := 1; i < len(s); i++ {
		if s[i] == '$' {
			return true
		}
		if !asciiLetter(s[i]) && s[i] != '_' && (s[i] < '0' || s[i] > '9') {
			break
		}
	}
	return false
}

func asciiLetter(b byte) bool {
	return b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z'
}

func identifierByte(b byte) bool {
	return asciiLetter(b) || b >= '0' && b <= '9' || strings.ContainsRune("_$#", rune(b))
}
