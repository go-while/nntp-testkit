package canlock

import "testing"

// INN's tests/lib/canlock-t.c vectors.
const (
	mid   = "<12345@mid.example>"
	user  = "JaneDoe"
	admin = "ExampleSecret"
	usec  = "AnotherSecret"
)

func TestVectors(t *testing.T) {
	for _, c := range []struct {
		name, got, want string
	}{
		{"sha1 admin key", Key(SHA1, admin, "", mid), "sha1:8HzrY7F4N+5SXkGQah1mcyW+01g="},
		{"sha256 admin key", Key(SHA256, admin, "", mid), "sha256:qv1VXHYiCGjkX/N1nhfYKcAeUn8bCVhrWhoKuBSnpMA="},
		{"sha1 user key", Key(SHA1, usec, user, mid), "sha1:mHkFFHF4DM97Oi+8vEsCXQb4aM0="},
		{"sha256 user key", Key(SHA256, usec, user, mid), "sha256:yM0ep490Fzt83CLYYAytm3S2HasHhYG4LAeAlmuSEys="},
		{"sha1 admin lock", Lock(SHA1, Key(SHA1, admin, "", mid)), "sha1:JD+QmQh5LH6lLLToKLcDl+Aemg0="},
		{"sha256 admin lock", Lock(SHA256, Key(SHA256, admin, "", mid)), "sha256:s/pmK/3grrz++29ce2/mQydzJuc7iqHn1nqcJiQTPMc="},
		{"sha1 user lock", Lock(SHA1, KeyValue(SHA1, usec, user, mid)), "sha1:zdoRY4lJw5jLLtJTfpEqu1epqzc="},
		{"sha256 user lock", Lock(SHA256, KeyValue(SHA256, usec, user, mid)), "sha256:NSBTz7BfcQFTCen+U4lQ0VS8VIlZao2b8mxD/xJaaeE="},
	} {
		if c.got != c.want {
			t.Errorf("%s: %s, want %s", c.name, c.got, c.want)
		}
	}
}

func TestVerify(t *testing.T) {
	k1 := Key(SHA1, admin, "", mid)
	k256 := Key(SHA256, usec, user, mid)
	lock := Lock(SHA1, k1) + " (admin)\r\n\t" + Lock(SHA256, k256)
	for _, c := range []struct {
		name, key string
		want      bool
	}{
		{"sha1 key", k1, true},
		{"sha256 key", k256, true},
		{"scheme in upper case", "SHA256:" + KeyValue(SHA256, usec, user, mid), true},
		{"several keys, one fits", "md5:xyz " + Key(SHA256, "wrong", user, mid) + " " + k1, true},
		{"wrong secret", Key(SHA1, "wrong", "", mid), false},
		{"other Message-ID", Key(SHA256, usec, user, "<other@mid.example>"), false},
		{"right value, wrong scheme", "sha256:" + KeyValue(SHA1, admin, "", mid), false},
		{"unknown scheme", "md5:" + KeyValue(SHA1, admin, "", mid), false},
		{"empty", "", false},
		{"no scheme", KeyValue(SHA1, admin, "", mid), false},
	} {
		if got := Verify(lock, c.key); got != c.want {
			t.Errorf("%s: Verify(%q, %q) = %v, want %v", c.name, lock, c.key, got, c.want)
		}
	}
}

func TestElements(t *testing.T) {
	got := Elements(" sha1:a (a comment (nested) here)\r\n\tsha256:b(x)sha1:c ")
	want := []string{"sha1:a", "sha256:b", "sha1:c"}
	if len(got) != len(want) {
		t.Fatalf("Elements = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Elements = %q, want %q", got, want)
		}
	}
}

func TestUnknownHashPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("Key with hash md5 did not panic")
		}
	}()
	Key("md5", admin, "", mid)
}
