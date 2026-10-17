// Command echo-server is the test fixture of package proc: a minimal line server that greets
// with "200 proc-echo ready", answers every line with "500 echo <line>" and QUIT with
// "205 bye". Without -listen it serves stdin/stdout (inetd style) and exits on stdin EOF.
// It exits 0 on SIGTERM (unless -ignore-term) and ignores SIGHUP.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
)

var (
	logMu   sync.Mutex
	logFile *os.File
)

func logf(format string, args ...any) {
	logMu.Lock()
	defer logMu.Unlock()
	if logFile != nil {
		fmt.Fprintf(logFile, format+"\n", args...)
	}
}

func main() {
	listen := flag.String("listen", "", "TCP listen address (default: serve stdin/stdout)")
	logPath := flag.String("log", "", "file that receives every command")
	ignoreTerm := flag.Bool("ignore-term", false, "ignore SIGTERM")
	flag.Parse()

	if *logPath != "" {
		f, err := os.OpenFile(*logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "log: %v\n", err)
			os.Exit(1)
		}
		logFile = f
	}

	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		for sig := range sigs {
			fmt.Fprintf(os.Stderr, "signal %v\n", sig)
			if sig == syscall.SIGTERM && !*ignoreTerm {
				os.Exit(0)
			}
		}
	}()

	fmt.Fprintln(os.Stderr, "started")

	if *listen == "" {
		serve(os.Stdin, os.Stdout)
		os.Exit(0)
	}
	ln, err := net.Listen("tcp", *listen)
	if err != nil {
		fmt.Fprintf(os.Stderr, "listen: %v\n", err)
		os.Exit(1)
	}
	for {
		c, err := ln.Accept()
		if err != nil {
			fmt.Fprintf(os.Stderr, "accept: %v\n", err)
			os.Exit(1)
		}
		go func() {
			defer c.Close()
			serve(c, c)
		}()
	}
}

// serve greets and answers lines until QUIT or EOF.
func serve(r io.Reader, w io.Writer) {
	bw := bufio.NewWriter(w)
	reply := func(line string) bool {
		if _, err := bw.WriteString(line + "\r\n"); err != nil {
			return false
		}
		return bw.Flush() == nil
	}
	if !reply("200 proc-echo ready") {
		return
	}
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		logf("%s", line)
		if strings.EqualFold(line, "QUIT") {
			reply("205 bye")
			return
		}
		if !reply("500 echo " + line) {
			return
		}
	}
}
