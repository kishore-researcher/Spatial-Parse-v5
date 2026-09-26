// auth.go: minimal API-key-to-tenant authentication.
//
// Honest scope note: this is deliberately simple -- a static key->tenant
// map, no key rotation, no scopes/roles, no rate limiting. That's
// appropriate for "light admin API" as asked, and it establishes the real
// tenant *isolation* boundary that matters (see store.go: every read path
// requires a tenant ID and filters by it). It is NOT a substitute for a
// real auth system (OAuth2/OIDC, short-lived tokens, per-tenant scopes) in
// an actual production multi-tenant product -- that's flagged in the
// README as a next step, not silently glossed over.
package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
)

type contextKey string

const tenantContextKey contextKey = "tenant_id"

// TenantAuth holds the API-key -> tenant-ID mapping, loaded once at
// startup from the TENANT_API_KEYS environment variable (a JSON object,
// e.g. `{"key-abc123":"acme-corp","key-def456":"globex"}`). Falls back to
// a single well-known dev-mode key so the API is usable out of the box in
// local/demo settings without requiring configuration -- this fallback is
// logged loudly and should never be relied on in a real deployment.
type TenantAuth struct {
	keys map[string]string // api key -> tenant id
}

func loadTenantAuth() *TenantAuth {
	raw := os.Getenv("TENANT_API_KEYS")
	keys := make(map[string]string)
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &keys); err != nil {
			panic("TENANT_API_KEYS is set but not valid JSON: " + err.Error())
		}
	} else {
		keys["dev-key"] = "dev-tenant"
		println("[auth] TENANT_API_KEYS not set -- using a single dev-mode key ('dev-key' -> 'dev-tenant'). Do not use this in production.")
	}
	return &TenantAuth{keys: keys}
}

func (a *TenantAuth) tenantFor(apiKey string) (string, bool) {
	t, ok := a.keys[apiKey]
	return t, ok
}

// requireTenant wraps a handler so it only runs once a valid API key has
// resolved to a tenant ID, which is then attached to the request context
// for the handler (and everything it calls) to use. Every admin API route
// and /ingest itself go through this -- there is no route that touches the
// store without a resolved tenant ID.
func (a *TenantAuth) requireTenant(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		apiKey := r.Header.Get("X-API-Key")
		if apiKey == "" {
			http.Error(w, `{"error":"missing X-API-Key header"}`, http.StatusUnauthorized)
			return
		}
		tenantID, ok := a.tenantFor(apiKey)
		if !ok {
			http.Error(w, `{"error":"invalid API key"}`, http.StatusUnauthorized)
			return
		}
		ctx := context.WithValue(r.Context(), tenantContextKey, tenantID)
		next(w, r.WithContext(ctx))
	}
}

func tenantFromContext(r *http.Request) string {
	t, _ := r.Context().Value(tenantContextKey).(string)
	return t
}
