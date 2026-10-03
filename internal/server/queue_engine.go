// internal/server/queue_engine.go
package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

// Sentinel errors mapped to explicit HTTP statuses by the queue/admission
// handlers — never rendered as 500 (spec 2026-10-01-c-queue-flow-display §4).
var (
	errFlowNotApplies = errors.New("flow does not apply")
	errNoQueueFlow    = errors.New("no queue flow configured")
)

// queueTypeForKind maps stage.kind to the legacy operations.queue.queue_type
// discriminator (spec §3) so queue_board, old screens and counters keep
// working alongside flow-driven tickets.
func queueTypeForKind(kind string) string {
	switch kind {
	case "admission":
		return "pendaftaran"
	case "nurse":
		return "perawat"
	case "physician":
		return "dokter"
	case "checkin":
		return "checkin"
	case "cashier":
		return "kasir"
	case "pharmacy":
		return "farmasi"
	case "support":
		return "penunjang"
	default:
		return "custom"
	}
}

// payerQueueCategory maps payer_type to a queue_flow.service_types category
// (spec §4: bpjs→bpjs, self_pay/none→umum, private_insurance/company_guarantor
// →asuransi).
func payerQueueCategory(payerType string) string {
	switch payerType {
	case "bpjs":
		return "bpjs"
	case "private_insurance", "company_guarantor":
		return "asuransi"
	default:
		return "umum"
	}
}

// flowAppliesTo reports whether the flow's service_types contain the payer
// category and its department_ids are empty (all polis) or contain department.
func flowAppliesTo(flow sqlcgen.OperationsQueueFlow, category string, departmentID pgtype.UUID) bool {
	matched := false
	for _, st := range flow.ServiceTypes {
		if st == category {
			matched = true
		}
	}
	if !matched {
		return false
	}
	if len(flow.DepartmentIds) == 0 {
		return true
	}
	for _, d := range flow.DepartmentIds {
		if d == departmentID {
			return true
		}
	}
	return false
}

// defaultFlowStages mirrors the migration 000057 backfill — the built-in
// "Rawat Jalan (bawaan)" flow with NULL number_prefix (legacy numbering).
var defaultFlowStages = []struct {
	Seq  int32
	Name string
	Kind string
	Perm string
}{
	{1, "Pendaftaran", "admission", PermCounterManage},
	{2, "Perawat", "nurse", PermVisitManage},
	{3, "Dokter", "physician", PermVisitManage},
}

// resolveFlow picks the active flow for an admission (spec §4): explicit flow
// (must still apply, else errFlowNotApplies) → matching default flow → first
// matching flow by name. A merchant with no flow at all gets the built-in
// 3-stage default flow created in the same transaction.
func resolveFlow(ctx context.Context, q *sqlcgen.Queries, merchantID, departmentID pgtype.UUID, payerType string, explicitFlowID pgtype.UUID) (sqlcgen.OperationsQueueFlow, []sqlcgen.OperationsQueueStage, error) {
	category := payerQueueCategory(payerType)
	flows, err := q.ListActiveQueueFlows(ctx, merchantID)
	if err != nil {
		return sqlcgen.OperationsQueueFlow{}, nil, err
	}

	if len(flows) == 0 {
		companyID, err := q.GetMerchantCompanyID(ctx, merchantID)
		if err != nil {
			return sqlcgen.OperationsQueueFlow{}, nil, err
		}
		flow, err := q.CreateQueueFlowAuto(ctx, sqlcgen.CreateQueueFlowAutoParams{
			CompanyID: companyID, MerchantID: merchantID, Name: "Rawat Jalan (bawaan)",
			ServiceTypes: []string{"umum", "bpjs", "asuransi"}, DepartmentIds: []pgtype.UUID{}, IsDefault: true,
		})
		if err != nil {
			return sqlcgen.OperationsQueueFlow{}, nil, err
		}
		for _, s := range defaultFlowStages {
			if _, err := q.CreateQueueStage(ctx, sqlcgen.CreateQueueStageParams{
				CompanyID: companyID, MerchantID: merchantID, FlowID: flow.ID,
				Seq: s.Seq, Name: s.Name, Kind: s.Kind, ServedByPermission: s.Perm,
			}); err != nil {
				return sqlcgen.OperationsQueueFlow{}, nil, err
			}
		}
		stages, err := q.ListQueueStagesByFlow(ctx, flow.ID)
		return flow, stages, err
	}

	var chosen *sqlcgen.OperationsQueueFlow
	if explicitFlowID.Valid {
		for i := range flows {
			if flows[i].ID == explicitFlowID {
				if flowAppliesTo(flows[i], category, departmentID) {
					chosen = &flows[i]
				}
				break
			}
		}
		if chosen == nil {
			return sqlcgen.OperationsQueueFlow{}, nil, errFlowNotApplies
		}
	} else {
		for i := range flows {
			if flows[i].IsDefault && flowAppliesTo(flows[i], category, departmentID) {
				chosen = &flows[i]
				break
			}
		}
		if chosen == nil {
			// ListActiveQueueFlows orders by name, so this is "first by name".
			for i := range flows {
				if flowAppliesTo(flows[i], category, departmentID) {
					chosen = &flows[i]
					break
				}
			}
		}
		if chosen == nil {
			return sqlcgen.OperationsQueueFlow{}, nil, errNoQueueFlow
		}
	}

	stages, err := q.ListQueueStagesByFlow(ctx, chosen.ID)
	return *chosen, stages, err
}

// scheduleRoomCounter resolves the schedule-room-bound counter for a physician
// stage (spec §4): the stage's counter with binding='schedule_room' whose
// location_id equals the session room — schedule_session.room_id for today
// (merchant timezone), else physician_schedule.room_id. No match → NULL.
func scheduleRoomCounter(ctx context.Context, q *sqlcgen.Queries, stage sqlcgen.OperationsQueueStage, admission sqlcgen.OperationsAdmission) pgtype.UUID {
	if !admission.ScheduleID.Valid {
		return pgtype.UUID{}
	}
	counters, err := q.ListCountersForStage(ctx, stage.ID)
	if err != nil || len(counters) == 0 {
		return pgtype.UUID{}
	}
	loc, err := merchantTimezone(ctx, q, admission.MerchantID)
	if err != nil {
		return pgtype.UUID{}
	}
	now := time.Now().In(loc)
	today := pgtype.Date{Time: time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc), Valid: true}

	var roomID pgtype.UUID
	session, err := q.GetScheduleSessionByScheduleAndDate(ctx, sqlcgen.GetScheduleSessionByScheduleAndDateParams{
		ScheduleID: admission.ScheduleID, SessionDate: today,
	})
	switch {
	case err == nil && session.RoomID.Valid:
		roomID = session.RoomID
	case errors.Is(err, pgx.ErrNoRows):
		pattern, err := q.GetPhysicianScheduleByID(ctx, admission.ScheduleID)
		if err == nil && pattern.RoomID.Valid {
			roomID = pattern.RoomID
		}
	case err != nil:
		return pgtype.UUID{}
	}

	for _, ctr := range counters {
		if ctr.Binding == "schedule_room" && ctr.LocationID.Valid && ctr.LocationID == roomID {
			return ctr.ID
		}
	}
	return pgtype.UUID{}
}

// merchantTimezone loads the merchant's timezone as a *time.Location.
func merchantTimezone(ctx context.Context, q *sqlcgen.Queries, merchantID pgtype.UUID) (*time.Location, error) {
	tz, err := q.GetMerchantTimezone(ctx, merchantID)
	if err != nil {
		return nil, err
	}
	return time.LoadLocation(tz)
}

// issueTicket creates the queue row for one stage of a journey (spec §4):
// number "<prefix>-NNN" or legacy "<poli code>-NNN" when prefix is NULL, NNN =
// today's ticket count for the stage (merchant timezone) + 1; queue_type from
// stage.kind; checked_in_at = now(). journey may be nil for legacy
// one-shot tickets created without a journey. For a physician stage the
// ticket's counter is resolved from the schedule room binding.
func issueTicket(ctx context.Context, q *sqlcgen.Queries, journey *sqlcgen.OperationsQueueJourney, stage sqlcgen.OperationsQueueStage, counterID pgtype.UUID) (sqlcgen.OperationsQueue, error) {
	admission, err := q.GetAdmissionByID(ctx, journey.AdmissionID)
	if err != nil {
		return sqlcgen.OperationsQueue{}, err
	}
	department, err := q.GetDepartmentByID(ctx, admission.DepartmentID)
	if err != nil {
		return sqlcgen.OperationsQueue{}, err
	}

	if !counterID.Valid && stage.Kind == "physician" {
		counterID = scheduleRoomCounter(ctx, q, stage, admission)
	}

	loc, err := merchantTimezone(ctx, q, journey.MerchantID)
	if err != nil {
		return sqlcgen.OperationsQueue{}, err
	}
	now := time.Now().In(loc)
	startOfDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	n, err := q.CountTicketsForStageSince(ctx, sqlcgen.CountTicketsForStageSinceParams{
		StageID: stage.ID, CreatedAt: pgtype.Timestamptz{Time: startOfDay, Valid: true},
	})
	if err != nil {
		return sqlcgen.OperationsQueue{}, err
	}
	number := fmt.Sprintf("%s-%03d", department.Code, n+1)
	if stage.NumberPrefix.Valid {
		number = fmt.Sprintf("%s-%03d", stage.NumberPrefix.String, n+1)
	}

	var journeyID pgtype.UUID
	if journey != nil {
		journeyID = journey.ID
	}
	return q.CreateQueueFlowTicket(ctx, sqlcgen.CreateQueueFlowTicketParams{
		CompanyID: journey.CompanyID, MerchantID: journey.MerchantID, QueueType: queueTypeForKind(stage.Kind),
		DepartmentID: admission.DepartmentID, PersonID: journey.PersonID, AdmissionID: journey.AdmissionID,
		QueueNumber: number, JourneyID: journeyID, StageID: stage.ID, CounterID: counterID, CreatedBy: journey.CreatedBy,
	})
}

// stageAdvance is the outcome of finishing a stage ticket: the finished row,
// optionally the next stage's ticket and/or the updated journey.
type stageAdvance struct {
	Ticket  *sqlcgen.OperationsQueue
	Next    *sqlcgen.OperationsQueue
	Journey *sqlcgen.OperationsQueueJourney
}

// legacyNextQueueType is the old hardcoded pipeline order, used exactly once
// for pre-#23 tickets that carry no journey_id (migration-day queues must not
// get stuck).
var legacyNextQueueType = map[string]string{"pendaftaran": "perawat", "perawat": "dokter"}

// advanceStageTicket finishes a ticket (status done or skipped) and advances
// the flow (spec §4): next stage exists → requires_checkin ? journey
// awaiting_checkin without a ticket : new ticket + journey in_progress; no
// next stage → journey completed. Legacy tickets without journey_id advance
// once through the default flow, keeping the old numbering.
func advanceStageTicket(ctx context.Context, q *sqlcgen.Queries, ticket sqlcgen.OperationsQueue, newStatus string, userID pgtype.UUID) (stageAdvance, error) {
	finished, err := q.FinishQueueTicket(ctx, sqlcgen.FinishQueueTicketParams{ID: ticket.ID, Status: newStatus, UpdatedBy: userID})
	if err != nil {
		return stageAdvance{}, err
	}
	res := stageAdvance{Ticket: &finished}

	if !ticket.JourneyID.Valid {
		nextType, ok := legacyNextQueueType[ticket.QueueType]
		if !ok {
			return res, nil
		}
		_, stages, err := resolveFlow(ctx, q, ticket.MerchantID, ticket.DepartmentID, "", pgtype.UUID{})
		if err != nil {
			if errors.Is(err, errNoQueueFlow) {
				return res, nil
			}
			return res, err
		}
		var nextStage *sqlcgen.OperationsQueueStage
		for i := range stages {
			if queueTypeForKind(stages[i].Kind) == nextType {
				nextStage = &stages[i]
				break
			}
		}
		if nextStage == nil {
			return res, nil
		}
		seq, err := q.CountTodayQueueByType(ctx, sqlcgen.CountTodayQueueByTypeParams{
			MerchantID: ticket.MerchantID, QueueType: nextType, DepartmentID: ticket.DepartmentID,
		})
		if err != nil {
			return res, err
		}
		department, err := q.GetDepartmentByID(ctx, ticket.DepartmentID)
		if err != nil {
			return res, err
		}
		created, err := q.CreateQueueFlowTicket(ctx, sqlcgen.CreateQueueFlowTicketParams{
			CompanyID: ticket.CompanyID, MerchantID: ticket.MerchantID, QueueType: nextType,
			DepartmentID: ticket.DepartmentID, PersonID: ticket.PersonID, AdmissionID: ticket.AdmissionID,
			QueueNumber: fmt.Sprintf("%s-%03d", department.Code, seq+1),
			JourneyID:   pgtype.UUID{}, StageID: nextStage.ID, CounterID: pgtype.UUID{}, CreatedBy: userID,
		})
		if err != nil {
			return res, err
		}
		res.Next = &created
		return res, nil
	}

	journey, err := q.GetQueueJourneyByID(ctx, ticket.JourneyID)
	if err != nil {
		return res, err
	}
	stage, err := q.GetQueueStageByID(ctx, ticket.StageID)
	if err != nil {
		return res, err
	}
	next, err := q.NextQueueStageForFlow(ctx, sqlcgen.NextQueueStageForFlowParams{FlowID: stage.FlowID, Seq: stage.Seq})
	if errors.Is(err, pgx.ErrNoRows) {
		updated, err := q.UpdateQueueJourneyStage(ctx, sqlcgen.UpdateQueueJourneyStageParams{
			ID: journey.ID, CurrentStageID: journey.CurrentStageID, Status: "completed", UpdatedBy: userID,
		})
		if err != nil {
			return res, err
		}
		res.Journey = &updated
		return res, nil
	}
	if err != nil {
		return res, err
	}
	if next.RequiresCheckin {
		updated, err := q.UpdateQueueJourneyStage(ctx, sqlcgen.UpdateQueueJourneyStageParams{
			ID: journey.ID, CurrentStageID: next.ID, Status: "awaiting_checkin", UpdatedBy: userID,
		})
		if err != nil {
			return res, err
		}
		res.Journey = &updated
		return res, nil
	}
	created, err := issueTicket(ctx, q, &journey, next, pgtype.UUID{})
	if err != nil {
		return res, err
	}
	updated, err := q.UpdateQueueJourneyStage(ctx, sqlcgen.UpdateQueueJourneyStageParams{
		ID: journey.ID, CurrentStageID: next.ID, Status: "in_progress", UpdatedBy: userID,
	})
	if err != nil {
		return res, err
	}
	res.Next = &created
	res.Journey = &updated
	return res, nil
}
