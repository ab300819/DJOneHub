package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ab300819/DJOneHub/internal/service"
)

func serve(instance *app, listen string) {
	if stdioMode {
		serveStdio(instance)
		return
	}

	server := &http.Server{
		Addr:              listen,
		Handler:           instance.routes(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if !instance.demo {
		log.Printf("DJOneHub is using %s", instance.port)
	}
	log.Printf("Open http://%s", listen)
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- server.ListenAndServe()
	}()

	var parentGone <-chan struct{}
	if parentPID > 0 {
		parentGone = watchParent(parentPID)
	}

	select {
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Printf("HTTP server stopped unexpectedly: %v", err)
		}
	case <-parentGone:
		log.Printf("DJOneHub parent process exited, stopping")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("HTTP server shutdown: %v", err)
		}
	case <-ctx.Done():
		log.Printf("DJOneHub is stopping")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("HTTP server shutdown: %v", err)
		}
	}
}

func (a *app) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", a.health)
	mux.HandleFunc("GET /api/status", a.status)
	mux.HandleFunc("GET /api/sms", a.listSMS)
	mux.HandleFunc("GET /api/sms/status", a.smsStatus)
	mux.HandleFunc("POST /api/sms/send", a.sendSMS)
	mux.HandleFunc("POST /api/sms/refresh", a.refreshSMS)
	mux.HandleFunc("POST /api/sms/clear-module", a.clearModuleSMS)
	mux.HandleFunc("POST /api/at", a.executeAT)
	mux.HandleFunc("GET /api/network", a.networkDiagnostic)
	mux.HandleFunc("GET /api/network/traffic", a.networkTraffic)
	mux.HandleFunc("GET /api/network/local", a.localNetworkConnection)
	mux.HandleFunc("GET /api/network/activity", a.networkActivity)
	mux.HandleFunc("POST /api/network/check-4g", a.check4GRoute)
	mux.HandleFunc("POST /api/network/check-proxy", a.checkProxyRoute)
	mux.HandleFunc("POST /api/network/usbnet", a.setUSBNetMode)
	mux.HandleFunc("POST /api/network/reboot-module", a.rebootModule)
	mux.HandleFunc("GET /api/esim", a.esimOverview)
	mux.HandleFunc("GET /api/esim/notes", a.listESIMNotes)
	mux.HandleFunc("PUT /api/esim/notes", a.saveESIMNote)
	mux.HandleFunc("GET /api/esim/module-notes", a.listModuleESIMNotes)
	mux.HandleFunc("PUT /api/esim/module-notes", a.saveModuleESIMNote)
	mux.HandleFunc("GET /api/esim/health", a.esimHealth)
	mux.HandleFunc("POST /api/esim/phonebook/probe", a.probeESIMPhonebook)
	mux.HandleFunc("POST /api/esim/switch", a.switchESIM)
	mux.HandleFunc("PATCH /api/esim/profile", a.renameESIMProfile)
	mux.HandleFunc("DELETE /api/esim/profile", a.deleteESIMProfile)
	mux.HandleFunc("POST /api/esim/download", a.downloadESIMProfile)
	content, _ := fs.Sub(webAssets, "web")
	mux.Handle("/", http.FileServer(http.FS(content)))
	return securityHeaders(mux)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

func (a *app) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, a.Health())
}

func (a *app) status(w http.ResponseWriter, _ *http.Request) {
	status, err := a.Status()
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	if status.Device != nil {
		writeJSON(w, http.StatusOK, status.Device)
		return
	}
	writeJSON(w, http.StatusOK, status.Degraded)
}

func (a *app) listSMS(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, a.ListSMS())
}

func (a *app) smsStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, a.SMSStatus())
}

func (a *app) refreshSMS(w http.ResponseWriter, _ *http.Request) {
	result, err := a.RefreshSMS()
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, result)
}

func (a *app) clearModuleSMS(w http.ResponseWriter, _ *http.Request) {
	result, err := a.ClearModuleSMS()
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (a *app) sendSMS(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Phone   string `json:"phone"`
		Message string `json:"message"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	result, err := a.SendSMS(body.Phone, body.Message)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// writeServiceError maps a service failure onto the status code this API has
// always used for that kind of failure.
func writeServiceError(w http.ResponseWriter, err error) {
	switch service.KindOf(err) {
	case service.KindInvalid:
		writeError(w, http.StatusBadRequest, err.Error())
	case service.KindUnavailable:
		writeError(w, http.StatusServiceUnavailable, err.Error())
	case service.KindConflict:
		writeError(w, http.StatusConflict, err.Error())
	case service.KindInternal:
		writeError(w, http.StatusInternalServerError, err.Error())
	default:
		writeError(w, http.StatusBadGateway, err.Error())
	}
}

func (a *app) executeAT(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Command string `json:"command"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	response, err := a.ExecuteAT(body.Command)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"response": response})
}

func (a *app) networkDiagnostic(w http.ResponseWriter, _ *http.Request) {
	diag, err := a.NetworkDiagnostic()
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, diag)
}

func (a *app) networkTraffic(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, a.NetworkTraffic())
}

func (a *app) localNetworkConnection(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, a.LocalNetworkConnection())
}

func (a *app) networkActivity(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, a.NetworkActivity())
}

func (a *app) check4GRoute(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, a.Check4GRoute())
}

func (a *app) checkProxyRoute(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, a.CheckProxyRoute())
}

func (a *app) setUSBNetMode(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Mode int `json:"mode"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	result, err := a.SetUSBNetMode(body.Mode)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (a *app) rebootModule(w http.ResponseWriter, _ *http.Request) {
	result, err := a.RebootModule()
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, result)
}

func (a *app) listESIMNotes(w http.ResponseWriter, _ *http.Request) {
	notes, err := a.ListESIMNotes()
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"notes": notes})
}

func (a *app) saveESIMNote(w http.ResponseWriter, r *http.Request) {
	var body service.ProfileNoteInput
	if !decodeJSON(w, r, &body) {
		return
	}
	result, err := a.SaveESIMNote(body)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// probeESIMPhonebook performs only AT test/read commands. It never writes a
// phonebook entry, so it is safe to use before enabling portable card notes.
func (a *app) probeESIMPhonebook(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, a.ProbeESIMPhonebook())
}

func (a *app) listModuleESIMNotes(w http.ResponseWriter, _ *http.Request) {
	result, err := a.ListModuleESIMNotes()
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (a *app) saveModuleESIMNote(w http.ResponseWriter, r *http.Request) {
	var body moduleProfileNote
	if !decodeJSON(w, r, &body) {
		return
	}
	result, err := a.SaveModuleESIMNote(body)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (a *app) esimOverview(w http.ResponseWriter, _ *http.Request) {
	result, err := a.ESIMOverview()
	if err != nil {
		writeServiceError(w, err)
		return
	}
	switch {
	case result.DemoPayload != nil:
		writeJSON(w, http.StatusOK, result.DemoPayload)
	case result.PhysicalSIM:
		writeJSON(w, http.StatusOK, map[string]any{
			"card_type": "physical_sim",
			"message":   result.Message,
		})
	default:
		writeJSON(w, http.StatusOK, result.Overview)
	}
}

func (a *app) esimHealth(w http.ResponseWriter, _ *http.Request) {
	result, err := a.ESIMHealth()
	if err != nil {
		writeServiceError(w, err)
		return
	}
	switch {
	case result.PhysicalSIM:
		writeJSON(w, http.StatusOK, map[string]any{"card_type": "physical_sim"})
	case result.ActiveProfile == nil:
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": result.Message})
	default:
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":             result.OK,
			"active_profile": result.ActiveProfile,
			"module_iccid":   result.ModuleICCID,
			"imsi":           result.IMSI,
			"operator":       result.Operator,
			"registration":   result.Registration,
			"registered":     result.Registered,
			"signal_dbm":     result.SignalDBM,
			"network_mode":   result.NetworkMode,
		})
	}
}

func (a *app) switchESIM(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ICCID string `json:"iccid"`
		AID   string `json:"aid"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	result, err := a.SwitchESIMProfile(r.Context(), body.ICCID, body.AID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	if result.Demo {
		writeJSON(w, http.StatusOK, map[string]any{
			"switch_accepted": result.SwitchAccepted,
			"phase":           result.Phase,
			"target_iccid":    result.TargetICCID,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"switch_accepted":         result.SwitchAccepted,
		"phase":                   result.Phase,
		"target_iccid":            result.TargetICCID,
		"recovery_pending":        result.RecoveryPending,
		"module_reboot_requested": result.ModuleRebootRequested,
		"module_reboot_response":  result.ModuleRebootResponse,
		"module_reboot_warning":   result.ModuleRebootWarning,
		"reconnect_wait_seconds":  result.ReconnectWaitSeconds,
	})
}

func (a *app) renameESIMProfile(w http.ResponseWriter, r *http.Request) {
	esimManager, _ := a.currentESIMManager()
	if !a.demo && esimManager == nil {
		writeError(w, http.StatusServiceUnavailable, "eSIM manager is unavailable")
		return
	}
	var body struct {
		ICCID string `json:"iccid"`
		AID   string `json:"aid"`
		Name  string `json:"name"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	body.ICCID = strings.TrimSpace(body.ICCID)
	body.Name = strings.TrimSpace(body.Name)
	if body.ICCID == "" || body.Name == "" {
		writeError(w, http.StatusBadRequest, "iccid and name are required")
		return
	}
	if a.demo {
		writeJSON(w, http.StatusOK, map[string]string{"message": "Profile 名称修改成功"})
		return
	}
	if err := esimManager.RenameProfile(body.ICCID, body.Name, body.AID); err != nil {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("修改 Profile 名称失败: %v", err))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "Profile 名称修改成功"})
}

func (a *app) deleteESIMProfile(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ICCID string `json:"iccid"`
		AID   string `json:"aid"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	result, err := a.DeleteESIMProfile(body.ICCID, body.AID)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	if result.Demo {
		writeJSON(w, http.StatusOK, map[string]string{"message": result.Message})
		return
	}
	writeJSON(w, http.StatusOK, result.Result)
}

func (a *app) downloadESIMProfile(w http.ResponseWriter, r *http.Request) {
	var body service.ESIMDownloadRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	result, err := a.DownloadESIMProfile(r.Context(), body)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	if result.Demo {
		writeJSON(w, http.StatusOK, map[string]string{"message": result.Message})
		return
	}
	writeJSON(w, http.StatusOK, result.Result)
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return false
	}
	return true
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
