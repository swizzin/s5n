package kube

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"k8s.io/client-go/rest"
)

// One cluster-wide LIST; every Service port with a nodePort comes back with its
// Service, and ports without one (ClusterIP) are skipped.
func TestNodePortsListsEveryNamespace(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/services" {
			t.Errorf("request path %q, want the all-namespaces list /api/v1/services", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"kind":"ServiceList","apiVersion":"v1","items":[
{"metadata":{"namespace":"envoy-gateway-system","name":"envoy"},"spec":{"ports":[{"port":80,"nodePort":31111},{"port":443,"nodePort":32001}]}},
{"metadata":{"namespace":"seedbox","name":"sonarr"},"spec":{"ports":[{"port":8989}]}},
{"metadata":{"namespace":"seedbox","name":"qbittorrent-peers"},"spec":{"ports":[{"port":30074,"nodePort":30074}]}}]}`))
	}))
	defer srv.Close()

	got, err := NodePorts(context.Background(), &rest.Config{Host: srv.URL})
	if err != nil {
		t.Fatalf("NodePorts: %v", err)
	}
	want := []NodePort{
		{Port: 31111, Service: "envoy-gateway-system/envoy"},
		{Port: 32001, Service: "envoy-gateway-system/envoy"},
		{Port: 30074, Service: "seedbox/qbittorrent-peers"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("NodePorts = %v, want %v", got, want)
	}
}
