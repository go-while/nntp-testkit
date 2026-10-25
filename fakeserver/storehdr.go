package fakeserver

import (
	"strconv"
	"strings"
)

// hdr answers HDR (RFC 3977 §8.5: 225) and XHDR (RFC 2980 §2.6: 221), "field [range|<id>]".
// A line is "number value"; by message-id HDR uses number 0 and XHDR the message-id itself.
// An absent header is an empty value for HDR and "(none)" for XHDR, as INN's nnrpd does.
// Metadata ":bytes" and ":lines" are computed as for OVER; "Xref" is the served Xref value.
// StoreOptions.HdrLines may rewrite the lines before they are sent.
func (h *storeConn) hdr(cmd string, args []string) []string {
	head, missing := "225 Headers follow", ""
	if cmd == "XHDR" {
		head, missing = "221 Header follows", "(none)"
	}
	field := args[0]
	var lines []string
	group := h.group
	if len(args) == 2 && strings.HasPrefix(args[1], "<") {
		a, locs, ok := h.st.ByMessageID(args[1])
		if !ok {
			return []string{"430 No article with that message-id"}
		}
		num := "0"
		if cmd == "XHDR" {
			num = a.MessageID
		}
		group = locs[0].Group
		lines = []string{h.hdrLine(num, field, a, group, missing)}
	} else {
		if h.group == "" {
			return []string{"412 No newsgroup selected"}
		}
		var arts []Article
		if len(args) == 1 {
			a, ok := h.st.Article(h.group, h.cur)
			if h.cur == 0 || !ok {
				return []string{"420 Current article number is invalid"}
			}
			arts = []Article{a}
		} else {
			r, ok := parseRange(args[1])
			if !ok {
				return []string{"501 Syntax error in range"}
			}
			if r.open {
				g, _ := h.st.Group(h.group)
				r.to, r.open = max(g.High, 0), false
			}
			if lim := h.st.GenLimit(h.group); lim > 0 {
				g, _ := h.st.Group(h.group)
				if span := min(r.to, g.High) - max(r.from, g.Low) + 1; span > lim {
					return []string{genTooLarge(span, lim)}
				}
			}
			if r.to >= r.from && r.to > 0 {
				arts = h.st.Overview(h.group, r.from, r.to)
			}
			if len(arts) == 0 {
				return []string{"423 No articles in that range"}
			}
		}
		for _, a := range arts {
			lines = append(lines, h.hdrLine(strconv.FormatInt(a.Number, 10), field, a, h.group, missing))
		}
	}
	if h.o.HdrLines != nil {
		lines = h.o.HdrLines(h.c.ID(), group, field, lines)
	}
	return append(append([]string{head}, lines...), ".")
}

// hdrLine formats one HDR/XHDR line of a for field.
func (h *storeConn) hdrLine(num, field string, a Article, group, missing string) string {
	a, xref := h.served(a, group)
	var v string
	switch strings.ToLower(field) {
	case ":bytes":
		n := 2 // the blank line
		for _, l := range a.Headers {
			n += len(l) + 2
		}
		for _, l := range a.Body {
			n += len(l) + 2
		}
		v = strconv.Itoa(n)
	case ":lines":
		v = strconv.Itoa(len(a.Body))
	case "xref":
		v = xref
	default:
		if v = a.Header(field); v == "" {
			v = missing
		}
	}
	clean := strings.NewReplacer("\t", " ", "\r", " ", "\n", " ", "\x00", " ")
	return num + " " + clean.Replace(v)
}
