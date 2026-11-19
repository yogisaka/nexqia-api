//go:build integration

package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"testing"
	"time"
)

// TestGiziStamp — hook STAMP (plan 2026-11-18-anak-a1-1-be Task 3, spec §5).
// Ekspektasi skor dihitung dari definisi kontrak (band M4: >=4 tinggi,
// 2–3 sedang, 0–1 rendah; s3 server: dz = |z_tb − z_bb| → >3:3, >2:1,
// else 0; z_bb < −2,05 → 3), bukan output kode. Fixture anak 84 bln
// (age_days 2557 → am 84, dalam gate 61–192): ref sintetis l=1 —
// bbu m=20 s=0.05 → z_bb(18.0)=-2.00, z_bb(17.9)=-2.10; tbu m=120 s=0.05 →
// z_tb(127.2)=1.20, z_tb(126.0)=1.00, z_tb(120)=0.00.
func TestGiziStamp(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := gzNewRouter(t, ctx, pool)
	companyID, merchantID, _, token := gzSeedEnv(t, ctx, pool, router, "stamp")

	gzSeedAnakCurve(t, ctx, pool, "bbu", 2557, 20, 0.05)
	gzSeedAnakCurve(t, ctx, pool, "tbu", 2557, 120, 0.05)

	newAnak := func(t *testing.T, label string, ageDays int) string {
		t.Helper()
		_, admissionID := gzSeedAnakAge(t, ctx, pool, router, token, companyID, merchantID, label, ageDays)
		rec, _ := cfDo(t, router, http.MethodPost, "/api/v1/clinical/nutrition-episode", token, companyID, merchantID, map[string]any{
			"admission_id": admissionID, "profile": "anak",
		})
		if rec.Code != http.StatusCreated {
			t.Fatalf("episode %s = %d: %s", label, rec.Code, rec.Body.String())
		}
		return admissionID
	}
	postAnthro := func(t *testing.T, admissionID string, bb, tb float64) {
		t.Helper()
		body := map[string]any{
			"admission_id": admissionID,
			"measured_at":  time.Now().Format(time.RFC3339),
			"bb_kg":        bb, "tb_cm": tb,
		}
		if rec, _ := cfDo(t, router, http.MethodPost, "/api/v1/clinical/anthropometry", token, companyID, merchantID, body); rec.Code != http.StatusCreated {
			t.Fatalf("anthro = %d: %s", rec.Code, rec.Body.String())
		}
	}
	gzStampForm := func(s1, s2 int) map[string]any {
		return map[string]any{"sections": map[string]any{
			"stamp_diagnosis": strconv.Itoa(s1),
			"stamp_asupan":    strconv.Itoa(s2),
		}}
	}
	postStamp := func(t *testing.T, admissionID string, s1, s2 int) string {
		t.Helper()
		rec, resp := cfDo(t, router, http.MethodPost, "/api/v1/clinical/forms", token, companyID, merchantID, map[string]any{
			"admission_id": admissionID, "form_type": "ases_gizi_stamp", "form_data": gzStampForm(s1, s2),
		})
		if rec.Code != http.StatusCreated {
			t.Fatalf("form stamp = %d: %s", rec.Code, rec.Body.String())
		}
		// Shape respons create form: data.entry = clinicalFormJSON.
		return resp["data"].(map[string]any)["entry"].(map[string]any)["id"].(string)
	}
	stampRow := func(t *testing.T, formID string) (int, int, string, map[string]any) {
		t.Helper()
		var score, maxScore int
		var interp, payloadRaw string
		if err := pool.QueryRow(ctx,
			`SELECT score, max_score, interpretation, payload::text
			 FROM clinical.nutrition_screening_result
			 WHERE form_instance_id = $1::uuid AND instrument = 'stamp'`,
			formID).Scan(&score, &maxScore, &interp, &payloadRaw); err != nil {
			t.Fatalf("stamp row %s: %v", formID, err)
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(payloadRaw), &payload); err != nil {
			t.Fatalf("payload %s: %v", payloadRaw, err)
		}
		return score, maxScore, interp, payload
	}
	stampCount := func(t *testing.T, admissionID string) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM clinical.nutrition_screening_result
			 WHERE admission_id = $1::uuid AND instrument = 'stamp'`,
			admissionID).Scan(&n); err != nil {
			t.Fatalf("count stamp: %v", err)
		}
		return n
	}
	num := func(t *testing.T, v any) float64 { return gzNum(t, v) }

	// (a) dz > 3 → s3 = 3: z_bb -2.00 (bukan < -2,05), z_tb 1.20 → dz 3.20
	// → s3 3; total = s1 2 + s2 3 + 3 = 8 → tinggi (band >=4).
	aAdm := newAnak(t, "a", 2557)
	postAnthro(t, aAdm, 18.0, 127.2)
	aForm := postStamp(t, aAdm, 2, 3)
	score, maxScore, interp, payload := stampRow(t, aForm)
	if score != 8 || maxScore != 9 || interp != "tinggi" {
		t.Fatalf("(a) stamp = %d/%d/%s, want 8/9/tinggi", score, maxScore, interp)
	}
	if num(t, payload["dz"]) != 3.2 || num(t, payload["z_bb"]) != -2.0 || num(t, payload["z_tb"]) != 1.2 {
		t.Fatalf("(a) payload = %v, want dz 3.2 z_bb -2.0 z_tb 1.2", payload)
	}
	if payload["sumber_step3"] != "antropometri" {
		t.Fatalf("(a) sumber = %v, want antropometri", payload["sumber_step3"])
	}
	// refreshGiziRules setelah hook: snapshot rules max_score 10 ada.
	var rulesMax int
	if err := pool.QueryRow(ctx,
		`SELECT max_score FROM clinical.nutrition_screening_result
		 WHERE admission_id = $1::uuid AND instrument = 'rules'`,
		aAdm).Scan(&rulesMax); err != nil || rulesMax != 10 {
		t.Fatalf("(a) rules max = %d (%v), want 10", rulesMax, err)
	}

	// (b) boundary band + dz > 2 → s3 = 1: z_tb 1.00, dz 3.00 (bukan >3).
	// s1 3 + s2 0 + 1 = 4 → tinggi (tepi atas); s1 2 + 1 = 3 → sedang (tepi
	// bawah band tinggi).
	bAdm := newAnak(t, "b", 2557)
	postAnthro(t, bAdm, 18.0, 126.0)
	if _, _, interp, _ = stampRow(t, postStamp(t, bAdm, 3, 0)); interp != "tinggi" {
		t.Fatalf("(b) skor 4 interp = %s, want tinggi", interp)
	}
	if _, _, interp, _ = stampRow(t, postStamp(t, bAdm, 2, 0)); interp != "sedang" {
		t.Fatalf("(b) skor 3 interp = %s, want sedang", interp)
	}

	// (b2) boundary cut 2 tanpa antropometri (pola (d), s3 = 0): s1 2 + s2 0
	// = 2 → sedang (tepi atas band rendah); s1 0 + s2 0 = 0 → rendah.
	b2Adm := newAnak(t, "b2", 2557)
	if _, _, interp, _ = stampRow(t, postStamp(t, b2Adm, 2, 0)); interp != "sedang" {
		t.Fatalf("(b2) skor 2 interp = %s, want sedang", interp)
	}
	if _, _, interp, _ = stampRow(t, postStamp(t, b2Adm, 0, 0)); interp != "rendah" {
		t.Fatalf("(b2) skor 0 interp = %s, want rendah", interp)
	}

	// (c) z_bb < -2,05 → s3 = 3 (bukan dz-branch): z_bb -2.10, z_tb 0 →
	// dz 2.10 (dz-branch hanya 1); total = 0 + 0 + 3 = 3 → sedang.
	cAdm := newAnak(t, "c", 2557)
	postAnthro(t, cAdm, 17.9, 120.0)
	_, _, interp, payload = stampRow(t, postStamp(t, cAdm, 0, 0))
	if interp != "sedang" || num(t, payload["stamp_step3"]) != 3 {
		t.Fatalf("(c) stamp = interp %s step3 %v, want sedang step3 3", interp, payload["stamp_step3"])
	}

	// (d) tanpa antropometri → s3 0 + sumber hint: total = 2 + 3 = 5 tinggi.
	dAdm := newAnak(t, "d", 2557)
	_, _, interp, payload = stampRow(t, postStamp(t, dAdm, 2, 3))
	if interp != "tinggi" || payload["sumber_step3"] != "hint" || payload["z_tb"] != nil {
		t.Fatalf("(d) stamp = %s %v, want tinggi hint tanpa z", interp, payload)
	}

	// (e) hook skip usia 204 bln (17 th > gate 61–192): form 201 tapi TANPA
	// baris 'stamp' (age_days 6210 → am floor(6210/30.4375) = 204).
	eAdm := newAnak(t, "e", 6210)
	postAnthro(t, eAdm, 40.0, 150.0)
	postStamp(t, eAdm, 2, 3)
	if n := stampCount(t, eAdm); n != 0 {
		t.Fatalf("(e) stamp rows = %d, want 0 (hook skip 204 bln)", n)
	}

	// (f) idempoten 2× write form yang sama (POST + PATCH) → tetap 1 baris
	// per (form_instance, 'stamp') dengan skor sama.
	if rec, _ := cfDo(t, router, http.MethodPatch, "/api/v1/clinical/forms/"+aForm, token, companyID, merchantID, map[string]any{
		"form_data": gzStampForm(2, 3),
	}); rec.Code != http.StatusOK {
		t.Fatalf("(f) patch form = %d: %s", rec.Code, rec.Body.String())
	}
	score, _, interp, _ = stampRow(t, aForm)
	if score != 8 || interp != "tinggi" {
		t.Fatalf("(f) stamp setelah PATCH = %d/%s, want 8/tinggi", score, interp)
	}
}

// TestGiziStampBalitaGate — gate STAMP 14 hari + usia tak diketahui (plan
// 2026-11-19-gizi-balita-1-be Task 3, D-BT-01). Ekspektasi dari kontrak:
// (g) usia 10 hari (< giziStampMinAgeDays 14) → skip TANPA baris 'stamp';
// (h) usia 913 hari = floor(913/30.4375) = 30 bln (>= 14 hari, <= 192 bln)
// → persist: tanpa antropometri → s3 = 0 hint, skor = 2 + 3 = 5 → tinggi;
// (i) age_days tak diketahui (DOB NULL → NULL) → hook TETAP jalan dengan
// stamp_step3 = 0 + sumber hint (paritas v2).
func TestGiziStampBalitaGate(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	router := gzNewRouter(t, ctx, pool)
	companyID, merchantID, _, token := gzSeedEnv(t, ctx, pool, router, "stampgate")

	newEpisode := func(t *testing.T, label string, ageDays int) string {
		t.Helper()
		_, admissionID := gzSeedAnakAge(t, ctx, pool, router, token, companyID, merchantID, label, ageDays)
		rec, _ := cfDo(t, router, http.MethodPost, "/api/v1/clinical/nutrition-episode", token, companyID, merchantID, map[string]any{
			"admission_id": admissionID, "profile": "anak",
		})
		if rec.Code != http.StatusCreated {
			t.Fatalf("episode %s = %d: %s", label, rec.Code, rec.Body.String())
		}
		return admissionID
	}
	postAnthro := func(t *testing.T, admissionID string, bb, tb float64) {
		t.Helper()
		body := map[string]any{
			"admission_id": admissionID,
			"measured_at":  time.Now().Format(time.RFC3339),
			"bb_kg":        bb, "tb_cm": tb,
		}
		if rec, _ := cfDo(t, router, http.MethodPost, "/api/v1/clinical/anthropometry", token, companyID, merchantID, body); rec.Code != http.StatusCreated {
			t.Fatalf("anthro = %d: %s", rec.Code, rec.Body.String())
		}
	}
	postStamp := func(t *testing.T, admissionID string, s1, s2 int) {
		t.Helper()
		rec, _ := cfDo(t, router, http.MethodPost, "/api/v1/clinical/forms", token, companyID, merchantID, map[string]any{
			"admission_id": admissionID, "form_type": "ases_gizi_stamp",
			"form_data": map[string]any{"sections": map[string]any{
				"stamp_diagnosis": strconv.Itoa(s1), "stamp_asupan": strconv.Itoa(s2),
			}},
		})
		if rec.Code != http.StatusCreated {
			t.Fatalf("form stamp = %d: %s", rec.Code, rec.Body.String())
		}
	}
	// stampPayload mengembalikan (jumlah baris 'stamp', payload baris terakhir).
	stampPayload := func(t *testing.T, admissionID string) (int, map[string]any) {
		t.Helper()
		var n int
		var payloadRaw string
		if err := pool.QueryRow(ctx,
			`SELECT count(*), COALESCE(max(payload::text), '{}')
			 FROM clinical.nutrition_screening_result
			 WHERE admission_id = $1::uuid AND instrument = 'stamp'`,
			admissionID).Scan(&n, &payloadRaw); err != nil {
			t.Fatalf("stamp payload %s: %v", admissionID, err)
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(payloadRaw), &payload); err != nil {
			t.Fatalf("payload %s: %v", payloadRaw, err)
		}
		return n, payload
	}

	// (g) usia 10 hari → skip tanpa baris 'stamp'.
	gAdm := newEpisode(t, "g10d", 10)
	postAnthro(t, gAdm, 3.5, 50.0)
	postStamp(t, gAdm, 2, 3)
	if n, _ := stampPayload(t, gAdm); n != 0 {
		t.Fatalf("(g) stamp rows = %d, want 0 (usia 10 hari < 14)", n)
	}

	// (h) usia 30 bln (913 hari) → persist skor 5 tinggi (s3 hint 0).
	hAdm := newEpisode(t, "h30bln", 913)
	postStamp(t, hAdm, 2, 3)
	n, payload := stampPayload(t, hAdm)
	if n != 1 {
		t.Fatalf("(h) stamp rows = %d, want 1 (usia 30 bln persist)", n)
	}
	if gzNum(t, payload["stamp_step3"]) != 0 || payload["sumber_step3"] != "hint" {
		t.Fatalf("(h) payload = %v, want step3 0 hint", payload)
	}
	var score int
	if err := pool.QueryRow(ctx,
		`SELECT score FROM clinical.nutrition_screening_result
		 WHERE admission_id = $1::uuid AND instrument = 'stamp'`,
		hAdm).Scan(&score); err != nil || score != 5 {
		t.Fatalf("(h) stamp score = %d (%v), want 5", score, err)
	}

	// (i) age_days tak diketahui (person DOB NULL → antropometri tanpa
	// age_days) → hook jalan, s3 = 0 (paritas v2).
	_, iAdm := cfSeedAdmission(t, ctx, pool, router, token, companyID, merchantID, "gzst-i")
	rec, _ := cfDo(t, router, http.MethodPost, "/api/v1/clinical/nutrition-episode", token, companyID, merchantID, map[string]any{
		"admission_id": iAdm, "profile": "dewasa",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("episode i = %d: %s", rec.Code, rec.Body.String())
	}
	postAnthro(t, iAdm, 62.0, 158.0)
	postStamp(t, iAdm, 1, 1)
	n, payload = stampPayload(t, iAdm)
	if n != 1 {
		t.Fatalf("(i) stamp rows = %d, want 1 (usia tak diketahui tetap jalan)", n)
	}
	if gzNum(t, payload["stamp_step3"]) != 0 || payload["sumber_step3"] != "hint" {
		t.Fatalf("(i) payload = %v, want step3 0 hint", payload)
	}
}
