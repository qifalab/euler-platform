// Euler derivative of WitShield (Apache-2.0); imports and integration may be modified. See module NOTICE.

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/qifalab/euler-platform/services/witshield-engine/internal/domain"
)

type approvedPlanAction struct {
	incidentID, planID, actionID, commandID string
	approvedAt                              time.Time
}

// prepareApprovedPlanAction drives the production state machine from a runtime
// signal to an administrator-approved response-plan action whose execute
// command is queued for the device: incident=responding, plan=executing.
func prepareApprovedPlanAction(t *testing.T, s *Store, deviceID string, now time.Time, actionType string, params json.RawMessage) approvedPlanAction {
	t.Helper()
	ctx := context.Background()
	event := domain.SecurityEvent{ID: "evt_plan_source", DeviceID: deviceID, Type: "file_integrity_changed", OccurredAt: now, Payload: json.RawMessage(`{"path":"/etc/witshield/test"}`)}
	if _, err := s.ProcessSecurityEvent(ctx, event, false, now); err != nil {
		t.Fatal(err)
	}
	incidents, err := s.ListIncidents(ctx, deviceID, nil, 10)
	if err != nil || len(incidents) != 1 {
		t.Fatalf("incidents=%#v err=%v", incidents, err)
	}
	investigation, err := s.CreateInvestigation(ctx, incidents[0].ID, "test", "test-model", now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	investigation.Status, investigation.Hypothesis, investigation.Conclusion, investigation.Confidence = domain.InvestigationCompleted, "drift", "needs remediation", 90
	plan := &domain.ResponsePlan{Title: "remediate", Rationale: "restore", Risk: "medium", RequiresApproval: true, Steps: []domain.ResponseStep{{ID: "step_one", ActionType: actionType, Title: "remediate", Rationale: "restore", Parameters: params, Risk: "medium", RequiresApproval: true}}}
	if err = s.CompleteInvestigation(ctx, investigation, plan, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	draft := domain.Action{ID: "act_plan_step", DeviceID: deviceID, Type: actionType, Parameters: params, Preview: json.RawMessage(`{"summary":"remediate"}`), Status: domain.ActionDraft, CreatedAt: now.Add(3 * time.Minute), UpdatedAt: now.Add(3 * time.Minute)}
	if err = s.CreateActionFromResponsePlan(ctx, plan.ID, "step_one", draft, "nonce-hash", "admin", 5); err != nil {
		t.Fatal(err)
	}
	approvedAt := now.Add(4 * time.Minute)
	payload, _ := json.Marshal(map[string]any{"actionId": draft.ID, "type": actionType, "parameters": params})
	command := domain.DeviceCommand{ID: "cmd_plan_step", DeviceID: deviceID, Type: domain.CommandExecuteAction, Payload: payload, CreatedAt: approvedAt}
	if _, err = s.ApproveActionAndEnqueue(ctx, draft.ID, "nonce-hash", "admin", command, approvedAt); err != nil {
		t.Fatal(err)
	}
	result := approvedPlanAction{incidentID: incidents[0].ID, planID: plan.ID, actionID: draft.ID, commandID: command.ID, approvedAt: approvedAt}
	assertPlanProjection(t, s, result, domain.ResponsePlanExecuting, domain.IncidentResponding)
	return result
}

func assertPlanProjection(t *testing.T, s *Store, item approvedPlanAction, wantPlan domain.ResponsePlanStatus, wantIncident domain.IncidentStatus) {
	t.Helper()
	plan, err := s.ResponsePlan(context.Background(), item.planID)
	if err != nil {
		t.Fatal(err)
	}
	incident, err := s.Incident(context.Background(), item.incidentID)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Status != wantPlan || incident.Status != wantIncident {
		t.Fatalf("plan=%s incident=%s, want plan=%s incident=%s", plan.Status, incident.Status, wantPlan, wantIncident)
	}
}

// A response-plan action can reach a terminal state without any device
// receipt: delivery TTL expiry, execution timeout, start-gate expiry, device
// revocation, or an elapsed SSH confirmation window. The plan and incident must
// follow, otherwise the incident stays "responding" forever and can never be
// investigated again (CreateInvestigation requires open/monitoring).
func TestResponsePlanFollowsActionsTerminatedWithoutReceipt(t *testing.T) {
	fileParams := json.RawMessage(`{"path":"/etc/witshield/test","mode":"600"}`)
	sshParams := json.RawMessage(`{"disablePasswordAuthentication":true}`)
	tests := []struct {
		name       string
		actionType string
		params     json.RawMessage
		terminate  func(t *testing.T, s *Store, deviceID string, item approvedPlanAction) domain.ActionStatus
	}{
		{name: "delivery TTL expired", actionType: "file_permission_repair", params: fileParams, terminate: func(t *testing.T, s *Store, _ string, item approvedPlanAction) domain.ActionStatus {
			if err := s.ExpireStaleActionCommands(context.Background(), item.approvedAt.Add(domain.ActionCommandTTL+time.Minute)); err != nil {
				t.Fatal(err)
			}
			return domain.ActionFailed
		}},
		{name: "started command timed out", actionType: "file_permission_repair", params: fileParams, terminate: func(t *testing.T, s *Store, deviceID string, item approvedPlanAction) domain.ActionStatus {
			if allowed, err := s.StartActionCommand(context.Background(), deviceID, item.commandID, item.approvedAt.Add(time.Minute)); err != nil || !allowed {
				t.Fatalf("start allowed=%v err=%v", allowed, err)
			}
			if err := s.ExpireStaleActionCommands(context.Background(), item.approvedAt.Add(domain.ActionExecutionTimeout+2*time.Minute)); err != nil {
				t.Fatal(err)
			}
			return domain.ActionIndeterminate
		}},
		{name: "start gate saw an expired command", actionType: "file_permission_repair", params: fileParams, terminate: func(t *testing.T, s *Store, deviceID string, item approvedPlanAction) domain.ActionStatus {
			if allowed, err := s.StartActionCommand(context.Background(), deviceID, item.commandID, item.approvedAt.Add(domain.ActionCommandTTL+time.Minute)); err != nil || allowed {
				t.Fatalf("start allowed=%v err=%v", allowed, err)
			}
			return domain.ActionCancelled
		}},
		{name: "device revoked", actionType: "file_permission_repair", params: fileParams, terminate: func(t *testing.T, s *Store, deviceID string, item approvedPlanAction) domain.ActionStatus {
			if err := s.RevokeDevice(context.Background(), deviceID, item.approvedAt.Add(time.Minute)); err != nil {
				t.Fatal(err)
			}
			return domain.ActionCancelled
		}},
		{name: "SSH confirmation window elapsed", actionType: "ssh_password_hardening", params: sshParams, terminate: func(t *testing.T, s *Store, deviceID string, item approvedPlanAction) domain.ActionStatus {
			ctx := context.Background()
			started := item.approvedAt.Add(time.Minute)
			if allowed, err := s.StartActionCommand(ctx, deviceID, item.commandID, started); err != nil || !allowed {
				t.Fatalf("start allowed=%v err=%v", allowed, err)
			}
			// Project a verified SSH execution that now awaits the administrator's
			// second-session confirmation (the receipt path keeps the plan executing).
			confirmBy := started.Add(10 * time.Minute)
			if _, err := s.db.ExecContext(ctx, `UPDATE actions SET status=?,rollback_payload='{"state":"sealed"}',confirm_by=?,completed_at=NULL WHERE id=?`, string(domain.ActionAwaitingConfirmation), timeText(confirmBy), item.actionID); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.ExecContext(ctx, `UPDATE device_commands SET completed_at=?,result='{"ok":true}' WHERE id=?`, timeText(started.Add(time.Second)), item.commandID); err != nil {
				t.Fatal(err)
			}
			if err := s.ExpireActionConfirmations(ctx, confirmBy.Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			return domain.ActionCancelled
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s, deviceID, now := openReportCommandTestStore(t)
			item := prepareApprovedPlanAction(t, s, deviceID, now, test.actionType, test.params)
			wantAction := test.terminate(t, s, deviceID, item)
			stored, _, err := s.Action(context.Background(), item.actionID)
			if err != nil || stored.Status != wantAction {
				t.Fatalf("action status=%s want=%s err=%v", stored.Status, wantAction, err)
			}
			assertPlanProjection(t, s, item, domain.ResponsePlanFailed, domain.IncidentOpen)
			if _, err = s.CreateInvestigation(context.Background(), item.incidentID, "retry", "test-model", item.approvedAt.Add(3*time.Hour)); err != nil {
				t.Fatalf("incident could not be investigated again: %v", err)
			}
		})
	}
}

// An administrator may resolve an incident while its response action is still
// running. A new signal for the same subject then opens a second active case
// with the same correlation key. The late action receipt must not try to move
// the resolved case back into an active state: that violates the unique
// active-correlation index, the receipt is rejected with HTTP 500 on every
// retry, and the Agent's durable FIFO queue (reports, events) blocks forever.
func TestActionReceiptDoesNotReopenAdministratorResolvedIncident(t *testing.T) {
	s, deviceID, now := openReportCommandTestStore(t)
	ctx := context.Background()
	item := prepareApprovedPlanAction(t, s, deviceID, now, "file_permission_repair", json.RawMessage(`{"path":"/etc/witshield/test","mode":"600"}`))
	if allowed, err := s.StartActionCommand(ctx, deviceID, item.commandID, item.approvedAt.Add(time.Minute)); err != nil || !allowed {
		t.Fatalf("start allowed=%v err=%v", allowed, err)
	}
	if err := s.SetIncidentStatus(ctx, item.incidentID, domain.IncidentResolved, "admin", "resolved by administrator", item.approvedAt.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	again := domain.SecurityEvent{ID: "evt_plan_source_again", DeviceID: deviceID, Type: "file_integrity_changed", OccurredAt: item.approvedAt.Add(2 * time.Minute), Payload: json.RawMessage(`{"path":"/etc/witshield/test"}`)}
	if _, err := s.ProcessSecurityEvent(ctx, again, false, item.approvedAt.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	active, err := s.ListIncidents(ctx, deviceID, []domain.IncidentStatus{domain.IncidentOpen}, 10)
	if err != nil || len(active) != 1 || active[0].ID == item.incidentID {
		t.Fatalf("expected one new open incident: %#v err=%v", active, err)
	}
	if _, err = s.CompleteCommandAndActionWithOutcome(ctx, deviceID, item.commandID, false, nil, nil, nil, "helper execution failed", item.approvedAt.Add(3*time.Minute)); err != nil {
		t.Fatalf("authentic failure receipt was rejected: %v", err)
	}
	assertPlanProjection(t, s, item, domain.ResponsePlanFailed, domain.IncidentResolved)
	stored, _, err := s.Action(ctx, item.actionID)
	if err != nil || stored.Status != domain.ActionFailed {
		t.Fatalf("action status=%s err=%v", stored.Status, err)
	}
}

// Response-plan step bindings reference their action with ON DELETE RESTRICT.
// Terminal-action retention must skip bound actions; otherwise SQLite aborts
// the whole retention statement and no device's action history is ever pruned.
func TestCompactActionRetentionSkipsResponsePlanBoundActions(t *testing.T) {
	s, deviceID, now := openReportCommandTestStore(t)
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	total := maxTerminalActionsPerDevice + 2
	for index := 0; index < total; index++ {
		created := now.Add(time.Duration(index) * time.Second)
		if _, err = tx.ExecContext(ctx, `INSERT INTO actions(id,device_id,finding_id,type,parameters,preview,status,approval_nonce_hash,approved_by,approved_at,executed_at,completed_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, fmt.Sprintf("act_retention_%04d", index), deviceID, "", "file_permission_repair", `{}`, `{}`, string(domain.ActionSucceeded), "", "admin", timeText(created), timeText(created), timeText(created), timeText(created), timeText(created)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO incidents(id,device_id,correlation_key,category,severity,status,title,summary,signal_count,first_seen_at,last_seen_at,created_at,updated_at) VALUES('inc_retention',?,'finding:retention','file_integrity','high','monitoring','t','s',1,?,?,?,?)`, deviceID, timeText(now), timeText(now), timeText(now), timeText(now)); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO response_plans(id,incident_id,investigation_id,title,rationale,risk,status,requires_approval,steps,created_at,updated_at) VALUES('rsp_retention','inc_retention','','t','r','medium','completed',1,'[{"id":"step_one","actionType":"file_permission_repair","title":"t","rationale":"r","parameters":{},"risk":"medium","requiresApproval":true,"actionId":"act_retention_0000"}]',?,?)`, timeText(now), timeText(now)); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO response_plan_actions(plan_id,step_id,action_id,created_at) VALUES('rsp_retention','step_one','act_retention_0000',?)`, timeText(now)); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = s.Compact(ctx, now.Add(2*time.Hour)); err != nil {
		t.Fatalf("compaction failed: %v", err)
	}
	if _, _, err = s.Action(ctx, "act_retention_0000"); err != nil {
		t.Fatalf("response-plan bound action was not retained: %v", err)
	}
	if _, _, err = s.Action(ctx, "act_retention_0001"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("oldest unbound terminal action was not pruned: %v", err)
	}
	var remaining int
	if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM actions WHERE device_id=?`, deviceID).Scan(&remaining); err != nil || remaining != total-1 {
		t.Fatalf("remaining actions=%d want=%d err=%v", remaining, total-1, err)
	}
}

// Reopening a resolved incident while a newer active case already owns its
// correlation key must be reported as a conflict (HTTP 409), not as an internal
// error (HTTP 500).
func TestReopenIncidentWithActiveDuplicateIsConflict(t *testing.T) {
	s, deviceID, now := openReportCommandTestStore(t)
	ctx := context.Background()
	first := domain.SecurityEvent{ID: "evt_reopen_one", DeviceID: deviceID, Type: "file_integrity_changed", OccurredAt: now, Payload: json.RawMessage(`{"path":"/etc/witshield/test"}`)}
	if _, err := s.ProcessSecurityEvent(ctx, first, false, now); err != nil {
		t.Fatal(err)
	}
	incidents, err := s.ListIncidents(ctx, deviceID, nil, 10)
	if err != nil || len(incidents) != 1 {
		t.Fatalf("incidents=%#v err=%v", incidents, err)
	}
	resolvedID := incidents[0].ID
	if err = s.SetIncidentStatus(ctx, resolvedID, domain.IncidentResolved, "admin", "resolved", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	second := first
	second.ID, second.OccurredAt = "evt_reopen_two", now.Add(2*time.Minute)
	if _, err = s.ProcessSecurityEvent(ctx, second, false, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err = s.SetIncidentStatus(ctx, resolvedID, domain.IncidentOpen, "admin", "reopen", now.Add(3*time.Minute)); !errors.Is(err, ErrConflict) {
		t.Fatalf("reopen with an active duplicate err=%v, want ErrConflict", err)
	}
	incident, err := s.Incident(ctx, resolvedID)
	if err != nil || incident.Status != domain.IncidentResolved {
		t.Fatalf("rejected reopen changed the incident: %#v err=%v", incident, err)
	}
}

// Version-1 development databases lack device_commands.started_at and the exact
// security-correlation columns. The base schema must not index those columns
// before the upgrade adds them, or opening such a database always fails.
func TestLegacyDatabaseWithoutLaterColumnsStillMigrates(t *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "legacy-v1.sqlite")
	s, err := Open(ctx, databasePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`DROP INDEX commands_unstarted_action_expiry_idx`,
		`DROP INDEX commands_device_unstarted_action_expiry_idx`,
		`DROP INDEX commands_started_action_expiry_idx`,
		`ALTER TABLE device_commands DROP COLUMN started_at`,
		`DROP INDEX security_event_windows_expiry_idx`,
		`ALTER TABLE security_event_windows DROP COLUMN window_ends_at`,
		`ALTER TABLE security_event_windows DROP COLUMN event_times`,
	} {
		if _, err = s.db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("prepare legacy schema %q: %v", statement, err)
		}
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, databasePath)
	if err != nil {
		t.Fatalf("legacy database could not be opened: %v", err)
	}
	defer s.Close()
	for _, column := range []struct{ table, name string }{{"device_commands", "started_at"}, {"security_event_windows", "window_ends_at"}, {"security_event_windows", "event_times"}} {
		exists, inspectErr := s.tableHasColumn(ctx, column.table, column.name)
		if inspectErr != nil || !exists {
			t.Fatalf("%s.%s missing after upgrade: exists=%v err=%v", column.table, column.name, exists, inspectErr)
		}
	}
	for _, index := range []string{"commands_unstarted_action_expiry_idx", "commands_device_unstarted_action_expiry_idx", "commands_started_action_expiry_idx", "security_event_windows_expiry_idx"} {
		var count int
		if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='index' AND name=?`, index).Scan(&count); err != nil || count != 1 {
			t.Fatalf("index %s missing after upgrade: count=%d err=%v", index, count, err)
		}
	}
}

// The incident detail view (limit 200) and the AI investigator context (limit
// 50) read a bounded timeline window. Retention keeps the newest entries, so
// the window must be the newest entries too (still in chronological order);
// otherwise a long-lived case shows only its opening history.
func TestIncidentTimelineWindowKeepsNewestEntriesInOrder(t *testing.T) {
	s, deviceID, now := openReportCommandTestStore(t)
	ctx := context.Background()
	for index := 0; index < 6; index++ {
		at := now.Add(time.Duration(index) * time.Minute)
		event := domain.SecurityEvent{ID: fmt.Sprintf("evt_timeline_%d", index), DeviceID: deviceID, Type: "service_definition_changed", OccurredAt: at, Payload: json.RawMessage(`{"path":"/etc/systemd/system/api.service"}`)}
		if _, err := s.ProcessSecurityEvent(ctx, event, false, at); err != nil {
			t.Fatal(err)
		}
	}
	incidents, err := s.ListIncidents(ctx, deviceID, nil, 10)
	if err != nil || len(incidents) != 1 {
		t.Fatalf("incidents=%#v err=%v", incidents, err)
	}
	all, err := s.ListIncidentTimeline(ctx, incidents[0].ID, 100)
	if err != nil || len(all) != 6 {
		t.Fatalf("timeline=%d err=%v", len(all), err)
	}
	window, err := s.ListIncidentTimeline(ctx, incidents[0].ID, 3)
	if err != nil || len(window) != 3 {
		t.Fatalf("window=%d err=%v", len(window), err)
	}
	for index, item := range window {
		if want := all[len(all)-3+index]; item.ID != want.ID {
			t.Fatalf("window[%d]=%d (%s at %s), want newest entries in order; full=%v", index, item.ID, item.Type, item.CreatedAt, all)
		}
	}
}

// An administrator may resolve or dismiss an incident while an AI
// investigation is in flight (up to 35 s). Completing that investigation must
// keep the administrator's decision (the plan is still recorded), and must not
// fail on the unique active-correlation index when a newer case is active.
func TestInvestigationCompletionKeepsAdministratorClosure(t *testing.T) {
	s, deviceID, now := openReportCommandTestStore(t)
	ctx := context.Background()
	first := domain.SecurityEvent{ID: "evt_investigation_close_one", DeviceID: deviceID, Type: "file_integrity_changed", OccurredAt: now, Payload: json.RawMessage(`{"path":"/etc/witshield/test"}`)}
	if _, err := s.ProcessSecurityEvent(ctx, first, false, now); err != nil {
		t.Fatal(err)
	}
	incidents, err := s.ListIncidents(ctx, deviceID, nil, 10)
	if err != nil || len(incidents) != 1 {
		t.Fatalf("incidents=%#v err=%v", incidents, err)
	}
	investigation, err := s.CreateInvestigation(ctx, incidents[0].ID, "test", "test-model", now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetIncidentStatus(ctx, incidents[0].ID, domain.IncidentDismissed, "admin", "false positive", now.Add(time.Minute+10*time.Second)); err != nil {
		t.Fatal(err)
	}
	second := first
	second.ID, second.OccurredAt = "evt_investigation_close_two", now.Add(time.Minute+20*time.Second)
	if _, err = s.ProcessSecurityEvent(ctx, second, false, second.OccurredAt); err != nil {
		t.Fatal(err)
	}
	investigation.Status, investigation.Hypothesis, investigation.Conclusion, investigation.Confidence = domain.InvestigationCompleted, "drift", "needs remediation", 80
	plan := &domain.ResponsePlan{Title: "repair", Rationale: "restore", Risk: "medium", RequiresApproval: true, Steps: []domain.ResponseStep{{ID: "step_one", ActionType: "file_permission_repair", Title: "repair", Rationale: "restore", Parameters: json.RawMessage(`{"path":"/etc/witshield/test","mode":"600"}`), Risk: "medium", RequiresApproval: true}}}
	if err = s.CompleteInvestigation(ctx, investigation, plan, now.Add(time.Minute+30*time.Second)); err != nil {
		t.Fatalf("completed investigation was rejected: %v", err)
	}
	incident, err := s.Incident(ctx, incidents[0].ID)
	if err != nil || incident.Status != domain.IncidentDismissed {
		t.Fatalf("administrator decision was overridden: %#v err=%v", incident, err)
	}
	investigations, err := s.ListInvestigations(ctx, incident.ID, 10)
	if err != nil || len(investigations) != 1 || investigations[0].Status != domain.InvestigationCompleted {
		t.Fatalf("investigation result was not recorded: %#v err=%v", investigations, err)
	}
	plans, err := s.ListResponsePlans(ctx, incident.ID, 10)
	if err != nil || len(plans) != 1 {
		t.Fatalf("response plan was not recorded: %#v err=%v", plans, err)
	}
}

// A revoked device can never authenticate again, so its schedules must not be
// due: otherwise every interval enqueues a scan that supersedes the previous one
// and can never be claimed.
func TestDueSchedulesSkipRevokedDevices(t *testing.T) {
	s, activeID, now := openReportCommandTestStore(t)
	ctx := context.Background()
	const revokedID = "dev_revoked_schedule"
	if _, err := s.db.ExecContext(ctx, `INSERT INTO devices(id,name,hostname,os,arch,agent_version,identity_key,status,enrolled_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, revokedID, "revoked", "host", "linux", "amd64", "test", "", string(domain.DeviceOnline), timeText(now), timeText(now), timeText(now)); err != nil {
		t.Fatal(err)
	}
	for _, deviceID := range []string{activeID, revokedID} {
		schedule := domain.Schedule{ID: "sch_" + deviceID, DeviceID: deviceID, Kind: domain.ScheduleScan, Interval: time.Hour, Enabled: true, NextRunAt: now.Add(-time.Minute), CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour)}
		if err := s.CreateSchedule(ctx, schedule); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.RevokeDevice(ctx, revokedID, now); err != nil {
		t.Fatal(err)
	}
	due, err := s.DueSchedules(ctx, now, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0].DeviceID != activeID {
		t.Fatalf("due schedules=%#v, want only the non-revoked device", due)
	}
}
