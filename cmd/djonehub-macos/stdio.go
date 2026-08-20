package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"sync"

	"golang.org/x/sys/unix"
)

// The stdio bridge speaks line-delimited JSON over the process's own pipes,
// which is how the native macOS app drives the core. Unlike the HTTP server it
// opens no socket, so nothing on the machine but the parent process can reach
// it. It is a thin loop over Call: read a frame, hand it over, write the answer.

// stdioSink pushes events onto the same stream the responses travel on. Frames
// are distinguished by shape rather than by channel: a response carries "id", an
// event carries "event".
type stdioSink struct {
	mu  *sync.Mutex
	out *os.File
}

func (s stdioSink) Emit(event Event) {
	encoded, err := json.Marshal(event)
	if err != nil {
		log.Printf("stdio: event could not be encoded: %v", err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.out.Write(append(encoded, '\n')); err != nil {
		log.Printf("stdio: event write failed: %v", err)
	}
}

func serveStdio(instance *app) {
	out, err := claimStdout()
	if err != nil {
		log.Printf("stdio: %v", err)
		return
	}

	var writeMu sync.Mutex
	instance.SetEventSink(stdioSink{mu: &writeMu, out: out})

	// A request is one line, but an eSIM payload can be long, so the reader is
	// given room to grow rather than failing on a token that exceeds the default.
	reader := bufio.NewReaderSize(os.Stdin, 64*1024)
	for {
		line, err := readFrame(reader)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				log.Printf("stdio: read failed: %v", err)
			}
			return
		}
		if len(line) == 0 {
			continue
		}
		response := Call(instance, line)

		writeMu.Lock()
		_, writeErr := out.Write(append(response, '\n'))
		writeMu.Unlock()
		if writeErr != nil {
			log.Printf("stdio: write failed: %v", writeErr)
			return
		}
	}
}

// readFrame returns one line without its terminator, growing past the reader's
// buffer when a request is larger than it.
func readFrame(reader *bufio.Reader) ([]byte, error) {
	var frame []byte
	for {
		chunk, isPrefix, err := reader.ReadLine()
		if err != nil {
			return nil, err
		}
		frame = append(frame, chunk...)
		if !isPrefix {
			return frame, nil
		}
	}
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
