//go:build integration

package server_test

import (
	"net/http"
	"testing"
)

// TestQueueFlow_* covers the flow configuration API (plan #23 task 3 kontrak
// §5): CRUD, stage validation, presets, delete guards and stage-in-use 409.

func TestQueueFlow_CRUDAndValidation(t *testing.T) {
	env := qeNewEnv(t, "crud")

	validStages := []map[string]any{
		{"seq": 1, "name": "Admisi", "kind": "admission", "served_by_permission": "operations.counter.manage"},
		{"seq": 2, "name": "Dokter", "kind": "physician", "served_by_permission": "operations.visit.manage"},
	}

	// served_by_permission must be a code in core.permission → 400.
	code, resp := qeRequest(t, env, http.MethodPost, "/api/v1/queue-flows", map[string]any{
		"merchant_id": env.merchantID, "name": "Alur Salah",
		"stages": []map[string]any{
			{"seq": 1, "name": "Admisi", "kind": "admission", "served_by_permission": "izin.palsu"},
		},
	})
	if code != http.StatusBadRequest {
		t.Fatalf("unknown served_by_permission: expected 400, got %d: %v", code, resp)
	}

	// Unknown kind → 400 (validated in Go, not by the DB CHECK).
	code, resp = qeRequest(t, env, http.MethodPost, "/api/v1/queue-flows", map[string]any{
		"merchant_id": env.merchantID, "name": "Alur Salah",
		"stages": []map[string]any{
			{"seq": 1, "name": "X", "kind": "wizard", "served_by_permission": "operations.counter.manage"},
		},
	})
	if code != http.StatusBadRequest {
		t.Fatalf("unknown kind: expected 400, got %d: %v", code, resp)
	}

	code, resp = qeRequest(t, env, http.MethodPost, "/api/v1/queue-flows", map[string]any{
		"merchant_id": env.merchantID, "name": "Alur Uji", "stages": validStages,
	})
	if code != http.StatusCreated {
		t.Fatalf("POST /queue-flows expected 201, got %d: %v", code, resp)
	}
	data := resp["data"].(map[string]any)
	flowID := data["id"].(string)
	stages := data["stages"].([]any)
	if len(stages) != 2 {
		t.Fatalf("expected 2 created stages, got %d", len(stages))
	}
	stage1 := stages[0].(map[string]any)
	stage1ID := stage1["id"].(string)

	// GET list returns the flow with its stages.
	code, resp = qeRequest(t, env, http.MethodGet, "/api/v1/merchants/"+env.merchantID+"/queue-flows", nil)
	if code != http.StatusOK {
		t.Fatalf("GET queue-flows expected 200, got %d: %v", code, resp)
	}
	list := resp["data"].([]any)
	if len(list) != 1 {
		t.Fatalf("expected 1 flow on list, got %d", len(list))
	}
	if got := list[0].(map[string]any); len(got["stages"].([]any)) != 2 {
		t.Fatalf("expected flow stages on list, got %v", got)
	}

	// PATCH renames.
	code, resp = qeRequest(t, env, http.MethodPatch, "/api/v1/queue-flows/"+flowID, map[string]any{"name": "Alur Uji Baru"})
	if code != http.StatusOK {
		t.Fatalf("PATCH flow expected 200, got %d: %v", code, resp)
	}
	if resp["data"].(map[string]any)["name"] != "Alur Uji Baru" {
		t.Fatalf("PATCH did not rename: %v", resp)
	}

	// PUT stages: keep stage 1 (renamed), replace stage 2 with a cashier one.
	code, resp = qeRequest(t, env, http.MethodPut, "/api/v1/queue-flows/"+flowID+"/stages", map[string]any{
		"stages": []map[string]any{
			{"id": stage1ID, "seq": 1, "name": "Admisi Depan", "kind": "admission", "served_by_permission": "operations.counter.manage"},
			{"seq": 2, "name": "Kasir", "kind": "cashier", "served_by_permission": "billing.invoice.create"},
		},
	})
	if code != http.StatusOK {
		t.Fatalf("PUT stages expected 200, got %d: %v", code, resp)
	}
	newStages := resp["data"].(map[string]any)["stages"].([]any)
	if len(newStages) != 2 {
		t.Fatalf("expected 2 stages after replace, got %d", len(newStages))
	}
	if first := newStages[0].(map[string]any); first["id"] != stage1ID || first["name"] != "Admisi Depan" {
		t.Fatalf("kept stage must keep its id and get the new name, got %v", first)
	}
}

func TestQueueFlow_Presets(t *testing.T) {
	env := qeNewEnv(t, "preset")

	code, resp := qeRequest(t, env, http.MethodPost, "/api/v1/queue-flows/from-preset/simple",
		map[string]string{"merchant_id": env.merchantID})
	if code != http.StatusCreated {
		t.Fatalf("preset simple expected 201, got %d: %v", code, resp)
	}
	stages := resp["data"].(map[string]any)["stages"].([]any)
	wantSimple := []struct {
		name, kind, perm string
	}{
		{"Pendaftaran", "admission", "operations.counter.manage"},
		{"Dokter", "physician", "operations.visit.manage"},
		{"Kasir & Apotek", "cashier", "billing.invoice.create"},
	}
	if len(stages) != len(wantSimple) {
		t.Fatalf("simple preset: expected %d stages, got %d", len(wantSimple), len(stages))
	}
	for i, w := range wantSimple {
		s := stages[i].(map[string]any)
		if s["name"] != w.name || s["kind"] != w.kind || s["served_by_permission"] != w.perm {
			t.Fatalf("simple stage %d: expected %v, got %v", i, w, s)
		}
	}

	code, resp = qeRequest(t, env, http.MethodPost, "/api/v1/queue-flows/from-preset/outpatient_full",
		map[string]string{"merchant_id": env.merchantID, "name": "Alur RJ Penuh"})
	if code != http.StatusCreated {
		t.Fatalf("preset outpatient_full expected 201, got %d: %v", code, resp)
	}
	full := resp["data"].(map[string]any)
	if full["name"] != "Alur RJ Penuh" {
		t.Fatalf("custom preset name not applied: %v", full["name"])
	}
	pstages := full["stages"].([]any)
	if len(pstages) != 6 {
		t.Fatalf("outpatient_full: expected 6 stages, got %d", len(pstages))
	}
	wantFull := []struct {
		seq       float64
		kind      string
		checkin   bool
		taskStart any
		taskEnd   any
	}{
		{1, "admission", false, float64(1), float64(3)},
		{2, "checkin", true, nil, nil},
		{3, "nurse", false, float64(3), float64(4)},
		{4, "physician", false, float64(4), float64(5)},
		{5, "cashier", true, nil, nil},
		{6, "pharmacy", true, float64(5), float64(7)},
	}
	for i, w := range wantFull {
		s := pstages[i].(map[string]any)
		if s["seq"] != w.seq || s["kind"] != w.kind || s["requires_checkin"] != w.checkin {
			t.Fatalf("outpatient_full stage %d: expected (seq %v kind %s checkin %v), got %v", i, w.seq, w.kind, w.checkin, s)
		}
		if s["bpjs_task_start"] != w.taskStart || s["bpjs_task_end"] != w.taskEnd {
			t.Fatalf("outpatient_full stage %d bpjs tasks: expected %v..%v, got %v..%v", i, w.taskStart, w.taskEnd, s["bpjs_task_start"], s["bpjs_task_end"])
		}
	}

	code, resp = qeRequest(t, env, http.MethodPost, "/api/v1/queue-flows/from-preset/tidak_ada",
		map[string]string{"merchant_id": env.merchantID})
	if code != http.StatusBadRequest {
		t.Fatalf("unknown preset expected 400, got %d: %v", code, resp)
	}

	// Katalog preset: 200, 5 entri alfabetis, tanpa igd (ditunda Fase 2).
	code, resp = qeRequest(t, env, http.MethodGet, "/api/v1/queue-flows/presets", nil)
	if code != http.StatusOK {
		t.Fatalf("GET presets expected 200, got %d: %v", code, resp)
	}
	presets := resp["data"].([]any)
	wantKeys := []string{"fast_track", "hemodialisa", "mcu", "outpatient_full", "simple"}
	if len(presets) != len(wantKeys) {
		t.Fatalf("expected %d presets, got %d: %v", len(wantKeys), len(presets), resp)
	}
	for i, w := range wantKeys {
		p := presets[i].(map[string]any)
		if p["key"] != w {
			t.Fatalf("preset %d: expected key %s, got %v", i, w, p)
		}
		if p["key"] == "hemodialisa" && p["name"] != "Hemodialisa" {
			t.Fatalf("hemodialisa preset name: expected Hemodialisa, got %v", p["name"])
		}
	}

	// Preset hemodialisa: 7 stages, check-in wajib, dokter skippable, tanpa BPJS tasks.
	code, resp = qeRequest(t, env, http.MethodPost, "/api/v1/queue-flows/from-preset/hemodialisa",
		map[string]string{"merchant_id": env.merchantID})
	if code != http.StatusCreated {
		t.Fatalf("preset hemodialisa expected 201, got %d: %v", code, resp)
	}
	hdstages := resp["data"].(map[string]any)["stages"].([]any)
	if len(hdstages) != 7 {
		t.Fatalf("hemodialisa: expected 7 stages, got %d", len(hdstages))
	}
	if hdstages[1].(map[string]any)["requires_checkin"] != true {
		t.Fatalf("hemodialisa stage 1 must require check-in: %v", hdstages[1])
	}
	if hdstages[3].(map[string]any)["skippable"] != true {
		t.Fatalf("hemodialisa stage 3 must be skippable: %v", hdstages[3])
	}
	for i, raw := range hdstages {
		s := raw.(map[string]any)
		if s["bpjs_task_start"] != nil || s["bpjs_task_end"] != nil {
			t.Fatalf("hemodialisa stage %d must have null bpjs tasks: %v", i, s)
		}
	}

	// Preset mcu: 6 stages, stage 2 = support, stage 4 skippable.
	code, resp = qeRequest(t, env, http.MethodPost, "/api/v1/queue-flows/from-preset/mcu",
		map[string]string{"merchant_id": env.merchantID})
	if code != http.StatusCreated {
		t.Fatalf("preset mcu expected 201, got %d: %v", code, resp)
	}
	mcustages := resp["data"].(map[string]any)["stages"].([]any)
	if len(mcustages) != 6 {
		t.Fatalf("mcu: expected 6 stages, got %d", len(mcustages))
	}
	if mcustages[2].(map[string]any)["kind"] != "support" {
		t.Fatalf("mcu stage 2 must be support: %v", mcustages[2])
	}
	if mcustages[4].(map[string]any)["skippable"] != true {
		t.Fatalf("mcu stage 4 must be skippable: %v", mcustages[4])
	}

	// Preset fast_track: 3 stages, semua prefix FT.
	code, resp = qeRequest(t, env, http.MethodPost, "/api/v1/queue-flows/from-preset/fast_track",
		map[string]string{"merchant_id": env.merchantID})
	if code != http.StatusCreated {
		t.Fatalf("preset fast_track expected 201, got %d: %v", code, resp)
	}
	ftstages := resp["data"].(map[string]any)["stages"].([]any)
	if len(ftstages) != 3 {
		t.Fatalf("fast_track: expected 3 stages, got %d", len(ftstages))
	}
	for i, raw := range ftstages {
		if got := raw.(map[string]any)["number_prefix"]; got != "FT" {
			t.Fatalf("fast_track stage %d must have prefix FT, got %v", i, got)
		}
	}

	// Tanpa izin operations.queue.configure → 403.
	noPermToken := mergeUserToken(t, t.Context(), env.pool, env.companyID, env.merchantID, "qeng.preset.noperm", nil)
	code, resp = qeRequestToken(t, env.router, http.MethodGet, "/api/v1/queue-flows/presets",
		noPermToken, env.companyID, env.merchantID, nil)
	if code != http.StatusForbidden {
		t.Fatalf("GET presets without permission expected 403, got %d: %v", code, resp)
	}
}

func TestQueueFlow_DeleteGuardsAndStageInUse(t *testing.T) {
	env := qeNewEnv(t, "guard")

	makeFlow := func(t *testing.T, name string) (string, []any) {
		t.Helper()
		code, resp := qeRequest(t, env, http.MethodPost, "/api/v1/queue-flows", map[string]any{
			"merchant_id": env.merchantID, "name": name,
			"stages": []map[string]any{
				{"seq": 1, "name": "Admisi", "kind": "admission", "served_by_permission": "operations.counter.manage"},
				{"seq": 2, "name": "Dokter", "kind": "physician", "served_by_permission": "operations.visit.manage"},
			},
		})
		if code != http.StatusCreated {
			t.Fatalf("seed flow %s: expected 201, got %d: %v", name, code, resp)
		}
		d := resp["data"].(map[string]any)
		return d["id"].(string), d["stages"].([]any)
	}

	// Unused flow deletes cleanly.
	flowID, _ := makeFlow(t, "Alur Hapus")
	code, resp := qeRequest(t, env, http.MethodDelete, "/api/v1/queue-flows/"+flowID, nil)
	if code != http.StatusNoContent {
		t.Fatalf("DELETE unused flow expected 204, got %d: %v", code, resp)
	}

	// Flow with an active journey refuses deletion with 409.
	flowID2, _ := makeFlow(t, "Alur Aktif")
	deptID := env.seedDepartment(t, "GUARD")
	personID := env.seedPerson(t, "Pasien Guard")
	payerID := env.seedPayer(t, "GUARD", "self_pay")
	admissionID := env.qeAdmit(t, deptID, personID, payerID, flowID2, http.StatusCreated)
	if admissionID == "" {
		t.Fatal("admission not created")
	}
	code, resp = qeRequest(t, env, http.MethodDelete, "/api/v1/queue-flows/"+flowID2, nil)
	if code != http.StatusConflict {
		t.Fatalf("DELETE flow with active journey expected 409, got %d: %v", code, resp)
	}

	// Stage already referenced by a ticket cannot be dropped via PUT → 409.
	code, resp = qeRequest(t, env, http.MethodGet, "/api/v1/merchants/"+env.merchantID+"/queue-flows", nil)
	var stages []any
	for _, f := range resp["data"].([]any) {
		if f.(map[string]any)["id"] == flowID2 {
			stages = f.(map[string]any)["stages"].([]any)
		}
	}
	stage2ID := stages[1].(map[string]any)["id"].(string)
	code, resp = qeRequest(t, env, http.MethodPut, "/api/v1/queue-flows/"+flowID2+"/stages", map[string]any{
		"stages": []map[string]any{
			// Only stage 2 survives — stage 1 has the waiting ticket.
			{"id": stage2ID, "seq": 1, "name": "Dokter", "kind": "physician", "served_by_permission": "operations.visit.manage"},
		},
	})
	if code != http.StatusConflict {
		t.Fatalf("PUT stages dropping used stage expected 409, got %d: %v", code, resp)
	}
}

func TestCounter_StageBinding(t *testing.T) {
	env := qeNewEnv(t, "ctrbind")

	// Flow with admission + physician stages.
	code, resp := qeRequest(t, env, http.MethodPost, "/api/v1/queue-flows", map[string]any{
		"merchant_id": env.merchantID, "name": "Alur Counter",
		"stages": []map[string]any{
			{"seq": 1, "name": "Admisi", "kind": "admission", "served_by_permission": "operations.counter.manage"},
			{"seq": 2, "name": "Dokter", "kind": "physician", "served_by_permission": "operations.visit.manage"},
		},
	})
	if code != http.StatusCreated {
		t.Fatalf("seed flow expected 201, got %d: %v", code, resp)
	}
	stages := resp["data"].(map[string]any)["stages"].([]any)
	admissionStage := stages[0].(map[string]any)["id"].(string)
	physicianStage := stages[1].(map[string]any)["id"].(string)

	// Locations: one room, one non-room.
	mkLoc := func(t *testing.T, code, kind string) string {
		t.Helper()
		st, r := qeRequest(t, env, http.MethodPost, "/api/v1/locations", map[string]string{
			"merchant_id": env.merchantID, "kind": kind, "code": code, "name": code,
		})
		if st != http.StatusCreated {
			t.Fatalf("seed location %s: expected 201, got %d: %v", code, st, r)
		}
		return r["data"].(map[string]any)["id"].(string)
	}
	roomID := mkLoc(t, "R-01", "room")
	buildingID := mkLoc(t, "B-01", "building")

	// schedule_room binding on a non-physician stage → 400.
	code, resp = qeRequest(t, env, http.MethodPost, "/api/v1/counters", map[string]string{
		"merchant_id": env.merchantID, "code": "L-01", "stage_id": admissionStage,
		"location_id": roomID, "binding": "schedule_room",
	})
	if code != http.StatusBadRequest {
		t.Fatalf("schedule_room on admission stage expected 400, got %d: %v", code, resp)
	}

	// Location kind != room → 400.
	code, resp = qeRequest(t, env, http.MethodPost, "/api/v1/counters", map[string]string{
		"merchant_id": env.merchantID, "code": "L-02", "stage_id": admissionStage,
		"location_id": buildingID, "binding": "fixed",
	})
	if code != http.StatusBadRequest {
		t.Fatalf("non-room location expected 400, got %d: %v", code, resp)
	}

	// Valid create: queue_type derived from the stage kind, binding default.
	code, resp = qeRequest(t, env, http.MethodPost, "/api/v1/counters", map[string]string{
		"merchant_id": env.merchantID, "code": "L-03", "stage_id": admissionStage, "location_id": roomID,
	})
	if code != http.StatusCreated {
		t.Fatalf("counter with stage expected 201, got %d: %v", code, resp)
	}
	ctr := resp["data"].(map[string]any)
	if ctr["queue_type"] != "pendaftaran" || ctr["binding"] != "fixed" || ctr["stage_id"] != admissionStage {
		t.Fatalf("counter create payload unexpected: %v", ctr)
	}
	counterID := ctr["id"].(string)

	// Update rebinds to the physician stage with schedule_room.
	code, resp = qeRequest(t, env, http.MethodPatch, "/api/v1/counters/"+counterID, map[string]any{
		"code": "L-03", "stage_id": physicianStage, "binding": "schedule_room",
	})
	if code != http.StatusOK {
		t.Fatalf("PATCH counter expected 200, got %d: %v", code, resp)
	}
	updated := resp["data"].(map[string]any)
	if updated["stage_id"] != physicianStage || updated["binding"] != "schedule_room" {
		t.Fatalf("PATCH counter binding not applied: %v", updated)
	}
	if updated["queue_type"] != "pendaftaran" {
		t.Fatalf("PATCH must keep queue_type, got %v", updated["queue_type"])
	}
}

// TestQueueFlow_UpdateScope covers the PATCH merge-patch contract (plan
// 2026-10-08-queue-settings-redesign §3.1): unsent fields are preserved,
// service_types/department_ids become editable, and validation is enforced.
func TestQueueFlow_UpdateScope(t *testing.T) {
	env := qeNewEnv(t, "scope")

	// 1. Create a flow from the simple preset, then promote it to default.
	code, resp := qeRequest(t, env, http.MethodPost, "/api/v1/queue-flows/from-preset/simple",
		map[string]string{"merchant_id": env.merchantID})
	if code != http.StatusCreated {
		t.Fatalf("preset simple expected 201, got %d: %v", code, resp)
	}
	flowID := resp["data"].(map[string]any)["id"].(string)

	code, resp = qeRequest(t, env, http.MethodPatch, "/api/v1/queue-flows/"+flowID, map[string]any{"is_default": true})
	if code != http.StatusOK {
		t.Fatalf("PATCH is_default expected 200, got %d: %v", code, resp)
	}
	if d := resp["data"].(map[string]any); d["is_default"] != true {
		t.Fatalf("PATCH is_default not applied: %v", d)
	}

	// 2. Rename only — is_default and is_active must survive.
	code, resp = qeRequest(t, env, http.MethodPatch, "/api/v1/queue-flows/"+flowID, map[string]any{"name": "Alur Baru"})
	if code != http.StatusOK {
		t.Fatalf("PATCH name expected 200, got %d: %v", code, resp)
	}
	d := resp["data"].(map[string]any)
	if d["name"] != "Alur Baru" || d["is_default"] != true || d["is_active"] != true {
		t.Fatalf("PATCH must keep unsent fields (is_default, is_active), got %v", d)
	}

	// 3. Deactivate, then rename again — is_active must stay false.
	code, resp = qeRequest(t, env, http.MethodPatch, "/api/v1/queue-flows/"+flowID, map[string]any{"is_active": false})
	if code != http.StatusOK {
		t.Fatalf("PATCH is_active=false expected 200, got %d: %v", code, resp)
	}
	code, resp = qeRequest(t, env, http.MethodPatch, "/api/v1/queue-flows/"+flowID, map[string]any{"name": "Alur Lagi"})
	if code != http.StatusOK {
		t.Fatalf("PATCH name 2 expected 200, got %d: %v", code, resp)
	}
	if d = resp["data"].(map[string]any); d["name"] != "Alur Lagi" || d["is_active"] != false {
		t.Fatalf("PATCH must keep is_active=false, got %v", d)
	}

	// 4. Scope fields are editable: set both, then clear department_ids only.
	deptID := "11111111-1111-1111-1111-111111111111"
	code, resp = qeRequest(t, env, http.MethodPatch, "/api/v1/queue-flows/"+flowID, map[string]any{
		"service_types": []string{"umum"}, "department_ids": []string{deptID},
	})
	if code != http.StatusOK {
		t.Fatalf("PATCH scope expected 200, got %d: %v", code, resp)
	}
	d = resp["data"].(map[string]any)
	if got, ok := d["service_types"].([]any); !ok || len(got) != 1 || got[0] != "umum" {
		t.Fatalf("expected service_types [umum], got %v", d["service_types"])
	}
	if got, ok := d["department_ids"].([]any); !ok || len(got) != 1 || got[0] != deptID {
		t.Fatalf("expected department_ids [%s], got %v", deptID, d["department_ids"])
	}
	code, resp = qeRequest(t, env, http.MethodPatch, "/api/v1/queue-flows/"+flowID, map[string]any{
		"department_ids": []string{},
	})
	if code != http.StatusOK {
		t.Fatalf("PATCH empty department_ids expected 200, got %d: %v", code, resp)
	}
	d = resp["data"].(map[string]any)
	if got, ok := d["department_ids"].([]any); !ok || len(got) != 0 {
		t.Fatalf("expected empty department_ids, got %v", d["department_ids"])
	}
	if got, ok := d["service_types"].([]any); !ok || len(got) != 1 || got[0] != "umum" {
		t.Fatalf("empty department_ids must keep service_types [umum], got %v", d["service_types"])
	}

	// 5. Invalid scope fields → 400 and the stored scope is untouched.
	const scopeErr = "service_types must be a non-empty subset of umum, bpjs, asuransi"
	code, resp = qeRequest(t, env, http.MethodPatch, "/api/v1/queue-flows/"+flowID, map[string]any{
		"service_types": []string{},
	})
	if code != http.StatusBadRequest || resp["error"] != scopeErr {
		t.Fatalf("empty service_types: expected 400 %q, got %d: %v", scopeErr, code, resp)
	}
	code, resp = qeRequest(t, env, http.MethodPatch, "/api/v1/queue-flows/"+flowID, map[string]any{
		"service_types": []string{"vip"},
	})
	if code != http.StatusBadRequest || resp["error"] != scopeErr {
		t.Fatalf("vip service_types: expected 400 %q, got %d: %v", scopeErr, code, resp)
	}
	code, resp = qeRequest(t, env, http.MethodPatch, "/api/v1/queue-flows/"+flowID, map[string]any{
		"department_ids": []string{"bukan-uuid"},
	})
	if code != http.StatusBadRequest || resp["error"] != "invalid department_ids entry" {
		t.Fatalf("bad department_ids: expected 400, got %d: %v", code, resp)
	}
	// GET list: the failed PATCHes must not have changed service_types.
	code, resp = qeRequest(t, env, http.MethodGet, "/api/v1/merchants/"+env.merchantID+"/queue-flows", nil)
	if code != http.StatusOK {
		t.Fatalf("GET list expected 200, got %d: %v", code, resp)
	}
	var found bool
	for _, e := range resp["data"].([]any) {
		f := e.(map[string]any)
		if f["id"] == flowID {
			found = true
			if got, ok := f["service_types"].([]any); !ok || len(got) != 1 || got[0] != "umum" {
				t.Fatalf("failed PATCHes must not change service_types, got %v", f["service_types"])
			}
		}
	}
	if !found {
		t.Fatalf("flow %s missing from list", flowID)
	}

	// 6. POST with an invalid service_types value → same 400 message.
	code, resp = qeRequest(t, env, http.MethodPost, "/api/v1/queue-flows", map[string]any{
		"merchant_id": env.merchantID, "name": "Alur X", "service_types": []string{"vip"},
	})
	if code != http.StatusBadRequest || resp["error"] != scopeErr {
		t.Fatalf("POST vip service_types: expected 400 %q, got %d: %v", scopeErr, code, resp)
	}
}

func TestQueueFlow_Duplicate(t *testing.T) {
	env := qeNewEnv(t, "dup")

	// 1. Flow with 3 custom stages → duplicate without body → 201, full copy.
	code, resp := qeRequest(t, env, http.MethodPost, "/api/v1/queue-flows", map[string]any{
		"merchant_id": env.merchantID, "name": "Alur Dup",
		"service_types":  []string{"umum", "bpjs"},
		"department_ids": []string{env.seedDepartment(t, "dup-dep-1")},
		"stages": []map[string]any{
			{"seq": 1, "name": "Admisi", "kind": "admission", "number_prefix": "AB", "served_by_permission": "operations.counter.manage"},
			{"seq": 2, "name": "Tunggu", "kind": "custom", "skippable": true, "served_by_permission": "operations.visit.manage"},
			{"seq": 3, "name": "Dokter", "kind": "physician", "bpjs_task_start": 1, "bpjs_task_end": 3, "served_by_permission": "operations.visit.manage"},
		},
	})
	if code != http.StatusCreated {
		t.Fatalf("create source flow expected 201, got %d: %v", code, resp)
	}
	src := resp["data"].(map[string]any)
	srcID := src["id"].(string)
	srcStages := src["stages"].([]any)

	code, resp = qeRequest(t, env, http.MethodPost, "/api/v1/queue-flows/"+srcID+"/duplicate", nil)
	if code != http.StatusCreated {
		t.Fatalf("duplicate expected 201, got %d: %v", code, resp)
	}
	dst := resp["data"].(map[string]any)
	if dst["name"] != "Alur Dup (salinan)" {
		t.Fatalf("duplicate name: expected 'Alur Dup (salinan)', got %v", dst["name"])
	}
	if dst["is_default"] != false || dst["is_active"] != true {
		t.Fatalf("duplicate flags: expected is_default=false is_active=true, got %v/%v", dst["is_default"], dst["is_active"])
	}
	if dst["id"] == srcID {
		t.Fatalf("duplicate must have a new id")
	}
	if got := dst["service_types"].([]any); len(got) != 2 || got[0] != "umum" || got[1] != "bpjs" {
		t.Fatalf("service_types not copied: %v", dst["service_types"])
	}
	if got := dst["department_ids"].([]any); len(got) != len(src["department_ids"].([]any)) {
		t.Fatalf("department_ids not copied: %v", dst["department_ids"])
	}
	dstStages := dst["stages"].([]any)
	if len(dstStages) != 3 || len(srcStages) != 3 {
		t.Fatalf("expected 3 stages on both, src %d dst %d", len(srcStages), len(dstStages))
	}
	for i, raw := range dstStages {
		d := raw.(map[string]any)
		s := srcStages[i].(map[string]any)
		if d["id"] == s["id"] || d["flow_id"] == s["flow_id"] {
			t.Fatalf("stage %d must get new id/flow_id: %v vs %v", i, d, s)
		}
		for _, k := range []string{"seq", "name", "kind", "number_prefix", "skippable", "requires_checkin", "bpjs_task_start", "bpjs_task_end", "served_by_permission"} {
			if d[k] != s[k] {
				t.Fatalf("stage %d field %s: expected %v, got %v", i, k, s[k], d[k])
			}
		}
	}

	// 2. "X" and "X (salinan)" exist → duplicate "X" lands on "X (salinan 2)".
	mkFlow := func(name string) string {
		code, resp := qeRequest(t, env, http.MethodPost, "/api/v1/queue-flows", map[string]any{
			"merchant_id": env.merchantID, "name": name,
		})
		if code != http.StatusCreated {
			t.Fatalf("create %s expected 201, got %d: %v", name, code, resp)
		}
		return resp["data"].(map[string]any)["id"].(string)
	}
	xID := mkFlow("Dup X")
	mkFlow("Dup X (salinan)")
	code, resp = qeRequest(t, env, http.MethodPost, "/api/v1/queue-flows/"+xID+"/duplicate", nil)
	if code != http.StatusCreated {
		t.Fatalf("duplicate X expected 201, got %d: %v", code, resp)
	}
	if got := resp["data"].(map[string]any)["name"]; got != "Dup X (salinan 2)" {
		t.Fatalf("expected 'Dup X (salinan 2)', got %v", got)
	}

	// 3. Explicit taken name → 409.
	code, resp = qeRequest(t, env, http.MethodPost, "/api/v1/queue-flows/"+xID+"/duplicate", map[string]any{"name": "Dup X"})
	if code != http.StatusConflict || resp["error"] != "flow name already exists" {
		t.Fatalf("duplicate taken name: expected 409 'flow name already exists', got %d: %v", code, resp)
	}

	// 4. Soft-deleted flow → 404.
	delID := mkFlow("Dup Hapus")
	code, resp = qeRequest(t, env, http.MethodDelete, "/api/v1/queue-flows/"+delID, nil)
	if code != http.StatusNoContent {
		t.Fatalf("delete expected 204, got %d: %v", code, resp)
	}
	code, resp = qeRequest(t, env, http.MethodPost, "/api/v1/queue-flows/"+delID+"/duplicate", nil)
	if code != http.StatusNotFound || resp["error"] != "queue flow not found" {
		t.Fatalf("duplicate deleted flow: expected 404 'queue flow not found', got %d: %v", code, resp)
	}

	// 5. Without operations.queue.configure → 403.
	noPermToken := mergeUserToken(t, t.Context(), env.pool, env.companyID, env.merchantID, "qeng.dup.noperm", nil)
	code, resp = qeRequestToken(t, env.router, http.MethodPost, "/api/v1/queue-flows/"+xID+"/duplicate",
		noPermToken, env.companyID, env.merchantID, nil)
	if code != http.StatusForbidden {
		t.Fatalf("duplicate without permission expected 403, got %d: %v", code, resp)
	}

	// 6. Flow without stages → 201 with empty stages array.
	emptyID := mkFlow("Dup Kosong")
	code, resp = qeRequest(t, env, http.MethodPost, "/api/v1/queue-flows/"+emptyID+"/duplicate", nil)
	if code != http.StatusCreated {
		t.Fatalf("duplicate empty flow expected 201, got %d: %v", code, resp)
	}
	if got, ok := resp["data"].(map[string]any)["stages"].([]any); !ok || len(got) != 0 {
		t.Fatalf("expected empty stages array, got %v", resp["data"].(map[string]any)["stages"])
	}
}
