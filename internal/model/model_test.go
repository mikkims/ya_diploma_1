package model

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func TestMoney(t *testing.T) {
	for _, tt := range []struct {
		input string
		want  Money
	}{{"0", 0}, {"0.01", 1}, {"500.5", 50050}, {"1e2", 10000}, {"1.2300", 123},
		{"92233720368547758.07", Money(math.MaxInt64)}} {
		var m Money
		if err := json.Unmarshal([]byte(tt.input), &m); err != nil || m != tt.want {
			t.Errorf("%s: amount %d, err %v", tt.input, m, err)
		}
		data, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		var roundtrip Money
		if err := json.Unmarshal(data, &roundtrip); err != nil || roundtrip != m {
			t.Fatalf("roundtrip: %v", err)
		}
	}
	for _, input := range []string{"-1", "null", "true", `"1"`, "0.001", "92233720368547758.08", "1e999999999", "1e-999999999", "[]", "1/2", ""} {
		var m Money
		if err := m.UnmarshalJSON([]byte(input)); err == nil {
			t.Errorf("accepted %q", input)
		}
	}
	if _, err := Money(-1).MarshalJSON(); err == nil {
		t.Fatal("negative marshalled")
	}
}

func TestValidOrder(t *testing.T) {
	for _, tt := range []struct {
		number string
		valid  bool
	}{{"12345678903", true}, {"00012345678903", true}, {strings.Repeat("0", 4096), true},
		{"0", true}, {"79927398713", true}, {"", false}, {"12345678904", false},
		{"+0", false}, {"0\n", false}, {"０", false}, {"12a", false}} {
		if got := ValidOrder(tt.number); got != tt.valid {
			t.Errorf("Luhn(%q)=%v", tt.number, got)
		}
	}
}

func FuzzMoney(f *testing.F) {
	for _, s := range []string{"0", "1.23", "1e999999999", "null", "92233720368547758.07"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		var m Money
		if m.UnmarshalJSON([]byte(s)) == nil {
			if m < 0 {
				t.Fatal("negative")
			}
			data, err := m.MarshalJSON()
			if err != nil {
				t.Fatal(err)
			}
			var r Money
			if r.UnmarshalJSON(data) != nil || r != m {
				t.Fatal("roundtrip")
			}
		}
	})
}

func FuzzOrder(f *testing.F) {
	f.Add("12345678903")
	f.Add("")
	f.Fuzz(func(t *testing.T, s string) {
		if ValidOrder(s) {
			for _, c := range []byte(s) {
				if c < '0' || c > '9' {
					t.Fatal("accepted non-digit")
				}
			}
			if !ValidOrder("00" + s) {
				t.Fatal("leading zero changed checksum")
			}
		}
	})
}
