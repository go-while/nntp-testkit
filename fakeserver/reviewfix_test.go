package fakeserver

import (
	"testing"

	"github.com/go-while/nntp-testkit/client"
)

// A POST to a generated group gets 441 before anything is filed (it used to panic in the
// connection goroutine and end the test process), also in a crosspost with a normal group.
func TestStorePostGeneratedGroup(t *testing.T) {
	st := testStore()
	st.AddGenerated("gen.big", "", Generator{Low: 1, High: 100})
	_, c := storeServer(t, st, StoreOptions{}, Faults{})
	for _, ng := range []string{"gen.big", "alt.test,gen.big"} {
		wantCode(t, c, "POST", 340)
		if err := c.Send("Newsgroups: " + ng + "\r\nSubject: s\r\nFrom: f\r\nMessage-ID: <g@x>\r\n\r\nb\r\n.\r\n"); err != nil {
			t.Fatal(err)
		}
		if line := wantStatus(t, c, 441); line != "441 Posting failed: gen.big is a generated group" {
			t.Errorf("Newsgroups %q: %q", ng, line)
		}
	}
	if n := len(st.Posted()); n != 0 {
		t.Errorf("Posted() = %d", n)
	}
	if _, _, ok := st.ByMessageID("<g@x>"); ok {
		t.Error("the refused article was filed")
	}
	wantCode(t, c, "DATE", 111)
}

// An overridden final first reply to POST with OverrideTimes uses one allowance per POST, and
// the server does not wait for an article after it.
func TestStorePostOverrideTimes(t *testing.T) {
	f := Faults{Override: map[string][]string{"POST": {"440 not now"}}, OverrideTimes: map[string]int{"POST": 2}}
	s, c := storeServer(t, testStore(), StoreOptions{}, f)
	wantCode(t, c, "POST", 440)
	wantCode(t, c, "DATE", 111) // used to be read as article data
	wantCode(t, c, "POST", 440) // the second allowance
	wantCode(t, c, "POST", 340) // the override is used up
	if err := c.Send("Newsgroups: alt.test\r\nSubject: s\r\nFrom: f\r\n\r\nb\r\n.\r\n"); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, c, 240)
	if got := s.Count("POST"); got != 3 {
		t.Errorf("Count(POST) = %d", got)
	}
}

// The Store password is compared byte for byte: repeated spaces count, a tab is not a space.
func TestStoreAuthPasswordVerbatim(t *testing.T) {
	for _, tc := range []struct {
		pass, sent string
		want       int
	}{
		{"p  q", "p  q", 281},
		{"p q", "p q", 281},
		{"p q", "p  q", 481},
		{"p q", "p\tq", 481},
		{"p q", "p q ", 481},
		{"p", "p", 281},
	} {
		_, c := storeServer(t, testStore(), StoreOptions{User: "u", Pass: tc.pass}, Faults{})
		wantCode(t, c, "AUTHINFO USER u", 381)
		if line := wantCode(t, c, "AUTHINFO PASS "+tc.sent, tc.want); line == "" {
			t.Errorf("pass %q sent %q", tc.pass, tc.sent)
		}
	}
	// the separators between the words before the value may be any run of spaces and tabs
	_, c := storeServer(t, testStore(), StoreOptions{User: "u", Pass: "p"}, Faults{})
	wantCode(t, c, "authinfo\tuser u", 381)
	wantCode(t, c, "AUTHINFO  PASS p", 281)
}

// PeerHandler splits the verb at any space or tab and trims the message-id, like the Faults
// keys and Counts do.
func TestPeerVerbSplit(t *testing.T) {
	s := New(t, Options{Handler: PeerHandler(&PeerOptions{Have: map[string]bool{"<h@x>": true}})})
	c := client.Dial(t, s.Addr())
	if line := wantCode(t, c, "check\t<a@b>", 238); line != "238 <a@b>" {
		t.Errorf("tab: %q", line)
	}
	if line := wantCode(t, c, "CHECK  <h@x>", 438); line != "438 <h@x>" {
		t.Errorf("two spaces: %q", line)
	}
	if got := s.Count("CHECK"); got != 2 {
		t.Errorf("Count(CHECK) = %d", got)
	}
}
