// Package canlock computes and checks Cancel-Lock and Cancel-Key header values (RFC 8315), for
// tests of servers that honour them: the Cancel-Lock an article carries, the Cancel-Key a cancel
// or supersede must present, and whether a key opens a lock. Only the standard library is used.
//
// Per RFC 8315 section 4, a key is base64(HMAC-H(secret, uid + Message-ID)) and its lock is
// base64(H(key)), where the key is hashed as its base64 text. Header values are lists of
// elements "<scheme>:<base64>"; the schemes here are sha1 and sha256. The results match INN's
// tests/lib/canlock-t.c vectors.
package canlock

import (
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"hash"
	"strings"
)

// Hash is an RFC 8315 scheme name.
type Hash string

// The schemes of RFC 8315 section 7 that this package computes.
const (
	SHA1   Hash = "sha1"
	SHA256 Hash = "sha256"
)

func (h Hash) new() func() hash.Hash {
	switch Hash(strings.ToLower(string(h))) {
	case SHA1:
		return sha1.New
	case SHA256:
		return sha256.New
	}
	return nil
}

func (h Hash) mustNew() func() hash.Hash {
	f := h.new()
	if f == nil {
		panic(fmt.Sprintf("canlock: unknown hash %q (want sha1 or sha256)", string(h)))
	}
	return f
}

// KeyValue returns the bare Cancel-Key value base64(HMAC-h(secret, uid+msgid)). uid is "" for a
// key that only depends on the Message-ID (an administrator's key); msgid includes the angle
// brackets. An unknown h panics.
func KeyValue(h Hash, secret, uid, msgid string) string {
	m := hmac.New(h.mustNew(), []byte(secret))
	m.Write([]byte(uid + msgid))
	return base64.StdEncoding.EncodeToString(m.Sum(nil))
}

// Key returns the Cancel-Key element "<h>:<KeyValue>".
func Key(h Hash, secret, uid, msgid string) string {
	return string(h) + ":" + KeyValue(h, secret, uid, msgid)
}

// LockValue returns the bare Cancel-Lock value base64(h(key)) for a Cancel-Key; key may be a
// bare value or an element "<scheme>:<value>" (the scheme is then dropped, h decides). An
// unknown h panics.
func LockValue(h Hash, key string) string {
	if _, v, ok := strings.Cut(key, ":"); ok {
		key = v
	}
	d := h.mustNew()()
	d.Write([]byte(key))
	return base64.StdEncoding.EncodeToString(d.Sum(nil))
}

// Lock returns the Cancel-Lock element "<h>:<LockValue>".
func Lock(h Hash, key string) string {
	return string(h) + ":" + LockValue(h, key)
}

// Elements splits a Cancel-Lock or Cancel-Key header value into its "<scheme>:<value>"
// elements: whitespace separates them and comments in parentheses are dropped.
func Elements(v string) []string {
	var b strings.Builder
	depth := 0
	for _, r := range v {
		switch {
		case r == '(':
			depth++
		case r == ')' && depth > 0:
			depth--
			b.WriteByte(' ')
		case depth == 0:
			b.WriteRune(r)
		}
	}
	return strings.Fields(b.String())
}

// Verify reports whether a Cancel-Key header value opens a Cancel-Lock header value: some key
// element hashes, with its own scheme, to a lock element of the same scheme. Schemes compare
// without regard to case; elements with unknown schemes are ignored.
func Verify(lock, key string) bool {
	locks := Elements(lock)
	for _, k := range Elements(key) {
		scheme, kv, ok := strings.Cut(k, ":")
		h := Hash(strings.ToLower(scheme))
		if !ok || h.new() == nil {
			continue
		}
		want := LockValue(h, kv)
		for _, l := range locks {
			ls, lv, ok := strings.Cut(l, ":")
			if ok && strings.EqualFold(ls, scheme) && lv == want {
				return true
			}
		}
	}
	return false
}
