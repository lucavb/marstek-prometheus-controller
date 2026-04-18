package promclient_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lucavb/marstek-prometheus-controller/internal/promclient"
)

func TestQuery_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if q := r.URL.Query().Get("query"); q != "electricity_power_watts" {
			t.Errorf("unexpected query param: %s", q)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"status":"success",
			"data":{
				"resultType":"vector",
				"result":[{"metric":{},"value":[1776501273.557,"125.5"]}]
			}
		}`))
	}))
	defer srv.Close()

	c := promclient.New(srv.URL, "electricity_power_watts", 5*time.Second)
	sample, err := c.Query(context.Background())
	if err != nil {
		t.Fatalf("Query() error = %v", err)
	}
	if sample.Watts != 125.5 {
		t.Errorf("Watts = %v, want 125.5", sample.Watts)
	}
	if sample.SampleTime.IsZero() {
		t.Error("SampleTime should not be zero")
	}
}

func TestQuery_NegativeWatts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1700000000,"-300"]}]}}`))
	}))
	defer srv.Close()

	c := promclient.New(srv.URL, "electricity_power_watts", 5*time.Second)
	sample, err := c.Query(context.Background())
	if err != nil {
		t.Fatalf("Query() error = %v", err)
	}
	if sample.Watts != -300 {
		t.Errorf("Watts = %v, want -300", sample.Watts)
	}
}

func TestQuery_EmptyResult(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	defer srv.Close()

	c := promclient.New(srv.URL, "electricity_power_watts", 5*time.Second)
	_, err := c.Query(context.Background())
	if err == nil {
		t.Fatal("expected error for empty result, got nil")
	}
}

func TestQuery_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := promclient.New(srv.URL, "electricity_power_watts", 5*time.Second)
	_, err := c.Query(context.Background())
	if err == nil {
		t.Fatal("expected error for HTTP 500, got nil")
	}
}

func TestQuery_PrometheusError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"error","error":"query timed out"}`))
	}))
	defer srv.Close()

	c := promclient.New(srv.URL, "electricity_power_watts", 5*time.Second)
	_, err := c.Query(context.Background())
	if err == nil {
		t.Fatal("expected error for prometheus error status, got nil")
	}
}

func TestQuery_BadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`not json`))
	}))
	defer srv.Close()

	c := promclient.New(srv.URL, "electricity_power_watts", 5*time.Second)
	_, err := c.Query(context.Background())
	if err == nil {
		t.Fatal("expected error for bad JSON, got nil")
	}
}

func TestQuery_ContextCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	c := promclient.New(srv.URL, "electricity_power_watts", 5*time.Second)
	_, err := c.Query(ctx)
	if err == nil {
		t.Fatal("expected error for cancelled context, got nil")
	}
}

func TestQuery_SampleTimestamp(t *testing.T) {
	// Verify the sample timestamp is parsed correctly from the Prometheus response.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1700000000,"42"]}]}}`))
	}))
	defer srv.Close()

	c := promclient.New(srv.URL, "electricity_power_watts", 5*time.Second)
	sample, err := c.Query(context.Background())
	if err != nil {
		t.Fatalf("Query() error = %v", err)
	}
	want := time.Unix(1700000000, 0)
	if !sample.SampleTime.Equal(want) {
		t.Errorf("SampleTime = %v, want %v", sample.SampleTime, want)
	}
}
