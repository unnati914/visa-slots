package main

import "testing"

func TestMatchCity(t *testing.T) {
	cases := []struct{ cc, in, want string }{
		{"IN", "NEW DELHI VAC", "New Delhi"}, {"IN", "Mumbai", "Mumbai"}, {"IN", "CHENNAI OFC", "Chennai"},
		{"IN", "Delhi", "New Delhi"}, {"CO", "Bogotá", "Bogota"}, {"MX", "Ciudad Juárez", "Ciudad Juarez"},
		{"MX", "Mexico City", "Mexico City"}, {"CA", "Toronto", "Toronto"}, {"CA", "Québec City", "Quebec City"},
		{"VN", "Ho Chi Minh City", "Ho Chi Minh City"}, {"IN", "Paris", ""}, {"IN", "", ""},
	}
	for _, c := range cases {
		if got := matchCity(c.cc, c.in); got != c.want {
			t.Errorf("matchCity(%s, %q) = %q, want %q", c.cc, c.in, got, c.want)
		}
	}
}
