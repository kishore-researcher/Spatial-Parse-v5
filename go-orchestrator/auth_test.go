package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestTenantAuthValidKey(t *testing.T) {
	os.Setenv("TENANT_API_KEYS", `{"key-a":"tenant-a","key-b":"tenant-b"}`)
	defer os.Unsetenv("TENANT_API_KEYS")
	auth := loadTenantAuth()

	tenant, ok := auth.tenantFor("key-a")
	if !ok || tenant != "tenant-a" {
		t.Fatalf("expected key-a to resolve to tenant-a, got %q, ok=%v", tenant, ok)
	}
}

func TestTenantAuthUnknownKey(t *testing.T) {
	os.Setenv("TENANT_API_KEYS", `{"key-a":"tenant-a"}`)
	defer os.Unsetenv("TENANT_API_KEYS")
	auth := loadTenantAuth()

	if _, ok := auth.tenantFor("not-a-real-key"); ok {
		t.Fatal("expected an unknown key to fail resolution")
	}
}

func TestRequireTenantMiddlewareRejectsMissingKey(t *testing.T) {
	os.Setenv("TENANT_API_KEYS", `{"key-a":"tenant-a"}`)
	defer os.Unsetenv("TENANT_API_KEYS")
	auth := loadTenantAuth()

	called := false
	handler := auth.requireTenant(func(w http.ResponseWriter, r *http.Request) { called = true })

	req := httptest.NewRequest(http.MethodGet, "/anything", nil)
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for missing API key, got %d", rec.Code)
	}
	if called {
		t.Error("handler must not run when the API key is missing")
	}
}

func TestRequireTenantMiddlewareRejectsInvalidKey(t *testing.T) {
	os.Setenv("TENANT_API_KEYS", `{"key-a":"tenant-a"}`)
	defer os.Unsetenv("TENANT_API_KEYS")
	auth := loadTenantAuth()

	called := false
	handler := auth.requireTenant(func(w http.ResponseWriter, r *http.Request) { called = true })

	req := httptest.NewRequest(http.MethodGet, "/anything", nil)
	req.Header.Set("X-API-Key", "totally-wrong-key")
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401 for invalid API key, got %d", rec.Code)
	}
	if called {
		t.Error("handler must not run when the API key is invalid")
	}
}

func TestRequireTenantMiddlewarePopulatesContext(t *testing.T) {
	os.Setenv("TENANT_API_KEYS", `{"key-a":"tenant-a"}`)
	defer os.Unsetenv("TENANT_API_KEYS")
	auth := loadTenantAuth()

	var seenTenant string
	handler := auth.requireTenant(func(w http.ResponseWriter, r *http.Request) {
		seenTenant = tenantFromContext(r)
	})

	req := httptest.NewRequest(http.MethodGet, "/anything", nil)
	req.Header.Set("X-API-Key", "key-a")
	rec := httptest.NewRecorder()
	handler(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected the wrapped handler to run (default 200), got %d", rec.Code)
	}
	if seenTenant != "tenant-a" {
		t.Errorf("expected tenant-a in request context, got %q", seenTenant)
	}
}
