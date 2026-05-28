package main

import (
	"embed"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
)

//go:embed static
var staticFiles embed.FS

func main() {
	orchestratorURL := os.Getenv("ORCHESTRATOR_URL")
	if orchestratorURL == "" {
		orchestratorURL = "http://orchestrator:8080"
	}

	staticFS, _ := fs.Sub(staticFiles, "static")
	mux := http.NewServeMux()

	// Serve static files
	mux.Handle("GET /", http.FileServer(http.FS(staticFS)))

	// Proxy handlers — forward to orchestrator, copy status+headers+body verbatim
	proxy := func(method, target string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			req, err := http.NewRequestWithContext(r.Context(), method, target, r.Body)
			if err != nil {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			req.Header = r.Header.Clone()
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				http.Error(w, "upstream error", http.StatusBadGateway)
				return
			}
			defer resp.Body.Close()
			for k, vs := range resp.Header {
				for _, v := range vs {
					w.Header().Add(k, v)
				}
			}
			w.WriteHeader(resp.StatusCode)
			io.Copy(w, resp.Body)
		}
	}

	mux.HandleFunc("GET /api/agents", proxy("GET", orchestratorURL+"/v1/agents"))
	mux.HandleFunc("POST /api/spawn", proxy("POST", orchestratorURL+"/spawn"))

	// For endpoints with path params, extract and forward
	mux.HandleFunc("GET /api/agents/{id}/events", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		proxy("GET", orchestratorURL+"/v1/agents/"+id+"/events")(w, r)
	})
	mux.HandleFunc("DELETE /api/agents/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		proxy("DELETE", orchestratorURL+"/v1/agents/"+id)(w, r)
	})
	mux.HandleFunc("POST /api/agents/{id}/message", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		proxy("POST", orchestratorURL+"/v1/agents/"+id+"/message")(w, r)
	})
	mux.HandleFunc("POST /api/agents/{id}/dismiss", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		proxy("POST", orchestratorURL+"/v1/agents/"+id+"/dismiss")(w, r)
	})
	mux.HandleFunc("GET /api/templates", proxy("GET", orchestratorURL+"/v1/templates"))

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Printf("dashboard listening on :%s (orchestrator: %s)", port, orchestratorURL)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}
