package httpapi

import "testing"

func TestMediaRangeReservation(t *testing.T) {
	for _, tc := range []struct {
		header string
		bytes  int64
		valid  bool
	}{
		{"", 100, true},
		{"bytes=0-11", 12, true},
		{"bytes=90-", 10, true},
		{"bytes=-12", 12, true},
		{"bytes=-200", 100, true},
		{"bytes=90-200", 10, true},
		{"bytes=100-", 0, false},
		{"bytes=20-10", 0, false},
		{"bytes=-0", 0, false},
		{"bytes=0-1,4-5", 0, false},
		{"other=0-1", 0, false},
		{"bytes=9999999999999999999999-", 0, false},
	} {
		t.Run(tc.header, func(t *testing.T) {
			n, valid := mediaRangeSize(tc.header, 100)
			if n != tc.bytes || valid != tc.valid {
				t.Fatalf("reservation = %d, %v; want %d, %v", n, valid, tc.bytes, tc.valid)
			}
		})
	}
}
