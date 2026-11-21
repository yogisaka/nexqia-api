package server

// Unit (non-integration) battery for clinicalValidateConsciousness shape
// validation, reproducing the payloads the FE ConsciousnessSection patch()
// sends (it always includes gcs/kategori/pupil_score/gcs_p/respons; pupil is
// an object with kiri_mm/kanan_mm numbers|null, bentuk string, refleks
// string|null). Spec: pupil mm 0<v<10, refleks enum, gcs_p int, respons
// ACVPU; calc consistency is NOT part of this shape test.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func consciousnessShapeCase(t *testing.T, name, raw string, want bool) {
	t.Helper()
	t.Run(name, func(t *testing.T) {
		gin.SetMode(gin.TestMode)
		w := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(w)
		ctx.Request = httptest.NewRequest(http.MethodPost, "/", nil)
		got := clinicalValidateConsciousness(ctx, "kesadaran", json.RawMessage(raw))
		if got != want {
			t.Fatalf("case %s: want %v got %v (http %d body %s)", name, want, got, w.Code, w.Body.String())
		}
	})
}

func TestConsciousnessShape(t *testing.T) {
	// a. empty object
	consciousnessShapeCase(t, "a_empty", `{}`, true)
	// b. only left pupil filled
	consciousnessShapeCase(t, "b_pupil_left", `{"gcs":null,"kategori":null,"pupil":{"kiri_mm":2.5,"kanan_mm":null,"bentuk":"","refleks_kiri":null,"refleks_kanan":null},"pupil_score":null,"gcs_p":null,"respons":null}`, true)
	// c. both pupils, no refleks
	consciousnessShapeCase(t, "c_pupil_both_no_refleks", `{"gcs":null,"kategori":null,"pupil":{"kiri_mm":2.5,"kanan_mm":2.5,"bentuk":"","refleks_kiri":null,"refleks_kanan":null},"pupil_score":null,"gcs_p":null,"respons":null}`, true)
	// d. pupil + refleks cepat/lambat, pupil_score 1, gcs null
	consciousnessShapeCase(t, "d_pupil_refleks_ps1", `{"gcs":null,"kategori":null,"pupil":{"kiri_mm":3,"kanan_mm":4,"bentuk":"bulat","refleks_kiri":"cepat","refleks_kanan":"lambat"},"pupil_score":1,"gcs_p":null,"respons":"a"}`, true)
	// e. full GCS 4/5/6 + pupil 2.5/2.5 + refleks cepat/cepat (gcs 15, ps 0, gcs_p 15)
	consciousnessShapeCase(t, "e_gcs15_with_pupil", `{"e":"4","v":"5","m":"6","gcs":15,"kategori":"compos_mentis","pupil":{"kiri_mm":2.5,"kanan_mm":2.5,"bentuk":"","refleks_kiri":"cepat","refleks_kanan":"cepat"},"pupil_score":0,"gcs_p":15,"respons":"a"}`, true)
	// f. GCS 3 (1/1/1) + refleks tidak_ada/tidak_ada (ps 4, gcs_p 1)
	consciousnessShapeCase(t, "f_gcs3", `{"e":"1","v":"1","m":"1","gcs":3,"kategori":"koma","pupil":{"kiri_mm":5,"kanan_mm":5,"bentuk":"","refleks_kiri":"tidak_ada","refleks_kanan":"tidak_ada"},"pupil_score":4,"gcs_p":1,"respons":"u"}`, true)
	// g. v="T" (hasT) + pupil terisi
	consciousnessShapeCase(t, "g_T_with_pupil", `{"e":"1","v":"T","m":"1","gcs":null,"kategori":null,"pupil":{"kiri_mm":2.5,"kanan_mm":null,"bentuk":"","refleks_kiri":"lambat","refleks_kanan":null},"pupil_score":2,"gcs_p":null,"respons":"v"}`, true)
	// h. v="" kosong
	consciousnessShapeCase(t, "h_v_empty", `{"e":"","v":"","m":"","gcs":null,"kategori":"","pupil":{"kiri_mm":null,"kanan_mm":null,"bentuk":"","refleks_kiri":null,"refleks_kanan":null},"pupil_score":null,"gcs_p":null,"respons":""}`, true)
	// i. kiri_mm 0 → di luar 0<v<10, ditolak shape
	consciousnessShapeCase(t, "i_mm_zero", `{"pupil":{"kiri_mm":0,"kanan_mm":null,"bentuk":"","refleks_kiri":null,"refleks_kanan":null}}`, false)
	// j. kiri_mm 10 → di luar 0<v<10, ditolak shape
	consciousnessShapeCase(t, "j_mm_ten", `{"pupil":{"kiri_mm":10,"kanan_mm":null,"bentuk":"","refleks_kiri":null,"refleks_kanan":null}}`, false)
	// k. kiri_mm 9.5 valid
	consciousnessShapeCase(t, "k_mm_nine_half", `{"pupil":{"kiri_mm":9.5,"kanan_mm":null,"bentuk":"","refleks_kiri":null,"refleks_kanan":null}}`, true)
	// l. pupil mm dikirim sebagai string "2.5" → bukan number, shape invalid
	consciousnessShapeCase(t, "l_mm_string", `{"pupil":{"kiri_mm":"2.5","kanan_mm":null,"bentuk":"","refleks_kiri":null,"refleks_kanan":null}}`, false)
	// m. bentuk "" + refleks null
	consciousnessShapeCase(t, "m_bentuk_empty", `{"pupil":{"kiri_mm":null,"kanan_mm":null,"bentuk":"","refleks_kiri":null,"refleks_kanan":null}}`, true)
	// n. respons "a"
	consciousnessShapeCase(t, "n_respons_a", `{"respons":"a"}`, true)
	// o. e/v/m parsial + pupil_score terisi
	consciousnessShapeCase(t, "o_evm_partial", `{"e":"4","gcs":4,"kategori":"","pupil":{"kiri_mm":null,"kanan_mm":null,"bentuk":"","refleks_kiri":null,"refleks_kanan":null},"pupil_score":2,"gcs_p":4,"respons":null}`, true)
	// negative shape controls
	consciousnessShapeCase(t, "neg_respons_bad", `{"respons":"x"}`, false)
	consciousnessShapeCase(t, "neg_refleks_bad", `{"pupil":{"kiri_mm":null,"kanan_mm":null,"bentuk":"","refleks_kiri":"cepat","refleks_kanan":"banjir"}}`, false)
	consciousnessShapeCase(t, "neg_kategori_bad", `{"kategori":"tidur"}`, false)
	consciousnessShapeCase(t, "neg_T_with_gcs", `{"e":"1","v":"T","m":"1","gcs":3}`, false)
	consciousnessShapeCase(t, "neg_pupil_score_5", `{"pupil_score":5}`, false)
	consciousnessShapeCase(t, "neg_gcs_p_negative", `{"gcs_p":-1}`, false)
	consciousnessShapeCase(t, "neg_e_out_of_range", `{"e":"5"}`, false)
}
