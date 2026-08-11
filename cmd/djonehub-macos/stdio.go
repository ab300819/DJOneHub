package main

import (
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
	switch request.Method {
	case "health":
		return stdioResponse{ID: request.ID, OK: true, Result: svc.Health()}

	case "status":
		status, err := svc.Status()
		if err != nil {
			return stdioFailure(request.ID, err)
		}
		if status.Device != nil {
			return stdioResponse{ID: request.ID, OK: true,
				Result: statusResult{Kind: "device", Device: status.Device}}
		}
		return stdioResponse{ID: request.ID, OK: true,
			Result: statusResult{Kind: "degraded", Degraded: status.Degraded}}

	case "at":
		var params struct {
			Command string `json:"command"`
		}
		if err := json.Unmarshal(request.Params, &params); err != nil {
			return stdioFailure(request.ID, err)
		}
		response, err := svc.ExecuteAT(params.Command)
		if err != nil {
			return stdioFailure(request.ID, err)
		}
		return stdioResponse{ID: request.ID, OK: true,
			Result: map[string]string{"response": response}}

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
