package updatebootstrap

import (
	"reflect"
	"testing"
)

func TestMappedProcessSets(t *testing.T) {
	first := "ftxt\ni11\nD0x123\nn/non-mesh\nftxt\ni12\nftxt\nD0x123\nn/incomplete\nftxt\ni13\nD0x123\nn/mesh-one\n"
	second := "ftxt\ni22\nD0x456\nn/mesh-two\n"
	tests := []struct {
		name, output string
		want         []string
		invalid      bool
	}{
		{name: "reordered realistic records", output: "p22\n" + second + "p11\n" + first, want: []string{first, second}},
		{name: "missing process", output: "p22\n" + second, want: []string{"", second}},
		{name: "empty output", want: []string{"", ""}},
		{name: "empty set", output: "p11\np22\n" + second, want: []string{"", second}},
		{name: "duplicate header", output: "p11\n" + first + "p11\n", invalid: true},
		{name: "foreign process", output: "p33\n" + first, invalid: true},
		{name: "missing identity", output: first + "p11\n", invalid: true},
		{name: "bad trailing identity", output: "p11\n" + first + "pbad\n", invalid: true},
		{name: "zero", output: "p0\n", invalid: true},
		{name: "negative", output: "p-11\n", invalid: true},
		{name: "signed", output: "p+11\n", invalid: true},
		{name: "blank", output: "p\n", invalid: true},
		{name: "space", output: "p 11\n", invalid: true},
		{name: "overflow", output: "p99999999999999999999999999\n", invalid: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := mappedProcessSets(test.output, []int{11, 22})
			if (err != nil) != test.invalid || !reflect.DeepEqual(got, test.want) {
				t.Fatalf("sets = %q, %v; want %q, invalid=%v", got, err, test.want, test.invalid)
			}
		})
	}
}
