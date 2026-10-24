package static

import (
	"strconv"
	"strings"
)

// Candidate is one entry of a srcset attribute.
type Candidate struct {
	URL     string
	Width   float64 // from "w" descriptors
	Density float64 // from "x" descriptors
}

// ParseSrcset parses a srcset attribute value per the HTML specification.
func ParseSrcset(s string) []Candidate {
	var out []Candidate
	i := 0
	for i < len(s) {
		for i < len(s) && (isSpace(s[i]) || s[i] == ',') {
			i++
		}
		if i >= len(s) {
			break
		}
		start := i
		for i < len(s) && !isSpace(s[i]) {
			i++
		}
		u := s[start:i]
		var desc string
		if strings.HasSuffix(u, ",") {
			u = strings.TrimRight(u, ",")
		} else {
			dstart := i
			depth := 0
			for i < len(s) {
				c := s[i]
				if c == '(' {
					depth++
				} else if c == ')' && depth > 0 {
					depth--
				} else if c == ',' && depth == 0 {
					break
				}
				i++
			}
			desc = strings.TrimSpace(s[dstart:i])
		}
		if u == "" {
			continue
		}
		c := Candidate{URL: u, Density: 1}
		for _, d := range strings.Fields(desc) {
			if len(d) < 2 {
				continue
			}
			v, err := strconv.ParseFloat(d[:len(d)-1], 64)
			if err != nil {
				continue
			}
			switch d[len(d)-1] {
			case 'w':
				c.Width = v
			case 'x':
				c.Density = v
			}
		}
		out = append(out, c)
	}
	return out
}

// Best returns the largest candidate, preferring width over density.
func Best(cands []Candidate) (Candidate, bool) {
	if len(cands) == 0 {
		return Candidate{}, false
	}
	best := cands[0]
	for _, c := range cands[1:] {
		if c.Width > best.Width || (c.Width == best.Width && c.Density > best.Density) {
			best = c
		}
	}
	return best, true
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f'
}
