package catalog

import (
	"errors"
	"strconv"
	"strings"
)

// namePrefix starts the line that starts a statement and names its tool, as in -- name: revenue_by_region.
const namePrefix = "-- name:"

// statement is one statement of a .sql file, from its name line to the next one or the end of the file.
type statement struct {
	// name is the tool name its name line gives.
	name string
	// file is the .sql file relative to the catalog folder, with slashes, such as finance/revenue.sql.
	file string
	// line is the line of its name line, from 1.
	line int
	// sql is the text below the name line, without the white space around it.
	sql string
	// listed is true once an index entry joins the statement.
	listed bool
}

// splitStatements splits the text of the .sql file at file into its
// statements. Each starts at a line that reads -- name: and the tool name,
// with white space around it allowed, and runs to the next such line or to
// the end of the file. Text before the first name line may hold only
// comments and white space, and every statement must hold SQL that does not
// start with a semicolon. A byte order
// mark that an editor wrote at the start of the file is not part of the text.
func splitStatements(file, text string) ([]*statement, error) {
	text = strings.TrimPrefix(text, "\ufeff")
	var out []*statement
	var cur *statement
	var body, preamble []string
	end := func() error {
		if cur == nil {
			return nil
		}
		cur.sql = strings.TrimSpace(strings.Join(body, "\n"))
		at := firstSQL(cur.sql)
		if at < 0 {
			return errors.New(file + " statement " + cur.name + " on line " + strconv.Itoa(cur.line) + " holds no SQL, only comments or blank lines. Write the statement below its -- name: line, or remove the line.")
		}
		if cur.sql[at] == ';' {
			return errors.New(file + " statement " + cur.name + " on line " + strconv.Itoa(cur.line) + " starts with a semicolon, which would cut the statement off from the EXPLAIN that the start checks put before it to check its grants. Remove the semicolon at the start of the statement.")
		}
		out = append(out, cur)
		body = nil
		return nil
	}
	for i, line := range strings.Split(text, "\n") {
		rest, named := strings.CutPrefix(strings.TrimSpace(line), namePrefix)
		switch {
		case !named && cur == nil:
			preamble = append(preamble, line)
			continue
		case !named:
			body = append(body, line)
			continue
		case cur == nil:
			if err := preambleSQL(file, preamble); err != nil {
				return nil, err
			}
		}
		if err := end(); err != nil {
			return nil, err
		}
		name := strings.TrimSpace(rest)
		if name == "" {
			return nil, errors.New(file + " line " + strconv.Itoa(i+1) + " has a -- name: line without a name. Write the tool name after it, such as -- name: revenue_by_region.")
		}
		cur = &statement{name: name, file: file, line: i + 1}
	}
	if cur == nil {
		return nil, preambleSQL(file, preamble)
	}
	return out, end()
}

// preambleSQL refuses the lines before the first name line of file when they hold SQL, and names the line where it starts.
func preambleSQL(file string, lines []string) error {
	text := strings.Join(lines, "\n")
	at := firstSQL(text)
	if at < 0 {
		return nil
	}
	line := 1 + strings.Count(text[:at], "\n")
	return errors.New(file + " line " + strconv.Itoa(line) + " holds SQL before the first -- name: line, so it belongs to no tool. Put a -- name: line before it, or remove it.")
}

// firstSQL returns the byte offset of the first character of text that is
// neither white space nor part of a comment, or -1 when text holds only
// comments and white space. A -- comment runs to the next line feed or
// carriage return, as Postgres ends it, and a
// /* comment runs to its matching */, nested as Postgres nests them. A /*
// comment that never closes counts as SQL, because Postgres refuses it.
func firstSQL(text string) int {
	for i := 0; i < len(text); {
		switch {
		case strings.ContainsRune(" \t\n\r\f\v", rune(text[i])):
			i++
		case strings.HasPrefix(text[i:], "--"):
			nl := strings.IndexAny(text[i:], "\n\r")
			if nl < 0 {
				return -1
			}
			i += nl + 1
		case strings.HasPrefix(text[i:], "/*"):
			end := commentEnd(text, i)
			if end < 0 {
				return i
			}
			i = end
		default:
			return i
		}
	}
	return -1
}

// commentEnd returns the offset just past the */ that closes the /* comment at start, or -1 when it never closes.
func commentEnd(text string, start int) int {
	depth := 0
	for i := start; i+1 < len(text); {
		switch {
		case text[i] == '/' && text[i+1] == '*':
			depth++
			i += 2
		case text[i] == '*' && text[i+1] == '/':
			depth--
			i += 2
			if depth == 0 {
				return i
			}
		default:
			i++
		}
	}
	return -1
}
