package updatenotice

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestConditionalResponsesPersistBetweenClients(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			w.Header().Set("ETag", `"release-2"`)
			w.Header().Set("Last-Modified", "Mon, 07 Sep 2026 12:00:00 GMT")
			_, _ = io.WriteString(w, `{"version":"v0.2.0"}`)
			return
		}
		if r.Header.Get("If-None-Match") != `"release-2"` || r.Header.Get("If-Modified-Since") == "" {
			t.Error("persisted conditional headers were absent")
		}
		w.WriteHeader(http.StatusNotModified)
	}))
	defer server.Close()
	directory := t.TempDir()
	for range 2 {
		client := &http.Client{Transport: &conditionalTransport{directory: directory}}
		response, err := client.Get(server.URL)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if err != nil || response.StatusCode != http.StatusOK || string(body) != `{"version":"v0.2.0"}` {
			t.Fatalf("conditional cache response = %d %q, %v", response.StatusCode, body, err)
		}
	}
}

func TestConditionalResponseSizeIsBounded(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, strings.Repeat("x", maximumResponseBytes+1))
	}))
	defer server.Close()
	client := &http.Client{Transport: &conditionalTransport{directory: t.TempDir()}}
	response, err := client.Get(server.URL)
	if response != nil {
		_ = response.Body.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "size limit") {
		t.Fatalf("oversized metadata accepted: %v", err)
	}
}

func TestHTTPFailureDoesNotDestroyConditionalCache(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch requests.Add(1) {
		case 1:
			w.Header().Set("ETag", `"retained"`)
			_, _ = io.WriteString(w, "saved release")
		case 2:
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			if r.Header.Get("If-None-Match") != `"retained"` {
				t.Error("outage destroyed cache headers")
			}
			w.WriteHeader(http.StatusNotModified)
		}
	}))
	defer server.Close()
	client := &http.Client{Transport: &conditionalTransport{directory: t.TempDir()}}
	for index := range 3 {
		response, err := client.Get(server.URL)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if index == 2 && string(body) != "saved release" {
			t.Fatalf("retained body = %q", body)
		}
	}
}
