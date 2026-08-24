package main

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ab300819/DJOneHub/core"
	"github.com/ab300819/DJOneHub/internal/service"
)

func serve(instance *core.App, listen string) {
	if stdioMode {
		serveStdio(instance)
		return
	}

	health := instance.Health()
	httpServer := &http.Server{
		Addr:              listen,
		Handler:           (&server{svc: instance}).routes(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if !health.Demo {
		log.Printf("DJOneHub is using %s", health.Port)
	}
	log.Printf("Open http://%s", listen)
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- httpServer.ListenAndServe()
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
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			log.Printf("HTTP server shutdown: %v", err)
		}
	case <-ctx.Done():
		log.Printf("DJOneHub is stopping")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			log.Printf("HTTP server shutdown: %v", err)
		}
	}
}

// server adapts HTTP to the service interface. It holds no state of its own:
// every handler below turns a request into one service call and its answer into
// JSON, which is why the core no longer has to be an HTTP server to be useful.
type server struct {
	svc service.Service
}

func (a *server) routes() http.Handler {
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

func (a *server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, a.svc.Health())
}

func (a *server) status(w http.ResponseWriter, _ *http.Request) {
	status, err := a.svc.Status()
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

func (a *server) listSMS(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, a.svc.ListSMS())
}

func (a *server) smsStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, a.svc.SMSStatus())
}

func (a *server) refreshSMS(w http.ResponseWriter, _ *http.Request) {
	result, err := a.svc.RefreshSMS()
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, result)
}

func (a *server) clearModuleSMS(w http.ResponseWriter, _ *http.Request) {
	result, err := a.svc.ClearModuleSMS()
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (a *server) sendSMS(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Phone   string `json:"phone"`
		Message string `json:"message"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	result, err := a.svc.SendSMS(body.Phone, body.Message)
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

func (a *server) executeAT(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Command string `json:"command"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	response, err := a.svc.ExecuteAT(body.Command)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"response": response})
}

func (a *server) networkDiagnostic(w http.ResponseWriter, _ *http.Request) {
	diag, err := a.svc.NetworkDiagnostic()
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, diag)
}

func (a *server) networkTraffic(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, a.svc.NetworkTraffic())
}

func (a *server) localNetworkConnection(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, a.svc.LocalNetworkConnection())
}

func (a *server) networkActivity(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, a.svc.NetworkActivity())
}

func (a *server) check4GRoute(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, a.svc.Check4GRoute())
}

func (a *server) checkProxyRoute(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, a.svc.CheckProxyRoute())
}

func (a *server) setUSBNetMode(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Mode int `json:"mode"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	result, err := a.svc.SetUSBNetMode(body.Mode)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (a *server) rebootModule(w http.ResponseWriter, _ *http.Request) {
	result, err := a.svc.RebootModule()
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, result)
}

func (a *server) listESIMNotes(w http.ResponseWriter, _ *http.Request) {
	notes, err := a.svc.ListESIMNotes()
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"notes": notes})
}

func (a *server) saveESIMNote(w http.ResponseWriter, r *http.Request) {
	var body service.ProfileNoteInput
	if !decodeJSON(w, r, &body) {
		return
	}
	result, err := a.svc.SaveESIMNote(body)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// probeESIMPhonebook performs only AT test/read commands. It never writes a
// phonebook entry, so it is safe to use before enabling portable card notes.
func (a *server) probeESIMPhonebook(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, a.svc.ProbeESIMPhonebook())
}

func (a *server) listModuleESIMNotes(w http.ResponseWriter, _ *http.Request) {
	result, err := a.svc.ListModuleESIMNotes()
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (a *server) saveModuleESIMNote(w http.ResponseWriter, r *http.Request) {
	var body service.ModuleProfileNote
	if !decodeJSON(w, r, &body) {
		return
	}
	result, err := a.svc.SaveModuleESIMNote(body)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (a *server) esimOverview(w http.ResponseWriter, _ *http.Request) {
	result, err := a.svc.ESIMOverview()
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

func (a *server) esimHealth(w http.ResponseWriter, _ *http.Request) {
	result, err := a.svc.ESIMHealth()
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

func (a *server) switchESIM(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ICCID string `json:"iccid"`
		AID   string `json:"aid"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	result, err := a.svc.SwitchESIMProfile(r.Context(), body.ICCID, body.AID)
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

func (a *server) renameESIMProfile(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ICCID string `json:"iccid"`
		AID   string `json:"aid"`
		Name  string `json:"name"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	result, err := a.svc.RenameESIMProfile(body.ICCID, body.AID, body.Name)
	if err != nil {
		writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": result.Message})
}

func (a *server) deleteESIMProfile(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ICCID string `json:"iccid"`
		AID   string `json:"aid"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	result, err := a.svc.DeleteESIMProfile(body.ICCID, body.AID)
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

func (a *server) downloadESIMProfile(w http.ResponseWriter, r *http.Request) {
	var body service.ESIMDownloadRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	result, err := a.svc.DownloadESIMProfile(r.Context(), body)
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
