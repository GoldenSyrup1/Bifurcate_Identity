// Bifurcate-Identity: a demo of safe, incremental migration off a legacy
// identity store, built around the exact mechanism a CIAM team worries
// about — a node dying mid-write during a staged rollout — rather than
// around OAuth/OIDC plumbing, which is the part everyone else demos.
//
// Run: go run .
// Then open http://localhost:8080
package main

import (
	"embed"
	"encoding/json"
	"io/fs"
	"log"
	"net/http"
	"os"

	"bifurcate-identity/internal/migration"
)

//go:embed web/static
var staticFiles embed.FS

func main() {
	m := migration.New()

	// Seed a couple of tenants so the dashboard isn't empty on first load.
	acme := m.CreateTenant("Acme Corp")
	m.Signup(acme.ID, "alice@acme.com", "hunter2", "seed-1")
	m.Signup(acme.ID, "bob@acme.com", "correcthorse", "seed-2")
	m.CreateTenant("Globex")

	mux := http.NewServeMux()

	sub, err := fs.Sub(staticFiles, "web/static")
	if err != nil {
		log.Fatal(err)
	}
	mux.Handle("/", http.FileServer(http.FS(sub)))

	mux.HandleFunc("GET /api/tenants", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, m.Tenants())
	})

	mux.HandleFunc("POST /api/tenants", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Name string `json:"name"` }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == "" {
			http.Error(w, "name required", http.StatusBadRequest)
			return
		}
		t := m.CreateTenant(body.Name)
		writeJSON(w, t)
	})

	mux.HandleFunc("POST /api/tenants/{id}/advance", func(w http.ResponseWriter, r *http.Request) {
		next, err := m.Advance(r.PathValue("id"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]any{"phase": next})
	})

	mux.HandleFunc("POST /api/tenants/{id}/chaos", func(w http.ResponseWriter, r *http.Request) {
		if err := m.ArmChaos(r.PathValue("id")); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]any{"armed": true})
	})

	mux.HandleFunc("POST /api/tenants/{id}/signup", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Email     string `json:"email"`
			Password  string `json:"password"`
			RequestID string `json:"request_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Email == "" {
			http.Error(w, "email required", http.StatusBadRequest)
			return
		}
		events, err := m.Signup(r.PathValue("id"), body.Email, body.Password, body.RequestID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, events)
	})

	mux.HandleFunc("POST /api/tenants/{id}/login", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Email    string `json:"email"`
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		res, err := m.Login(r.PathValue("id"), body.Email, body.Password)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, res)
	})

	mux.HandleFunc("POST /api/tenants/{id}/reconcile", func(w http.ResponseWriter, r *http.Request) {
		report, err := m.Reconcile(r.PathValue("id"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, report)
	})

	mux.HandleFunc("GET /api/tenants/{id}/log", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, m.Log(r.PathValue("id")))
	})

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	log.Printf("bifurcate-identity listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, mux))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
