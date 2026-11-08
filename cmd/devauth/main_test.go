package main

import "testing"

func TestCheckLocalOnly(t *testing.T) {
	for _, c := range []struct {
		addr, env string
		ok        bool
	}{
		{"127.0.0.1:9999", "", true},
		{"localhost:9999", "development", true},
		{"[::1]:9999", "test", true},
		{"0.0.0.0:9999", "", false},
		{":9999", "", false},
		{"192.168.1.5:9999", "", false},
		{"127.0.0.1:9999", "production", false},
		{"nonsense", "", false},
	} {
		if err := checkLocalOnly(c.addr, c.env); (err == nil) != c.ok {
			t.Errorf("checkLocalOnly(%q, %q) = %v, want ok=%v", c.addr, c.env, err, c.ok)
		}
	}
}
