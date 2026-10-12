// Euler derivative of WitShield (Apache-2.0); imports and integration may be modified. See module NOTICE.

// Euler integration changes to the Apache-2.0 WitShield engine; see ../../NOTICE.
package httpapi

import (
	"context"
	"net/http"
	"time"

	"github.com/qifalab/euler-platform/services/witshield-engine/internal/domain"
)

// ManagementRoute is the same versioned business surface used by the standalone
// engine. Authentication, CSRF, project selection and permission checks belong to
// Euler's appkit boundary; no HTTP identity header is accepted here.
type ManagementRoute struct{ Pattern, Permission string }

func ManagementRoutes() []ManagementRoute {
	return []ManagementRoute{
		{"GET /api/v1/system/health", "read"},
		{"GET /api/v1/enrollment-tokens", "read"},
		{"POST /api/v1/enrollment-tokens", "manage"},
		{"DELETE /api/v1/enrollment-tokens/{id}", "manage"},
		{"GET /api/v1/devices", "read"},
		{"GET /api/v1/devices/{id}", "read"},
		{"DELETE /api/v1/devices/{id}", "manage"},
		{"POST /api/v1/devices/{id}/scan", "write"},
		{"GET /api/v1/reports", "read"},
		{"GET /api/v1/reports/{id}", "read"},
		{"GET /api/v1/findings", "read"},
		{"GET /api/v1/schedules", "read"},
		{"POST /api/v1/schedules", "write"},
		{"PATCH /api/v1/schedules/{id}", "write"},
		{"DELETE /api/v1/schedules/{id}", "write"},
		{"GET /api/v1/ai/settings", "read"},
		{"PUT /api/v1/ai/settings", "manage"},
		{"POST /api/v1/ai/test", "manage"},
		{"POST /api/v1/ai/chat", "write"},
		{"GET /api/v1/ai/investigation-policy", "read"},
		{"PUT /api/v1/ai/investigation-policy", "manage"},
		{"GET /api/v1/sensors", "read"},
		{"GET /api/v1/notifications/settings", "read"},
		{"PUT /api/v1/notifications/settings", "manage"},
		{"POST /api/v1/notifications/test", "manage"},
		{"GET /api/v1/actions", "read"},
		{"POST /api/v1/actions", "manage"},
		{"GET /api/v1/actions/{id}", "read"},
		{"POST /api/v1/actions/{id}/approve", "manage"},
		{"POST /api/v1/actions/{id}/rollback", "manage"},
		{"POST /api/v1/actions/{id}/confirm", "manage"},
		{"GET /api/v1/audit", "read"},
		{"GET /api/v1/security-events", "read"},
		{"GET /api/v1/incidents", "read"},
		{"GET /api/v1/incidents/{id}", "read"},
		{"PATCH /api/v1/incidents/{id}", "write"},
		{"POST /api/v1/incidents/{id}/investigate", "write"},
		{"POST /api/v1/response-plans/{id}/steps/{stepId}/prepare", "manage"},
		{"GET /api/v1/devices/{id}/policy-grants", "read"},
		{"PUT /api/v1/devices/{id}/policy-grants/{capability}", "manage"},
		{"GET /api/v1/devices/{id}/defense-policy", "read"},
		{"PUT /api/v1/devices/{id}/defense-policy", "manage"},
		{"POST /api/v1/devices/{id}/defense-policy/simulate", "manage"},
		{"POST /api/v1/devices/{id}/emergency-stop", "manage"},
	}
}

func (s *Server) EulerManagementHandler(actorID string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/system/health", s.systemHealth)
	mux.HandleFunc("GET /api/v1/enrollment-tokens", s.listEnrollmentTokens)
	mux.HandleFunc("POST /api/v1/enrollment-tokens", s.createEnrollmentToken)
	mux.HandleFunc("DELETE /api/v1/enrollment-tokens/{id}", s.revokeEnrollmentToken)
	mux.HandleFunc("GET /api/v1/devices", s.listDevices)
	mux.HandleFunc("GET /api/v1/devices/{id}", s.getDevice)
	mux.HandleFunc("DELETE /api/v1/devices/{id}", s.revokeDevice)
	mux.HandleFunc("POST /api/v1/devices/{id}/scan", s.triggerScan)
	mux.HandleFunc("GET /api/v1/reports", s.listReports)
	mux.HandleFunc("GET /api/v1/reports/{id}", s.getReport)
	mux.HandleFunc("GET /api/v1/findings", s.listFindings)
	mux.HandleFunc("GET /api/v1/schedules", s.listSchedules)
	mux.HandleFunc("POST /api/v1/schedules", s.createSchedule)
	mux.HandleFunc("PATCH /api/v1/schedules/{id}", s.updateSchedule)
	mux.HandleFunc("DELETE /api/v1/schedules/{id}", s.deleteSchedule)
	mux.HandleFunc("GET /api/v1/ai/settings", s.getAISettings)
	mux.HandleFunc("PUT /api/v1/ai/settings", s.putAISettings)
	mux.HandleFunc("POST /api/v1/ai/test", s.testAI)
	mux.HandleFunc("POST /api/v1/ai/chat", s.chatAI)
	mux.HandleFunc("GET /api/v1/ai/investigation-policy", s.getAIInvestigationPolicy)
	mux.HandleFunc("PUT /api/v1/ai/investigation-policy", s.putAIInvestigationPolicy)
	mux.HandleFunc("GET /api/v1/sensors", s.listSensorHealth)
	mux.HandleFunc("GET /api/v1/notifications/settings", s.getNotificationSettings)
	mux.HandleFunc("PUT /api/v1/notifications/settings", s.putNotificationSettings)
	mux.HandleFunc("POST /api/v1/notifications/test", s.testNotification)
	mux.HandleFunc("GET /api/v1/actions", s.listActions)
	mux.HandleFunc("POST /api/v1/actions", s.createAction)
	mux.HandleFunc("GET /api/v1/actions/{id}", s.getAction)
	mux.HandleFunc("POST /api/v1/actions/{id}/approve", s.approveAction)
	mux.HandleFunc("POST /api/v1/actions/{id}/rollback", s.rollbackAction)
	mux.HandleFunc("POST /api/v1/actions/{id}/confirm", s.confirmAction)
	mux.HandleFunc("GET /api/v1/audit", s.audit)
	mux.HandleFunc("GET /api/v1/security-events", s.listSecurityEvents)
	mux.HandleFunc("GET /api/v1/incidents", s.listIncidents)
	mux.HandleFunc("GET /api/v1/incidents/{id}", s.getIncident)
	mux.HandleFunc("PATCH /api/v1/incidents/{id}", s.updateIncident)
	mux.HandleFunc("POST /api/v1/incidents/{id}/investigate", s.investigateIncident)
	mux.HandleFunc("POST /api/v1/response-plans/{id}/steps/{stepId}/prepare", s.prepareResponseStep)
	mux.HandleFunc("GET /api/v1/devices/{id}/policy-grants", s.listPolicyGrants)
	mux.HandleFunc("PUT /api/v1/devices/{id}/policy-grants/{capability}", s.putPolicyGrant)
	mux.HandleFunc("GET /api/v1/devices/{id}/defense-policy", s.getDefensePolicy)
	mux.HandleFunc("PUT /api/v1/devices/{id}/defense-policy", s.putDefensePolicy)
	mux.HandleFunc("POST /api/v1/devices/{id}/defense-policy/simulate", s.simulateDefense)
	mux.HandleFunc("POST /api/v1/devices/{id}/emergency-stop", s.emergencyStop)
	return securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if actorID == "" {
			writeError(w, 401, "unauthenticated", "Euler actor required")
			return
		}
		if !s.operationEnabled(r.Context()) {
			writeError(w, 503, "application_disabled", "application is disabled")
			return
		}
		// This context is created only by the in-process integration entrypoint.
		// Existing business handlers retain their original audit/approval actor.
		operation, cancel := s.operationContext(r.Context())
		defer cancel()
		ctx := context.WithValue(operation, adminKey, domain.Admin{ID: actorID, Username: actorID})
		mux.ServeHTTP(w, r.WithContext(ctx))
	}), nil)
}

type originalRequestURIKey struct{}

// EulerAgentHandler keeps the external URI covered by the device signature,
// while dispatching the already-validated relative route inside the engine.
func (s *Server) EulerAgentHandler(originalURI string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /agent/v1/enroll/challenge", s.agentEnrollChallenge)
	mux.HandleFunc("POST /agent/v1/enroll", s.agentEnroll)
	mux.Handle("POST /agent/v1/heartbeat", s.requireAgent(http.HandlerFunc(s.agentHeartbeat)))
	mux.Handle("GET /agent/v1/sync", s.requireAgent(http.HandlerFunc(s.agentSync)))
	mux.Handle("POST /agent/v1/commands/{id}/start", s.requireAgent(http.HandlerFunc(s.agentCommandStart)))
	mux.Handle("POST /agent/v1/commands/{id}/result", s.requireAgent(http.HandlerFunc(s.agentCommandResult)))
	mux.Handle("POST /agent/v1/reports", s.requireAgent(http.HandlerFunc(s.agentReport)))
	mux.Handle("POST /agent/v1/events", s.requireAgent(http.HandlerFunc(s.agentEvents)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if !s.operationEnabled(r.Context()) {
			writeError(w, 503, "application_disabled", "application is disabled")
			return
		}
		operation, cancel := s.operationContext(r.Context())
		defer cancel()
		ctx := context.WithValue(operation, originalRequestURIKey{}, originalURI)
		mux.ServeHTTP(w, r.WithContext(ctx))
	})
}
func signedRequestURI(r *http.Request) string {
	if original, ok := r.Context().Value(originalRequestURIKey{}).(string); ok && original != "" {
		return original
	}
	return r.URL.RequestURI()
}
func (s *Server) operationEnabled(ctx context.Context) bool {
	return ctx.Err() == nil && (s.enabled == nil || s.enabled(ctx))
}

// Stop outbound background work when an installation is disabled during an AI
// investigation or notification attempt. Device command gates additionally
// recheck immediately before claiming/authorizing commands.
func (s *Server) operationContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	if !s.operationEnabled(ctx) {
		cancel()
		return ctx, cancel
	}
	if s.enabled != nil {
		go func() {
			ticker := time.NewTicker(250 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if !s.operationEnabled(ctx) {
						cancel()
						return
					}
				}
			}
		}()
	}
	return ctx, cancel
}
