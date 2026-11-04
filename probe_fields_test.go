package config

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The RIP prober writes probe_status / probe_code / probe_latency_ms /
// probe_at on every record (vex-config smithy/rip.smithy ServiceRecord). An
// SDK generated before those members existed silently dropped them, so a
// caller could not tell a probed-alive backend from an unprobed one.
func TestResolveService_DecodesProbeMembers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rip/resolve/vex-iam/dev-1" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"vrn":"vrn:vex:vex-iam:*:dev-1:instance/a","service":"vex-iam","region":"dev-1",` +
			`"endpoint":"http://iam.vex.internal","status":"healthy",` +
			`"probe_status":"up","probe_code":200,"probe_latency_ms":7,"probe_at":"2026-11-03T10:00:00Z"}`))
	}))
	defer srv.Close()

	rec, err := New(srv.URL, "").ResolveService(context.Background(), ResolveInput{Service: "vex-iam", Region: "dev-1"})
	if err != nil {
		t.Fatalf("ResolveService: %v", err)
	}
	if rec.ProbeStatus != "up" || rec.ProbeCode != 200 || rec.ProbeLatencyMs != 7 || rec.ProbeAt != "2026-11-03T10:00:00Z" {
		t.Fatalf("probe members not decoded: %+v", rec)
	}
}

// RIPForbidden (403) must match ErrForbidden; RIPAuthorizerUnavailable (503)
// has no sentinel and is reported through the envelope's Status.
func TestRegisterService_ScopedWriteErrors(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		want   error
	}{
		{http.StatusForbidden, `{"code":"RIPForbidden","message":"not authorized"}`, ErrForbidden},
		{http.StatusServiceUnavailable, `{"code":"RIPAuthorizerUnavailable","message":"authorizer unavailable"}`, nil},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		}))
		_, err := New(srv.URL, "").RegisterService(context.Background(), ServiceRecord{Vrn: "vrn:vex:x:t:r:instance/a"})
		srv.Close()
		var env ErrorEnvelope
		if !errors.As(err, &env) || env.Status != tc.status {
			t.Fatalf("status %d: err = %v, want ErrorEnvelope with that status", tc.status, err)
		}
		if tc.want != nil && !errors.Is(err, tc.want) {
			t.Fatalf("status %d: errors.Is(%v) = false", tc.status, tc.want)
		}
	}
}
