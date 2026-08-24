package core

import (
	"context"
	"encoding/json"

	"github.com/ab300819/DJOneHub/internal/service"
)

// The wire contract every transport funnels through. The stdio bridge, the
// c-archive export for a linked macOS build and the gomobile export for Android
// all carry these same frames, so the client code above them differs only in
// language, not in shape.

// Request is one call from a client.
type Request struct {
	ID     int             `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

// Response is the answer to a Request. Kind classifies a failure so a client
// can branch without matching on message text — the HTTP handlers map the same
// classification onto status codes.
type Response struct {
	ID     int    `json:"id"`
	OK     bool   `json:"ok"`
	Result any    `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
	Kind   string `json:"kind,omitempty"`
}

// Event is an unsolicited message: progress and the like. It carries no ID
// because it answers no request.
type Event struct {
	Event   string `json:"event"`
	Percent int    `json:"percent,omitempty"`
	Message string `json:"message,omitempty"`
}

// EventSink receives Events. Transports that can push install one; those that
// cannot leave it unset, and events are dropped — which is what happened to
// eSIM download progress before this existed.
type EventSink interface {
	Emit(Event)
}

// Call is the single entry point. Every transport hands it a request frame and
// writes back what it returns; nothing above this line knows how the bytes
// travelled.
//
// It never fails: a request that cannot even be parsed comes back as an error
// response, because a transport has no better way to report one.
func Call(svc service.Service, requestJSON []byte) []byte {
	var request Request
	if err := json.Unmarshal(requestJSON, &request); err != nil {
		return encode(failure(0, service.Fail(service.KindInvalid, "malformed request: %v", err)))
	}
	return encode(dispatch(svc, request))
}

func encode(response Response) []byte {
	encoded, err := json.Marshal(response)
	if err != nil {
		// Marshalling a Response can only fail on a result the service built,
		// so report that rather than returning nothing at all.
		fallback, _ := json.Marshal(Response{
			ID:    response.ID,
			Error: "response could not be encoded: " + err.Error(),
			Kind:  string(service.KindInternal),
		})
		return fallback
	}
	return encoded
}

// SetEventSink installs the destination for unsolicited events. A transport
// that cannot push simply never calls this, and emit becomes a no-op.
func (a *App) SetEventSink(sink EventSink) {
	a.eventMu.Lock()
	defer a.eventMu.Unlock()
	a.eventSink = sink
}

func (a *App) emit(event Event) {
	a.eventMu.RLock()
	sink := a.eventSink
	a.eventMu.RUnlock()
	if sink == nil {
		return
	}
	sink.Emit(event)
}

func failure(id int, err error) Response {
	return Response{ID: id, OK: false, Error: err.Error(), Kind: string(service.KindOf(err))}
}

// statusResult carries an explicit discriminator. The HTTP endpoint has to
// return one of two bare shapes for backwards compatibility; this protocol is
// new, so it can tell the client which one it is getting.
type statusResult struct {
	Kind     string `json:"kind"`
	Device   any    `json:"device,omitempty"`
	Degraded any    `json:"degraded,omitempty"`
}

func dispatch(svc service.Service, request Request) Response {
	ok := func(result any) Response {
		return Response{ID: request.ID, OK: true, Result: result}
	}
	// bind decodes this request's params, reporting a failure the caller can
	// return directly when the payload does not fit.
	bind := func(target any) *Response {
		if len(request.Params) == 0 {
			return nil
		}
		if err := json.Unmarshal(request.Params, target); err != nil {
			failure := failure(request.ID, err)
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
			return failure(request.ID, err)
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
			return failure(request.ID, err)
		}
		return ok(map[string]string{"response": response})

	case "sms.list":
		return ok(svc.ListSMS())

	case "sms.status":
		return ok(svc.SMSStatus())

	case "sms.refresh":
		result, err := svc.RefreshSMS()
		if err != nil {
			return failure(request.ID, err)
		}
		return ok(result)

	case "sms.clear":
		result, err := svc.ClearModuleSMS()
		if err != nil {
			return failure(request.ID, err)
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
			return failure(request.ID, err)
		}
		return ok(result)

	case "network.diagnostic":
		result, err := svc.NetworkDiagnostic()
		if err != nil {
			return failure(request.ID, err)
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
			return failure(request.ID, err)
		}
		return ok(result)

	case "network.reboot":
		result, err := svc.RebootModule()
		if err != nil {
			return failure(request.ID, err)
		}
		return ok(result)

	case "esim.overview":
		result, err := svc.ESIMOverview()
		if err != nil {
			return failure(request.ID, err)
		}
		return ok(result)

	case "esim.health":
		result, err := svc.ESIMHealth()
		if err != nil {
			return failure(request.ID, err)
		}
		return ok(result)

	case "esim.notes.list":
		notes, err := svc.ListESIMNotes()
		if err != nil {
			return failure(request.ID, err)
		}
		return ok(notes)

	case "esim.notes.save":
		var params service.ProfileNoteInput
		if failure := bind(&params); failure != nil {
			return *failure
		}
		result, err := svc.SaveESIMNote(params)
		if err != nil {
			return failure(request.ID, err)
		}
		return ok(result)

	case "esim.moduleNotes.list":
		result, err := svc.ListModuleESIMNotes()
		if err != nil {
			return failure(request.ID, err)
		}
		return ok(result)

	case "esim.moduleNotes.save":
		var params service.ModuleProfileNote
		if failure := bind(&params); failure != nil {
			return *failure
		}
		result, err := svc.SaveModuleESIMNote(params)
		if err != nil {
			return failure(request.ID, err)
		}
		return ok(result)

	case "esim.download":
		var params service.ESIMDownloadRequest
		if failure := bind(&params); failure != nil {
			return *failure
		}
		result, err := svc.DownloadESIMProfile(context.Background(), params)
		if err != nil {
			return failure(request.ID, err)
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
			return failure(request.ID, err)
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
			return failure(request.ID, err)
		}
		return ok(result)

	case "esim.rename":
		var params struct {
			ICCID string `json:"iccid"`
			AID   string `json:"aid"`
			Name  string `json:"name"`
		}
		if failure := bind(&params); failure != nil {
			return *failure
		}
		result, err := svc.RenameESIMProfile(params.ICCID, params.AID, params.Name)
		if err != nil {
			return failure(request.ID, err)
		}
		return ok(result)

	case "esim.phonebookProbe":
		return ok(svc.ProbeESIMPhonebook())

	default:
		return failure(request.ID, service.Fail(service.KindInvalid, "unknown method: %s", request.Method))
	}
}

// Event names. Kept as constants so a client can match on them without
// duplicating string literals across two languages.
const (
	// EventESIMDownloadProgress reports how far an eSIM profile download has
	// got. It fires many times per download and carries no completion status;
	// the download's own response reports that.
	EventESIMDownloadProgress = "esim.download.progress"
)
