// dashboard.go: serves the embedded single-file web dashboard (Track D).
//
// No Node/React/build step by design: dashboard.html is a single static
// file (HTML + CSS + vanilla JS, no external script/style tags) compiled
// directly into the Go binary via go:embed. This is what "builds
// effortlessly in a restricted sandbox" means in practice here -- there is
// no npm install, no bundler, nothing to fetch over the network at build
// time, and the resulting binary is fully self-contained (the dashboard
// works even if the machine serving it has no internet access at all).
package main

import (
	"embed"
	"net/http"
)

//go:embed dashboard.html
var dashboardFS embed.FS

func serveDashboard(w http.ResponseWriter, r *http.Request) {
	// http.ServeMux registers "/" as a catch-all, so explicitly reject
	// anything that isn't exactly "/" or "/dashboard" rather than silently
	// serving the dashboard for every unmatched path (which would mask
	// real 404s for typos in API paths, e.g. someone hitting
	// /api/v1/qurantine by mistake).
	if r.URL.Path != "/" && r.URL.Path != "/dashboard" {
		http.NotFound(w, r)
		return
	}
	data, err := dashboardFS.ReadFile("dashboard.html")
	if err != nil {
		http.Error(w, "dashboard asset missing from build", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(data)
}
