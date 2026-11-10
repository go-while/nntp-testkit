package article_test

import (
	"fmt"
	"testing"

	"github.com/go-while/nntp-testkit/article"
)

// A broken article: an empty Path, a folded Subject and a body line that starts with a dot.
func Example() {
	t := &testing.T{}
	a := article.New(t, article.Options{MessageID: "<ex@testkit.invalid>", Subject: "x"})
	a.Del("Path").AddRaw("Path:")
	a.Set("Subject", "folded\r\n subject")
	a.SetBodyLines("text", ".dotted")

	fmt.Printf("%q\n", a.Lines()[:4])
	fmt.Println(a.Get("subject"), a.Count("Path"))
	// Output:
	// ["From: Test User <test@testkit.invalid>" "Newsgroups: test" "Subject: folded" " subject"]
	// folded subject 1
}
