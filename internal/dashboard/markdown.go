package dashboard

import (
	"bytes"
	"html/template"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// md renders an agent's answer, which is usually Markdown. It is safe to put in the page as it
// is: raw HTML in the answer is dropped and javascript: and similar links are not linked, as
// goldmark does by default, and links open in a new tab.
var md = goldmark.New(
	goldmark.WithExtensions(extension.GFM), // tables, task lists, strikethrough, bare URLs
	goldmark.WithParserOptions(parser.WithASTTransformers(util.Prioritized(newTab{}, 100))),
)

// markdown renders s as HTML, or escapes it should rendering fail.
func markdown(s string) template.HTML {
	var b bytes.Buffer
	if err := md.Convert([]byte(s), &b); err != nil {
		return template.HTML(template.HTMLEscapeString(s))
	}
	return template.HTML(b.String())
}

// newTab opens links in a new tab, keeping the playground where it is.
type newTab struct{}

func (newTab) Transform(doc *ast.Document, _ text.Reader, _ parser.Context) {
	ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		switch n.(type) {
		case *ast.Link, *ast.AutoLink:
			if entering {
				n.SetAttributeString("target", "_blank")
				n.SetAttributeString("rel", "noopener noreferrer")
			}
		}
		return ast.WalkContinue, nil
	})
}
