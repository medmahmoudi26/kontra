package vectors

import "fmt"

// Every family is a pure function of one integer, which is what makes a MATCHED CONTROL
// possible: the control for a vector is the same construction applied to value+1.
//
// This matters more than it looks. Comparing "/%DC%8A" against "/" makes every target
// that echoes the path — every reflective 404 on the internet — look vulnerable, because
// the bodies differ for a reason that has nothing to do with folding. Comparing it
// against "/%DC%8B" (U+070B) holds path length, encoding shape and byte count fixed and
// varies exactly one thing: whether the codepoint's low byte is a newline.
//
// Values ending in 0x0A/0x0D never sit on a UTF-8 length boundary (those end in 0x00),
// so value+1 is always the same wire length as value.

func encRaw(v rune) string { return pct([]byte{byte(v)}) }

func encFold(cp rune) string { return pct([]byte(string(cp))) }

func encOverlong(n int, v rune) string {
	b := byte(v)
	switch n {
	case 2:
		return pct([]byte{0xC0 | b>>6, 0x80 | (b & 0x3F)})
	case 3:
		return pct([]byte{0xE0, 0x80 | b>>6, 0x80 | (b & 0x3F)})
	default:
		return pct([]byte{0xF0, 0x80, 0x80 | b>>6, 0x80 | (b & 0x3F)})
	}
}

// encDouble builds the forms that only become a control character after a SECOND decode
// pass — the classic two-tier proxy/app split, where the edge decodes once and the
// application decodes again.
func encDouble(form int, v rune) string {
	const hexdig = "0123456789ABCDEF"
	hi, lo := hexdig[byte(v)>>4], hexdig[byte(v)&0x0F]
	switch form {
	case 1:
		return "%25" + string(hi) + string(lo) // %250A -> %0A -> LF
	case 2:
		return "%25" + pctByte(hi) + pctByte(lo) // %25%30%41
	default:
		return "%" + pctByte(hi) + pctByte(lo) // %%30%41
	}
}

func pctByte(c byte) string { return fmt.Sprintf("%%%02X", c) }
