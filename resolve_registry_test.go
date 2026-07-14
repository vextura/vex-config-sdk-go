package config

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// ── RegistryClient.Resolve ────────────────────────────────────────────────────

func TestRegistryClient_Resolve_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/registry/instances/vex-iam" {
			t.Errorf("path=%q", r.URL.Path)
		}
		w.Write([]byte(`{"instances":[
			{"id":"i1","addr":"10.0.0.5:8096","region":"kz-north-1","healthy":true}
		]}`))
	}))
	defer srv.Close()

	rc := NewRegistryClient(srv.URL)
	rec, err := rc.Resolve(context.Background(), "vex-iam", "kz-north-1")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Endpoint != "http://10.0.0.5:8096" {
		t.Errorf("Endpoint=%q", rec.Endpoint)
	}
	if rec.Service != "vex-iam" || rec.Status != "healthy" {
		t.Errorf("rec=%+v", rec)
	}
}

func TestRegistryClient_Resolve_FiltersRegionAndHealth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"instances":[
			{"id":"i1","addr":"10.0.0.1:8096","region":"eu-west-1","healthy":true},
			{"id":"i2","addr":"10.0.0.2:8096","region":"kz-north-1","healthy":false},
			{"id":"i3","addr":"10.0.0.3:8096","region":"kz-north-1","healthy":true}
		]}`))
	}))
	defer srv.Close()

	rc := NewRegistryClient(srv.URL)
	rec, err := rc.Resolve(context.Background(), "vex-iam", "kz-north-1")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Endpoint != "http://10.0.0.3:8096" {
		t.Errorf("expected filtered pick, got %q", rec.Endpoint)
	}
}

func TestRegistryClient_Resolve_StarRegionMatchesAll(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"instances":[
			{"id":"i1","addr":"10.0.0.1:8096","region":"eu-west-1","healthy":true}
		]}`))
	}))
	defer srv.Close()

	rc := NewRegistryClient(srv.URL)
	rec, err := rc.Resolve(context.Background(), "vex-iam", "*")
	if err != nil {
		t.Fatalf("* region should match any, got %v", err)
	}
	if rec.Endpoint == "" {
		t.Error("empty endpoint on * region")
	}
}

func TestRegistryClient_Resolve_404IsNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	rc := NewRegistryClient(srv.URL)
	_, err := rc.Resolve(context.Background(), "vex-gone", "")
	if err != ErrNotFound {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestRegistryClient_Resolve_NilOrEmptyBase(t *testing.T) {
	if _, err := (*RegistryClient)(nil).Resolve(context.Background(), "vex-iam", ""); err != ErrNotFound {
		t.Errorf("nil registry: want ErrNotFound, got %v", err)
	}
	rc := NewRegistryClient("")
	if _, err := rc.Resolve(context.Background(), "vex-iam", ""); err != ErrNotFound {
		t.Errorf("empty base: want ErrNotFound, got %v", err)
	}
}

func TestRegistryClient_Resolve_RoundRobin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"instances":[
			{"id":"i1","addr":"a:1","region":"","healthy":true},
			{"id":"i2","addr":"b:1","region":"","healthy":true}
		]}`))
	}))
	defer srv.Close()

	rc := NewRegistryClient(srv.URL)
	seen := map[string]bool{}
	for i := 0; i < 4; i++ {
		rec, _ := rc.Resolve(context.Background(), "s", "")
		seen[rec.Endpoint] = true
	}
	if len(seen) != 2 {
		t.Errorf("expected 2 endpoints via round-robin, got %v", seen)
	}
}

// ── ResolveServiceRegistryFirst ──────────────────────────────────────────────

func TestResolveServiceRegistryFirst_UsesRegistryWhenConfigured(t *testing.T) {
	var registryCalls, ripCalls int
	regSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		registryCalls++
		w.Write([]byte(`{"instances":[{"id":"i1","addr":"10.0.0.5:8096","region":"","healthy":true}]}`))
	}))
	defer regSrv.Close()
	ripSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ripCalls++
	}))
	defer ripSrv.Close()

	c := New(ripSrv.URL, "", WithRegistry(regSrv.URL))
	defer c.ClearRegistry()

	rec, err := c.ResolveServiceRegistryFirst(context.Background(), ResolveInput{
		Service: "vex-iam", Region: "",
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Endpoint != "http://10.0.0.5:8096" {
		t.Errorf("endpoint=%q", rec.Endpoint)
	}
	if registryCalls != 1 || ripCalls != 0 {
		t.Errorf("expected registry=1 rip=0, got registry=%d rip=%d", registryCalls, ripCalls)
	}
}

func TestResolveServiceRegistryFirst_FallsBackToRIPOnNotFound(t *testing.T) {
	regSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer regSrv.Close()
	ripSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"vrn":"vrn:vex:iam:*:*:endpoint/1","service":"vex-iam","region":"kz-north-1","endpoint":"http://rip-fallback:9000","status":"healthy"}`))
	}))
	defer ripSrv.Close()

	c := New(ripSrv.URL, "", WithRegistry(regSrv.URL))
	defer c.ClearRegistry()

	rec, err := c.ResolveServiceRegistryFirst(context.Background(), ResolveInput{
		Service: "vex-iam", Region: "kz-north-1",
	})
	if err != nil {
		t.Fatalf("expected RIP fallback, got %v", err)
	}
	if rec.Endpoint != "http://rip-fallback:9000" {
		t.Errorf("endpoint=%q (expected RIP fallback)", rec.Endpoint)
	}
}

func TestResolveServiceRegistryFirst_EnvVarWorks(t *testing.T) {
	regSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"instances":[{"id":"i1","addr":"env-host:9000","region":"","healthy":true}]}`))
	}))
	defer regSrv.Close()

	t.Setenv("VEX_REGISTRY_URL", regSrv.URL)
	c := New("http://not-called", "")

	rec, err := c.ResolveServiceRegistryFirst(context.Background(), ResolveInput{
		Service: "vex-iam", Region: "",
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Endpoint != "http://env-host:9000" {
		t.Errorf("endpoint=%q", rec.Endpoint)
	}
}

func TestResolveServiceRegistryFirst_NoRegistryConfigured_DelegatesToRIP(t *testing.T) {
	var called int
	ripSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called++
		w.Write([]byte(`{"vrn":"vrn:vex:iam:*:*:endpoint/1","service":"vex-iam","endpoint":"http://rip-only:9000","status":"healthy"}`))
	}))
	defer ripSrv.Close()

	// Ensure env is empty so we exercise the no-registry path.
	t.Setenv("VEX_REGISTRY_URL", "")

	c := New(ripSrv.URL, "")
	rec, err := c.ResolveServiceRegistryFirst(context.Background(), ResolveInput{
		Service: "vex-iam", Region: "",
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Endpoint != "http://rip-only:9000" || called != 1 {
		t.Errorf("expected 1 RIP call, endpoint from RIP; got calls=%d ep=%q", called, rec.Endpoint)
	}
}

func TestResolveServiceRegistryFirst_ExplicitWithRegistryBeatsEnv(t *testing.T) {
	// Env points to a registry that returns 500 (should NOT be used).
	envReg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "should not be reached", http.StatusInternalServerError)
	}))
	defer envReg.Close()

	// Explicit registry that succeeds.
	explicit := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"instances":[{"id":"i1","addr":"explicit:1","region":"","healthy":true}]}`))
	}))
	defer explicit.Close()

	t.Setenv("VEX_REGISTRY_URL", envReg.URL)
	c := New("http://unused", "", WithRegistry(explicit.URL))
	defer c.ClearRegistry()

	rec, err := c.ResolveServiceRegistryFirst(context.Background(), ResolveInput{
		Service: "svc", Region: "",
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Endpoint != "http://explicit:1" {
		t.Errorf("expected explicit registry to win, got %q", rec.Endpoint)
	}
}

// ClearRegistry removes state so tests are isolated.
func TestClearRegistry_RemovesState(t *testing.T) {
	c := New("x", "", WithRegistry("http://reg:1"))
	if u := c.registryURL(); u != "http://reg:1" {
		t.Fatalf("state not set: %q", u)
	}
	c.ClearRegistry()
	if u := c.registryURL(); u != "" {
		t.Errorf("state not cleared: %q", u)
	}
}
