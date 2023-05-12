package fs

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/skeema/skeema/internal/tengo"
)

// SQLFile represents a file containing SQL statements.
type SQLFile struct {
	FilePath   string
	Statements []*tengo.Statement
	Dirty      bool
}

// IsCaseSensitive checks whether or not the file system is case-sensitive, instead of assuming it based on OS
func IsCaseSensitiveFilesystem(dir string) (bool, error) {
	alt := filepath.Join(filepath.Dir(dir), genCaseTestFilename(filepath.Base(dir)))

	dInfo, err := os.Stat(dir)
	if err != nil {
		return false, fmt.Errorf("could not determine the case-sensitivity of the filesystem: %v", err)
	}

	aInfo, err := os.Stat(alt)
	if err != nil {
		// If the file doesn't exists, assume we are on a case-sensitive filesystem.
		if os.IsNotExist(err) {
			return true, nil
		}

		return false, fmt.Errorf("could not determine the case-sensitivity of the filesystem: %v", err)
	}

	return !os.SameFile(dInfo, aInfo), nil
}

// genTestFilename returns a string with at most one rune case-flipped.
//
// The transformation is applied only to the first rune that can be
// reversibly case-flipped, meaning:
//
// * A lowercase rune for which it's true that lower(upper(r)) == r
// * An uppercase rune for which it's true that upper(lower(r)) == r
//
// All the other runes are left intact.
func genCaseTestFilename(str string) string {
	flip := true
	return strings.Map(func(r rune) rune {
		if flip {
			if unicode.IsLower(r) {
				u := unicode.ToUpper(r)
				if unicode.ToLower(u) == r {
					r = u
					flip = false
				}
			} else if unicode.IsUpper(r) {
				l := unicode.ToLower(r)
				if unicode.ToUpper(l) == r {
					r = l
					flip = false
				}
			}
		}
		return r
	}, str)
}

// FileName returns the file name of sqlFile without its directory path.
func (sqlFile *SQLFile) FileName() string {
	return filepath.Base(sqlFile.FilePath)
}

// Exists returns true if sqlFile already exists in the filesystem, false if not.
func (sqlFile *SQLFile) Exists() (bool, error) {
	_, err := os.Stat(sqlFile.FilePath)
	if err == nil {
		return true, nil
	} else if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

// Delete unlinks the file.
func (sqlFile *SQLFile) Delete() error {
	return os.Remove(sqlFile.FilePath)
}

// Write creates or replaces the SQLFile with the current statements, returning
// the number of bytes written. If the file's statements now only consist of
// comments, whitespace, and commands (e.g. USE, DELIMITER) then the file will
// be deleted instead, and a length of 0 will be returned. The file will be
// unmarked as dirty if the operation was successful.
func (sqlFile *SQLFile) Write() (n int, err error) {
	var b bytes.Buffer
	var keepFile bool
	for _, stmt := range sqlFile.Statements {
		b.WriteString(stmt.Text)
		if stmt.Type != tengo.StatementTypeNoop && stmt.Type != tengo.StatementTypeCommand {
			keepFile = true
		}
	}
	if keepFile {
		n, err = b.Len(), os.WriteFile(sqlFile.FilePath, b.Bytes(), 0666)
	} else {
		err = sqlFile.Delete()
	}
	if err == nil {
		sqlFile.Dirty = false
	}
	return n, err
}

func makeDelimiterCommand(newDelimiter, defaultDatabase, filePath string) *tengo.Statement {
	return &tengo.Statement{
		File:            filePath,
		Text:            "DELIMITER " + newDelimiter + "\n",
		Type:            tengo.StatementTypeCommand,
		Delimiter:       "\000",
		DefaultDatabase: defaultDatabase,
	}
}

// AddStatement appends stmt to sqlFile's list of statements. This method marks
// the file as dirty, but does not rewrite the file.
// This method may adjust stmt.Text and stmt.Delimiter as needed to ensure the
// text contains the appropriate delimiter for the type of statement, as well as
// a trailing newline. DELIMITER command statements may also be inserted into
// sqlFile as necessary for stmt.
func (sqlFile *SQLFile) AddStatement(stmt *tengo.Statement) {
	// Prune any trailing DELIMITER or USE commands from the end of the file, as
	// these have no effect at the end of the file anyway.
	for len(sqlFile.Statements) > 0 && sqlFile.Statements[len(sqlFile.Statements)-1].Type == tengo.StatementTypeCommand {
		sqlFile.Statements = sqlFile.Statements[:len(sqlFile.Statements)-1]
	}

	// If there are any statements left, examine the last statement to see what
	// the delimiter and default database are at the end of the file. Also ensure
	// the last statement has a trailing delimiter and newline.
	currentDelimiter := ";"
	var defaultDatabase string
	if len(sqlFile.Statements) > 0 {
		lastStmt := sqlFile.Statements[len(sqlFile.Statements)-1]
		currentDelimiter = lastStmt.Delimiter
		defaultDatabase = lastStmt.DefaultDatabase
		lastStmt.NormalizeTrailer()
	}

	// Add a DELIMITER command before stmt, if needed
	if stmt.Compound && currentDelimiter == ";" {
		sqlFile.Statements = append(sqlFile.Statements, makeDelimiterCommand("//", defaultDatabase, sqlFile.FilePath))
		currentDelimiter = "//"
	} else if !stmt.Compound && currentDelimiter != ";" {
		sqlFile.Statements = append(sqlFile.Statements, makeDelimiterCommand(";", defaultDatabase, sqlFile.FilePath))
		currentDelimiter = ";"
	}

	// Adjust a few stmt fields as needed; append it to the file's statement list;
	// add another DELIMITER command if needed; mark file as dirty
	stmt.File = sqlFile.FilePath
	stmt.DefaultDatabase = defaultDatabase
	stmt.Delimiter = currentDelimiter
	stmt.NormalizeTrailer()
	sqlFile.Statements = append(sqlFile.Statements, stmt)
	if currentDelimiter != ";" {
		sqlFile.Statements = append(sqlFile.Statements, makeDelimiterCommand(";", defaultDatabase, sqlFile.FilePath))
	}
	sqlFile.Dirty = true
}

// EditStatementText sets stmt.Text to a new value consisting of newText plus
// an appropriate delimiter and newline. It marks the file as dirty, and (if
// needed for a compound statement) adds DELIMITER commands around stmt in the
// file's list of statements. The supplied newText should NOT have a delimiter
// or trailing newline. This method panics if stmt's address is not actually
// found among the file's statement pointers slice.
func (sqlFile *SQLFile) EditStatementText(stmt *tengo.Statement, newText string, compound bool) {
	sqlFile.Dirty = true
	i := sqlFile.statementIndex(stmt)

	// Short-cut in situations that don't require inserting new DELIMITER commands
	// TODO: remove extraneous DELIMITER commands if they are unnecessary.
	// Currently we only add them if needed, but never remove them, nor avoid
	// introducing duplicate ones in a multi-statement file.
	if stmt.Delimiter != ";" || !compound {
		_, oldFooter := stmt.SplitTextBody()
		stmt.Text = newText + oldFooter
		stmt.Compound = compound
		return
	}

	newStatements := make([]*tengo.Statement, len(sqlFile.Statements)+2)
	copy(newStatements, sqlFile.Statements[0:i])
	newStatements[i] = makeDelimiterCommand("//", stmt.DefaultDatabase, sqlFile.FilePath)
	stmt.Delimiter = "//"
	stmt.Text = newText + "//\n"
	stmt.Compound = compound
	newStatements[i+1] = stmt
	newStatements[i+2] = makeDelimiterCommand(";", stmt.DefaultDatabase, sqlFile.FilePath)
	copy(newStatements[i+3:], sqlFile.Statements[i+1:])
	sqlFile.Statements = newStatements
}

// RemoveStatement removes stmt from the file's in-memory list of statements,
// and marks the file as dirty. Panics if the address of stmt is not actually
// found in its expected file's in-memory representation.
func (sqlFile *SQLFile) RemoveStatement(stmt *tengo.Statement) {
	i := sqlFile.statementIndex(stmt)
	sqlFile.Dirty = true
	copy(sqlFile.Statements[i:], sqlFile.Statements[i+1:])
	sqlFile.Statements[len(sqlFile.Statements)-1] = nil
	sqlFile.Statements = sqlFile.Statements[:len(sqlFile.Statements)-1]
}

func (sqlFile *SQLFile) statementIndex(stmt *tengo.Statement) int {
	for n := range sqlFile.Statements {
		if sqlFile.Statements[n] == stmt {
			return n
		}
	}
	panic(fmt.Errorf("Statement previously at %s not actually found in file", stmt.Location()))
}

// NormalizeFileName forces name to lowercase on operating systems that
// traditionally have case-insensitive operating systems. This is intended for
// use in string-keyed maps, to avoid the possibility of having multiple
// distinct map keys which actually refer to the same file.
func NormalizeFileName(name string, lowerCase bool) string {
	if lowerCase {
		return strings.ToLower(name)
	}
	return name
}

// FileNameForObject returns a string containing the filename to use for the
// SQLFile representing the supplied object name. Special characters in the
// objectName will be removed; however, there is no risk of "conflicts" since
// a single SQLFile can store definitions for multiple objects.
func FileNameForObject(objectName string, lowerCase bool) string {
	objectName = strings.Map(removeSpecialChars, objectName)
	if objectName == "" {
		objectName = "symbols"
	}
	return NormalizeFileName(objectName, lowerCase) + ".sql"
}

// PathForObject returns a string containing a path to use for the SQLFile
// representing the supplied object name. Special characters in the objectName
// will be removed; however, there is no risk of "conflicts" since a single
// SQLFile can store definitions for multiple objects.
func PathForObject(dirPath, objectName string) string {
	caseSensitive, _ := IsCaseSensitiveFilesystem(dirPath)
	return filepath.Join(dirPath, FileNameForObject(objectName, !caseSensitive))
}

func removeSpecialChars(r rune) rune {
	if unicode.IsSpace(r) {
		return -1
	}
	banned := []rune{
		'.',
		'\\', '/',
		'"', '\'', '`',
		':', '*', '?', '|', '~', '#', '&', '-',
		'<', '>', '{', '}', '[', ']', '(', ')',
	}
	for _, bad := range banned {
		if r == bad {
			return -1
		}
	}
	return r
}
