package converter

import (
	"bytes"
	"strings"

	htmltomarkdown "github.com/JohannesKaufmann/html-to-markdown/v2"
	"github.com/JohannesKaufmann/html-to-markdown/v2/converter"
	wkhtmltopdf "github.com/SebastiaanKlippert/go-wkhtmltopdf"
	mdtopdf "github.com/solworktech/md2pdf/v2"
)

// ToMarkdown converts HTML to Markdown, resolving relative links against baseURL.
func ToMarkdown(html, baseURL string) (string, error) {
	return htmltomarkdown.ConvertString(html, converter.WithDomain(baseURL))
}

// ToPDF converts HTML directly to PDF bytes via wkhtmltopdf.
func ToPDF(html string) ([]byte, error) {
	pdfg, err := wkhtmltopdf.NewPDFGenerator()
	if err != nil {
		return nil, err
	}
	pdfg.AddPage(wkhtmltopdf.NewPageReader(strings.NewReader(html)))
	if err := pdfg.Create(); err != nil {
		return nil, err
	}
	return pdfg.Bytes(), nil
}

// ToPDFSimplified converts HTML to Markdown first, then to PDF, producing a
// cleaner debloated document.
func ToPDFSimplified(html, baseURL string) ([]byte, error) {
	md, err := ToMarkdown(html, baseURL)
	if err != nil {
		return nil, err
	}

	renderer := mdtopdf.NewPdfRenderer(mdtopdf.PdfRendererParams{
		Theme: mdtopdf.LIGHT,
	})
	if err := renderer.Run([]byte(md)); err != nil {
		return nil, err
	}

	var buf bytes.Buffer
	if err := renderer.Pdf.Output(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
