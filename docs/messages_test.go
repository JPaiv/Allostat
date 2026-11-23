// Package docs_test keeps the documentation in step with the code.
package docs_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// sources lists where messages are made: a file, and the functions whose
// first argument is a message.
var sources = map[string][]string{
	"admission/admission.go":               {"refuse", "keep"},
	"internal/controller/actionrequest.go": {"Sprintf"},
	"api/v1alpha1/record.go":               {"Errorf", "New"},
	"cmd/allostat/replay.go":               {"Errorf", "New"},
	"api/v1alpha1/types.go":                nil,
	"api/v1alpha1/record_types.go":         nil,
}

var (
	verb       = regexp.MustCompile(`%[+#]?[a-z]`)
	validation = regexp.MustCompile(`XValidation:.*message="([^"]+)"`)
)

// messages returns the message formats of one file.
func messages(t *testing.T, path string, funcs []string) []string {
	t.Helper()
	var out []string
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range validation.FindAllStringSubmatch(string(data), -1) {
		out = append(out, m[1])
	}
	file, err := parser.ParseFile(token.NewFileSet(), path, data, 0)
	if err != nil {
		t.Fatal(err)
	}
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		name := ""
		switch f := call.Fun.(type) {
		case *ast.Ident:
			name = f.Name
		case *ast.SelectorExpr:
			name = f.Sel.Name
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		for _, want := range funcs {
			if name == want {
				text, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatal(err)
				}
				out = append(out, text)
			}
		}
		return true
	})
	return out
}

// pattern turns a message format into what the page must have: the same
// text, with a name in <...> where the format has a verb.
func pattern(format string) *regexp.Regexp {
	var b strings.Builder
	rest := format
	for {
		loc := verb.FindStringIndex(rest)
		if loc == nil {
			break
		}
		b.WriteString(regexp.QuoteMeta(rest[:loc[0]]))
		if rest[loc[0]:loc[1]] == "%q" {
			// A quoted name, or a quoted list in its own form.
			b.WriteString(`("<[^<>]+>"|<[^<>]+>)`)
		} else {
			b.WriteString(`<[^<>]+>`)
		}
		rest = rest[loc[1]:]
	}
	b.WriteString(regexp.QuoteMeta(rest))
	return regexp.MustCompile("`" + b.String() + "`")
}

// Every message the code can give must be in docs/messages.md, in a row
// that says what to do.
func TestEveryMessageIsInThePage(t *testing.T) {
	page, err := os.ReadFile("messages.md")
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for path, funcs := range sources {
		found := messages(t, filepath.Join("..", filepath.FromSlash(path)), funcs)
		if len(found) == 0 {
			t.Errorf("%s: no message found, the test does not look at this file any more", path)
		}
		for _, format := range found {
			if strings.HasPrefix(format, "usage:") {
				continue
			}
			total++
			row := regexp.MustCompile(`(?m)^\| ` + pattern(format).String() + ` \|.*[^| ] \|$`)
			if !row.Match(page) {
				t.Errorf("%s: the message %q has no row in docs/messages.md (want %s)", path, format, pattern(format))
			}
		}
	}
	if total < 40 {
		t.Fatalf("only %d messages found, the test lost a source", total)
	}
}

// A row of the page that no message matches is a message that is gone.
func TestThePageHasNoMessageThatIsGone(t *testing.T) {
	page, err := os.ReadFile("messages.md")
	if err != nil {
		t.Fatal(err)
	}
	var patterns []*regexp.Regexp
	for path, funcs := range sources {
		for _, format := range messages(t, filepath.Join("..", filepath.FromSlash(path)), funcs) {
			patterns = append(patterns, pattern(format))
		}
	}
	rows := regexp.MustCompile("(?m)^\\| (`[^`]+`) \\|").FindAllStringSubmatch(string(page), -1)
	if len(rows) < 35 {
		t.Fatalf("only %d rows found in the page", len(rows))
	}
next:
	for _, row := range rows {
		for _, p := range patterns {
			if p.FindString(row[1]) == row[1] {
				continue next
			}
		}
		t.Errorf("the page has %s, the code has no such message", row[1])
	}
}
