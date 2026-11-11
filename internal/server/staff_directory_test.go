//go:build integration

package server_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"
)

// Staff directory endpoint tests (fix review-ready plan-c): clinical pickers
// ("Petugas yang mengorder") need a read-only staff listing guarded by
// clinical.record.view instead of user.manage, without management fields.
func TestStaffDirectory(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := newTestRouter(t, ctx, pool)
	companyID, merchantID, ownerID, ownerToken := cfSeedEnv(t, ctx, pool, router, "sd")

	// (a) Dokter-role user (clinical.record.view only) → 200, payload has
	// active staff with minimal fields only (no email/phone/person detail).
	// Owner is active, so the directory must contain them.
	dokter := cfSeedUserWithPerms(t, ctx, pool, router, companyID, merchantID, "cf.sd.dokter", "clinical.record.view")
	rec, resp := cfDo(t, router, http.MethodGet, "/api/v1/merchants/"+merchantID+"/staff", dokter, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("staff directory = %d: %s", rec.Code, resp["error"])
	}
	items, ok := resp["data"].([]any)
	if !ok {
		t.Fatalf("data must be an array, got %T", resp["data"])
	}
	found := false
	for _, it := range items {
		u, ok := it.(map[string]any)
		if !ok {
			t.Fatalf("item = %v, want object", it)
		}
		for _, forbidden := range []string{"email", "phone", "person_id", "created_at"} {
			if _, present := u[forbidden]; present {
				t.Fatalf("management field %q leaked into staff directory", forbidden)
			}
		}
		if _, present := u["full_name"]; !present {
			t.Fatalf("full_name missing from staff item: %v", u)
		}
		if _, present := u["roles"]; !present {
			t.Fatalf("roles missing from staff item: %v", u)
		}
		if id, _ := u["id"].(string); id == ownerID {
			found = true
		}
	}
	if !found {
		t.Fatalf("owner (active staff) not in directory, got %d items", len(items))
	}

	// (b) Inactive user of the same merchant must NOT appear (aktif only).
	inactiveID := fmt.Sprintf("bd000000-0000-0000-0000-%012x", cfNext())
	passwordHash, err := testHasher().Hash(ctx, "Passw0rd123!")
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO core.app_user (id, company_id, username, password_hash, is_active) VALUES ($1, $2, $3, $4, false)", inactiveID, companyID, "cf.sd.inactive", passwordHash); err != nil {
		t.Fatalf("seed inactive user: %v", err)
	}
	rec, resp = cfDo(t, router, http.MethodGet, "/api/v1/merchants/"+merchantID+"/staff", dokter, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("staff directory after inactive = %d: %s", rec.Code, resp["error"])
	}
	for _, it := range resp["data"].([]any) {
		u := it.(map[string]any)
		if id, _ := u["id"].(string); id == inactiveID {
			t.Fatalf("inactive user present in directory")
		}
		if isAct, _ := u["is_active"].(bool); !isAct {
			t.Fatalf("inactive user in directory: %v", u)
		}
	}

	// (c) User without clinical.record.view → 403.
	none := cfSeedUserWithPerms(t, ctx, pool, router, companyID, merchantID, "cf.sd.noperm")
	rec, _ = cfDo(t, router, http.MethodGet, "/api/v1/merchants/"+merchantID+"/staff", none, companyID, merchantID, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("no-perm = %d, want 403", rec.Code)
	}

	// (d) Owner token still works (has clinical.record.view via migration seed).
	rec, _ = cfDo(t, router, http.MethodGet, "/api/v1/merchants/"+merchantID+"/staff", ownerToken, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("owner = %d, want 200", rec.Code)
	}
}
