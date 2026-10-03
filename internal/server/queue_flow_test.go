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
