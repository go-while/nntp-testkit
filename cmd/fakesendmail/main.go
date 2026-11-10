// Command fakesendmail stands in for sendmail in tests: it records each mail into a directory
// instead of sending it, and exits with a status the test chooses. See package mailrec for the
// directory layout; mailrec.New builds this command for one test.
//
// The record directory is the one built in with -ldflags "-X main.recordDir=DIR" (mailrec.New),
// else the one named by the file fakesendmail.dir next to the executable
// (mailrec.NewFromBinary), else $NNTP_TEST_MAIL_DIR; without any the message is read and
// dropped.
package main

import (
	"os"

	"github.com/go-while/nntp-testkit/mailrec"
)

// recordDir is set at build time by mailrec.New.
var recordDir string

func main() {
	dir := recordDir
	if dir == "" {
		if exe, err := os.Executable(); err == nil {
			dir = mailrec.DirFromMarker(exe)
		}
	}
	os.Exit(mailrec.Main(os.Args, os.Stdin, os.Getenv, dir))
}
