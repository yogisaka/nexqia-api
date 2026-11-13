//go:build integration

package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestGiziRiskRules — rules engine risiko D5 (plan 2026-11-13-gizi-nutrition-
// care chunk 3 Task 2, spec §6 vektor LITERAL). Ekspektasi dihitung dari
// definisi kontrak (trigger/level §6 + band migration 000097), bukan output
// kode: (a) MST 3 → sedang; (b) balita bb 7.9→7.8 band bbu sedang → sedang
// (bb_stagnan + zscore_anak); (c) tanpa data → rendah; (d) bumil lila 22.1 +
// hb 10.3 → tinggi; (e) MST 5 → tinggi; (f) mnasf 6 → tinggi; (g) dewasa imt
// 24.8,24.9,24.9 + mst 2 → imt_flatten; (h) override tampil tanpa mengubah
// engine; (i) gate zscore_anak hanya profil balita.
func TestGiziRiskRules(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := gzNewRouter(t, ctx, pool)
	companyID, merchantID, userID, token := gzSeedEnv(t, ctx, pool, router, "risk")

	newEpisode := func(t *testing.T, label, profile string) (string, string) {
		t.Helper()
		_, admissionID := cfSeedAdmission(t, ctx, pool, router, token, companyID, merchantID, "gzr-"+label)
		rec, resp := cfDo(t, router, http.MethodPost, "/api/v1/clinical/nutrition-episode", token, companyID, merchantID, map[string]any{
			"admission_id": admissionID, "profile": profile,
		})
		if rec.Code != http.StatusCreated {
			t.Fatalf("episode %s = %d: %s", label, rec.Code, rec.Body.String())
		}
		return admissionID, resp["data"].(map[string]any)["id"].(string)
	}
	getRisk := func(t *testing.T, admissionID string) map[string]any {
		t.Helper()
		rec, resp := cfDo(t, router, http.MethodGet, "/api/v1/clinical/nutrition-episode?admission_id="+admissionID, token, companyID, merchantID, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("get episode = %d: %s", rec.Code, rec.Body.String())
		}
		risk, _ := resp["risk"].(map[string]any)
		return risk
	}
	hasTrigger := func(t *testing.T, risk map[string]any, code string) bool {
		t.Helper()
		for _, raw := range risk["triggers"].([]any) {
			if tr := raw.(map[string]any); tr["code"] == code {
				return true
			}
		}
		return false
	}
	postAnthro := func(t *testing.T, admissionID string, offset time.Duration, bb float64, extras map[string]any) {
		t.Helper()
		body := map[string]any{
			"admission_id": admissionID,
			"measured_at":  time.Now().Add(-offset).Format(time.RFC3339),
			"bb_kg":        bb, "tb_cm": 158.0,
		}
		if extras != nil {
			body["extras"] = extras
		}
		if rec, _ := cfDo(t, router, http.MethodPost, "/api/v1/clinical/anthropometry", token, companyID, merchantID, body); rec.Code != http.StatusCreated {
			t.Fatalf("anthro = %d: %s", rec.Code, rec.Body.String())
		}
	}
	postForm := func(t *testing.T, admissionID, formType string, formData map[string]any) {
		t.Helper()
		rec, _ := cfDo(t, router, http.MethodPost, "/api/v1/clinical/forms", token, companyID, merchantID, map[string]any{
			"admission_id": admissionID, "form_type": formType, "form_data": formData,
		})
		if rec.Code != http.StatusCreated {
			t.Fatalf("form %s = %d: %s", formType, rec.Code, rec.Body.String())
		}
	}
	rulesSnapshot := func(t *testing.T, admissionID string) (int, int, string, map[string]any) {
		t.Helper()
		var n, score, maxScore int
		var interp, payloadRaw string
		if err := pool.QueryRow(ctx,
			`SELECT (SELECT count(*) FROM clinical.nutrition_screening_result
			   WHERE admission_id = $1::uuid AND instrument = 'rules'),
			       score, max_score, interpretation, payload::text
			 FROM clinical.nutrition_screening_result
			 WHERE admission_id = $1::uuid AND instrument = 'rules'`,
			admissionID).Scan(&n, &score, &maxScore, &interp, &payloadRaw); err != nil || n != 1 {
			t.Fatalf("rules snapshot rows=%d (%v), want 1", n, err)
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(payloadRaw), &payload); err != nil {
			t.Fatalf("payload %s: %v", payloadRaw, err)
		}
		return score, maxScore, interp, payload
	}

	t.Run("a mst 3 sedang", func(t *testing.T) {
		admissionID, _ := newEpisode(t, "a", "dewasa")
		postForm(t, admissionID, "ases_gizi_skrining", gzMSTForm(3))
		risk := getRisk(t, admissionID)
		if risk["level"] != "sedang" || !hasTrigger(t, risk, "mst") {
			t.Fatalf("(a) risk = %v, want sedang + mst", risk)
		}
		// Hook skrining menulis snapshot rules dalam tx yang sama.
		score, maxScore, interp, payload := rulesSnapshot(t, admissionID)
		if score != 1 || maxScore != 8 || interp != "sedang" {
			t.Fatalf("(a) snapshot = %d/%d/%s, want 1/8/sedang", score, maxScore, interp)
		}
		if payload["reason"] == "" || payload["triggers"] == nil {
			t.Fatalf("(a) payload = %v, want triggers+reason", payload)
		}
	})

	t.Run("b balita stagnan zscore sedang", func(t *testing.T) {
		gzSeedBBURef(t, ctx, pool) // z(7.9)=-2.10, z(7.8)=-2.20 → band gizi_kurang (sedang)
		personID, admissionID := gzSeedBalita(t, ctx, pool, router, token, companyID, merchantID, "gzrb")
		rec, _ := cfDo(t, router, http.MethodPost, "/api/v1/clinical/nutrition-episode", token, companyID, merchantID, map[string]any{
			"admission_id": admissionID, "profile": "balita",
		})
		if rec.Code != http.StatusCreated {
			t.Fatalf("episode balita = %d: %s", rec.Code, rec.Body.String())
		}
		postAnthro(t, admissionID, 24*time.Hour, 7.9, nil)
		postAnthro(t, admissionID, 0, 7.8, nil)
		risk := getRisk(t, admissionID)
		if risk["level"] != "sedang" || !hasTrigger(t, risk, "bb_stagnan") || !hasTrigger(t, risk, "zscore_anak") {
			t.Fatalf("(b) risk = %v, want sedang + bb_stagnan + zscore_anak", risk)
		}
		score, _, interp, _ := rulesSnapshot(t, admissionID)
		if score != 2 || interp != "sedang" { // bb_stagnan + zscore_anak
			t.Fatalf("(b) snapshot = score %d interp %s, want 2/sedang", score, interp)
		}
		_ = personID
	})

	t.Run("c tanpa data rendah", func(t *testing.T) {
		admissionID, _ := newEpisode(t, "c", "dewasa")
		risk := getRisk(t, admissionID)
		if risk["level"] != "rendah" || len(risk["triggers"].([]any)) != 0 {
			t.Fatalf("(c) risk = %v, want rendah tanpa trigger", risk)
		}
	})

	t.Run("d bumil lila hb tinggi", func(t *testing.T) {
		admissionID, _ := newEpisode(t, "d", "bumil")
		postAnthro(t, admissionID, time.Hour, 60.0, map[string]any{"lila_cm": 22.1})
		postForm(t, admissionID, "asuhan_gizi", map[string]any{"sections": map[string]any{
			"biokimia": []map[string]any{{"item": "hb", "nilai": 10.3}},
		}})
		risk := getRisk(t, admissionID)
		if risk["level"] != "tinggi" || !hasTrigger(t, risk, "lila_kek") || !hasTrigger(t, risk, "hb_rendah") {
			t.Fatalf("(d) risk = %v, want tinggi + lila_kek + hb_rendah", risk)
		}
	})

	t.Run("e mst 5 tinggi", func(t *testing.T) {
		admissionID, _ := newEpisode(t, "e", "dewasa")
		postForm(t, admissionID, "ases_gizi_skrining", gzMSTForm(5))
		risk := getRisk(t, admissionID)
		if risk["level"] != "tinggi" {
			t.Fatalf("(e) risk = %v, want tinggi", risk)
		}
	})

	t.Run("f mnasf 6 tinggi", func(t *testing.T) {
		admissionID, _ := newEpisode(t, "f", "lansia")
		postForm(t, admissionID, "ases_gizi_mnasf", gzMnasfForm(6, "malnutrisi"))
		risk := getRisk(t, admissionID)
		if risk["level"] != "tinggi" || !hasTrigger(t, risk, "mnasf") {
			t.Fatalf("(f) risk = %v, want tinggi + mnasf", risk)
		}
	})

	t.Run("g dewasa imt flatten", func(t *testing.T) {
		admissionID, _ := newEpisode(t, "g", "dewasa")
		// imt: 62/1.58²=24.8; 62.2/1.58²=24.9 (dua kali → pasangan |Δ|<=0,1).
		postAnthro(t, admissionID, 72*time.Hour, 62.0, nil)
		postAnthro(t, admissionID, 48*time.Hour, 62.2, nil)
		postAnthro(t, admissionID, 24*time.Hour, 62.2, nil)
		postForm(t, admissionID, "ases_gizi_skrining", gzMSTForm(2))
		risk := getRisk(t, admissionID)
		if !hasTrigger(t, risk, "imt_flatten") || risk["level"] != "sedang" {
			t.Fatalf("(g) risk = %v, want sedang + imt_flatten", risk)
		}
	})

	t.Run("h override tanpa ubah engine", func(t *testing.T) {
		admissionID, episodeID := newEpisode(t, "h", "dewasa")
		risk := getRisk(t, admissionID)
		if risk["level"] != "rendah" || risk["override"] != nil {
			t.Fatalf("(h) awal = %v, want rendah tanpa override", risk)
		}
		rec, _ := cfDo(t, router, http.MethodPatch, "/api/v1/clinical/nutrition-episode/"+episodeID, token, companyID, merchantID, map[string]any{
			"row_version": 1, "risk_override": "tinggi",
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("patch override = %d: %s", rec.Code, rec.Body.String())
		}
		risk = getRisk(t, admissionID)
		if risk["override"] != "tinggi" || risk["level"] != "rendah" {
			t.Fatalf("(h) risk = %v, want override tinggi + engine rendah", risk)
		}
		if _, _, interp, _ := rulesSnapshot(t, admissionID); interp != "rendah" {
			t.Fatalf("(h) snapshot interp = %s, want rendah (engine tak berubah)", interp)
		}
	})

	t.Run("i gate zscore_anak", func(t *testing.T) {
		admissionID, episodeID := newEpisode(t, "i", "dewasa")
		// Ref bbu sintetis sudah di-seed subtest (b) — DB satu pool; INSERT
		// kedua bentrok unique (indicator, gender, x, source_version).
		// Baris antropometri band anak disisipkan langsung SQL (kontrak):
		// trigger snapshot menghitung z -2.20 + band gizi_kurang (sedang).
		if _, err := pool.Exec(ctx, `
			INSERT INTO clinical.anthropometry
				(company_id, merchant_id, person_id, admission_id, measured_at,
				 gender, age_days, bb_kg, tb_cm, source_ref, created_by)
			VALUES ($1::uuid, $2::uuid, $3::uuid, $4::uuid, now(), 'laki_laki',
			        548, 7.8, 75.0, 'gizi_tab', $5::uuid)`,
			companyID, merchantID, personOfAdmission(t, ctx, pool, admissionID), admissionID, userID); err != nil {
			t.Fatalf("insert anthro SQL: %v", err)
		}
		// Episode dewasa → gate: zscore_anak TIDAK aktif.
		risk := getRisk(t, admissionID)
		if hasTrigger(t, risk, "zscore_anak") {
			t.Fatalf("(i) dewasa risk = %v, want tanpa zscore_anak", risk)
		}
		// Episode sama, profil balita → trigger aktif.
		rec, _ := cfDo(t, router, http.MethodPatch, "/api/v1/clinical/nutrition-episode/"+episodeID, token, companyID, merchantID, map[string]any{
			"row_version": 1, "profile": "balita",
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("patch profil = %d: %s", rec.Code, rec.Body.String())
		}
		risk = getRisk(t, admissionID)
		if !hasTrigger(t, risk, "zscore_anak") || risk["level"] != "sedang" {
			t.Fatalf("(i) balita risk = %v, want sedang + zscore_anak", risk)
		}
	})

	t.Run("risk terisi", func(t *testing.T) {
		// Kontrak TestGiziNutritionEpisodeAPI +sub: struktur risk GET lengkap.
		admissionID, _ := newEpisode(t, "rt", "dewasa")
		postForm(t, admissionID, "ases_gizi_skrining", gzMSTForm(2))
		risk := getRisk(t, admissionID)
		for _, key := range []string{"level", "override", "triggers", "reason"} {
			if _, ok := risk[key]; !ok {
				t.Fatalf("risk tanpa key %s: %v", key, risk)
			}
		}
		if risk["level"] != "sedang" {
			t.Fatalf("risk terisi = %v, want sedang", risk)
		}
	})
}

// personOfAdmission membaca person_id admission untuk fixture SQL langsung.
func personOfAdmission(t *testing.T, ctx context.Context, pool *pgxpool.Pool, admissionID string) string {
	t.Helper()
	var personID string
	if err := pool.QueryRow(ctx,
		"SELECT person_id::text FROM operations.admission WHERE id = $1::uuid", admissionID).
		Scan(&personID); err != nil {
		t.Fatalf("person admission: %v", err)
	}
	return personID
}
