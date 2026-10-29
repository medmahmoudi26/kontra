package stdiobridge

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"kontra.sh/kontra/internal/mcproots"
)

// Errors of child.send.
var (
	errGone = errors.New("stdiobridge: the child is gone")
	errBusy = errors.New("stdiobridge: the child does not read its stdin")
)

// child is one server process and its MCP session.
type child struct {
	b     *Bridge
	owner any
	id    string // its Mcp-Session-Id, minted before it starts; reachable once open
	start int64  // the <child start> of its event ids: its start time, Unix nanoseconds
	pid   int    // the leader's pid, the process group's id
	log   *slog.Logger
	roots *mcproots.Session

	exited     chan struct{} // closed once the leader exited and was reaped
	stdoutDone chan struct{} // closed once its stdout is read to its end

	// stdin: a queue a writer goroutine drains, so that neither a POST nor the stdout reader ever blocks on a child
	// that does not read.
	inMu     sync.Mutex
	inQueue  [][]byte
	inSize   int
	inClosed bool
	inWake   chan struct{}
	stdin    *os.File
	inOnce   sync.Once

	mu        sync.Mutex
	gone      bool   // stopped: the session answers 404
	seq       uint64 // the last event number
	pending   map[string]*stream
	progress  map[string]*stream
	taskRes   map[string]*stream
	queue     []event // the GET stream's queue and replay history
	queueSize int
	delivered uint64 // the last event number handed to a GET stream
	get       *stream
	inflight  int
	lastUsed  time.Time
	idle      *time.Timer
	stopOnce  sync.Once
	requested atomic.Bool // the bridge stopped it (DELETE, idle, a tunnel session's end, Close), not its own exit
}

// spawn starts a child (b.reserve counted it): its own process group, the scrubbed environment, pipes the bridge
// reads and writes. The leader is reaped by a goroutine of its own, so its exit is seen even while a process it
// started keeps the pipes open.
func (b *Bridge) spawn(owner any, id string) (*child, error) {
	fail := func(err error, files ...*os.File) (*child, error) {
		for _, f := range files {
			if f != nil {
				_ = f.Close()
			}
		}
		b.mu.Lock()
		b.reserved--
		b.mu.Unlock()
		return nil, err
	}
	inR, inW, err := os.Pipe()
	if err != nil {
		return fail(err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		return fail(err, inR, inW)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		return fail(err, inR, inW, outR, outW)
	}
	cmd := exec.Command(b.cfg.Command[0], b.cfg.Command[1:]...)
	cmd.Env = b.env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = inR, outW, errW
	inGroup(cmd)
	err = cmd.Start()
	_, _ = inR.Close(), outW.Close()
	_ = errW.Close()
	if err != nil {
		return fail(err, inW, outR, errR)
	}
	c := &child{
		b: b, owner: owner, id: id, start: time.Now().UnixNano(), pid: cmd.Process.Pid,
		roots:  mcproots.NewSession(b.cfg.Roots),
		exited: make(chan struct{}), stdoutDone: make(chan struct{}),
		inWake: make(chan struct{}, 1), stdin: inW,
		pending: map[string]*stream{}, progress: map[string]*stream{}, taskRes: map[string]*stream{},
		lastUsed: time.Now(),
	}
	c.log = b.log.With("pid", c.pid, "mcp_session", id[:6])
	b.mu.Lock()
	b.reserved--
	b.all[c] = struct{}{}
	b.mu.Unlock()
	go c.writeStdin()
	go c.readStdout(outR)
	go c.readStderr(errR)
	go func() {
		err := cmd.Wait()
		close(c.exited)
		// What it wrote before it exited is routed first (a last response), unless a process it started still holds
		// its stdout.
		select {
		case <-c.stdoutDone:
		case <-time.After(500 * time.Millisecond):
		}
		if !c.requested.Load() {
			c.log.Warn("the stdio server exited: its session answers 404 from now on", "exit", exitString(err))
		} else {
			c.log.Debug("the stdio server exited", "exit", exitString(err))
		}
		c.stop("exited")
	}()
	return c, nil
}

func exitString(err error) string {
	if err == nil {
		return "status 0"
	}
	return err.Error()
}

// eventID is the SSE id of event n: <child start>-<n>.
func (c *child) eventID(n uint64) string {
	return strconv.FormatInt(c.start, 10) + "-" + strconv.FormatUint(n, 10)
}

// parseEventID reads a Last-Event-ID of this child; ok false for another child's or a malformed one.
func (c *child) parseEventID(v string) (uint64, bool) {
	start, n, ok := bytes.Cut([]byte(v), []byte("-"))
	if !ok || string(start) != strconv.FormatInt(c.start, 10) {
		return 0, false
	}
	k, err := strconv.ParseUint(string(n), 10, 64)
	return k, err == nil
}

// --- stdin ---

// send queues one message for the child's stdin, as one line: compacted, so that no newline of the client's JSON
// splits it. errGone after the child's stdin closed; errBusy when the child has not read what waits already.
func (c *child) send(msg []byte) error {
	var buf bytes.Buffer
	if err := compact(&buf, msg); err != nil {
		return err
	}
	buf.WriteByte('\n')
	c.inMu.Lock()
	defer c.inMu.Unlock()
	switch {
	case c.inClosed:
		return errGone
	case c.inSize+buf.Len() > maxStdinQueue:
		return errBusy
	}
	c.inQueue = append(c.inQueue, buf.Bytes())
	c.inSize += buf.Len()
	select {
	case c.inWake <- struct{}{}:
	default:
	}
	return nil
}

// writeStdin writes the queue to the child's stdin until it closes.
func (c *child) writeStdin() {
	defer c.closeStdin()
	for {
		c.inMu.Lock()
		for len(c.inQueue) == 0 && !c.inClosed {
			c.inMu.Unlock()
			<-c.inWake
			c.inMu.Lock()
		}
		if c.inClosed {
			c.inMu.Unlock()
			return
		}
		msg := c.inQueue[0]
		c.inQueue = c.inQueue[1:]
		c.inSize -= len(msg)
		c.inMu.Unlock()
		if _, err := c.stdin.Write(msg); err != nil {
			c.log.Debug("the stdio server's stdin is closed", "err", err)
			return
		}
	}
}

// closeStdin closes the child's stdin now, dropping what waits (a blocked write returns).
func (c *child) closeStdin() {
	c.inMu.Lock()
	c.inClosed = true
	c.inQueue, c.inSize = nil, 0
	c.inMu.Unlock()
	select {
	case c.inWake <- struct{}{}:
	default:
	}
	c.inOnce.Do(func() { _ = c.stdin.Close() })
}

// --- stdout and stderr ---

// errLineTooLong is a line of the child's stdout over MaxMessage, which was read through and dropped.
var errLineTooLong = errors.New("stdiobridge: a line over MaxMessage")

// readStdout reads the child's stdout line by line, never stopping while it is open (§10), and routes each line.
// When it closes, the session is over.
func (c *child) readStdout(f *os.File) {
	defer f.Close()
	defer close(c.stdoutDone)
	br := bufio.NewReaderSize(f, 64<<10)
	for {
		line, err := readLine(br, MaxMessage)
		switch {
		case errors.Is(err, errLineTooLong):
			c.log.Error("a message from the stdio server is over 8 MiB: dropped", "max_bytes", MaxMessage)
			continue
		case len(line) > 0:
			c.route(line)
		}
		if err != nil {
			break
		}
	}
	c.stop("its stdout closed")
}

// readLine reads one line without its end (\n or \r\n); a line over max is read through and errLineTooLong.
func readLine(br *bufio.Reader, max int) ([]byte, error) {
	var line []byte
	tooLong := false
	for {
		chunk, err := br.ReadSlice('\n')
		if !tooLong {
			if len(line)+len(chunk) > max+2 {
				tooLong, line = true, nil
			} else {
				line = append(line, chunk...)
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if tooLong {
			return nil, errLineTooLong
		}
		return bytes.TrimRight(line, "\r\n"), err
	}
}

// readStderr copies the child's stderr to the node's log, line by line, at most StderrPerMinute bytes a minute:
// what is over is read and dropped, and the drop is logged once a minute. It never stops reading.
func (c *child) readStderr(f *os.File) {
	defer f.Close()
	var (
		window  time.Time
		used    int
		dropped int64
		partial []byte
	)
	report := func() {
		if dropped > 0 {
			c.log.Warn("the stdio server's stderr was over 1 MiB a minute: the rest was not logged", "dropped_bytes", dropped)
		}
	}
	buf := make([]byte, 32<<10)
	for {
		n, err := f.Read(buf)
		if n > 0 {
			now := time.Now()
			if now.Sub(window) >= time.Minute {
				report()
				window, used, dropped = now, 0, 0
			}
			chunk, cut := buf[:n], false
			if allow := max(StderrPerMinute-used, 0); len(chunk) > allow {
				if dropped == 0 {
					c.log.Warn("the stdio server writes more than 1 MiB a minute to stderr: the rest of this minute is not logged")
				}
				dropped += int64(len(chunk) - allow)
				chunk, cut = chunk[:allow], true
			}
			used += len(chunk)
			partial = append(partial, chunk...)
			if cut && len(partial) > 0 {
				partial = append(partial, '\n') // a line cut by the cap ends here
			}
			partial = c.stderrLines(partial)
		}
		if err != nil {
			if len(partial) > 0 {
				c.stderrLine(partial)
			}
			report()
			return
		}
	}
}

// stderrLines logs every complete line of p (a line over 4 KiB in pieces) and returns the rest.
func (c *child) stderrLines(p []byte) []byte {
	const maxLine = 4 << 10
	for {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			for len(p) > maxLine {
				c.stderrLine(p[:maxLine])
				p = p[maxLine:]
			}
			return append([]byte(nil), p...)
		}
		for len(p[:i]) > maxLine {
			c.stderrLine(p[:maxLine])
			p, i = p[maxLine:], i-maxLine
		}
		if i > 0 {
			c.stderrLine(p[:i])
		}
		p = p[i+1:]
	}
}

func (c *child) stderrLine(line []byte) {
	line = bytes.TrimRight(line, "\r")
	if len(line) == 0 {
		return
	}
	if w := c.b.cfg.Stderr; w != nil {
		_, _ = w.Write(append(append([]byte(nil), line...), '\n'))
		return
	}
	c.log.Info("stdio server stderr", "line", string(bytes.ToValidUTF8(line, []byte("�"))))
}

// --- the session's life ---

// begin counts an exchange on the session; false when the session is gone.
func (c *child) begin() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gone {
		return false
	}
	c.inflight++
	c.lastUsed = time.Now()
	return true
}

func (c *child) end() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.inflight--
	c.lastUsed = time.Now()
}

// checkIdle stops a session that has had no exchange for SessionIdle (none open, none started).
func (c *child) checkIdle() {
	idle := c.b.cfg.SessionIdle
	c.mu.Lock()
	if c.gone {
		c.mu.Unlock()
		return
	}
	if c.inflight > 0 {
		c.idle.Reset(idle)
		c.mu.Unlock()
		return
	}
	if left := idle - time.Since(c.lastUsed); left > 0 {
		c.idle.Reset(left)
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()
	c.log.Info("the stdio session was idle for --session-idle: stopping the child", "session_idle", idle)
	c.shutdown("idle")
}

// shutdown is stop asked for by the bridge (DELETE, idle, a tunnel session's end, Close, a failed initialize): the
// child's exit that follows is not a crash.
func (c *child) shutdown(reason string) {
	c.requested.Store(true)
	c.stop(reason)
}

// stop ends the session at once (every open stream ends, later requests are 404) and stops the process group in
// the background: stdin closed, SIGTERM after TerminateAfter, SIGKILL after KillAfter more (D184).
func (c *child) stop(reason string) {
	c.stopOnce.Do(func() {
		c.mu.Lock()
		c.gone = true
		var streams []*stream
		for _, m := range []map[string]*stream{c.pending, c.progress, c.taskRes} {
			for k, st := range m {
				streams = append(streams, st)
				delete(m, k)
			}
		}
		if c.get != nil {
			streams = append(streams, c.get)
			c.get = nil
		}
		if c.idle != nil {
			c.idle.Stop()
		}
		c.queue, c.queueSize = nil, 0
		c.mu.Unlock()
		for _, st := range streams {
			st.end()
		}
		c.b.forget(c)
		go c.terminate(reason)
	})
}

// terminate stops the process group and then releases the child's place under MaxSessions.
func (c *child) terminate(reason string) {
	defer c.b.release(c)
	log := c.log.With("reason", reason)
	c.closeStdin()
	if c.waitGone(c.b.cfg.TerminateAfter) {
		log.Debug("the stdio server stopped after its stdin closed")
		return
	}
	if err := signalGroup(c.pid, sigTerm); err == nil {
		log.Info("the stdio server's process group did not stop when its stdin closed: SIGTERM")
	}
	if c.waitGone(c.b.cfg.KillAfter) {
		return
	}
	if err := signalGroup(c.pid, sigKill); err == nil {
		log.Warn("the stdio server's process group did not stop on SIGTERM: SIGKILL")
	}
	if !c.waitGone(c.b.cfg.KillAfter) {
		log.Error("processes of the stdio server's group remain after SIGKILL", "pgid", c.pid)
	}
}

// waitGone waits up to d for the leader to be reaped and its group to be empty.
func (c *child) waitGone(d time.Duration) bool {
	deadline := time.NewTimer(d)
	defer deadline.Stop()
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	for {
		exited := false
		select {
		case <-c.exited:
			exited = true
		default:
		}
		if exited && !groupAlive(c.pid) {
			return true
		}
		if exited {
			select {
			case <-deadline.C:
				return false
			case <-tick.C:
			}
			continue
		}
		select {
		case <-deadline.C:
			return false
		case <-c.exited:
		case <-tick.C:
		}
	}
}

// compact writes msg without the white space between its tokens (one line on stdin and in an SSE data field).
func compact(buf *bytes.Buffer, msg []byte) error {
	if err := json.Compact(buf, msg); err != nil {
		return fmt.Errorf("stdiobridge: %w", err)
	}
	return nil
}
