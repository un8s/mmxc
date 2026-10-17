package mmxc

import (
	"database/sql"
	"io"
	"log"
	"net/http"
	"sync"
)

// The open database and everything tied to it (config, compatibility report,
// currency format, audit store, the picker's file list) can be replaced while
// the app runs: Settings switches or creates a database, converts one, or just
// saves a changed setting. Requests run concurrently, so that state is never
// written in place. It lives in one runtimeState that is replaced as a whole,
// and every request runs on its own copy of app built from the state that was
// current when it started (handle). A request therefore never sees half of a
// switch, and the database pool it started with stays open until it finishes:
// a replaced pool or audit store is closed only once no request still uses it.

// runtimeState is the replaceable part of app. Its fields mirror app's.
type runtimeState struct {
	db          *sql.DB
	config      config
	compat      compatibilityReport
	currFmt     currencyFormat
	audits      *auditStore
	auditStatus string
	dbFiles     []databaseFile
}

// closers lists the resources the state owns, for closing once unused.
func (s *runtimeState) closers() []io.Closer {
	var out []io.Closer
	if s.db != nil {
		out = append(out, s.db)
	}
	if s.audits != nil {
		out = append(out, s.audits)
	}
	return out
}

func (s *runtimeState) owns(c io.Closer) bool {
	for _, own := range s.closers() {
		if own == c {
			return true
		}
	}
	return false
}

// runtimeHolder holds the current runtimeState and counts, per resource, the
// requests still using it.
type runtimeHolder struct {
	mu    sync.Mutex
	cur   *runtimeState
	inUse map[io.Closer]int
}

func newRuntimeHolder(s runtimeState) *runtimeHolder {
	return &runtimeHolder{cur: &s, inUse: map[io.Closer]int{}}
}

// acquire returns the current state and marks its resources as in use until
// the matching release.
func (h *runtimeHolder) acquire() *runtimeState {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, c := range h.cur.closers() {
		h.inUse[c]++
	}
	return h.cur
}

// release ends a request's use of s, closing any of its resources that have
// since been replaced and that no other request still uses.
func (h *runtimeHolder) release(s *runtimeState) {
	h.mu.Lock()
	var done []io.Closer
	for _, c := range s.closers() {
		h.inUse[c]--
		if h.inUse[c] > 0 {
			continue
		}
		delete(h.inUse, c)
		if !h.cur.owns(c) {
			done = append(done, c)
		}
	}
	h.mu.Unlock()
	closeAll(done)
}

// update installs a new state derived from the current one by fn, and returns
// it. Resources the new state no longer owns are closed now if unused, or by
// the last release that still holds them.
func (h *runtimeHolder) update(fn func(*runtimeState)) runtimeState {
	h.mu.Lock()
	next := *h.cur
	fn(&next)
	old := h.cur
	h.cur = &next
	var done []io.Closer
	for _, c := range old.closers() {
		if !next.owns(c) && h.inUse[c] == 0 {
			done = append(done, c)
		}
	}
	h.mu.Unlock()
	closeAll(done)
	return next
}

// snapshot returns a copy of the current state, for code that runs outside a
// request (template functions, the translator's availability check).
func (h *runtimeHolder) snapshot() runtimeState {
	h.mu.Lock()
	defer h.mu.Unlock()
	return *h.cur
}

func closeAll(cs []io.Closer) {
	for _, c := range cs {
		if err := c.Close(); err != nil {
			log.Printf("close replaced runtime resource: %v", err)
		}
	}
}

// runtimeFields reads app's replaceable fields as a runtimeState.
func (a *app) runtimeFields() runtimeState {
	return runtimeState{
		db: a.db, config: a.config, compat: a.compat, currFmt: a.currFmt,
		audits: a.audits, auditStatus: a.auditStatus, dbFiles: a.dbFiles,
	}
}

func (a *app) setRuntimeFields(s runtimeState) {
	a.db, a.config, a.compat, a.currFmt = s.db, s.config, s.compat, s.currFmt
	a.audits, a.auditStatus, a.dbFiles = s.audits, s.auditStatus, s.dbFiles
}

// startRuntime makes app's current fields the shared state requests are served
// from. newApp calls it once the app is fully built.
func (a *app) startRuntime() {
	a.rt = newRuntimeHolder(a.runtimeFields())
	a.pushConfig(a.config)
}

// view returns a copy of app carrying state s: the receiver every request runs
// on. Only the replaceable fields differ; the rest is shared and immutable.
func (a *app) view(s *runtimeState) *app {
	v := *a
	v.setRuntimeFields(*s)
	return &v
}

// current is a view of the latest state for code outside a request. It holds
// no reference, so it must not be used for database work.
func (a *app) current() *app {
	if a.rt == nil {
		return a
	}
	s := a.rt.snapshot()
	return a.view(&s)
}

// handle adapts a handler method so each request runs on a view of the state
// current when it starts, and holds that state's resources open until it ends.
func (a *app) handle(h func(*app, http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s := a.rt.acquire()
		defer a.rt.release(s)
		h(a.view(s), w, r)
	}
}

// publish changes the runtime state: the shared state, so requests that start
// afterwards see the change, and this app's own fields, so the rest of the
// current request does too. Without a shared state (an app a test assembled
// by hand) it changes the fields alone and closes what they no longer own.
func (a *app) publish(fn func(*runtimeState)) {
	if a.rt != nil {
		next := a.rt.update(fn)
		a.pushConfig(next.config)
	}
	own := a.runtimeFields()
	fn(&own)
	if a.rt == nil {
		before := a.runtimeFields()
		for _, c := range before.closers() {
			if !own.owns(c) {
				closeAll([]io.Closer{c})
			}
		}
		a.pushConfig(own.config)
	}
	a.setRuntimeFields(own)
}

// pushConfig hands the LLM client and the translator their own copy of cfg.
// They keep the pointer, so it must never be one that is later written to.
func (a *app) pushConfig(cfg config) {
	if a.llm != nil {
		c := cfg
		a.llm.setConfig(&c)
	}
	if a.tr != nil {
		c := cfg
		a.tr.setConfig(&c)
	}
}

// currency is the base currency format for template rendering, which runs
// through functions registered once at startup rather than per request.
func (a *app) currency() currencyFormat {
	if a.rt == nil {
		return a.currFmt
	}
	return a.rt.snapshot().currFmt
}

// closeRuntime closes the current database and audit store, at shutdown.
func (a *app) closeRuntime() {
	if a.rt == nil {
		return
	}
	s := a.rt.snapshot()
	closeAll(s.closers())
}
