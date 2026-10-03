package session

import (
	"go/ast"
	"go/doc/comment"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

func TestCommandSandboxDeprecationParagraphs(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "command_sandbox.go", nil, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	docs := make(map[string]*ast.CommentGroup)
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			name := d.Name.Name
			if d.Recv != nil {
				name = d.Recv.List[0].Type.(*ast.StarExpr).X.(*ast.Ident).Name + "." + name
			}
			docs[name] = d.Doc
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					docs[s.Name.Name] = d.Doc
				case *ast.ValueSpec:
					for _, n := range s.Names {
						docs[n.Name] = s.Doc
					}
				}
			}
		}
	}
	for _, name := range []string{
		"OpenCommandSandbox", "CommandSandboxOptions", "CommandSandbox", "CommandRequest", "CommandResult", "StartedCommand",
		"CommandStartFailed", "CommandOutcomeUnknown", "CommandProcessLimit", "CommandCleanupUnknown", "CommandSandboxClosed",
		"CommandSandbox.Run", "CommandSandbox.Start", "CommandSandbox.Close", "StartedCommand.Stop", "StartedCommand.Done", "StartedCommand.Result",
	} {
		t.Run(name, func(t *testing.T) {
			d := docs[name]
			if d == nil {
				t.Fatal("missing API documentation")
			}
			var parser comment.Parser
			var printer comment.Printer
			for _, block := range parser.Parse(d.Text()).Content {
				if _, ok := block.(*comment.Paragraph); ok {
					text := printer.Text(&comment.Doc{Content: []comment.Block{block}})
					if strings.HasPrefix(string(text), "Deprecated: use sandbox.") {
						return
					}
				}
			}
			t.Fatal("Go tooling requires Deprecated: to start its own paragraph")
		})
	}
	if !strings.Contains(docs["CommandSandboxOptions"].Text(), "RuntimeHome must be an existing private directory outside WorkDir") {
		t.Fatal("legacy RuntimeHome contract is missing")
	}
}
