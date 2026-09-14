package billing

import "testing"

func TestParseAndFormatNanoUSD(t *testing.T) {
	cases := map[string]int64{"0": 0, "1": 1_000_000_000, "1.2": 1_200_000_000, "0.000000001": 1, "10.000000000": 10_000_000_000}
	for in, want := range cases {
		got, err := ParseNanoUSD(in)
		if err != nil || got != want {
			t.Fatalf("%q=%d,%v", in, got, err)
		}
		if got >= 0 && FormatDecimal(got) == "" {
			t.Fatal("empty")
		}
	}
	for _, bad := range []string{"", "-1", "1.0000000001", "NaN", "1e-3", ".1"} {
		if _, err := ParseNanoUSD(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}
