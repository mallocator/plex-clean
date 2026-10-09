package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealthcheck(t *testing.T) {
	srv := httptest.NewServer((&App{}).Routes())
	defer srv.Close()
	port := srv.Listener.Addr().(*net.TCPAddr).Port
	if code := healthcheck(port); code != 0 {
		t.Fatalf("healthy server: exit %d", code)
	}
	srv.Close()
	if code := healthcheck(port); code != 1 {
		t.Fatalf("stopped server: exit %d, want 1", code)
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer bad.Close()
	if code := healthcheck(bad.Listener.Addr().(*net.TCPAddr).Port); code != 1 {
		t.Fatalf("500: exit %d, want 1", code)
	}
}
