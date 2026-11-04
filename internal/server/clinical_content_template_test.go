//go:build integration

package server_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/yogisaka/nexqia-api/internal/server"
)

// Content template pribadi endpoints ("Jadikan template"; plan
// 2026-11-04-cppt-2a-soap-editor-a-backend Task 3, spec
// 2026-11-04-cppt-2a-soap-editor-design §7). Fixtures reuse cfDo and the
// cfSeedEnv/cfSeedUserWithPerms helpers from clinical_form_test.go.
//
// cfTplBody is a minimal valid soap_dokter v2 form_data: subjective only;
// empty optional sections are soft warnings and never block (user decision 7).
const cfTplBody = `{"sections":{"subjective":"Demam 3 hari."}}`

func TestClinicalContentTemplate(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := newTestRouter(t, ctx, pool)
	companyID, merchantID, _, token := cfSeedEnv(t, ctx, pool, router, "ctpl")

	// ── Happy path: create → list → duplicate 409 → delete → list empty ──
	rec, resp := cfDo(t, router, http.MethodPost, "/api/v1/clinical/content-templates", token, companyID, merchantID, map[string]any{
		"form_type": "soap_dokter", "name": "  Template B  ", "form_data": cfRawJSON(t, cfTplBody),
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d: %s", rec.Code, rec.Body.String())
	}
	created := resp["data"].(map[string]any)
	// Name must be trimmed server-side (contract: trim 1–80).
	if created["name"] != "Template B" || created["form_type"] != "soap_dokter" {
		t.Fatalf("created = %v", created)
	}
	if _, ok := created["created_at"].(string); !ok {
		t.Fatalf("created_at missing: %v", created)
	}
	if _, ok := created["form_data"].(map[string]any); !ok {
		t.Fatalf("form_data must decode as object: %v", created)
	}
	id := created["id"].(string)

	// Second template with a name sorting before the first, to prove order name.
	rec, resp = cfDo(t, router, http.MethodPost, "/api/v1/clinical/content-templates", token, companyID, merchantID, map[string]any{
		"form_type": "soap_dokter", "name": "Template A", "form_data": cfRawJSON(t, cfTplBody),
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create second = %d: %s", rec.Code, rec.Body.String())
	}
	idA := resp["data"].(map[string]any)["id"].(string)

	rec, resp = cfDo(t, router, http.MethodGet, "/api/v1/clinical/content-templates?form_type=soap_dokter", token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d: %s", rec.Code, rec.Body.String())
	}
	items, _ := resp["data"].([]any)
	if len(items) != 2 {
		t.Fatalf("list = %d items, want 2", len(items))
	}
	if items[0].(map[string]any)["name"] != "Template A" || items[1].(map[string]any)["name"] != "Template B" {
		t.Fatalf("list not ordered by name: %v", resp)
	}

	// Duplicate name (same owner + form_type + name, UNIQUE uq_content_template_name) → 409.
	rec, resp = cfDo(t, router, http.MethodPost, "/api/v1/clinical/content-templates", token, companyID, merchantID, map[string]any{
		"form_type": "soap_dokter", "name": "Template A", "form_data": cfRawJSON(t, cfTplBody),
	})
	if rec.Code != http.StatusConflict || resp["error"] != "nama template sudah ada" {
		t.Fatalf("duplicate = %d %v", rec.Code, resp)
	}

	// Same name on another form_type is allowed (UNIQUE is per form_type).
	rec, _ = cfDo(t, router, http.MethodPost, "/api/v1/clinical/content-templates", token, companyID, merchantID, map[string]any{
		"form_type": "ases_awal_perawat", "name": "Template A", "form_data": cfRawJSON(t, `{"sections":{"tanda_vital":{"sistolic":120}}}`),
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("same name other form_type = %d: %s", rec.Code, rec.Body.String())
	}

	// ── Validation: name kosong / 81 karakter → 400 ──
	rec, resp = cfDo(t, router, http.MethodPost, "/api/v1/clinical/content-templates", token, companyID, merchantID, map[string]any{
		"form_type": "soap_dokter", "name": "   ", "form_data": cfRawJSON(t, cfTplBody),
	})
	if rec.Code != http.StatusBadRequest || resp["error"] != "nama template wajib 1-80 karakter" {
		t.Fatalf("empty name = %d %v", rec.Code, resp)
	}
	rec, resp = cfDo(t, router, http.MethodPost, "/api/v1/clinical/content-templates", token, companyID, merchantID, map[string]any{
		"form_type": "soap_dokter", "name": strings.Repeat("x", 81), "form_data": cfRawJSON(t, cfTplBody),
	})
	if rec.Code != http.StatusBadRequest || resp["error"] != "nama template wajib 1-80 karakter" {
		t.Fatalf("81-char name = %d %v", rec.Code, resp)
	}
	// Unknown form_type → 400 (contract message).
	rec, resp = cfDo(t, router, http.MethodPost, "/api/v1/clinical/content-templates", token, companyID, merchantID, map[string]any{
		"form_type": "tidak_ada", "name": "Template C", "form_data": cfRawJSON(t, cfTplBody),
	})
	if rec.Code != http.StatusBadRequest || resp["error"] != "form_type tidak dikenal" {
		t.Fatalf("unknown form_type = %d %v", rec.Code, resp)
	}
	// form_data invalid: unknown section key → 400, message identical to create form.
	rec, resp = cfDo(t, router, http.MethodPost, "/api/v1/clinical/content-templates", token, companyID, merchantID, map[string]any{
		"form_type": "soap_dokter", "name": "Template D", "form_data": cfRawJSON(t, `{"sections":{"halo":"x"}}`),
	})
	if rec.Code != http.StatusBadRequest || resp["error"] != "unknown section key: halo" {
		t.Fatalf("invalid form_data = %d %v", rec.Code, resp)
	}

	// ── Delete: happy path, then list empty ──
	rec, resp = cfDo(t, router, http.MethodDelete, "/api/v1/clinical/content-templates/"+id, token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK || resp["data"].(map[string]any)["id"] != id {
		t.Fatalf("delete = %d %v", rec.Code, resp)
	}
	if rec, _ = cfDo(t, router, http.MethodDelete, "/api/v1/clinical/content-templates/"+id, token, companyID, merchantID, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("delete again = %d, want 404", rec.Code)
	}
	rec, resp = cfDo(t, router, http.MethodGet, "/api/v1/clinical/content-templates?form_type=soap_dokter", token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK || len(resp["data"].([]any)) != 1 {
		t.Fatalf("list after delete = %d %v", rec.Code, resp)
	}

	// ── Owner isolation: another user (same merchant) delete → 404 ──
	other := cfSeedUserWithPerms(t, ctx, pool, router, companyID, merchantID, "ctpl.other", server.PermClinicalRecordWrite)
	if rec, resp = cfDo(t, router, http.MethodDelete, "/api/v1/clinical/content-templates/"+idA, other, companyID, merchantID, nil); rec.Code != http.StatusNotFound || resp["error"] != "template tidak ditemukan" {
		t.Fatalf("delete by other user = %d %v", rec.Code, resp)
	}
	// …and the other user's own list is empty (owner = active user only).
	if rec, resp = cfDo(t, router, http.MethodGet, "/api/v1/clinical/content-templates?form_type=soap_dokter", other, companyID, merchantID, nil); rec.Code != http.StatusOK || len(resp["data"].([]any)) != 0 {
		t.Fatalf("other list = %d %v", rec.Code, resp)
	}

	// ── Permission: without clinical.record.write → 403 ──
	none := cfSeedUserWithPerms(t, ctx, pool, router, companyID, merchantID, "ctpl.none")
	if rec, _ = cfDo(t, router, http.MethodPost, "/api/v1/clinical/content-templates", none, companyID, merchantID, map[string]any{
		"form_type": "soap_dokter", "name": "Template E", "form_data": cfRawJSON(t, cfTplBody),
	}); rec.Code != http.StatusForbidden {
		t.Fatalf("no-perm create = %d, want 403: %s", rec.Code, rec.Body.String())
	}
	if rec, _ = cfDo(t, router, http.MethodGet, "/api/v1/clinical/content-templates?form_type=soap_dokter", none, companyID, merchantID, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("no-perm list = %d, want 403: %s", rec.Code, rec.Body.String())
	}

	// ── Tenant isolation: another merchant does not see merchant A's template ──
	companyB, merchantB, _, tokenB := cfSeedEnv(t, ctx, pool, router, "ctpb")
	if rec, resp = cfDo(t, router, http.MethodGet, "/api/v1/clinical/content-templates?form_type=soap_dokter", tokenB, companyB, merchantB, nil); rec.Code != http.StatusOK || len(resp["data"].([]any)) != 0 {
		t.Fatalf("merchant B list = %d %v", rec.Code, resp)
	}
	if rec, _ = cfDo(t, router, http.MethodDelete, "/api/v1/clinical/content-templates/"+idA, tokenB, companyB, merchantB, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("merchant B delete = %d, want 404", rec.Code)
	}
}
