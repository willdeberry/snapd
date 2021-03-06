// -*- Mode: Go; indent-tabs-mode: t -*-

/*
 * Copyright (C) 2015-2016 Canonical Ltd
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License version 3 as
 * published by the Free Software Foundation.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

package daemon

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	unix "syscall"
	"time"

	"github.com/coreos/go-systemd/activation"
	"github.com/gorilla/mux"
	"gopkg.in/tomb.v2"

	"github.com/snapcore/snapd/dirs"
	"github.com/snapcore/snapd/httputil"
	"github.com/snapcore/snapd/i18n/dumb"
	"github.com/snapcore/snapd/logger"
	"github.com/snapcore/snapd/osutil"
	"github.com/snapcore/snapd/overlord"
	"github.com/snapcore/snapd/overlord/auth"
	"github.com/snapcore/snapd/overlord/state"
	"github.com/snapcore/snapd/polkit"
)

// A Daemon listens for requests and routes them to the right command
type Daemon struct {
	Version       string
	overlord      *overlord.Overlord
	snapdListener net.Listener
	snapdServe    *shutdownServer
	snapListener  net.Listener
	snapServe     *shutdownServer
	tomb          tomb.Tomb
	router        *mux.Router
	// enableInternalInterfaceActions controls if adding and removing slots and plugs is allowed.
	enableInternalInterfaceActions bool
}

// A ResponseFunc handles one of the individual verbs for a method
type ResponseFunc func(*Command, *http.Request, *auth.UserState) Response

// A Command routes a request to an individual per-verb ResponseFUnc
type Command struct {
	Path string
	//
	GET    ResponseFunc
	PUT    ResponseFunc
	POST   ResponseFunc
	DELETE ResponseFunc
	// can guest GET?
	GuestOK bool
	// can non-admin GET?
	UserOK bool
	// is this path accessible on the snapd-snap socket?
	SnapOK bool

	// can polkit grant access? set to polkit action ID if so
	PolkitOK string

	d *Daemon
}

var polkitCheckAuthorizationForPid = polkit.CheckAuthorizationForPid

func (c *Command) canAccess(r *http.Request, user *auth.UserState) bool {
	if user != nil {
		// Authenticated users do anything for now.
		return true
	}

	isUser := false
	pid, uid, err := ucrednetGet(r.RemoteAddr)
	if err == nil {
		if uid == 0 {
			// Superuser does anything.
			return true
		}

		if c.PolkitOK != "" {
			if authorized, err := polkitCheckAuthorizationForPid(pid, c.PolkitOK, nil, polkit.CheckAllowInteraction); err == nil {
				if authorized {
					// polkit says user is authorised
					return true
				}
			} else if err != polkit.ErrDismissed {
				logger.Noticef("polkit error: %s", err)
			}
		}

		isUser = true
	} else if err != errNoID {
		logger.Noticef("unexpected error when attempting to get UID: %s", err)
		return false
	} else if c.SnapOK {
		return true
	}

	if r.Method != "GET" {
		return false
	}

	if isUser && c.UserOK {
		return true
	}

	if c.GuestOK {
		return true
	}

	return false
}

func (c *Command) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	state := c.d.overlord.State()
	state.Lock()
	// TODO Look at the error and fail if there's an attempt to authenticate with invalid data.
	user, _ := UserFromRequest(state, r)
	state.Unlock()

	if !c.canAccess(r, user) {
		Unauthorized("access denied").ServeHTTP(w, r)
		return
	}

	var rspf ResponseFunc
	var rsp = MethodNotAllowed("method %q not allowed", r.Method)

	switch r.Method {
	case "GET":
		rspf = c.GET
	case "PUT":
		rspf = c.PUT
	case "POST":
		rspf = c.POST
	case "DELETE":
		rspf = c.DELETE
	}

	if rspf != nil {
		rsp = rspf(c, r, user)
	}

	rsp.ServeHTTP(w, r)
}

type wrappedWriter struct {
	w http.ResponseWriter
	s int
}

func (w *wrappedWriter) Header() http.Header {
	return w.w.Header()
}

func (w *wrappedWriter) Write(bs []byte) (int, error) {
	return w.w.Write(bs)
}

func (w *wrappedWriter) WriteHeader(s int) {
	w.w.WriteHeader(s)
	w.s = s
}

func (w *wrappedWriter) Flush() {
	if f, ok := w.w.(http.Flusher); ok {
		f.Flush()
	}
}

func logit(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ww := &wrappedWriter{w: w}
		t0 := time.Now()
		handler.ServeHTTP(ww, r)
		t := time.Now().Sub(t0)
		url := r.URL.String()
		if !strings.Contains(url, "/changes/") {
			logger.Debugf("%s %s %s %s %d", r.RemoteAddr, r.Method, r.URL, t, ww.s)
		}
	})
}

// getListener tries to get a listener for the given socket path from
// the listener map, and if it fails it tries to set it up directly.
func getListener(socketPath string, listenerMap map[string]net.Listener) (net.Listener, error) {
	if listener, ok := listenerMap[socketPath]; ok {
		return listener, nil
	}

	if c, err := net.Dial("unix", socketPath); err == nil {
		c.Close()
		return nil, fmt.Errorf("socket %q already in use", socketPath)
	}

	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
		return nil, err
	}

	address, err := net.ResolveUnixAddr("unix", socketPath)
	if err != nil {
		return nil, err
	}

	runtime.LockOSThread()
	oldmask := unix.Umask(0111)
	listener, err := net.ListenUnix("unix", address)
	unix.Umask(oldmask)
	runtime.UnlockOSThread()
	if err != nil {
		return nil, err
	}

	logger.Debugf("socket %q was not activated; listening", socketPath)

	return listener, nil
}

// Init sets up the Daemon's internal workings.
// Don't call more than once.
func (d *Daemon) Init() error {
	listeners, err := activation.Listeners(false)
	if err != nil {
		return err
	}

	listenerMap := make(map[string]net.Listener, len(listeners))

	for _, listener := range listeners {
		listenerMap[listener.Addr().String()] = listener
	}

	// The SnapdSocket is required-- without it, die.
	if listener, err := getListener(dirs.SnapdSocket, listenerMap); err == nil {
		d.snapdListener = &ucrednetListener{listener}
	} else {
		return fmt.Errorf("when trying to listen on %s: %v", dirs.SnapdSocket, err)
	}

	if listener, err := getListener(dirs.SnapSocket, listenerMap); err == nil {
		// Note that the SnapSocket listener does not use ucrednet. We use the lack
		// of remote information as an indication that the request originated with
		// this socket. This listener may also be nil if that socket wasn't among
		// the listeners, so check it before using it.
		d.snapListener = listener
	} else {
		logger.Debugf("cannot get listener for %q: %v", dirs.SnapSocket, err)
	}

	d.addRoutes()

	logger.Noticef("started %v.", httputil.UserAgent())

	return nil
}

func (d *Daemon) addRoutes() {
	d.router = mux.NewRouter()

	for _, c := range api {
		c.d = d
		d.router.Handle(c.Path, c).Name(c.Path)
	}

	// also maybe add a /favicon.ico handler...

	d.router.NotFoundHandler = NotFound("not found")
}

var (
	shutdownTimeout = 5 * time.Second
)

// shutdownServer supplements a http.Server with graceful shutdown.
// TODO: with go1.8 http.Server itself grows a graceful Shutdown method
type shutdownServer struct {
	l       net.Listener
	httpSrv *http.Server

	mu           sync.Mutex
	conns        map[net.Conn]http.ConnState
	shuttingDown bool
}

func newShutdownServer(l net.Listener, h http.Handler) *shutdownServer {
	srv := &http.Server{
		Handler: h,
	}
	ssrv := &shutdownServer{
		l:       l,
		httpSrv: srv,
		conns:   make(map[net.Conn]http.ConnState),
	}
	srv.ConnState = ssrv.trackConn
	return ssrv
}

func (srv *shutdownServer) Serve() error {
	return srv.httpSrv.Serve(srv.l)
}

func (srv *shutdownServer) trackConn(conn net.Conn, state http.ConnState) {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	// we ignore hijacked connections, if we do things with websockets
	// we'll need custom shutdown handling for them
	if state == http.StateClosed || state == http.StateHijacked {
		delete(srv.conns, conn)
		return
	}
	if srv.shuttingDown && state == http.StateIdle {
		conn.Close()
		delete(srv.conns, conn)
		return
	}
	srv.conns[conn] = state
}

func (srv *shutdownServer) finishShutdown() error {
	toutC := time.After(shutdownTimeout)

	srv.mu.Lock()
	defer srv.mu.Unlock()

	srv.shuttingDown = true
	for conn, state := range srv.conns {
		if state == http.StateIdle {
			conn.Close()
			delete(srv.conns, conn)
		}
	}

	doWait := true
	for doWait {
		if len(srv.conns) == 0 {
			return nil
		}
		srv.mu.Unlock()
		select {
		case <-time.After(200 * time.Millisecond):
		case <-toutC:
			doWait = false
		}
		srv.mu.Lock()
	}
	return fmt.Errorf("cannot gracefully finish, still active connections on %v after %v", srv.l.Addr(), shutdownTimeout)
}

var shutdownMsg = i18n.G("reboot scheduled to update the system - temporarily cancel with 'sudo shutdown -c'")

// Start the Daemon
func (d *Daemon) Start() {
	// die when asked to restart (systemd should get us back up!)
	d.overlord.SetRestartHandler(func(t state.RestartType) {
		switch t {
		case state.RestartDaemon:
			d.tomb.Kill(nil)
		case state.RestartSystem:
			cmd := exec.Command("shutdown", "+10", "-r", shutdownMsg)
			if out, err := cmd.CombinedOutput(); err != nil {
				logger.Noticef("%s", osutil.OutputErr(out, err))
			}
		default:
			logger.Noticef("internal error: restart handler called with unknown restart type: %v", t)
			d.tomb.Kill(nil)
		}
	})

	if d.snapListener != nil {
		d.snapServe = newShutdownServer(d.snapListener, logit(d.router))
	}
	d.snapdServe = newShutdownServer(d.snapdListener, logit(d.router))

	// the loop runs in its own goroutine
	d.overlord.Loop()

	d.tomb.Go(func() error {
		if d.snapListener != nil {
			d.tomb.Go(func() error {
				if err := d.snapServe.Serve(); err != nil && d.tomb.Err() == tomb.ErrStillAlive {
					return err
				}

				return nil
			})
		}

		if err := d.snapdServe.Serve(); err != nil && d.tomb.Err() == tomb.ErrStillAlive {
			return err
		}

		return nil
	})
}

// Stop shuts down the Daemon
func (d *Daemon) Stop() error {
	d.tomb.Kill(nil)
	d.snapdListener.Close()
	if d.snapListener != nil {
		d.snapListener.Close()
	}

	d.tomb.Kill(d.snapdServe.finishShutdown())
	if d.snapListener != nil {
		d.tomb.Kill(d.snapServe.finishShutdown())
	}

	d.overlord.Stop()

	return d.tomb.Wait()
}

// Dying is a tomb-ish thing
func (d *Daemon) Dying() <-chan struct{} {
	return d.tomb.Dying()
}

// New Daemon
func New() (*Daemon, error) {
	ovld, err := overlord.New()
	if err != nil {
		return nil, err
	}
	return &Daemon{
		overlord: ovld,
		// TODO: Decide when this should be disabled by default.
		enableInternalInterfaceActions: true,
	}, nil
}
