package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"sync"

	"github.com/ab300819/DJOneHub/internal/service"
	"golang.org/x/sys/unix"
)

// The stdio bridge speaks line-delimited JSON over the process's own pipes,
// which is how the native app drives the core. Unlike the HTTP server it opens
// no socket, so nothing on the machine but the parent process can reach it.

type stdioRequest struct {
	ID     int             `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

type stdioResponse struct {
	ID     int    `json:"id"`
	OK     bool   `json:"ok"`
	Result any    `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}

// statusResult carries an explicit discriminator. The HTTP endpoint has to
// return one of two bare shapes for backwards compatibility; this protocol is
// new, so it can tell the client which one it is getting.
type statusResult struct {
	Kind     string `json:"kind"`
	Device   any    `json:"device,omitempty"`
	Degraded any    `json:"degraded,omitempty"`
}

func serveStdio(svc service.Service) {
	out, err := claimStdout()
	if err != nil {
		log.Printf("stdio: %v", err)
		return
	}

	decoder := json.NewDecoder(os.Stdin)
	encoder := json.NewEncoder(out)
	var writeMu sync.Mutex

	for {
		var request stdioRequest
		if err := decoder.Decode(&request); err != nil {
			if errors.Is(err, io.EOF) {
				return
			}
			log.Printf("stdio: malformed request: %v", err)
			return
		}
		response := dispatchStdio(svc, request)
		writeMu.Lock()
		encodeErr := encoder.Encode(response)
		writeMu.Unlock()
		if encodeErr != nil {
			log.Printf("stdio: write failed: %v", encodeErr)
			return
		}
	}
}

func dispatchStdio(svc service.Service, request stdioRequest) stdioResponse {
	ok := func(result any) stdioResponse {
		return stdioResponse{ID: request.ID, OK: true, Result: result}
	}
	// bind decodes this request's params, reporting a failure the caller can
	// return directly when the payload does not fit.
	bind := func(target any) *stdioResponse {
		if len(request.Params) == 0 {
			return nil
		}
		if err := json.Unmarshal(request.Params, target); err != nil {
			failure := stdioFailure(request.ID, err)
			return &failure
		}
		return nil
	}

	switch request.Method {
	case "health":
		return ok(svc.Health())

	case "status":
		status, err := svc.Status()
		if err != nil {
			return stdioFailure(request.ID, err)
		}
		if status.Device != nil {
			return ok(statusResult{Kind: "device", Device: status.Device})
		}
		return ok(statusResult{Kind: "degraded", Degraded: status.Degraded})

	case "at":
		var params struct {
			Command string `json:"command"`
		}
		if failure := bind(&params); failure != nil {
			return *failure
		}
		response, err := svc.ExecuteAT(params.Command)
		if err != nil {
			return stdioFailure(request.ID, err)
		}
		return ok(map[string]string{"response": response})

	case "sms.list":
		return ok(svc.ListSMS())

	case "sms.status":
		return ok(svc.SMSStatus())

	case "sms.refresh":
		result, err := svc.RefreshSMS()
		if err != nil {
			return stdioFailure(request.ID, err)
		}
		return ok(result)

	case "sms.clear":
		result, err := svc.ClearModuleSMS()
		if err != nil {
			return stdioFailure(request.ID, err)
		}
		return ok(result)

	case "sms.send":
		var params struct {
			Phone   string `json:"phone"`
			Message string `json:"message"`
		}
		if failure := bind(&params); failure != nil {
			return *failure
		}
		result, err := svc.SendSMS(params.Phone, params.Message)
		if err != nil {
			return stdioFailure(request.ID, err)
		}
		return ok(result)

	case "network.diagnostic":
		result, err := svc.NetworkDiagnostic()
		if err != nil {
			return stdioFailure(request.ID, err)
		}
		return ok(result)

	case "network.traffic":
		return ok(svc.NetworkTraffic())

	case "network.local":
		return ok(svc.LocalNetworkConnection())

	case "network.activity":
		return ok(svc.NetworkActivity())

	case "network.check4g":
		return ok(svc.Check4GRoute())

	case "network.checkProxy":
		return ok(svc.CheckProxyRoute())

	case "network.usbnet":
		var params struct {
			Mode int `json:"mode"`
		}
		if failure := bind(&params); failure != nil {
			return *failure
		}
		result, err := svc.SetUSBNetMode(params.Mode)
		if err != nil {
			return stdioFailure(request.ID, err)
		}
		return ok(result)

	case "network.reboot":
		result, err := svc.RebootModule()
		if err != nil {
			return stdioFailure(request.ID, err)
		}
		return ok(result)

	case "esim.overview":
		result, err := svc.ESIMOverview()
		if err != nil {
			return stdioFailure(request.ID, err)
		}
		return ok(result)

	case "esim.health":
		result, err := svc.ESIMHealth()
		if err != nil {
			return stdioFailure(request.ID, err)
		}
		return ok(result)

	case "esim.notes.list":
		notes, err := svc.ListESIMNotes()
		if err != nil {
			return stdioFailure(request.ID, err)
		}
		return ok(notes)

	case "esim.notes.save":
		var params service.ProfileNoteInput
		if failure := bind(&params); failure != nil {
			return *failure
		}
		result, err := svc.SaveESIMNote(params)
		if err != nil {
			return stdioFailure(request.ID, err)
		}
		return ok(result)

	case "esim.moduleNotes.list":
		result, err := svc.ListModuleESIMNotes()
		if err != nil {
			return stdioFailure(request.ID, err)
		}
		return ok(result)

	case "esim.moduleNotes.save":
		var params service.ModuleProfileNote
		if failure := bind(&params); failure != nil {
			return *failure
		}
		result, err := svc.SaveModuleESIMNote(params)
		if err != nil {
			return stdioFailure(request.ID, err)
		}
		return ok(result)

	case "esim.download":
		var params service.ESIMDownloadRequest
		if failure := bind(&params); failure != nil {
			return *failure
		}
		result, err := svc.DownloadESIMProfile(context.Background(), params)
		if err != nil {
			return stdioFailure(request.ID, err)
		}
		return ok(result)

	case "esim.switch":
		var params struct {
			ICCID string `json:"iccid"`
			AID   string `json:"aid"`
		}
		if failure := bind(&params); failure != nil {
			return *failure
		}
		result, err := svc.SwitchESIMProfile(context.Background(), params.ICCID, params.AID)
		if err != nil {
			return stdioFailure(request.ID, err)
		}
		return ok(result)

	case "esim.delete":
		var params struct {
			ICCID string `json:"iccid"`
			AID   string `json:"aid"`
		}
		if failure := bind(&params); failure != nil {
			return *failure
		}
		result, err := svc.DeleteESIMProfile(params.ICCID, params.AID)
		if err != nil {
			return stdioFailure(request.ID, err)
		}
		return ok(result)

	case "esim.phonebookProbe":
		return ok(svc.ProbeESIMPhonebook())

	default:
		return stdioFailure(request.ID, errors.New("unknown method: "+request.Method))
	}
}

func stdioFailure(id int, err error) stdioResponse {
	return stdioResponse{ID: id, OK: false, Error: err.Error()}
}

// claimStdout hands the protocol a private copy of the real stdout and points
// file descriptor 1 at stderr. Log output reaches os.Stdout through several
// layers that captured it at init time, and a single stray line would corrupt
// the stream; redirecting the descriptor itself covers all of them at once.
func claimStdout() (*os.File, error) {
	saved, err := unix.Dup(int(os.Stdout.Fd()))
	if err != nil {
		return nil, err
	}
	if err := unix.Dup2(int(os.Stderr.Fd()), int(os.Stdout.Fd())); err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(saved), "stdout-protocol"), nil
}
