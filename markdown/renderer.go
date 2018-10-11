// The package markdown outputs normalized mmark markdown. It useful to have as a mmarkfmt.
package markdown

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"strings"

	"github.com/gomarkdown/markdown/ast"
	"github.com/gomarkdown/markdown/html"
	"github.com/mmarkdown/mmark/mast"
)

// Flags control optional behavior of Markdown renderer.
type Flags int

// HTML renderer configuration options.
const (
	FlagsNone Flags = 0

	CommonFlags Flags = FlagsNone
)

// RendererOptions is a collection of supplementary parameters tweaking
// the behavior of various parts of Markdown renderer.
type RendererOptions struct {
	Flags Flags // Flags allow customizing this renderer's behavior

	TextWidth int

	// if set, called at the start of RenderNode(). Allows replacing rendering of some nodes
	RenderNodeHook html.RenderNodeFunc
}

// Renderer implements Renderer interface for Markdown output.
type Renderer struct {
	opts RendererOptions

	// TODO(miek): paraStart should probably be a stack, aside in para in aside, etc.
	paraStart    int
	headingStart int

	prefix     *prefixStack // track current prefix, quote, aside, etc.
	listMarker string

	// tables
	cellStart int
	col       int
	colWidth  []int
	tableType ast.Node
}

// NewRenderer creates and configures an Renderer object, which satisfies the Renderer interface.
func NewRenderer(opts RendererOptions) *Renderer {
	if opts.TextWidth == 0 {
		opts.TextWidth = 80
	}
	return &Renderer{opts: opts, prefix: &prefixStack{p: [][]byte{}}}
}

func (r *Renderer) hardBreak(w io.Writer, node *ast.Hardbreak) {}

func (r *Renderer) matter(w io.Writer, node *ast.DocumentMatter, entering bool) {
	if !entering {
		return
	}
	switch node.Matter {
	case ast.DocumentMatterFront:
		r.outs(w, "{frontmatter}\n")
	case ast.DocumentMatterMain:
		r.outs(w, "{mainmatter}\n")
	case ast.DocumentMatterBack:
		r.outs(w, "{backmatter}\n")
	}
	r.cr(w)
}

func (r *Renderer) heading(w io.Writer, node *ast.Heading, entering bool) {
	if !entering {
		if node.HeadingID != "" {
			var content string
			if buf, ok := w.(*bytes.Buffer); ok {
				content = buf.String()[r.headingStart:buf.Len()]
			}
			// skip node level and one space and only print if the sanitized string
			// is not equal to the autogenerated HeadingID.
			if sanitizeAnchorName(content[node.Level+1:]) != node.HeadingID {
				r.outs(w, " {#"+node.HeadingID+"}")
			}
		}
		r.cr(w)
		r.cr(w)
		return
	}
	if buf, ok := w.(*bytes.Buffer); ok {
		r.headingStart = buf.Len()
	}
	if node.IsSpecial {
		r.outs(w, ".")
	}
	hashes := strings.Repeat("#", node.Level)
	r.outs(w, hashes)
	r.outs(w, " ")
}

func (r *Renderer) horizontalRule(w io.Writer, node *ast.HorizontalRule) {
	r.cr(w)
	r.outs(w, "******")
	r.cr(w)
}

func (r *Renderer) citation(w io.Writer, node *ast.Citation, entering bool) {
	r.outs(w, "[@")
	for i, dest := range node.Destination {
		if i > 0 {
			r.outs(w, ", ")
		}
		switch node.Type[i] {
		case ast.CitationTypeInformative:
			// skip outputting ? as it's the default
		case ast.CitationTypeNormative:
			r.outs(w, "!")
		case ast.CitationTypeSuppressed:
			r.outs(w, "-")
		}
		r.out(w, dest)

	}
	r.outs(w, "]")
}

func (r *Renderer) paragraph(w io.Writer, para *ast.Paragraph, entering bool) {
	if entering {
		if buf, ok := w.(*bytes.Buffer); ok {
			r.paraStart = buf.Len()
		}
		return
	}

	buf, ok := w.(*bytes.Buffer)
	end := 0
	if ok {
		end = buf.Len()
	}

	// Reformat the entire buffer and rewrite to the writer.
	b := buf.Bytes()[r.paraStart:end]
	indented := r.wrapText(b, r.prefix.flatten())

	buf.Truncate(r.paraStart)

	// Now an indented list didn't get is marker yet, override the 3 spaces that have been
	// created with the list marker, taking the current prefix into account.
	if item, inList := para.Parent.(*ast.ListItem); inList {
		plen := r.prefix.len() - 3
		switch x := item.ListFlags; {
		case x&ast.ListTypeOrdered != 0:
			indented[plen+0] = '1'
			indented[plen+1] = '.'
			indented[plen+2] = ' '
		case x&ast.ListTypeTerm != 0:
			indented = indented[plen+3:] // remove prefix.
		case x&ast.ListTypeDefinition != 0:
			indented[plen+0] = ':'
			indented[plen+1] = ' '
			indented[plen+2] = ' '
		default:
			indented[plen+0] = '*'
			indented[plen+1] = ' '
			indented[plen+2] = ' '
		}
	}

	r.out(w, indented)

	if !last(para) {
		r.cr(w)
		r.cr(w)
	}
}

func (r *Renderer) list(w io.Writer, list *ast.List, entering bool) {
	if entering {
		r.prefix.push(Space3)
	} else {
		r.prefix.pop()
		if !last(list) {
			r.cr(w)
			r.newline(w)
		}
	}
}

func (r *Renderer) listItem(w io.Writer, listItem *ast.ListItem, entering bool) {
	if !entering {
		if !last(listItem) {
			r.cr(w)
			r.newline(w)
		}
	}
}

func (r *Renderer) codeBlock(w io.Writer, codeBlock *ast.CodeBlock, entering bool) {
	if !entering {
		return
	}

	r.outPrefix(w)
	r.outs(w, "~~~")
	if codeBlock.Info != nil {
		r.outs(w, " ")
		r.out(w, codeBlock.Info)
	}

	r.cr(w)
	indented := r.indentText(codeBlock.Literal, r.prefix.flatten())
	r.out(w, indented)
	r.outPrefix(w)
	r.outs(w, "~~~")
	r.cr(w)
	if _, ok := ast.GetNextNode(codeBlock).(*ast.Caption); !ok {
		r.cr(w)
	}
}

func (r *Renderer) table(w io.Writer, tab *ast.Table, entering bool) {
	if entering {
		r.colWidth = r.tableColWidth(tab)
		r.col = 0
	} else {
		r.colWidth = []int{}
	}
}

func (r *Renderer) tableRow(w io.Writer, tableRow *ast.TableRow, entering bool) {
	if entering {
		r.outPrefix(w)
		r.col = 0
		for i, width := range r.colWidth {
			if _, isFooter := r.tableType.(*ast.TableFooter); isFooter {
				r.out(w, bytes.Repeat([]byte("="), width+1))
				if i == len(r.colWidth)-1 {
					r.cr(w)
					r.outPrefix(w)
				} else {
					r.outs(w, "|")
				}
			}
		}

		return
	}

	for i, width := range r.colWidth {
		if _, isHeader := r.tableType.(*ast.TableHeader); isHeader {
			if i == 0 {
				r.outPrefix(w)
			}
			r.out(w, bytes.Repeat([]byte("-"), width+1))
			if i == len(r.colWidth)-1 {
				r.cr(w)
			} else {
				r.outs(w, "|")
			}
		}
	}
}

func (r *Renderer) tableCell(w io.Writer, tableCell *ast.TableCell, entering bool) {
	// we get called when we're calculating the column width, only when r.tableColWidth is set we need to output.
	if len(r.colWidth) == 0 {
		return
	}
	if entering {
		if buf, ok := w.(*bytes.Buffer); ok {
			r.cellStart = buf.Len() + 1
		}
		if r.col > 0 {
			r.out(w, Space)
		}
		return
	}

	cur := 0
	if buf, ok := w.(*bytes.Buffer); ok {
		cur = buf.Len()
	}
	size := r.colWidth[r.col]
	fill := bytes.Repeat(Space, size-(cur-r.cellStart))
	r.out(w, fill)
	if r.col == len(r.colWidth)-1 {
		r.cr(w)
	} else {
		r.outs(w, "|")
	}
	r.col++
}

func (r *Renderer) htmlSpan(w io.Writer, span *ast.HTMLSpan) {}

func (r *Renderer) crossReference(w io.Writer, cr *ast.CrossReference, entering bool) {
	if entering {
		r.outs(w, "(#")
		r.out(w, cr.Destination)
		return
	}
	r.outs(w, ")")
}

func (r *Renderer) index(w io.Writer, index *ast.Index, entering bool) {
	if !entering {
		return
	}

	r.outs(w, "(!")
	if index.Primary {
		r.outs(w, "!")
	}
	r.out(w, index.Item)

	if len(index.Subitem) > 0 {
		r.outs(w, ", ")
		r.out(w, index.Subitem)
	}
	r.outs(w, ")")
}

func (r *Renderer) link(w io.Writer, link *ast.Link, entering bool) {
	if !entering {
		return
	}

	// Render the text here, because we need it before the link.
	r.outs(w, "[")
	for _, child := range link.GetChildren() {
		ast.WalkFunc(child, func(node ast.Node, entering bool) ast.WalkStatus {
			return r.RenderNode(w, node, entering)
		})
	}
	r.outs(w, "]")
	ast.RemoveFromTree(link) // nothing needs to be rendered anymore

	r.outs(w, "(")
	r.out(w, link.Destination)
	if len(link.Title) > 0 {
		r.outs(w, ` "`)
		r.out(w, link.Title)
		r.outs(w, `"`)
	}
	r.outs(w, ")")
}

func (r *Renderer) image(w io.Writer, node *ast.Image, entering bool) {
	if !entering {
		return
	}
	r.outs(w, "![")
	for _, child := range node.GetChildren() {
		ast.WalkFunc(child, func(node ast.Node, entering bool) ast.WalkStatus {
			return r.RenderNode(w, node, entering)
		})
	}
	r.outs(w, "]")
	ast.RemoveFromTree(node) // nothing needs to be rendered anymore

	r.outs(w, "(")
	r.out(w, node.Destination)
	if len(node.Title) > 0 {
		r.outs(w, ` "`)
		r.out(w, node.Title)
		r.outs(w, `"`)
	}
	r.outs(w, ")")
}

func (r *Renderer) mathBlock(w io.Writer, mathBlock *ast.MathBlock) {
}

func (r *Renderer) caption(w io.Writer, caption *ast.Caption, entering bool) {
	if !entering {
		return
	}

	r.outPrefix(w)
	switch ast.GetPrevNode(caption).(type) {
	case *ast.BlockQuote:
		r.outs(w, "Quote: ")
	case *ast.Table:
		r.outs(w, "Table: ")
	case *ast.CodeBlock:
		r.outs(w, "Figure: ")
	}
}

func (r *Renderer) captionFigure(w io.Writer, captionFigure *ast.CaptionFigure, entering bool) {
	if !entering && !last(captionFigure) {
		r.cr(w)
		r.cr(w)
	}
}

func (r *Renderer) blockQuote(w io.Writer, block *ast.BlockQuote, entering bool) {
	if entering {
		r.prefix.push(Quote)
		return
	}
	r.prefix.pop()

	if !last(block) {
		r.cr(w)
		r.newline(w)
	}
}

func (r *Renderer) aside(w io.Writer, block *ast.Aside, entering bool) {
	if entering {
		r.prefix.push(Aside)
		return
	}
	r.prefix.pop()

	if !last(block) {
		r.cr(w)
		r.newline(w)
	}
}

// RenderNode renders a markdown node to markdown.
func (r *Renderer) RenderNode(w io.Writer, node ast.Node, entering bool) ast.WalkStatus {
	if r.opts.RenderNodeHook != nil {
		status, didHandle := r.opts.RenderNodeHook(w, node, entering)
		if didHandle {
			return status
		}
	}

	if attr := mast.AttributeFromNode(node); attr != nil && entering {
		switch node.(type) {
		case *ast.CaptionFigure:
			// captionFigure also gets the attribute for a codeblock, don't output that.
			if childs := node.GetChildren(); len(childs) > 0 {

				switch childs[0].(type) {
				case *ast.CodeBlock:
				case *ast.BlockQuote:
				default:
					r.outPrefix(w)
					w.Write((mast.AttributeBytes(attr)))
					r.cr(w)
				}
			}

		default:
			r.outPrefix(w)
			w.Write((mast.AttributeBytes(attr)))
			r.cr(w)

		}
	}

	switch node := node.(type) {
	case *ast.Document:
		// do nothing
	case *mast.Title:
		r.outs(w, "%%%")
		r.out(w, node.Content)
		r.cr(w)
		r.outs(w, "%%%")
		if !last(node) {
			r.cr(w)
			r.cr(w)
		}
	case *mast.Bibliography:
	case *mast.BibliographyItem:
	case *mast.DocumentIndex, *mast.IndexLetter, *mast.IndexItem, *mast.IndexSubItem, *mast.IndexLink:
	case *ast.Text:
		r.text(w, node, entering)
	case *ast.Softbreak:
	case *ast.Hardbreak:
	case *ast.Callout:
		r.callout(w, node, entering)
	case *ast.Emph:
		r.outOneOf(w, entering, "*", "*")
	case *ast.Strong:
		r.outOneOf(w, entering, "**", "**")
	case *ast.Del:
		r.outOneOf(w, entering, "~~", "~~")
	case *ast.Citation:
		r.citation(w, node, entering)
	case *ast.DocumentMatter:
		r.matter(w, node, entering)
	case *ast.Heading:
		r.heading(w, node, entering)
	case *ast.HorizontalRule:
		if entering {
			r.outPrefix(w)
			r.outs(w, "********\n")
		}
	case *ast.Paragraph:
		r.paragraph(w, node, entering)
	case *ast.HTMLSpan:
	case *ast.HTMLBlock:
		r.out(w, node.Content)
		if !last(node) {
			r.cr(w)
			r.cr(w)
		}
	case *ast.List:
		r.list(w, node, entering)
	case *ast.ListItem:
		r.listItem(w, node, entering)
	case *ast.CodeBlock:
		r.codeBlock(w, node, entering)
	case *ast.Caption:
		r.caption(w, node, entering)
	case *ast.CaptionFigure:
		r.captionFigure(w, node, entering)
	case *ast.Table:
		r.table(w, node, entering)
	case *ast.TableCell:
		r.tableCell(w, node, entering)
	case *ast.TableHeader:
		r.tableType = node
	case *ast.TableBody:
		r.tableType = node
	case *ast.TableFooter:
		r.tableType = node
	case *ast.TableRow:
		r.tableRow(w, node, entering)
	case *ast.BlockQuote:
		r.blockQuote(w, node, entering)
	case *ast.Aside:
		r.aside(w, node, entering)
	case *ast.CrossReference:
		r.crossReference(w, node, entering)
	case *ast.Index:
		r.index(w, node, entering)
	case *ast.Link:
		r.link(w, node, entering)
	case *ast.Math:
	case *ast.Image:
		r.image(w, node, entering)
	case *ast.Code:
		r.outs(w, "`")
		r.out(w, node.Literal)
		r.outs(w, "`")
	case *ast.MathBlock:
	case *ast.Subscript:
		r.outOneOf(w, true, "~", "~")
		if entering {
			r.out(w, escapeText(node.Literal))
		}
		r.outOneOf(w, false, "~", "~")
	case *ast.Superscript:
		r.outOneOf(w, true, "^", "^")
		if entering {
			r.out(w, escapeText(node.Literal))
		}
		r.outOneOf(w, false, "^", "^")
	default:
		panic(fmt.Sprintf("Unknown node %T", node))
	}
	return ast.GoToNext
}

func (r *Renderer) callout(w io.Writer, node *ast.Callout, entering bool) {
	if !entering {
		return
	}
	r.outs(w, "<<")
	r.out(w, node.ID)
	r.outs(w, ">>")
}

func (r *Renderer) text(w io.Writer, node *ast.Text, entering bool) {
	if !entering {
		return
	}
	r.out(w, escapeText(node.Literal))
}

func (r *Renderer) RenderHeader(_ io.Writer, _ ast.Node) {}
func (r *Renderer) writeDocumentHeader(_ io.Writer)      {}

func (r *Renderer) RenderFooter(w io.Writer, _ ast.Node) {
	buf, ok := w.(*bytes.Buffer)
	if !ok {
		return
	}

	trimmed := &bytes.Buffer{}

	scanner := bufio.NewScanner(buf)
	for scanner.Scan() {
		trimmed.Write(bytes.TrimRight(scanner.Bytes(), " "))
		trimmed.WriteString("\n")
	}
	if err := scanner.Err(); err != nil {
		return
	}
	buf.Truncate(0)
	buf.Write(trimmed.Bytes())
}

var (
	Space  = []byte(" ")
	Space3 = []byte("   ")
	Aside  = []byte("A> ")
	Quote  = []byte("> ")
)
