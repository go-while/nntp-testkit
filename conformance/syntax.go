package conformance

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// syntaxVariant is one spelling of a command the server must treat like its plain form.
type syntaxVariant struct {
	sent  string // the line without CRLF, as sent
	plain string // the plain spelling whose reply code is the expected one
}

// showSent formats a command line for a Detail: quoted, TAB shown as <TAB>.
func showSent(line string) string {
	return `"` + strings.ReplaceAll(line, "\t", "<TAB>") + `"`
}

// checkCommandSyntax is RFC 3977 §3.1: keywords and arguments are separated by one or more SP
// or TAB, and the case of a command keyword is ignored. Each variant is sent on one connection
// and its reply code compared with the plain spelling's. Only what the RFC requires is sent:
// no leading or trailing white space, no other separators, no control bytes, no long lines
// (servers differ there, and the RFC does not require either).
func checkCommandSyntax(s *session, c check) Result {
	if r, ok := s.ready(c); !ok {
		return r
	}
	t, r, ok := s.pickTarget(c)
	if !ok {
		return r
	}
	start := time.Now()
	var fails []string
	var total int

	// try sends line and reads the reply (a multi-line one fully); fatal is a wire or I/O error
	// after which the connection cannot be used.
	try := func(line string) (code int, reply string, fatal error) {
		code, reply, err := s.cmd("%s", line)
		if err != nil {
			return code, reply, fmt.Errorf("%s: %s", showSent(line), describe(err))
		}
		switch code {
		case 215, 220, 221, 222:
			if _, err := s.block(); err != nil {
				return code, reply, fmt.Errorf("%s: block after %q: %s", showSent(line), reply, describe(err))
			}
		}
		return code, reply, nil
	}
	// run sends the plain spelling, then each variant. A plain form that does not answer 2xx is
	// not this check's business (see the article, group and list-active checks): its variants
	// are not compared.
	run := func(plain string, variants []syntaxVariant) (skipWhy string, fatal error) {
		pcode, preply, err := try(plain)
		if err != nil {
			return "", err
		}
		if pcode < 200 || pcode > 299 {
			return fmt.Sprintf("%s got %q: nothing to compare the variants with", showSent(plain), preply), nil
		}
		for _, v := range variants {
			if time.Since(start) > s.o.deadline() {
				return "", fmt.Errorf("not finished within the hang guard (%v)", s.o.deadline())
			}
			code, reply, err := try(v.sent)
			if err != nil {
				return "", err
			}
			total++
			if code != pcode {
				fails = append(fails, fmt.Sprintf("%s got %q, but %s got %q", showSent(v.sent), reply, showSent(v.plain), preply))
			}
		}
		return "", nil
	}
	id := t.id
	stat := "STAT " + id
	space5 := strings.Repeat(" ", 5)
	statVariants := []syntaxVariant{
		{"STAT  " + id, stat},
		{"STAT" + space5 + id, stat},
		{"STAT\t" + id, stat},
		{"STAT\t\t" + id, stat},
		{"STAT \t " + id, stat},
		{"stat " + id, stat},
		{"StAt " + id, stat},
		{"stat  " + id, stat},
	}
	article := "ARTICLE " + id
	articleVariants := []syntaxVariant{{"ARTICLE  " + id, article}, {"article\t" + id, article}}
	group := "GROUP " + t.g.name
	groupVariants := []syntaxVariant{
		{"GROUP  " + t.g.name, group},
		{"GROUP\t\t" + t.g.name, group},
		{"group " + t.g.name, group},
	}
	steps := []struct {
		plain    string
		variants []syntaxVariant
	}{{stat, statVariants}, {article, articleVariants}, {group, groupVariants}}
	if slices.ContainsFunc(capArgs(s.caps, "LIST"), func(a string) bool { return strings.EqualFold(a, "ACTIVE") }) {
		list := "LIST ACTIVE " + t.g.name
		steps = append(steps, struct {
			plain    string
			variants []syntaxVariant
		}{list, []syntaxVariant{
			{"LIST  ACTIVE  " + t.g.name, list},
			{"LIST\tACTIVE\t" + t.g.name, list},
			{"list active " + t.g.name, list},
		}})
	}
	skipped := ""
	for _, st := range steps {
		why, err := run(st.plain, st.variants)
		if err != nil {
			return c.fail(err.Error())
		}
		if why != "" && skipped == "" {
			skipped = why
		}
	}
	if len(fails) > 0 {
		return c.failf("RFC 3977 §3.1 (one or more SP or TAB between words, keywords case-insensitive): %s", strings.Join(fails, "; "))
	}
	if total == 0 {
		return c.skip(skipped)
	}
	if err := s.quiet(quietWindow); err != nil {
		return c.failf("after the replies: %s", describe(err))
	}
	return c.pass()
}
