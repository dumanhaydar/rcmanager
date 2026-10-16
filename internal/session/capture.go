package session

import (
	"regexp"
	"strings"
	"time"

	"github.com/dumanhaydar/rcmanager/internal/ansi"
	"github.com/dumanhaydar/rcmanager/internal/logstream"
	"github.com/dumanhaydar/rcmanager/internal/provider"
	"github.com/dumanhaydar/rcmanager/internal/sio"
)

// chunk is one slab of raw output on its way to analysis.
type chunk struct {
	text   string
	stderr bool
}

// ingestQueue is deliberately generous: an agent TUI repaints several times a
// second, and a full queue only delays analysis, never a log write.
const ingestQueue = 1024

// matchTail is how much of the previous output each new chunk is matched
// against.
//
// Prompts render across several writes, so some overlap is needed — but the
// patterns are phrases of a few dozen characters, and re-scanning kilobytes of
// repaint on every chunk is how a supervisor watching ten TUIs spends a
// measurable slice of a core. A few hundred characters covers any way a short
// phrase can be split; the profile is what settled the number.
const matchTail = 512

// promptWindow is how long a startup modal is still expected.
//
// After it, the prompts rcmanager answers by itself are no longer plausible —
// they are the ones an agent CLI shows while starting — so they stop being
// looked for. The ones rcmanager refuses to answer on the user's behalf keep
// being watched for as long as the session runs: those are a diagnosis, and a
// session that stalls on one should say so whenever it happens.
const promptWindow = 3 * time.Minute

// startCaptureLocked follows the raw capture files. Offsets are persisted, so
// a daemon that replaces another picks up mid-stream rather than replaying or
// skipping.
//
// The tailer callbacks write straight to the log streams (which have their own
// locks) and hand the text to a single analysis goroutine over a channel. That
// indirection is what lets Stop hold the session lock while draining capture:
// a callback never waits on it, so the two can never deadlock.
func (m *Managed) startCaptureLocked(offsets sio.Offsets) {
	m.stopCaptureLocked()
	m.openLogsLocked()
	m.captureAt = offsets
	ch := m.ensureIngestLocked()

	// Tailer.Start polls once synchronously, with this lock held, so the sinks
	// are built around values captured here: a sink that reached back into the
	// session for anything would deadlock against its own caller the moment
	// the raw file already had output in it.
	m.refreshAnalysisNeededLocked()
	m.tailers = []*sio.Tailer{
		sio.NewTailer(m.io.Files.Out, m.sink(m.out, ch, false)).Start(offsets.Out),
		sio.NewTailer(m.io.Files.Err, m.sink(m.err, ch, true)).Start(offsets.Err),
	}
}

// sink is what a tailer hands its bytes to. It writes the log (which has its
// own lock) and queues the text for analysis, and deliberately touches nothing
// else: it runs on the tailer's goroutine, and on the caller's during the
// first synchronous poll, so taking the session lock here would hang the
// session that was starting.
//
// A full queue drops the chunk for *analysis* only — the log keeps every byte.
// That costs a slightly later URL or prompt match on a session producing output
// faster than it can be read, which is a better failure than a stalled tailer.
func (m *Managed) sink(stream *logstream.Stream, ch chan<- chunk, stderr bool) func([]byte) {
	return func(data []byte) {
		if stream != nil {
			stream.Write(data)
		}
		// Nothing left to match: skip the copy and the queue entirely. This is
		// the steady state of every healthy session, and it is where a
		// supervisor watching ten repainting TUIs spends its day.
		if ch == nil || !m.needAnalysis.Load() {
			return
		}
		select {
		case ch <- chunk{text: string(data), stderr: stderr}:
		default:
		}
	}
}

func (m *Managed) ensureIngestLocked() chan chunk {
	if m.ingestCh != nil {
		return m.ingestCh
	}
	ch := make(chan chunk, ingestQueue)
	m.ingestCh = ch
	go func() {
		for c := range ch {
			m.analyse(c.text)
		}
	}()
	return ch
}

func (m *Managed) stopIngest() {
	m.mu.Lock()
	ch := m.ingestCh
	m.ingestCh = nil
	m.mu.Unlock()
	if ch != nil {
		close(ch)
	}
}

// stopCapture drops the tailers. It must not be called with the session lock
// held, because it waits for in-flight polls to finish.
func (m *Managed) stopCapture() {
	m.mu.Lock()
	tailers := m.tailers
	if len(tailers) > 0 {
		// Remember where we got to before dropping the tailers. Shutdown stops
		// capture and *then* persists, and a forgotten offset means the next
		// daemon re-reads the raw file from the start — replaying old output
		// into the log and, worse, re-answering prompts by typing into a live
		// agent.
		m.captureAt = m.readOffsetsLocked()
	}
	m.tailers = nil
	m.mu.Unlock()

	for _, t := range tailers {
		t.Stop()
	}
}

// stopCaptureLocked is stopCapture for a caller that already holds the lock.
// Stopping a tailer waits for its in-flight poll, which is only safe because
// a sink never reaches back into the session.
func (m *Managed) stopCaptureLocked() {
	tailers := m.tailers
	if len(tailers) > 0 {
		m.captureAt = m.readOffsetsLocked()
	}
	m.tailers = nil
	for _, t := range tailers {
		t.Stop()
	}
}

func (m *Managed) readOffsetsLocked() sio.Offsets {
	var out sio.Offsets
	if len(m.tailers) > 0 {
		out.Out = m.tailers[0].Position()
	}
	if len(m.tailers) > 1 {
		out.Err = m.tailers[1].Position()
	}
	return out
}

func (m *Managed) captureOffsetsLocked() sio.Offsets {
	if len(m.tailers) > 0 {
		return m.readOffsetsLocked()
	}
	return m.captureAt
}

// analyse reads a chunk of session output for the two things rcmanager cares
// about: the remote-control URL, and a startup prompt that would otherwise
// stall the session forever.
func (m *Managed) analyse(text string) {
	m.captureRemoteURL(text)
	m.handleInteractions(text)
}

// refreshAnalysisNeededLocked works out whether anything in this session's
// output still matters.
//
// An unanswered one-shot prompt does, and so does a URL that has not been seen
// yet — including for a provider that does not advertise publishing one, since
// the window closes on its own. A recurring prompt keeps mattering for as long
// as the session runs.
func (m *Managed) refreshAnalysisNeededLocked() {
	need := !m.urlLocked
	if !need {
		for _, action := range m.prov.Interactions() {
			if !action.Once || m.pending[action.ID] {
				need = true
				break
			}
		}
	}
	m.needAnalysis.Store(need)
}

// armURLWatchdogLocked says so when a session that should have published a URL
// has not.
//
// Deliberately a message rather than an error status: the process is running
// and may yet recover, and rcmanager marking it failed would trigger a restart
// that solves nothing. What was missing was any signal at all.
//
// It keeps checking rather than firing once. A session can come good long
// after the deadline — a prompt answered by hand, an announcement whose every
// copy the repaint damaged — and a provider that records the URL itself
// (provider.URLReader) is asked on every check, so the URL is found and the
// complaint withdrawn instead of standing for the rest of the session's life.
func (m *Managed) armURLWatchdogLocked() {
	if m.urlTimer != nil {
		m.urlTimer.Stop()
		m.urlTimer = nil
	}
	if !m.prov.Capabilities().PublishesRemoteURL {
		return
	}
	m.urlTimer = time.AfterFunc(urlRecheck, m.checkRemoteURL)
}

func (m *Managed) checkRemoteURL() {
	m.mu.Lock()
	if m.remoteURL != "" || (m.status != Online && m.status != Launching) {
		m.mu.Unlock()
		return
	}
	entry := m.entry
	m.mu.Unlock()

	// File I/O, so outside the lock.
	url := ""
	if r, ok := m.prov.(provider.URLReader); ok {
		url = r.ReadRemoteURL(entry)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.remoteURL != "" || (m.status != Online && m.status != Launching) {
		return
	}
	if url != "" {
		m.urlTimer = nil
		m.urlLocked = true
		m.urlBuf = ""
		m.refreshAnalysisNeededLocked()
		m.setRemoteURLLocked(url, "remote control URL (from the provider's session record): ")
		return
	}

	// Only into an empty slot: a blocked prompt's own description says more.
	if m.status == Online && time.Since(m.urlWatchFrom) >= urlWatchdog && m.message == "" {
		msg := urlWatchdogMessage(m.entry.Name)
		m.updateLocked(func() { m.message = msg })
		m.logLocked(msg)
	}
	m.urlTimer = time.AfterFunc(urlRecheck, m.checkRemoteURL)
}

func urlWatchdogMessage(name string) string {
	return "no remote-control URL after " + itoa(int(urlWatchdog.Seconds())) + "s — " +
		"the session may be stuck on a prompt; check: rcmanager logs " + name
}

// setRemoteURLLocked records the URL, and withdraws the watchdog's complaint
// if that is what the session is still showing.
func (m *Managed) setRemoteURLLocked(url, logPrefix string) {
	stale := m.message == urlWatchdogMessage(m.entry.Name)
	m.updateLocked(func() {
		m.remoteURL = url
		if stale {
			m.message = ""
		}
	})
	m.logLocked(logPrefix + url)
}

var whitespaceRe = regexp.MustCompile(`\s+`)

// captureRemoteURL learns the remote-control URL from the session's own
// output — once.
//
// A managed session prints whatever its agent prints, and an agent that
// mentions another session's claude.ai/code link would otherwise have that
// link recorded as its own. (Observed: the session working on rcmanager echoed a
// status table and adopted a neighbour's URL.) The URL is announced at startup
// and does not change for the life of a process, so the first one found is the
// right one and everything after it is noise.
//
// A session that never announces one keeps looking, but only for a while:
// after that a stray URL in the output is far likelier than a late
// announcement.
func (m *Managed) captureRemoteURL(text string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.urlLocked {
		return
	}
	if !m.urlWatchFrom.IsZero() && time.Since(m.urlWatchFrom) > urlWindow {
		// The window has closed: a stray URL in the output is now likelier
		// than a late announcement, and there may be nothing left to read for.
		m.urlLocked = true
		m.urlBuf = ""
		m.refreshAnalysisNeededLocked()
		return
	}

	// The url arrives split across writes (and often across a line wrap), so
	// match against a rolling window with whitespace collapsed rather than
	// against one chunk — a per-chunk match silently misses it.
	m.urlBuf = tailOf(m.urlBuf+whitespaceRe.ReplaceAllString(ansi.Strip(text), " "), 4096)
	url := m.prov.ExtractRemoteURL(m.urlBuf)
	if url == "" {
		return
	}

	m.urlLocked = true
	m.urlBuf = ""
	if m.urlTimer != nil {
		m.urlTimer.Stop()
		m.urlTimer = nil
	}
	if url == m.remoteURL {
		return
	}
	m.refreshAnalysisNeededLocked()
	m.setRemoteURLLocked(url, "remote control URL: ")
}

// handleInteractions answers the blocking prompts an agent CLI shows on
// startup.
//
// Unattended supervision is the whole point of rcmanager, and a modal that waits
// for a keypress stalls a session forever. Providers declare exactly which
// prompts they know how to answer; anything else is left alone, and a prompt
// marked fatal (a trust decision, say) is reported rather than clicked
// through — rcmanager must never accept a security prompt on the user's behalf.
func (m *Managed) handleInteractions(text string) {
	m.mu.Lock()
	interactions := m.prov.Interactions()
	if len(interactions) == 0 {
		m.mu.Unlock()
		return
	}

	// Prompts render across several writes, so match against a rolling tail —
	// kept to pattern size rather than kilobytes.
	m.tailBuf = tailOf(m.tailBuf+stripForMatch(text), matchTail)

	// Collect first, act after. Clearing the buffer inside the loop would hide
	// a second prompt that arrived in the same chunk from every interaction
	// still to be tested.
	// A startup modal that has not appeared by now is not going to, and every
	// pattern retired here is one fewer regex run against every repaint for
	// the rest of the session's life.
	if len(m.pending) > 0 && !m.urlWatchFrom.IsZero() && time.Since(m.urlWatchFrom) > promptWindow {
		for _, action := range interactions {
			if action.Once && !action.Fatal {
				delete(m.pending, action.ID)
			}
		}
		m.refreshAnalysisNeededLocked()
	}

	var matched []int
	for i, action := range interactions {
		if action.Once && !m.pending[action.ID] {
			continue
		}
		if !action.Match.MatchString(m.tailBuf) {
			continue
		}
		if action.Once {
			delete(m.pending, action.ID)
		}
		matched = append(matched, i)
	}
	if len(matched) == 0 {
		m.mu.Unlock()
		return
	}
	m.tailBuf = ""
	m.refreshAnalysisNeededLocked()
	entry := m.entry
	m.mu.Unlock()

	for _, i := range matched {
		action := interactions[i]
		keys := ""
		if action.Respond != nil {
			keys = action.Respond(entry)
		}

		// No answer configured: report the block instead of guessing. This is
		// how a security prompt stays the user's decision.
		if keys == "" {
			if action.Fatal {
				m.mu.Lock()
				m.blocked = action.Describe
				m.updateLocked(func() { m.message = action.Describe })
				m.logLocked("blocked: " + action.Describe)
				m.mu.Unlock()
			}
			continue
		}

		delay := action.Delay
		if delay <= 0 {
			delay = 750 * time.Millisecond
		}
		label := action.Answered
		if label == "" {
			label = action.Describe
		}
		id := action.ID
		time.AfterFunc(delay, func() {
			if !m.io.WriteKeys(keys) {
				m.log("could not answer \"" + id + "\": no stdin fifo for this session")
				return
			}
			m.log(label + " (sent " + quote(keys) + ")")
		})
	}
}

// stripForMatch flattens output into a form that prompt patterns can match
// reliably.
//
// A TUI paints its screen with absolute cursor positioning rather than writing
// spaces, so once the escapes are removed the words run together
// ("Resume from summary" -> "Resumefromsummary") and any pattern containing a
// space fails. Rather than emulate a terminal, rcmanager removes whitespace
// entirely on both sides of the comparison: provider patterns are written
// without spaces and matched against this.
func stripForMatch(text string) string {
	return strings.ToLower(whitespaceRe.ReplaceAllString(ansi.Strip(text), ""))
}

func tailOf(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[len(s)-max:]
}

func quote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case 0x1b:
			b.WriteString(`\u001b`)
		case '"':
			b.WriteString(`\"`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	if neg {
		return "-" + string(digits)
	}
	return string(digits)
}
