package oceanbase

import (
	"fmt"
	"strings"
	"unicode/utf8"

	ob "github.com/cocosip/go-oceanbase"
)

const objectFunction = "FUNCTION"
const objectProcedure = "PROCEDURE"
const objectTrigger = "TRIGGER"
const keywordBegin = "BEGIN"
const keywordCommit = "COMMIT"
const keywordRollback = "ROLLBACK"

// splitStatements preserves MySQL batches; Oracle blocks use standalone slash lines.
// The command denylist applies to top-level statements. Inside procedural bodies only
// transaction control and system-level SET are rejected, so loops and EXIT remain valid.
func splitStatements(script string, mode ob.Mode, sqlMode string) ([]string, error) {
	sp := &splitter{script: script, mode: mode, seg: newSegment()}
	backslash := mode == ob.ModeMySQL && !hasSQLMode(sqlMode, "NO_BACKSLASH_ESCAPES")
	ansiQuotes := hasSQLMode(sqlMode, "ANSI_QUOTES")
	for i := 0; i < len(script); {
		switch {
		case mode == ob.ModeOracle && script[i] == '/' && slashLine(script, i):
			if err := validateCommand(sp.words, mode); err != nil {
				return nil, err
			}
			if err := sp.appendStatement(i); err != nil {
				return nil, err
			}
			i++
			sp.start = i
		case script[i] == ';':
			if err := sp.semicolon(i); err != nil {
				return nil, err
			}
			i++
		default:
			end, err := sp.token(i, backslash, ansiQuotes)
			if err != nil {
				return nil, err
			}
			i = end
		}
	}
	if err := validateCommand(sp.words, mode); err != nil {
		return nil, err
	}
	if mode == ob.ModeMySQL {
		if sp.sawSQL {
			return []string{script}, nil
		}
		return nil, nil
	}
	if err := sp.appendStatement(len(script)); err != nil {
		return nil, err
	}
	return sp.statements, nil
}

type splitter struct {
	script                          string
	mode                            ob.Mode
	statements, words               []string
	seg                             segment
	start                           int
	hasSQL, sawSQL, blockTerminator bool
}

func (sp *splitter) appendStatement(end int) error {
	if sp.hasSQL {
		if procedural(sp.words, sp.mode) && !sp.blockTerminator {
			return fmt.Errorf("oceanbase: terminate Oracle procedural blocks with a semicolon and a slash line")
		}
		sp.statements = append(sp.statements, strings.TrimSpace(sp.script[sp.start:end]))
	}
	sp.words, sp.hasSQL, sp.blockTerminator, sp.seg = nil, false, false, newSegment()
	return nil
}

// semicolon ends a top-level statement or only a statement inside a procedural body.
func (sp *splitter) semicolon(i int) error {
	oracle := sp.mode == ob.ModeOracle
	inner := procedural(sp.words, sp.mode) && (oracle || sp.seg.depth > 0)
	if oracle {
		sp.blockTerminator = true
	}
	if inner {
		sp.seg.next()
		return nil
	}
	if err := validateCommand(sp.words, sp.mode); err != nil {
		return err
	}
	if !oracle {
		sp.words, sp.seg = nil, newSegment()
		return nil
	}
	err := sp.appendStatement(i)
	sp.start = i + 1
	return err
}

func (sp *splitter) token(i int, backslash, ansiQuotes bool) (int, error) {
	if sp.seg.set > 0 && strings.HasPrefix(sp.script[i:], "@@") {
		return 0, errSystemSet
	}
	end, word, meaningful, err := sqlToken(sp.script, i, sp.mode, backslash, ansiQuotes)
	if err != nil {
		return 0, err
	}
	if meaningful {
		if !sp.hasSQL && (sp.script[i] == '\\' || sp.script[i] == '@') {
			return 0, fmt.Errorf("oceanbase: client commands are not migration SQL")
		}
		sp.hasSQL, sp.sawSQL, sp.blockTerminator = true, true, false
	}
	if word != "" {
		if len(sp.words) < 8 {
			sp.words = append(sp.words, word)
		}
		if err := sp.seg.word(word, nextWord(sp.script, end), procedural(sp.words, sp.mode), sp.mode); err != nil {
			return 0, err
		}
	}
	return end, nil
}

var errTransactionControl = fmt.Errorf("oceanbase: session, transaction and client commands are not supported in migration scripts")
var errSystemSet = fmt.Errorf("oceanbase: migration scripts cannot change session or system variables")

// segment tracks statement boundaries inside procedural bodies.
type segment struct {
	start    bool // next word begins a statement inside a procedural body
	set      int  // words left to inspect after a MySQL SET in a body
	depth    int  // MySQL BEGIN/CASE nesting; a semicolon at depth zero ends the statement
	skipCase bool // the CASE after END CASE closes the construct already counted
}

func newSegment() segment { return segment{start: true} }

func (s *segment) next() { s.start, s.set = true, 0 }

func (s *segment) word(word, following string, proc bool, mode ob.Mode) error {
	if proc && s.start {
		switch word {
		case keywordCommit, keywordRollback, "SAVEPOINT":
			return errTransactionControl
		case "START", "XA":
			if mode == ob.ModeMySQL {
				return errTransactionControl
			}
		case "SET":
			if mode == ob.ModeMySQL {
				s.set = 3
				s.start = false
				return nil
			}
		}
	} else if s.set > 0 {
		s.set--
		switch word {
		case "SESSION", "GLOBAL", "LOCAL", "PERSIST", "PERSIST_ONLY", "NAMES", "CHARACTER", "CHARSET", "TRANSACTION", "AUTOCOMMIT", "SQL_MODE":
			return errSystemSet
		}
	}
	if proc && mode == ob.ModeMySQL {
		switch word {
		case keywordBegin:
			s.depth++
		case "CASE":
			if s.skipCase {
				s.skipCase = false
			} else {
				s.depth++
			}
		case "END":
			switch following {
			case "IF", "LOOP", "WHILE", "REPEAT":
			case "CASE":
				s.skipCase = true
				fallthrough
			default:
				s.depth = max(s.depth-1, 0)
			}
		}
	}
	s.start = proc && s.set == 0 && (word == keywordBegin || word == "THEN" || word == "ELSE" || word == "LOOP" || word == "REPEAT" || word == "DO")
	return nil
}

// nextWord returns the upper-case identifier following position i, or an empty string.
func nextWord(script string, i int) string {
	for i < len(script) && strings.ContainsRune(" \t\r\n\v\f", rune(script[i])) {
		i++
	}
	end := i
	for end < len(script) && asciiLetter(script[end]) {
		end++
	}
	return strings.ToUpper(script[i:end])
}

func validateCommand(words []string, mode ob.Mode) error {
	if len(words) == 0 {
		return nil
	}
	switch words[0] {
	case "USE", "SET", "CONNECT", "DELIMITER", "SOURCE", "PROMPT", "SPOOL", "EXIT", "QUIT", "WHENEVER", "ACCEPT", "DEFINE", "UNDEFINE", "VARIABLE", "PRINT", "COPY", keywordCommit, keywordRollback, "SAVEPOINT", "START", "XA", "LOCK", "UNLOCK":
		return errTransactionControl
	case "ALTER":
		if len(words) > 1 && (words[1] == "SESSION" || words[1] == "SYSTEM") {
			return fmt.Errorf("oceanbase: migration scripts cannot alter the session or system")
		}
	case "WITH":
		if mode == ob.ModeOracle && len(words) > 1 && (words[1] == objectFunction || words[1] == objectProcedure) {
			return fmt.Errorf("oceanbase: inline WITH routines are not supported")
		}
	case keywordBegin:
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

func procedural(words []string, mode ob.Mode) bool {
	if len(words) == 0 {
		return false
	}
	if mode == ob.ModeOracle {
		if words[0] == keywordBegin || words[0] == "DECLARE" {
			return true
		}
		if words[0] == "WITH" && len(words) > 1 {
			return words[1] == objectFunction || words[1] == objectProcedure
		}
	}
	if words[0] != "CREATE" {
		return false
	}
	if mode == ob.ModeMySQL && len(words) > 1 && words[1] == "DEFINER" {
		for _, word := range words[2:] {
			if word == objectFunction || word == objectProcedure || word == objectTrigger || word == "EVENT" {
				return true
			}
		}
		return false
	}
	for _, word := range words[1:] {
		switch word {
		case "OR", "REPLACE", "EDITIONABLE", "NONEDITIONABLE":
			continue
		case objectFunction, objectProcedure, objectTrigger:
			return true
		case "PACKAGE", "TYPE":
			return mode == ob.ModeOracle
		case "EVENT":
			return mode == ob.ModeMySQL
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
