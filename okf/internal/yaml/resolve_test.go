package yaml

import "testing"

func TestResolveKeepsIntegersWhole(t *testing.T) {
	for in, want := range map[string]interface{}{
		"-0o17":              -15,
		"-0b101":             -5,
		"0o17":               15,
		"0xffffffffffffffff": uint64(1<<64 - 1),
	} {
		if _, got := resolve("", in); got != want {
			t.Errorf("%s resolved to %#v, want %#v", in, got, want)
		}
	}
}
