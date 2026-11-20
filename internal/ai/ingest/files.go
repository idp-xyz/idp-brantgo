package ingest

import (
	"errors"
	"strconv"

	"go.idp.xyz/idp-brant/internal/ai/ingest/pdftext"
	"go.idp.xyz/idp-brant/internal/ai/ingest/xlsx"
	evdapi "go.idp.xyz/idp-brant/internal/contexts/evd/api"
)

// The extractor versions of the PDF and XLSX readers.
const (
	PDFExtractor  = "brant.ingest.pdf/" + pdftext.Version
	XLSXExtractor = "brant.ingest.xlsx/1"
)

// Extract reads a business file of one of EVD's media types (text/plain, application/pdf, XLSX)
// under the profile. Any other type is refused with INPUT_PROFILE_UNSUPPORTED.
func (p Profile) Extract(mimeType string, content []byte) (*Extract, error) {
	switch mimeType {
	case evdapi.MimeText:
		return p.ExtractText(content)
	case evdapi.MimePDF:
		return p.ExtractPDF(content)
	case evdapi.MimeXLSX:
		return p.ExtractXLSX(content)
	}
	return nil, p.refuse(LimitType, "")
}

// ExtractPDF reads the text layer of a PDF: one segment per visual line at "page:line", pages
// without text (blank) counted in Pages. Images on a page, invisible text and glyphs without Unicode
// are noted at the page ("3") or line. PDFs that need a password or forbid extracting their text,
// more than MaxPDFPages pages, pages with images and no text (scans), text without a Unicode mapping
// and more than MaxCharacters characters are refused.
func (p Profile) ExtractPDF(content []byte) (*Extract, error) {
	if err := p.checkSize(content); err != nil {
		return nil, err
	}
	doc, err := pdftext.Read(content, pdftext.DefaultLimits(p.MaxPDFPages, p.MaxCharacters))
	if err != nil {
		return nil, p.pdfRefusal(err)
	}
	e := &Extract{Kind: KindPDF, Extractor: PDFExtractor, Profile: p.ID, FileSHA256: sha(content), Pages: len(doc.Pages)}
	for i, page := range doc.Pages {
		if page.Images {
			e.Notes = append(e.Notes, Note{Locator: strconv.Itoa(i + 1), Kind: NoteImagesNotExtracted})
		}
		for j, line := range page.Lines {
			loc := PDFLocator(i+1, j+1)
			e.Segments = append(e.Segments, Segment{Locator: loc, Text: line.Text})
			if line.Invisible {
				e.Notes = append(e.Notes, Note{Locator: loc, Kind: NoteInvisibleText})
			}
			if line.Undecodable {
				e.Notes = append(e.Notes, Note{Locator: loc, Kind: NoteUndecodableGlyphs})
			}
		}
	}
	return p.within(e.seal())
}

func (p Profile) pdfRefusal(err error) error {
	var e *pdftext.Error
	if !errors.As(err, &e) {
		return p.refuse(LimitStructure, "")
	}
	where := ""
	if e.Page > 0 {
		where = strconv.Itoa(e.Page)
	}
	switch e.Reason {
	case pdftext.ReasonType:
		return p.refuse(LimitType, "")
	case pdftext.ReasonEncrypted:
		return p.refuse(LimitEncrypted, "")
	case pdftext.ReasonPages:
		return p.over(LimitPages, "", e.Actual, int64(p.MaxPDFPages), e.AtLeast)
	case pdftext.ReasonTextLayer:
		return p.refuse(LimitTextLayer, where)
	case pdftext.ReasonEncoding:
		return p.refuse(LimitEncoding, where)
	case pdftext.ReasonCharacters:
		// The reader counts glyphs, at most four per character of the profile: far beyond it.
		return p.over(LimitCharacters, where, e.Actual, int64(p.MaxCharacters), true)
	}
	return p.refuse(LimitStructure, where)
}

// ExtractXLSX reads the cell values of a workbook: one segment per cell holding a value at
// "Sheet!A1", sheets in workbook order, cells by row then column. A formula's cached value, a date
// shown from its serial number and an error value are noted at the cell; hidden sheets, rows and
// columns at the sheet ('Sheet'), row (Sheet!5:5) or column (Sheet!C:C); chart sheets, external
// links, embedded objects, drawings and comments at the sheet or the file (""). Macros, formulas
// without a cached value, control characters and workbooks beyond the profile are refused.
func (p Profile) ExtractXLSX(content []byte) (*Extract, error) {
	if err := p.checkSize(content); err != nil {
		return nil, err
	}
	wb, err := xlsx.Read(content, xlsx.DefaultLimits(p.MaxSheets, p.MaxSheetRows, p.MaxSheetColumns, p.MaxCharacters))
	if err != nil {
		return nil, p.xlsxRefusal(err)
	}
	e := &Extract{Kind: KindXLSX, Extractor: XLSXExtractor, Profile: p.ID, FileSHA256: sha(content)}
	for _, n := range []struct {
		on   bool
		kind string
	}{
		{wb.ExternalLinks, NoteExternalLinksNotFollow}, {wb.Embedded, NoteEmbeddedNotOpened},
		{wb.Drawings, NoteDrawingsNotExtracted}, {wb.Comments, NoteCommentsNotExtracted},
	} {
		if n.on {
			e.Notes = append(e.Notes, Note{Kind: n.kind})
		}
	}
	for _, s := range wb.Sheets {
		e.Sheets = append(e.Sheets, s.Name)
		if s.Chartsheet {
			e.Notes = append(e.Notes, Note{Locator: SheetPrefix(s.Name), Kind: NoteChartsheet})
		}
		if s.Hidden {
			e.Notes = append(e.Notes, Note{Locator: SheetPrefix(s.Name), Kind: NoteHiddenSheet})
		}
		for _, c := range s.HiddenColumns {
			e.Notes = append(e.Notes, Note{Locator: columnLocator(s.Name, c), Kind: NoteHiddenColumn})
		}
		for _, r := range s.HiddenRows {
			e.Notes = append(e.Notes, Note{Locator: rowLocator(s.Name, r), Kind: NoteHiddenRow})
		}
		for _, c := range s.Cells {
			loc := CellLocator(s.Name, c.Row, c.Col)
			e.Segments = append(e.Segments, Segment{Locator: loc, Text: c.Value})
			switch {
			case c.Error:
				e.Notes = append(e.Notes, Note{Locator: loc, Kind: NoteErrorValue})
			case c.Date:
				e.Notes = append(e.Notes, Note{Locator: loc, Kind: NoteDateFromSerial})
			}
			if c.Formula {
				e.Notes = append(e.Notes, Note{Locator: loc, Kind: NoteFormulaCachedValue})
			}
		}
	}
	if len(e.Segments) == 0 {
		return nil, p.refuse(LimitEmpty, "")
	}
	return p.within(e.seal())
}

func (p Profile) xlsxRefusal(err error) error {
	var e *xlsx.Error
	if !errors.As(err, &e) {
		return p.refuse(LimitStructure, "")
	}
	where := e.Part
	switch {
	case e.Row > 0:
		where = CellLocator(e.Sheet, e.Row, e.Col)
	case e.Sheet != "":
		where = SheetPrefix(e.Sheet)
	}
	limit := map[xlsx.Reason]Limit{
		xlsx.ReasonType: LimitType, xlsx.ReasonMacros: LimitMacros, xlsx.ReasonSheets: LimitSheets,
		xlsx.ReasonRows: LimitRows, xlsx.ReasonColumns: LimitColumns, xlsx.ReasonCharacters: LimitCharacters,
		xlsx.ReasonFormula: LimitFormula, xlsx.ReasonEncoding: LimitEncoding,
	}[e.Reason]
	if limit == "" {
		limit = LimitStructure
	}
	if e.Max > 0 {
		return p.over(limit, where, e.Actual, e.Max, e.AtLeast)
	}
	return p.refuse(limit, where)
}

// within refuses an extract with more characters than the profile allows.
func (p Profile) within(e *Extract) (*Extract, error) {
	if e.Characters > p.MaxCharacters {
		return nil, p.over(LimitCharacters, "", int64(e.Characters), int64(p.MaxCharacters), false)
	}
	return e, nil
}
