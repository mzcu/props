package props

import (
	"net/netip"
	"reflect"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	tests := []struct {
		input string
		want  any
	}{
		{"hello", "hello"},
		{"true", true},
		{"-42", int(-42)},
		{"0x10", int64(16)},
		{"100", uint8(100)},
		{"3.5", float32(3.5)},
		{"5m30s", 5*time.Minute + 30*time.Second},
		{"a, b,c", []string{"a", "b", "c"}},
		{"1,2", []int{1, 2}},
		{"", []string{}},
		{"10.0.0.1", netip.MustParseAddr("10.0.0.1")},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			v := reflect.New(reflect.TypeOf(tt.want)).Elem()
			if err := parse(v, tt.input); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(v.Interface(), tt.want) {
				t.Errorf("got %#v, want %#v", v.Interface(), tt.want)
			}
		})
	}
}

func TestParse_Errors(t *testing.T) {
	tests := []struct {
		input string
		typ   any
	}{
		{"notabool", false},
		{"1.5", 0},
		{"300", uint8(0)},
		{"x", 0.0},
		{"soon", time.Duration(0)},
		{"1,x", []int{}},
		{"k=v", map[string]string{}},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			v := reflect.New(reflect.TypeOf(tt.typ)).Elem()
			if err := parse(v, tt.input); err == nil {
				t.Errorf("expected an error parsing %q into %T", tt.input, tt.typ)
			}
		})
	}
}
