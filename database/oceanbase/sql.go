package oceanbase

import (
	"fmt"
	"strings"
	"unicode/utf8"

	ob "github.com/cocosip/go-oceanbase"
)

const objectFunction = "FUNCTION"
const objectProcedure = "PROCEDURE"

// splitStatements preserves MySQL batches; Oracle blocks use standalone slash lines.
func splitStatements(script string, mode ob.Mode, sqlMode string) ([]string, error) {
	var statements, words, guard []string
	start, hasSQL, blockTerminator := 0, false, false
	appendStatement := func(end int) error {
		if hasSQL {
			if procedural(words) && !blockTerminator {
				return fmt.Errorf("oceanbase: terminate Oracle procedural blocks with a semicolon and a slash line")
			}
			statements = append(statements, strings.TrimSpace(script[start:end]))
		}
		words, hasSQL, blockTerminator = nil, false, false
		return nil
	}
	backslash := mode == ob.ModeMySQL && !hasSQLMode(sqlMode, "NO_BACKSLASH_ESCAPES")
	for i := 0; i < len(script); {
		if mode == ob.ModeOracle && script[i] == '/' && slashLine(script, i) {
			if err := validateCommand(guard, mode); err != nil {
				return nil, err
			}
			if err := appendStatement(i); err != nil {
				return nil, err
			}
			guard = nil
			i++
			start = i
			continue
		}
		if script[i] == ';' {
			if err := validateCommand(guard, mode); err != nil {
				return nil, err
			}
			guard = nil
			if mode == ob.ModeOracle {
				blockTerminator = true
				if !procedural(words) {
					if err := appendStatement(i); err != nil {
						return nil, err
					}
					start = i + 1
				}
			}
			i++
			continue
		}
		end, word, meaningful, err := sqlToken(script, i, mode, backslash, hasSQLMode(sqlMode, "ANSI_QUOTES"))
		if err != nil {
			return nil, err
		}
		if meaningful {
			if !hasSQL && (script[i] == '\\' || script[i] == '@') {
				return nil, fmt.Errorf("oceanbase: client commands are not migration SQL")
			}
			hasSQL = true
			blockTerminator = false
		}
		if word != "" {
			if len(words) < 8 {
				words = append(words, word)
			}
			if len(guard) < 8 {
				guard = append(guard, word)
			}
		}
		i = end
	}
	if err := validateCommand(guard, mode); err != nil {
		return nil, err
	}
	if mode == ob.ModeMySQL {
		if hasSQL {
			return []string{script}, nil
		}
		return nil, nil
	}
	if err := appendStatement(len(script)); err != nil {
		return nil, err
	}
	return statements, nil
}

func validateCommand(words []string, mode ob.Mode) error {
	if len(words) == 0 {
		return nil
	}
	switch words[0] {
	case "USE", "SET", "CONNECT", "DELIMITER", "SOURCE", "PROMPT", "SPOOL", "EXIT", "QUIT", "WHENEVER", "ACCEPT", "DEFINE", "UNDEFINE", "VARIABLE", "PRINT", "COPY", "COMMIT", "ROLLBACK", "SAVEPOINT", "START", "XA", "LOCK", "UNLOCK":
		return fmt.Errorf("oceanbase: session, transaction and client commands are not supported in migration scripts")
	case "ALTER":
		if len(words) > 1 && (words[1] == "SESSION" || words[1] == "SYSTEM") {
			return fmt.Errorf("oceanbase: migration scripts cannot alter the session or system")
		}
	case "WITH":
		if mode == ob.ModeOracle && len(words) > 1 && (words[1] == objectFunction || words[1] == objectProcedure) {
			return fmt.Errorf("oceanbase: inline WITH routines are not supported")
		}
	case "BEGIN":
		if mode == ob.ModeMySQL || len(words) > 1 && (words[1] == "TRANSACTION" || words[1] == "WORK") {
			return fmt.Errorf("oceanbase: transaction control is not supported in migration scripts")
		}
	}
	return nil
}

func hasSQLMode(modes, mode string) bool {
	for _, value := range strings.Split(modes, ",") {
		if strings.EqualFold(strings.TrimSpace(value), mode) {
			return true
		}
	}
	return false
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
		case objectFunction, objectProcedure, "PACKAGE", "TRIGGER", "TYPE":
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

func sqlToken(script string, i int, mode ob.Mode, backslash, ansiQuotes bool) (end int, word string, meaningful bool, err error) {
	switch {
	case strings.ContainsRune(" \t\r\n\v\f", rune(script[i])):
		return i + 1, "", false, nil
	case strings.HasPrefix(script[i:], "--") && (mode == ob.ModeOracle || i+2 == len(script) || script[i+2] <= ' '), mode == ob.ModeMySQL && script[i] == '#':
		end = strings.IndexByte(script[i:], '\n')
		if end < 0 {
			return len(script), "", false, nil
		}
		return i + end + 1, "", false, nil
	case strings.HasPrefix(script[i:], "/*"):
		if strings.HasPrefix(script[i:], "/*!") || strings.HasPrefix(script[i:], "/*M!") || strings.HasPrefix(script[i:], "/*m!") {
			return 0, "", false, fmt.Errorf("oceanbase: executable comments are not supported")
		}
		end = strings.Index(script[i+2:], "*/")
		if end < 0 || strings.Contains(script[i+2:i+2+end], "/*") {
			return 0, "", false, fmt.Errorf("oceanbase: unclosed or nested SQL comment")
		}
		return i + end + 4, "", false, nil
	}
	if mode == ob.ModeOracle {
		if offset := alternativeQuoteOffset(script[i:]); offset > 0 {
			end, err = alternativeQuoteEnd(script, i+offset)
			return end, "", true, err
		}
		if strings.HasPrefix(script[i:], "<<") {
			end = strings.Index(script[i+2:], ">>")
			if end < 0 {
				return 0, "", false, fmt.Errorf("oceanbase: unclosed PL/SQL label")
			}
			return i + end + 4, "", true, nil
		}
	}
	if script[i] == '\'' || script[i] == '"' || mode == ob.ModeMySQL && script[i] == '`' {
		end, err = quotedEnd(script, i, backslash && script[i] != '`' && (!ansiQuotes || script[i] != '"'))
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

func quotedEnd(script string, i int, backslash bool) (int, error) {
	quote := script[i]
	for j := i + 1; j < len(script); j++ {
		if backslash && script[j] == '\\' {
			j++
			continue
		}
		if script[j] != quote {
			continue
		}
		if j+1 < len(script) && script[j+1] == quote {
			j++
			continue
		}
		return j + 1, nil
	}
	return 0, fmt.Errorf("oceanbase: unclosed quoted value at byte %d", i)
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
		return 0, fmt.Errorf("oceanbase: unclosed alternative quote")
	}
	return delimiter + size + end + len(terminator), nil
}

func asciiLetter(b byte) bool { return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' }
func identifierByte(b byte) bool {
	return asciiLetter(b) || b >= '0' && b <= '9' || b == '_' || b == '$' || b == '#'
}
