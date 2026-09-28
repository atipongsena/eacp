package bench

import (
	"slices"
	"testing"
)

func TestSaturationEachConditionBothSides(t *testing.T) {
	ok := Verdict{Throughput: 95, Offered: 100, Requests: 100, Errors: 1, Throttled: 0, EndToEndP99Ms: 5000}
	for _, c := range []struct {
		name string
		edit func(*Verdict)
		want []string
	}{
		{"at every threshold", func(*Verdict) {}, nil},
		{"throughput below 95%", func(v *Verdict) { v.Throughput = 94.9 }, []string{"throughput"}},
		{"errors and 429s above 1%", func(v *Verdict) { v.Errors, v.Throttled = 1, 1 }, []string{"errors"}},
		{"p99 above 5 s", func(v *Verdict) { v.EndToEndP99Ms = 5001 }, []string{"p99"}},
		{"all three", func(v *Verdict) { v.Throughput, v.Errors, v.EndToEndP99Ms = 10, 50, 9000 },
			[]string{"throughput", "errors", "p99"}},
	} {
		v := ok
		c.edit(&v)
		if got := SaturationReasons(v); !slices.Equal(got, c.want) {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}
