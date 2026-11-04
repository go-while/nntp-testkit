package dotstuff

import (
	"slices"
	"testing"
)

func TestAppend(t *testing.T) {
	got := Append([]string{"220 1 <a@b> Article follows"}, []string{"a", ".", "..b", "", ".c"})
	want := []string{"220 1 <a@b> Article follows", "a", "..", "...b", "", "..c"}
	if !slices.Equal(got, want) {
		t.Errorf("Append = %q, want %q", got, want)
	}
	if got := Append(nil, nil); got != nil {
		t.Errorf("Append(nil, nil) = %q, want nil", got)
	}
}
