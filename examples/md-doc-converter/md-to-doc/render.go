// render.go converts a goldmark Markdown AST into a godocx
// document. We deliberately handle a small subset of Markdown — the
// shapes a typical Hive demo throws at us — and gracefully fall back
// to plain text for the rest. The complete unsupported list lives in
// README.md so users know what to expect.
package main

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/gomutex/godocx"
	godocxlib "github.com/gomutex/godocx/docx"
	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	gtext "github.com/yuin/goldmark/text"
)

// renderMarkdownToDocx parses src as Markdown and emits docx bytes.
// templatePath, when non-empty, points at a .docx file whose styles
// (Heading1..6, ListBullet, ListNumber, font defaults) take effect on
// the rendered output. New paragraphs are appended after whatever
// content the template already contains.
func renderMarkdownToDocx(src []byte, templatePath string) ([]byte, error) {
	var doc *godocxlib.RootDoc
	var err error
	if templatePath != "" {
		doc, err = godocx.OpenDocument(templatePath)
		if err != nil {
			return nil, fmt.Errorf("open template: %w", err)
		}
	} else {
		doc, err = godocx.NewDocument()
		if err != nil {
			return nil, fmt.Errorf("new docx: %w", err)
		}
	}

	md := goldmark.New()
	root := md.Parser().Parse(gtext.NewReader(src))

	r := &renderer{doc: doc, src: src}
	if err := ast.Walk(root, r.walk); err != nil {
		return nil, fmt.Errorf("walk markdown: %w", err)
	}

	var buf bytes.Buffer
	if _, err := doc.WriteTo(&buf); err != nil {
		return nil, fmt.Errorf("write docx: %w", err)
	}
	return buf.Bytes(), nil
}

// renderer carries the state we need across the AST walk: the active
// paragraph being filled (nil when between blocks) and the current
// inline format flags. Bold/italic stack via simple counters because
// goldmark nests Emphasis nodes for things like ***bold-italic***.
type renderer struct {
	doc *godocxlib.RootDoc
	src []byte

	// Block-level paragraph being filled. Nil when we're between
	// block elements; created on first inline content of a block.
	curPara *godocxlib.Paragraph
	// Block-level style override applied to the next paragraph that
	// gets created. Reset after use. Used for Heading1..6, ListBullet,
	// ListNumber, etc.
	pendingStyle string

	// Inline flags. Each Emphasis enter increments and leave decrements
	// — > 0 means apply to runs created in this scope.
	bold   int
	italic int
	code   int

	// List state — Word's outline numbering needs id+level. We only
	// support flat unordered + ordered lists this round; nested lists
	// flatten to a single level (still readable).
	listOrdered bool
	inList      bool
}

// walk is the goldmark.Walker. Returns one of ast.WalkContinue /
// WalkSkipChildren / WalkStop (we mostly use WalkContinue).
func (r *renderer) walk(n ast.Node, entering bool) (ast.WalkStatus, error) {
	switch node := n.(type) {

	case *ast.Document:
		// nothing — root container

	case *ast.Heading:
		if entering {
			lvl := node.Level
			if lvl < 1 {
				lvl = 1
			}
			if lvl > 6 {
				lvl = 6
			}
			// Use AddParagraph + Style so children inline runs can
			// carry their own emphasis. AddHeading(text, level) takes
			// a literal string and would lose nested Emphasis nodes.
			r.pendingStyle = fmt.Sprintf("Heading%d", lvl)
		} else {
			r.curPara = nil
		}

	case *ast.Paragraph:
		if entering {
			// In list-item context the ListItem already opened the
			// paragraph — don't open a second one.
			if r.curPara == nil {
				r.openParagraph()
			}
		} else {
			r.curPara = nil
		}

	case *ast.List:
		if entering {
			r.inList = true
			r.listOrdered = node.IsOrdered()
		} else {
			r.inList = false
		}

	case *ast.ListItem:
		if entering {
			if r.listOrdered {
				r.pendingStyle = "ListNumber"
			} else {
				r.pendingStyle = "ListBullet"
			}
			// Pre-open the paragraph so the inner Paragraph node
			// (goldmark wraps list-item content in a Paragraph) reuses
			// it instead of starting a fresh one.
			r.openParagraph()
		} else {
			r.curPara = nil
		}

	case *ast.FencedCodeBlock, *ast.CodeBlock:
		// Render code blocks as one paragraph per line, monospace-
		// styled via Run.Style("Code") if the template defines it.
		// godocx's defaults give us a Courier-ish run via Style().
		if entering {
			lines := node.Lines()
			for i := 0; i < lines.Len(); i++ {
				seg := lines.At(i)
				text := strings.TrimRight(string(seg.Value(r.src)), "\n")
				p := r.doc.AddParagraph("")
				p.Style("Code") // template-defined or ignored if missing
				run := p.AddText(text)
				run.Style("Code")
			}
			return ast.WalkSkipChildren, nil
		}

	case *ast.ThematicBreak:
		if entering {
			// Word's traditional thematic break is a paragraph with
			// a horizontal rule border — we don't have a clean API
			// for it, so emit a row of em-dashes as a stand-in.
			r.doc.AddParagraph("———")
		}

	case *ast.Emphasis:
		// Emphasis level: 1 = italic, 2 = bold (goldmark default).
		if entering {
			if node.Level >= 2 {
				r.bold++
			} else {
				r.italic++
			}
		} else {
			if node.Level >= 2 {
				r.bold--
			} else {
				r.italic--
			}
		}

	case *ast.CodeSpan:
		if entering {
			r.code++
		} else {
			r.code--
		}

	case *ast.Text:
		if entering {
			seg := node.Segment
			r.emitText(string(seg.Value(r.src)))
			if node.HardLineBreak() || node.SoftLineBreak() {
				r.emitText(" ")
			}
		}

	case *ast.AutoLink:
		if entering {
			r.emitText(string(node.URL(r.src)))
		}

	case *ast.Link:
		// Inline text only — hyperlinks need a relationship part
		// that's beyond the current scope. URL is dropped silently.

	case *ast.Image:
		if entering {
			alt := string(node.Text(r.src))
			r.emitText(fmt.Sprintf("[image: %s]", alt))
			return ast.WalkSkipChildren, nil
		}

	case *ast.RawHTML, *ast.HTMLBlock:
		// Skip raw HTML — Word can't render it usefully.
		if entering {
			return ast.WalkSkipChildren, nil
		}

	default:
		// Unknown node — keep walking children, fall back to text.
	}
	return ast.WalkContinue, nil
}

// openParagraph creates a new paragraph and applies any pendingStyle
// (heading, list bullet/number, etc.). Called whenever a block needs
// a fresh paragraph and r.curPara is nil.
func (r *renderer) openParagraph() {
	r.curPara = r.doc.AddParagraph("")
	if r.pendingStyle != "" {
		r.curPara.Style(r.pendingStyle)
		r.pendingStyle = ""
	}
}

// emitText writes a text run into the active paragraph, respecting
// the current bold/italic/code flags. If no paragraph is open we open
// one — happens for stray text outside any explicit block.
func (r *renderer) emitText(s string) {
	if s == "" {
		return
	}
	if r.curPara == nil {
		r.openParagraph()
	}
	run := r.curPara.AddText(s)
	if r.bold > 0 {
		run.Bold(true)
	}
	if r.italic > 0 {
		run.Italic(true)
	}
	if r.code > 0 {
		// Use a character style if the template defines "Code"; godocx
		// defaults won't render this as monospace but the bare style
		// reference is harmless.
		run.Style("Code")
	}
}
