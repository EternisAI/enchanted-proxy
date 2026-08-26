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
