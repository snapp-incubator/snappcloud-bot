package bestpractice

import (
	"bytes"
	"fmt"
	"strings"
	"time"

	"codeberg.org/go-pdf/fpdf"
)

// pdfCompress is off in tests, so they can read the text back.
var pdfCompress = true

const (
	pdfMargin = 15.0 // mm
	pdfFont   = "Helvetica"
	pdfMono   = "Courier"
)

type rgb struct{ r, g, b int }

var (
	pdfInk    = rgb{31, 35, 40}
	pdfMuted  = rgb{89, 99, 110}
	pdfRule   = rgb{209, 217, 224}
	pdfLink   = rgb{9, 105, 218}
	pdfSevere = map[Severity]rgb{High: {209, 36, 47}, Medium: {188, 76, 0}}
)

// pdfDoc wraps fpdf with the report's text style. The core fonts cover
// Windows-1252 only; tr maps what it can and turns the rest into dots.
type pdfDoc struct {
	*fpdf.Fpdf
	tr func(string) string
}

func (d pdfDoc) color(c rgb) { d.SetTextColor(c.r, c.g, c.b) }

// rich writes flowing text whose `backtick` spans are set in a monospace font.
func (d pdfDoc) rich(h, size float64, style, s string) {
	parts := strings.Split(s, "`")
	for i, p := range parts {
		switch {
		case i%2 == 1 && i < len(parts)-1:
			d.SetFont(pdfMono, style, size)
		case i%2 == 1: // an unmatched backtick stays as written
			p = "`" + p
			fallthrough
		default:
			d.SetFont(pdfFont, style, size)
		}
		d.Write(h, d.tr(p))
	}
}

// reportPDF renders a mailing's findings, team by team, each with its fix.
func reportPDF(m Mailing, docs map[string]string, footer string, day time.Time) ([]byte, error) {
	f := fpdf.New("P", "mm", "A4", "")
	d := pdfDoc{Fpdf: f, tr: f.UnicodeTranslatorFromDescriptor("")}
	f.SetCompression(pdfCompress)
	f.SetCreationDate(day)
	f.SetModificationDate(day)
	f.SetTitle("SnappCloud best-practice check, "+day.Format("2 Jan 2006"), true)
	f.SetAuthor("SnappCloud", true)
	f.SetMargins(pdfMargin, pdfMargin, pdfMargin)
	f.SetAutoPageBreak(true, pdfMargin+5)
	f.AliasNbPages("{nb}")
	f.SetFooterFunc(func() {
		f.SetY(-pdfMargin)
		f.SetFont(pdfFont, "", 8)
		d.color(pdfMuted)
		f.CellFormat(0, 4, d.tr(fmt.Sprintf("SnappCloud best-practice check · %s · page %d of {nb}",
			day.Format("2 Jan 2006"), f.PageNo())), "", 0, "C", false, 0, "")
	})
	f.AddPage()
	width, _ := f.GetPageSize()
	body := width - 2*pdfMargin

	f.SetFont(pdfFont, "B", 18)
	d.color(pdfInk)
	f.CellFormat(0, 9, "SnappCloud best-practice check", "", 1, "", false, 0, "")
	f.SetFont(pdfFont, "", 10)
	d.color(pdfMuted)
	f.CellFormat(0, 5, d.tr(day.Format("Monday 2 January 2006")), "", 1, "", false, 0, "")
	f.Ln(4)
	d.color(pdfInk)
	high, medium := counts(m.Findings)
	f.MultiCell(0, 5, d.tr(fmt.Sprintf("%s %d finding%s: %d high, %d medium.", emailIntro,
		len(m.Findings), plural(len(m.Findings)), high, medium)), "", "", false)
	f.Ln(4)

	// The summary table.
	cols := []float64{body - 50, 25, 25}
	f.SetFont(pdfFont, "B", 10)
	f.SetDrawColor(pdfRule.r, pdfRule.g, pdfRule.b)
	for i, h := range []string{"Team", "High", "Medium"} {
		align := "R"
		if i == 0 {
			align = "L"
		}
		f.CellFormat(cols[i], 7, h, "B", 0, align, false, 0, "")
	}
	f.Ln(-1)
	byTeam := GroupByTeam(m.Findings)
	for _, t := range m.Teams {
		h, md := counts(byTeam[t])
		f.SetFont(pdfFont, "", 10)
		d.color(pdfInk)
		f.CellFormat(cols[0], 6, d.tr(t), "", 0, "L", false, 0, "")
		if h > 0 {
			f.SetFont(pdfFont, "B", 10)
			d.color(pdfSevere[High])
		}
		f.CellFormat(cols[1], 6, fmt.Sprint(h), "", 0, "R", false, 0, "")
		f.SetFont(pdfFont, "", 10)
		d.color(pdfInk)
		f.CellFormat(cols[2], 6, fmt.Sprint(md), "", 1, "R", false, 0, "")
	}

	for _, t := range m.Teams {
		f.Ln(6)
		if f.GetY() > 250 { // keep a heading with its first finding
			f.AddPage()
		}
		f.SetFont(pdfFont, "B", 14)
		d.color(pdfInk)
		f.CellFormat(0, 8, d.tr("Team "+t), "B", 1, "", false, 0, "")
		f.Ln(3)
		for _, fd := range byTeam[t] {
			d.finding(fd, docs)
		}
	}

	if footer != "" {
		f.Ln(4)
		f.SetFont(pdfFont, "", 9)
		d.color(pdfMuted)
		d.rich(4.5, 9, "", footer)
	}

	var b bytes.Buffer
	if err := f.Output(&b); err != nil {
		return nil, fmt.Errorf("render report PDF: %w", err)
	}
	return b.Bytes(), nil
}

// ignoreStatement tells how to exclude a finding, or that it cannot be. The
// annotation gets a line of its own, so it is not split and copies whole.
func (d pdfDoc) ignoreStatement(fd Finding) {
	d.color(pdfMuted)
	if DrainBlocker(fd.Rule) {
		d.rich(4.5, 9, "", fmt.Sprintf("This PDB blocks node drains, so it cannot be excluded by annotation: "+
			"it must be fixed. Rule `%s`.", fd.Rule))
		d.Ln(4.5)
		return
	}
	obj := "object"
	if fd.Kind != "" {
		obj = fd.Kind
	}
	d.SetFont(pdfFont, "", 9)
	d.Write(4.5, d.tr(fmt.Sprintf("Intentional? Exclude this finding by annotating the %s with:", obj)))
	d.Ln(4.5)
	d.SetFont(pdfMono, "", 9)
	d.color(pdfInk)
	d.Write(4.5, d.tr(AnnotationIgnore+": "+fd.Rule))
	d.Ln(4.5)
}

// finding writes one finding, indented behind a bar in its severity's colour.
func (d pdfDoc) finding(fd Finding, docs map[string]string) {
	const bar, indent = 1.2, 4.0
	_, pageH := d.GetPageSize()
	if d.GetY() > pageH-60 { // most findings fit in 45mm; don't split one
		d.AddPage()
	}
	page, y0 := d.PageNo(), d.GetY()
	d.SetLeftMargin(pdfMargin + indent)
	d.SetX(pdfMargin + indent)
	defer d.SetLeftMargin(pdfMargin)

	c := pdfSevere[fd.Severity]
	d.SetFont(pdfFont, "B", 10)
	d.color(c)
	d.Write(5, strings.ToUpper(severityLabel[fd.Severity])+"   ")
	d.color(pdfInk)
	d.Write(5, d.tr(fd.Cluster+" / "+fd.Namespace))
	d.Ln(5)
	d.rich(5, 10, "B", fd.Title)
	d.Ln(6)
	d.color(pdfInk)
	d.rich(5, 10, "", fd.Detail)
	d.Ln(6)
	d.SetFont(pdfFont, "B", 10)
	d.Write(5, "Fix: ")
	d.rich(5, 10, "", fd.Fix)
	d.Ln(6)
	if url := docs[Topic(fd.Rule)]; url != "" {
		d.SetFont(pdfFont, "U", 10)
		d.color(pdfLink)
		d.WriteLinkString(5, "Docs", url)
		d.Ln(6)
	}
	d.ignoreStatement(fd)
	if d.PageNo() == page {
		d.SetFillColor(c.r, c.g, c.b)
		d.Rect(pdfMargin, y0, bar, d.GetY()-y0, "F")
	}
	d.Ln(5)
}
