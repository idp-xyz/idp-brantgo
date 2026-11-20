package pdftext

import (
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// glyphList maps the glyph names of the Latin text encodings (StandardEncoding, WinAnsiEncoding,
// MacRomanEncoding; Adobe Glyph List names) and a few common symbols to Unicode. Letters and digits
// are added by init; uniXXXX and uXXXX[XX] names are read by glyphRune; other names are unknown.
const glyphList = `space 20 exclam 21 quotedbl 22 numbersign 23 dollar 24 percent 25 ampersand 26 quotesingle 27
parenleft 28 parenright 29 asterisk 2A plus 2B comma 2C hyphen 2D period 2E slash 2F colon 3A semicolon 3B
less 3C equal 3D greater 3E question 3F at 40 bracketleft 5B backslash 5C bracketright 5D asciicircum 5E
underscore 5F grave 60 braceleft 7B bar 7C braceright 7D asciitilde 7E
Euro 20AC quotesinglbase 201A florin 192 quotedblbase 201E ellipsis 2026 dagger 2020 daggerdbl 2021
circumflex 2C6 perthousand 2030 Scaron 160 guilsinglleft 2039 OE 152 Zcaron 17D quoteleft 2018 quoteright 2019
quotedblleft 201C quotedblright 201D bullet 2022 endash 2013 emdash 2014 tilde 2DC trademark 2122 scaron 161
guilsinglright 203A oe 153 zcaron 17E Ydieresis 178 nbspace A0 nonbreakingspace A0 exclamdown A1 cent A2
sterling A3 currency A4 yen A5 brokenbar A6 section A7 dieresis A8 copyright A9 ordfeminine AA guillemotleft AB
logicalnot AC sfthyphen AD registered AE macron AF degree B0 plusminus B1 twosuperior B2 threesuperior B3
acute B4 mu B5 paragraph B6 periodcentered B7 middot B7 cedilla B8 onesuperior B9 ordmasculine BA
guillemotright BB onequarter BC onehalf BD threequarters BE questiondown BF Agrave C0 Aacute C1 Acircumflex C2
Atilde C3 Adieresis C4 Aring C5 AE C6 Ccedilla C7 Egrave C8 Eacute C9 Ecircumflex CA Edieresis CB Igrave CC
Iacute CD Icircumflex CE Idieresis CF Eth D0 Ntilde D1 Ograve D2 Oacute D3 Ocircumflex D4 Otilde D5
Odieresis D6 multiply D7 Oslash D8 Ugrave D9 Uacute DA Ucircumflex DB Udieresis DC Yacute DD Thorn DE
germandbls DF agrave E0 aacute E1 acircumflex E2 atilde E3 adieresis E4 aring E5 ae E6 ccedilla E7 egrave E8
eacute E9 ecircumflex EA edieresis EB igrave EC iacute ED icircumflex EE idieresis EF eth F0 ntilde F1
ograve F2 oacute F3 ocircumflex F4 otilde F5 odieresis F6 divide F7 oslash F8 ugrave F9 uacute FA
ucircumflex FB udieresis FC yacute FD thorn FE ydieresis FF fraction 2044 fi FB01 fl FB02 ff FB00 ffi FB03
ffl FB04 dotlessi 131 Lslash 141 lslash 142 breve 2D8 dotaccent 2D9 ring 2DA hungarumlaut 2DD ogonek 2DB
caron 2C7 minus 2212 Delta 2206 Omega 2126 pi 3C0 approxequal 2248 notequal 2260 lessequal 2264
greaterequal 2265 infinity 221E partialdiff 2202 summation 2211 product 220F integral 222B radical 221A
lozenge 25CA multiply D7 numero 2116 Ohm 2126 estimated 212E`

var glyphNames = map[string]rune{}

func init() {
	f := strings.Fields(glyphList)
	for i := 0; i+1 < len(f); i += 2 {
		v, err := strconv.ParseUint(f[i+1], 16, 32)
		if err != nil {
			panic("pdftext: glyph list " + f[i])
		}
		glyphNames[f[i]] = rune(v)
	}
	for c := 'A'; c <= 'Z'; c++ {
		glyphNames[string(c)] = c
		glyphNames[string(c+'a'-'A')] = c + 'a' - 'A'
	}
	for i, n := range []string{"zero", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine"} {
		glyphNames[n] = rune('0' + i)
	}
}

// glyphText maps a glyph name to text as the Adobe Glyph List specification does: the part before
// the first period, components joined by underscores, each a list name, uniXXXX (one or more code
// units) or uXXXX to uXXXXXX. An unknown component makes the whole name unknown.
func glyphText(name string) (string, bool) {
	if i := strings.IndexByte(name, '.'); i >= 0 {
		name = name[:i]
	}
	if name == "" {
		return "", false
	}
	var b strings.Builder
	for _, part := range strings.Split(name, "_") {
		if r, ok := glyphNames[part]; ok {
			b.WriteRune(r)
			continue
		}
		switch {
		case strings.HasPrefix(part, "uni") && len(part) >= 7 && (len(part)-3)%4 == 0:
			for i := 3; i < len(part); i += 4 {
				r, ok := hexRune(part[i:i+4], true)
				if !ok {
					return "", false
				}
				b.WriteRune(r)
			}
		case strings.HasPrefix(part, "u") && len(part) >= 5 && len(part) <= 7:
			r, ok := hexRune(part[1:], false)
			if !ok {
				return "", false
			}
			b.WriteRune(r)
		default:
			return "", false
		}
	}
	return b.String(), true
}

// hexRune reads upper-case hex digits as a Unicode scalar value (no surrogates).
func hexRune(s string, bmp bool) (rune, bool) {
	if strings.ToUpper(s) != s {
		return 0, false
	}
	v, err := strconv.ParseUint(s, 16, 32)
	r := rune(v)
	if err != nil || (r >= 0xD800 && r <= 0xDFFF) || r > unicode.MaxRune || (bmp && r > 0xFFFF) || !utf8.ValidRune(r) {
		return 0, false
	}
	return r, true
}

// standardHigh is StandardEncoding from 0xA1 (ISO 32000-1 Annex D); 0x20 to 0x7E are ASCII except
// 0x27 (quoteright) and 0x60 (quoteleft).
var standardHigh = map[byte]rune{
	0xA1: 0xA1, 0xA2: 0xA2, 0xA3: 0xA3, 0xA4: 0x2044, 0xA5: 0xA5, 0xA6: 0x192, 0xA7: 0xA7, 0xA8: 0xA4,
	0xA9: 0x27, 0xAA: 0x201C, 0xAB: 0xAB, 0xAC: 0x2039, 0xAD: 0x203A, 0xAE: 0xFB01, 0xAF: 0xFB02,
	0xB1: 0x2013, 0xB2: 0x2020, 0xB3: 0x2021, 0xB4: 0xB7, 0xB6: 0xB6, 0xB7: 0x2022, 0xB8: 0x201A,
	0xB9: 0x201E, 0xBA: 0x201D, 0xBB: 0xBB, 0xBC: 0x2026, 0xBD: 0x2030, 0xBF: 0xBF, 0xC1: 0x60, 0xC2: 0xB4,
	0xC3: 0x2C6, 0xC4: 0x2DC, 0xC5: 0xAF, 0xC6: 0x2D8, 0xC7: 0x2D9, 0xC8: 0xA8, 0xCA: 0x2DA, 0xCB: 0xB8,
	0xCD: 0x2DD, 0xCE: 0x2DB, 0xCF: 0x2C7, 0xD0: 0x2014, 0xE1: 0xC6, 0xE3: 0xAA, 0xE8: 0x141, 0xE9: 0xD8,
	0xEA: 0x152, 0xEB: 0xBA, 0xF1: 0xE6, 0xF5: 0x131, 0xF8: 0x142, 0xF9: 0xF8, 0xFA: 0x153, 0xFB: 0xDF,
}

func standardRune(c byte) (rune, bool) {
	switch {
	case c == 0x27:
		return 0x2019, true
	case c == 0x60:
		return 0x2018, true
	case c >= 0x20 && c <= 0x7E:
		return rune(c), true
	}
	r, ok := standardHigh[c]
	return r, ok
}

// pdfDocHigh is PDFDocEncoding 0x80 to 0x9F (ISO 32000-1 Annex D); 0xA0 and up are Latin-1.
var pdfDocHigh = [32]rune{
	0x2022, 0x2020, 0x2021, 0x2026, 0x2014, 0x2013, 0x192, 0x2044, 0x2039, 0x203A, 0x2212, 0x2030, 0x201E,
	0x201C, 0x201D, 0x2018, 0x2019, 0x201A, 0x2122, 0xFB01, 0xFB02, 0x141, 0x152, 0x160, 0x178, 0x17D, 0x131,
	0x142, 0x153, 0x161, 0x17E, 0xFFFD,
}

func pdfDocRune(c byte) (rune, bool) {
	switch {
	case c >= 0x20 && c <= 0x7E, c >= 0xA1:
		return rune(c), true
	case c >= 0x80 && c <= 0x9F:
		r := pdfDocHigh[c-0x80]
		return r, r != 0xFFFD
	}
	return 0, false
}
