package api

import (
	"encoding/json"
	"net/http"

	"github.com/JingxuanKang/cospace/internal/spaces"
)

type CodexRouteOption struct {
	ID         string `json:"id"`
	Label      string `json:"label"`
	Configured bool   `json:"configured"`
}

func (s *Server) routeOptions() []CodexRouteOption {
	if s.CodexRoutes != nil {
		return s.CodexRoutes
	}
	return []CodexRouteOption{{"official", "Official account", true}, {"sub2api-i", "Sub2API I", false}, {"sub2api-ii", "Sub2API II", false}}
}
func (s *Server) routeConfigured(id string) bool {
	if id == "" {
		id = "official"
	}
	for _, r := range s.routeOptions() {
		if r.ID == id {
			return r.Configured
		}
	}
	return false
}
func (s *Server) codexOptions(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"routes": s.routeOptions(), "models": spaces.CodexModels()})
}
func (s *Server) setCodex(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Route string `json:"route"`
		Model string `json:"model"`
	}
	if json.NewDecoder(r.Body).Decode(&in) != nil || in.Route == "" || in.Model == "" {
		writeJSON(w, 400, map[string]string{"error": "want {route, model}"})
		return
	}
	if !s.routeConfigured(in.Route) {
		writeJSON(w, 400, map[string]string{"error": "Codex route is not configured on this host"})
		return
	}
	if err := s.Spaces.SetCodex(r.PathValue("name"), in.Route, in.Model); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (s *Server) setNetwork(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Internet *bool `json:"internet_access"`
	}
	if json.NewDecoder(r.Body).Decode(&in) != nil || in.Internet == nil {
		writeJSON(w, 400, map[string]string{"error": "want {internet_access}"})
		return
	}
	mode := "gateway-only"
	if *in.Internet {
		mode = "open"
	}
	if err := s.Spaces.SetNetwork(r.PathValue("name"), mode); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) upgradeNetwork(w http.ResponseWriter, r *http.Request) {
	if err := s.Spaces.StartNetworkUpgrade(r.PathValue("name")); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}
