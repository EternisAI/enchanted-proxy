package main

import "testing"

func TestIsLoopbackAddr(t *testing.T) {
	tests := []struct {
		addr string
		want bool
	}{
		// Accepted: the loopback interface only.
		{"127.0.0.1:9091", true},
		{"127.0.0.1:0", true},
		{"127.9.9.9:9091", true}, // all of 127.0.0.0/8 is loopback
		{"[::1]:9091", true},
		{"localhost:9091", true},

		// Rejected: reachable from outside the pod.
		{":9091", false},        // every interface
		{"0.0.0.0:9091", false}, // every interface, explicitly
		{"[::]:9091", false},
		{"10.3.115.94:9091", false}, // a pod IP
		{"192.168.1.9:9091", false},
		{"example.com:9091", false}, // never resolved, so never accepted

		// Rejected: malformed.
		{"127.0.0.1", false}, // no port
		{"", false},
		{"garbage", false},
	}

	for _, tt := range tests {
		if got := isLoopbackAddr(tt.addr); got != tt.want {
			t.Errorf("isLoopbackAddr(%q) = %v, want %v", tt.addr, got, tt.want)
		}
	}
}

func TestDebugAddrs(t *testing.T) {
	tests := []struct {
		addr string
		want []string
	}{
		// "localhost" may resolve to either family, so bind both.
		{"localhost:9091", []string{"127.0.0.1:9091", "[::1]:9091"}},
		// An explicit literal is a deliberate choice; bind exactly it.
		{"127.0.0.1:9091", []string{"127.0.0.1:9091"}},
		{"[::1]:9091", []string{"[::1]:9091"}},
		// Unparseable yields nothing to bind.
		{"garbage", nil},
		{"", nil},
	}

	for _, tt := range tests {
		got := debugAddrs(tt.addr)
		if len(got) != len(tt.want) {
			t.Errorf("debugAddrs(%q) = %v, want %v", tt.addr, got, tt.want)
			continue
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("debugAddrs(%q)[%d] = %q, want %q", tt.addr, i, got[i], tt.want[i])
			}
		}
	}
}

// Every address debugAddrs produces must still pass the loopback guard, so the
// expansion cannot widen what the guard admitted.
func TestDebugAddrsStayLoopback(t *testing.T) {
	for _, addr := range []string{"localhost:9091", "127.0.0.1:9091", "[::1]:9091"} {
		if !isLoopbackAddr(addr) {
			t.Fatalf("test input %q is not loopback", addr)
		}
		for _, expanded := range debugAddrs(addr) {
			if !isLoopbackAddr(expanded) {
				t.Errorf("debugAddrs(%q) produced non-loopback address %q", addr, expanded)
			}
		}
	}
}
