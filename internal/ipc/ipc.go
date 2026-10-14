// Package ipc is newline-delimited JSON over a unix socket.
//
//	Request : {id, cmd, args}
//	Progress: {id, progress: {...}}   zero or more, before the response
//	Response: {id, ok, data} | {id, ok:false, error, hint, code}
//
// The progress line exists because some commands are slow by nature —
// restarting nine sessions takes as long as nine sessions take — and a
// request/response pair can only report that after the fact. A caller that
// ignores progress lines still behaves exactly as before.
package ipc

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/dumanhaydar/rcmanager/internal/rcerr"
	"github.com/dumanhaydar/rcmanager/internal/registry"
)

// Args is everything a daemon command can be asked to act on.
type Args struct {
	Name         string          `json:"name,omitempty"`
	Entry        *registry.Entry `json:"entry,omitempty"`
	Force        bool            `json:"force,omitempty"`
	Reason       string          `json:"reason,omitempty"`
	StopSessions bool            `json:"stopSessions,omitempty"`
}

// Request is one command from the CLI.
type Request struct {
	ID   int    `json:"id"`
	Cmd  string `json:"cmd"`
	Args Args   `json:"args"`
}

// Response is the daemon's answer.
type Response struct {
	ID       int             `json:"id"`
	OK       bool            `json:"ok"`
	Data     json.RawMessage `json:"data,omitempty"`
	Error    string          `json:"error,omitempty"`
	Hint     string          `json:"hint,omitempty"`
	Code     string          `json:"code,omitempty"`
	Progress json.RawMessage `json:"progress,omitempty"`
}

// Progress is what a bulk command reports as it works through sessions.
type Progress struct {
	Phase   string   `json:"phase"` // plan | begin | end
	Names   []string `json:"names,omitempty"`
	Name    string   `json:"name,omitempty"`
	OK      bool     `json:"ok,omitempty"`
	Error   string   `json:"error,omitempty"`
	Pid     int      `json:"pid,omitempty"`
	Adopted bool     `json:"adopted,omitempty"`
}

// Handler answers one request. Progress may be called any number of times
// before it returns.
type Handler func(req Request, progress func(Progress)) (any, error)

// Server accepts CLI connections on a unix socket.
type Server struct {
	listener net.Listener
	handler  Handler

	mu      sync.Mutex
	conns   map[net.Conn]struct{}
	closing bool
	done    chan struct{}
}

// Listen binds the socket and starts serving.
func Listen(socketPath string, handler Handler) (*Server, error) {
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o700); err != nil {
		return nil, err
	}
	// A socket left behind by a crashed daemon would block bind().
	if _, err := os.Stat(socketPath); err == nil {
		_ = os.Remove(socketPath)
	}

	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, err
	}
	_ = os.Chmod(socketPath, 0o600)

	s := &Server{listener: ln, handler: handler, conns: map[net.Conn]struct{}{}, done: make(chan struct{})}
	go s.accept()
	return s, nil
}

func (s *Server) accept() {
	defer close(s.done)
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			s.mu.Lock()
			closing := s.closing
			s.mu.Unlock()
			if closing {
				return
			}
			// A transient accept error must not end the daemon.
			time.Sleep(50 * time.Millisecond)
			continue
		}
		s.mu.Lock()
		s.conns[conn] = struct{}{}
		s.mu.Unlock()
		go s.serve(conn)
	}
}

func (s *Server) serve(conn net.Conn) {
	defer func() {
		s.mu.Lock()
		delete(s.conns, conn)
		s.mu.Unlock()
		_ = conn.Close()
	}()

	encMu := sync.Mutex{}
	write := func(v any) {
		blob, err := json.Marshal(v)
		if err != nil {
			return
		}
		encMu.Lock()
		defer encMu.Unlock()
		_, _ = conn.Write(append(blob, '\n'))
	}

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var req Request
		if err := json.Unmarshal(line, &req); err != nil {
			write(Response{OK: false, Error: "malformed request", Code: "ERROR"})
			continue
		}

		progress := func(p Progress) {
			blob, err := json.Marshal(p)
			if err != nil {
				return
			}
			write(Response{ID: req.ID, Progress: blob})
		}

		data, err := s.handler(req, progress)
		if err != nil {
			write(Response{ID: req.ID, OK: false, Error: err.Error(), Hint: rcerr.HintOf(err), Code: rcerr.CodeOf(err)})
			continue
		}
		blob, merr := json.Marshal(data)
		if merr != nil {
			write(Response{ID: req.ID, OK: false, Error: merr.Error(), Code: "ERROR"})
			continue
		}
		write(Response{ID: req.ID, OK: true, Data: blob})
	}
}

// Close stops accepting and drops open connections.
//
// Closing the listener alone would wait on open connections, so a client that
// never hangs up would hold the daemon open through shutdown.
func (s *Server) Close() {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return
	}
	s.closing = true
	conns := make([]net.Conn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()

	_ = s.listener.Close()
	for _, c := range conns {
		_ = c.Close()
	}
	<-s.done
}

// CallOptions tunes one request.
type CallOptions struct {
	Timeout    time.Duration
	OnProgress func(Progress)
}

// Call sends one command and decodes the response into out.
func Call(socketPath, cmd string, args Args, out any, opts CallOptions) error {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	conn, err := net.DialTimeout("unix", socketPath, 5*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	deadline := time.Now().Add(timeout)
	_ = conn.SetDeadline(deadline)

	req, err := json.Marshal(Request{ID: 1, Cmd: cmd, Args: args})
	if err != nil {
		return err
	}
	if _, err := conn.Write(append(req, '\n')); err != nil {
		return err
	}

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		var res Response
		if err := json.Unmarshal(scanner.Bytes(), &res); err != nil {
			return err
		}
		// Progress lines arrive before the response, so read every complete
		// line rather than only the first.
		if len(res.Progress) > 0 {
			if opts.OnProgress != nil {
				var p Progress
				if json.Unmarshal(res.Progress, &p) == nil {
					// A rendering failure must never break the command itself.
					func() {
						defer func() { _ = recover() }()
						opts.OnProgress(p)
					}()
				}
			}
			continue
		}
		if !res.OK {
			msg := res.Error
			if msg == "" {
				msg = "daemon error"
			}
			return &rcerr.Error{Message: msg, Hint: res.Hint, Code: res.Code}
		}
		if out != nil && len(res.Data) > 0 {
			return json.Unmarshal(res.Data, out)
		}
		return nil
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return fmt.Errorf("rcmanager daemon closed the connection without answering %q", cmd)
}

// Ping reports whether something is listening on the socket right now.
func Ping(socketPath string, timeout time.Duration) bool {
	return Call(socketPath, "ping", Args{}, nil, CallOptions{Timeout: timeout}) == nil
}
