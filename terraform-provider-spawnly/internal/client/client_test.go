package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// newTestClient spins an httptest.Server with the given handler and returns a
// Client pointed at it. token is the bearer the client will send (empty sends
// none).
func newTestClient(t *testing.T, token string, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return New(srv.URL, token)
}

// assertAuth checks the Authorization header matches the expectation for token.
func assertAuth(t *testing.T, r *http.Request, token string) {
	t.Helper()
	got := r.Header.Get("Authorization")
	if token == "" {
		if got != "" {
			t.Fatalf("expected no Authorization header, got %q", got)
		}
		return
	}
	if want := "Bearer " + token; got != want {
		t.Fatalf("Authorization header = %q, want %q", got, want)
	}
}

func TestGetTemplate(t *testing.T) {
	const token = "s3cret"
	want := Template{AgentType: "worker", Version: "1", Status: "active"}

	tests := []struct {
		name      string
		token     string
		status    int
		body      string
		wantFound bool
		wantErr   bool
		wantAPI   int // expected APIError.Status when wantErr
	}{
		{name: "success", token: token, status: http.StatusOK, body: `{"agentType":"worker","version":"1","status":"active"}`, wantFound: true},
		{name: "no token sends no header", token: "", status: http.StatusOK, body: `{"agentType":"worker","version":"1","status":"active"}`, wantFound: true},
		{name: "404 -> not found, no error", token: token, status: http.StatusNotFound, body: ``, wantFound: false},
		{name: "500 -> APIError", token: token, status: http.StatusInternalServerError, body: `boom`, wantErr: true, wantAPI: 500},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestClient(t, tc.token, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/v1/templates/worker" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				assertAuth(t, r, tc.token)
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})

			tmpl, found, err := c.GetTemplate(context.Background(), "worker")
			if tc.wantErr {
				var apiErr *APIError
				if !errors.As(err, &apiErr) {
					t.Fatalf("expected *APIError, got %v", err)
				}
				if apiErr.Status != tc.wantAPI {
					t.Fatalf("APIError.Status = %d, want %d", apiErr.Status, tc.wantAPI)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if found != tc.wantFound {
				t.Fatalf("found = %v, want %v", found, tc.wantFound)
			}
			if found && (tmpl.AgentType != want.AgentType || tmpl.Version != want.Version || tmpl.Status != want.Status) {
				t.Fatalf("template = %+v, want %+v", *tmpl, want)
			}
		})
	}
}

func TestListTemplateTypes(t *testing.T) {
	const token = "tok"
	tests := []struct {
		name    string
		status  int
		body    string
		want    []string
		wantErr bool
	}{
		{name: "success", status: http.StatusOK, body: `["a","b"]`, want: []string{"a", "b"}},
		{name: "500 -> APIError", status: http.StatusInternalServerError, body: `nope`, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestClient(t, token, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/v1/templates" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				assertAuth(t, r, token)
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			got, err := c.ListTemplateTypes(context.Background())
			if tc.wantErr {
				var apiErr *APIError
				if !errors.As(err, &apiErr) {
					t.Fatalf("expected *APIError, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestPutTemplate(t *testing.T) {
	const token = "tok"
	in := Template{AgentType: "worker", Version: "1"}

	tests := []struct {
		name    string
		status  int
		wantErr bool
	}{
		{name: "201 created", status: http.StatusCreated},
		{name: "409 -> APIError", status: http.StatusConflict, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestClient(t, token, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/v1/templates" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				assertAuth(t, r, token)
				if ct := r.Header.Get("Content-Type"); ct != "application/json" {
					t.Errorf("Content-Type = %q, want application/json", ct)
				}
				var body Template
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decode body: %v", err)
				}
				if body.AgentType != in.AgentType {
					t.Errorf("body.AgentType = %q, want %q", body.AgentType, in.AgentType)
				}
				w.WriteHeader(tc.status)
			})
			err := c.PutTemplate(context.Background(), in)
			if tc.wantErr {
				var apiErr *APIError
				if !errors.As(err, &apiErr) {
					t.Fatalf("expected *APIError, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestSetStatus(t *testing.T) {
	const token = "tok"
	tests := []struct {
		name    string
		status  int
		wantErr bool
	}{
		{name: "200 ok", status: http.StatusOK},
		{name: "404 -> APIError", status: http.StatusNotFound, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestClient(t, token, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPatch || r.URL.Path != "/v1/templates/worker" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				assertAuth(t, r, token)
				var body map[string]string
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decode body: %v", err)
				}
				if body["status"] != "disabled" {
					t.Errorf("status = %q, want disabled", body["status"])
				}
				w.WriteHeader(tc.status)
			})
			err := c.SetStatus(context.Background(), "worker", "disabled")
			if tc.wantErr {
				var apiErr *APIError
				if !errors.As(err, &apiErr) {
					t.Fatalf("expected *APIError, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestDeleteTemplate(t *testing.T) {
	const token = "tok"
	tests := []struct {
		name    string
		status  int
		wantErr bool
	}{
		{name: "204 no content", status: http.StatusNoContent},
		{name: "409 -> APIError", status: http.StatusConflict, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestClient(t, token, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodDelete || r.URL.Path != "/v1/templates/worker" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				assertAuth(t, r, token)
				w.WriteHeader(tc.status)
			})
			err := c.DeleteTemplate(context.Background(), "worker")
			if tc.wantErr {
				var apiErr *APIError
				if !errors.As(err, &apiErr) {
					t.Fatalf("expected *APIError, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestGetSchema(t *testing.T) {
	const token = "tok"
	tests := []struct {
		name    string
		status  int
		body    string
		want    string
		wantErr bool
	}{
		{name: "success", status: http.StatusOK, body: `{"schema":"definition tenant {}","version":"v1","source":"registry"}`, want: "definition tenant {}"},
		{name: "500 -> APIError", status: http.StatusInternalServerError, body: `boom`, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := newTestClient(t, token, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/v1/schema" {
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				assertAuth(t, r, token)
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			got, err := c.GetSchema(context.Background())
			if tc.wantErr {
				var apiErr *APIError
				if !errors.As(err, &apiErr) {
					t.Fatalf("expected *APIError, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Schema != tc.want {
				t.Fatalf("Schema = %q, want %q", got.Schema, tc.want)
			}
		})
	}
}
