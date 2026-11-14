package main

import (
	"net/http"
	"testing"
)

func TestFTPSessionDefaults(t *testing.T) {
	o, e := parseFTPOptions([]string{"alice@work.example"})
	if e != nil {
		t.Fatal(e)
	}
	if o.remote != "." || o.mountpoint != "" || o.foreground || o.name != "work-example" {
		t.Fatalf("unexpected defaults: %+v", o)
	}
	o, e = parseFTPOptions([]string{"work", "project", "--name", "code", "--foreground"})
	if e != nil || o.name != "code" || !o.foreground || o.remote != "project" {
		t.Fatalf("options: %+v %v", o, e)
	}
	for _, name := range []string{"../escape", "a/b", "", "a.b"} {
		if _, e = parseFTPOptions([]string{"work", "--name", name}); e == nil {
			t.Fatalf("accepted name %q", name)
		}
	}
}
func TestFTPControlCapability(t *testing.T) {
	called := make(chan struct{}, 1)
	l, s, e := resourceListen("secret", func(w http.ResponseWriter, r *http.Request) { called <- struct{}{}; w.WriteHeader(204) })
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	r, e := resourceHTTP(l.Addr().String(), "wrong", "POST", "/stop", nil)
	if e != nil {
		t.Fatal(e)
	}
	r.Body.Close()
	if r.StatusCode != 401 {
		t.Fatal(r.Status)
	}
	select {
	case <-called:
		t.Fatal("unauthorized control request dispatched")
	default:
	}
	r, e = resourceHTTP(l.Addr().String(), "secret", "POST", "/stop", nil)
	if e != nil {
		t.Fatal(e)
	}
	r.Body.Close()
	if r.StatusCode != 204 {
		t.Fatal(r.Status)
	}
	if _, e = resourceHTTP("example.com:80", "secret", "POST", "/stop", nil); e == nil {
		t.Fatal("accepted non-loopback address")
	}
}
