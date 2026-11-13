-- FIX-FORWARD (plan 2026-11-13-gizi-nutrition-care-chunk-1 Task 3 amend):
-- tabel clinical.nutrition_profile_change (000097) punya trigger
-- trg_npc_touch → trg_touch_row() yang men-set updated_at + row_version,
-- tapi kolom updated_at belum ada → SETIAP UPDATE gagal SQLSTATE 42703.
ALTER TABLE clinical.nutrition_profile_change ADD COLUMN updated_at timestamptz NOT NULL DEFAULT now();
