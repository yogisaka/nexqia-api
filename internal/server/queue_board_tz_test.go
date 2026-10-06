//go:build integration

package server_test

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// TestQueueBoard_DateFilterUsesMerchantTimezone — ?date= selects tickets
// created on that merchant-local day, not the DB session's date.
func TestQueueBoard_DateFilterUsesMerchantTimezone(t *testing.T) {
	env := dnNewEnv(t, "board", dnTimezone)
	deptID := env.seedDepartment(t, "DNB")
	personID := env.seedPerson(t, "Board Person")
	midnight := dnLocalMidnight(t, dnTimezone)
	for _, tk := range []struct {
		number string
		at     time.Time
	}{
		{"DNB-BEFORE", midnight.Add(-time.Minute)},
		{"DNB-AFTER", midnight.Add(time.Minute)},
	} {
		if _, err := env.pool.Exec(context.Background(),
			"INSERT INTO operations.queue (company_id, merchant_id, queue_type, department_id, person_id, queue_number, status, created_at) VALUES ($1, $2, 'dokter', $3, $4, $5, 'waiting', $6)",
			env.companyID, env.merchantID, deptID, personID, tk.number, tk.at); err != nil {
			t.Fatalf("seed ticket %s: %v", tk.number, err)
		}
	}

	code, resp := qeRequest(t, env, http.MethodGet, "/api/v1/queue/board?queue_type=dokter&date="+midnight.Format("2006-01-02"), nil)
	if code != http.StatusOK {
		t.Fatalf("queue board expected 200, got %d: %v", code, resp)
	}
	rows, ok := resp["data"].([]any)
	if !ok {
		t.Fatalf("queue board data is not a list: %v", resp)
	}
	var got []string
	for _, r := range rows {
		got = append(got, r.(map[string]any)["QueueNumber"].(string))
	}
	if len(got) != 1 || got[0] != "DNB-AFTER" {
		t.Fatalf("queue board for local today = %v, want [DNB-AFTER]", got)
	}
}
