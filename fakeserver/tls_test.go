package fakeserver

import (
	"crypto/tls"
	"net"
	"testing"
	"time"
)

// NewTLSConfig, the tb-less TLSConfig: its certificate verifies against its pool for
// 127.0.0.1 and localhost.
func TestNewTLSConfig(t *testing.T) {
	server, roots, err := NewTLSConfig()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"127.0.0.1", "localhost"} {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			a, err := ln.Accept()
			if err != nil {
				return
			}
			defer a.Close()
			_ = a.SetDeadline(time.Now().Add(10 * time.Second))
			_ = tls.Server(a, server).Handshake()
		}()
		b, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		_ = b.SetDeadline(time.Now().Add(10 * time.Second))
		err = tls.Client(b, &tls.Config{RootCAs: roots, ServerName: name, MinVersion: tls.VersionTLS12}).Handshake()
		b.Close()
		ln.Close()
		if err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if server.Certificates[0].Leaf == nil {
		t.Error("Leaf not set")
	}
}
