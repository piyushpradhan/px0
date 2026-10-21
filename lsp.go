package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"px0/internal/metrics"
	"px0/internal/render"
	"px0/internal/symbols"
)

// A minimal LSP client: enough of the protocol to answer navigation questions
// (definition, references, document symbols) and nothing else. Everything is
// best-effort — if a server is missing, slow, or broken, callers fall back to
// the regex index, which is always available.

// ---------------------------------------------------------------- protocol

type lspPosition struct {
	Line      int `json:"line"`      // 0-based
	Character int `json:"character"` // 0-based, in the negotiated encoding
}

type lspRange struct {
	Start lspPosition `json:"start"`
	End   lspPosition `json:"end"`
}

type lspLocation struct {
	URI   string   `json:"uri"`
	Range lspRange `json:"range"`
}

type lspDocumentSymbol struct {
	Name           string              `json:"name"`
	Kind           int                 `json:"kind"`
	Range          lspRange            `json:"range"`
	SelectionRange lspRange            `json:"selectionRange"`
	Children       []lspDocumentSymbol `json:"children"`
	// symbolInformation form, used by servers without hierarchical support
	Location *lspLocation `json:"location"`
}

// Kind numbers come from the LSP spec; we only need display names.
var lspSymbolKind = map[int]string{
	1: "file", 2: "module", 3: "namespace", 4: "package", 5: "class",
	6: "method", 7: "property", 8: "field", 9: "ctor", 10: "enum",
	11: "interface", 12: "func", 13: "var", 14: "const", 15: "string",
	16: "number", 17: "bool", 18: "array", 19: "object", 20: "key",
	21: "null", 22: "enum", 23: "struct", 24: "event", 25: "operator",
	26: "type",
}

// LSPDiagnostic represents a single compiler/linter diagnostic reported by a
// language server via textDocument/publishDiagnostics.
type LSPDiagnostic struct {
	Line     int    `json:"line"`              // 1-based start line
	EndLine  int    `json:"endLine,omitempty"` // 1-based end line
	Col      int    `json:"col"`               // 0-based start character
	EndCol   int    `json:"endCol,omitempty"`  // 0-based end character
	Severity int    `json:"severity"`          // 1=Error, 2=Warning, 3=Info, 4=Hint
	Message  string `json:"message"`
	Source   string `json:"source,omitempty"`
	Code     string `json:"code,omitempty"`
}

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("lsp error %d: %s", e.Code, e.Message) }

// ---------------------------------------------------------------- client

// writeReq is one framed message handed to the dedicated writer goroutine.
// res is buffered(1) so the writer never blocks delivering the result.
type writeReq struct {
	body []byte
	res  chan error
}

// writeQueueCap bounds how many unsent messages may pile up behind a slow
// server. A full queue applies backpressure to the sender (write blocks)
// instead of growing memory without bound.
const writeQueueCap = 256

type lspClient struct {
	def  lspServerDef
	root string

	cmd  *exec.Cmd
	in   io.WriteCloser
	out  *bufio.Reader
	logf func(string, ...any)

	// mu guards nextID, pending, opened, openedText, syncGen, dead and the
	// writer handles below. It is an RWMutex so hot read paths (alive, status
	// lookups) don't serialize against each other. The mutex is NEVER held
	// across pipe I/O: all writes go through the dedicated writer goroutine,
	// so a stalled server can't wedge every other caller on this client.
	mu          sync.RWMutex
	nextID      int64
	pending     map[int64]chan rpcMessage
	opened      map[string]int             // uri -> document version
	openedText  map[string]string          // uri -> last text sent (unchanged skip + incremental diffs)
	syncGen     map[string]uint64          // uri -> generation of the latest syncDoc read that applied
	diagnostics map[string][]LSPDiagnostic // uri -> latest published diagnostics
	syncGenNext atomic.Uint64              // dispenses syncDoc generations; no lock needed
	lastUsed    atomic.Int64               // unix nanos of most recent client interaction
	dead        error

	// writeCh feeds the dedicated writer goroutine. It is created with the
	// client (never closed: closing it would race with concurrent senders,
	// since send-on-closed panics) and drained+abandoned on fail().
	writeCh chan writeReq
	// deadCh is closed by fail(); it unblocks senders stuck enqueueing and
	// tells the writer to drain with errors and exit.
	deadCh           chan struct{}
	writeLoopStarted bool

	// docMu serializes document open/sync/close sequences per client, so the
	// check → notify → record steps in ensureOpen/syncDoc/closeDoc are atomic
	// across goroutines. Version assignment can't interleave, and a document
	// can't be didOpen'ed twice.
	docMu sync.Mutex

	// encoding is how the server counts Character offsets: "utf-8", "utf-16"
	// (the spec default) or "utf-32".
	encoding string

	// syncIncremental is true when the server advertised incremental
	// textDocumentSync during initialize. Only then may didChange carry
	// ranged edits; otherwise the full text is always sent. Set once by
	// initialize before the client is published; read under c.mu.
	syncIncremental bool

	readyCh  chan struct{}
	once     sync.Once
	failOnce sync.Once

	// shutdownOnce makes shutdown idempotent: manager.Close, Stop, and the
	// client restart path can all trigger it, including concurrently. A
	// second shutdown on the same client must not re-send exit or double-close
	// the pipes.
	shutdownOnce sync.Once

	// indexing tracks $/progress tokens so callers can tell "no result" from
	// "the server has not finished indexing yet".
	indexing atomic.Int32
}

func newLSPClient(def lspServerDef, root string) *lspClient {
	c := &lspClient{
		def: def, root: root,
		pending:     map[int64]chan rpcMessage{},
		opened:      map[string]int{},
		openedText:  map[string]string{},
		syncGen:     map[string]uint64{},
		diagnostics: map[string][]LSPDiagnostic{},
		encoding:    "utf-16",
		writeCh:     make(chan writeReq, writeQueueCap),
		deadCh:      make(chan struct{}),
		readyCh:     make(chan struct{}),
		logf:        func(string, ...any) {},
	}
	c.lastUsed.Store(time.Now().UnixNano())
	return c
}

func (c *lspClient) start(ctx context.Context) error {
	c.cmd = exec.Command(c.def.Cmd[0], c.def.Cmd[1:]...)
	c.cmd.Dir = c.root
	c.cmd.Stderr = io.Discard

	stdin, err := c.cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := c.cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := c.cmd.Start(); err != nil {
		return err
	}
	c.in, c.out = stdin, bufio.NewReaderSize(stdout, 64<<10)
	c.beginWriteLoop()

	go c.readLoop()
	go func() {
		c.cmd.Wait()
		c.fail(fmt.Errorf("%s exited", c.def.Name))
	}()

	return c.initialize(ctx)
}

// beginWriteLoop starts the dedicated writer goroutine. start() calls it once
// the pipes exist; tests that wire a client by hand (no spawned process) call
// it directly after assigning c.in/c.out.
func (c *lspClient) beginWriteLoop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.writeLoopStarted {
		return
	}
	c.writeLoopStarted = true
	select {
	case <-c.deadCh:
		return // already failed; nothing will ever be enqueued
	default:
	}
	go c.writeLoop()
}

// writeLoop is the single goroutine that touches the server's stdin. One
// writer means frames can't interleave, and no caller ever holds c.mu while
// blocked in a pipe write.
func (c *lspClient) writeLoop() {
	for {
		select {
		case req := <-c.writeCh:
			if err := c.writeFrame(req.body); err != nil {
				req.res <- err
				// A failed pipe write means the server is gone: mark the
				// client dead now so senders blocked on a full queue are
				// released via deadCh and pending calls fail fast, instead
				// of every subsequent write failing individually forever.
				// fail is idempotent; the drain branch above takes it from
				// here. The writer holds no locks, so this can't deadlock.
				c.fail(err)
			} else {
				req.res <- nil
			}
		case <-c.deadCh:
			// The client died: fail everything still queued, then exit.
			// writeCh itself is never closed (send-on-closed panics), so
			// drain it here instead.
			err := c.alive()
			if err == nil {
				err = fmt.Errorf("%s: connection lost", c.def.Name)
			}
			for {
				select {
				case req := <-c.writeCh:
					req.res <- err
				default:
					return
				}
			}
		}
	}
}

// writeFrame performs the actual pipe I/O for one message. It snapshots the
// stdin handle under a read lock and never holds the mutex across the write.
func (c *lspClient) writeFrame(body []byte) error {
	c.mu.RLock()
	in := c.in
	c.mu.RUnlock()
	if in == nil {
		return fmt.Errorf("lsp client stdin closed")
	}
	// Frame header without fmt.Fprintf's allocation.
	var hdr [48]byte
	p := append(hdr[:0], "Content-Length: "...)
	p = strconv.AppendInt(p, int64(len(body)), 10)
	p = append(p, '\r', '\n', '\r', '\n')
	if _, err := in.Write(p); err != nil {
		return err
	}
	_, err := in.Write(body)
	return err
}

func (c *lspClient) fail(err error) {
	c.mu.Lock()
	if c.dead == nil {
		c.dead = err
	}
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
	c.mu.Unlock()
	c.failOnce.Do(func() { close(c.deadCh) })
	c.once.Do(func() { close(c.readyCh) })
}

func (c *lspClient) alive() error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.dead
}

// readLoop consumes framed messages and routes them: responses to their waiting
// caller, server-initiated requests to a stub reply (a server that never hears
// back from us can stall), notifications to progress tracking.
func (c *lspClient) readLoop() {
	for {
		msg, err := readFrame(c.out)
		if err != nil {
			c.fail(err)
			return
		}
		switch {
		case msg.Method == "" && len(msg.ID) > 0: // response
			id, err := strconv.ParseInt(strings.Trim(string(msg.ID), `"`), 10, 64)
			if err != nil {
				continue
			}
			c.mu.Lock()
			ch, ok := c.pending[id]
			delete(c.pending, id)
			c.mu.Unlock()
			if ok {
				ch <- msg
				close(ch)
			}
		case len(msg.ID) > 0: // server -> client request; must be answered
			c.reply(msg.ID, msg.Method)
		default: // notification
			c.onNotification(msg)
		}
	}
}

func (c *lspClient) reply(id json.RawMessage, method string) {
	var result any
	switch method {
	case "workspace/configuration":
		result = []any{map[string]any{}}
	case "workspace/workspaceFolders":
		result = []any{map[string]string{"uri": pathToURI(c.root), "name": filepath.Base(c.root)}}
	default:
		result = nil
	}
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id), "result": result})
	// Fire-and-forget: reply() runs on the readLoop, and a synchronous write
	// could deadlock against a server that is blocked writing to us.
	// Best-effort is fine here; the server will time out a lost reply.
	_ = c.writeAsync(body)
}

func (c *lspClient) onNotification(msg rpcMessage) {
	switch msg.Method {
	case "$/progress":
		var p struct {
			Value struct {
				Kind string `json:"kind"`
			} `json:"value"`
		}
		if json.Unmarshal(msg.Params, &p) != nil {
			return
		}
		switch p.Value.Kind {
		case "begin":
			c.indexing.Add(1)
		case "end":
			// Floor at zero: a stray "end" without a matching "begin"
			// must not drive the counter negative.
			for {
				v := c.indexing.Load()
				if v <= 0 || c.indexing.CompareAndSwap(v, v-1) {
					break
				}
			}
		}
	case "textDocument/publishDiagnostics":
		var p struct {
			URI         string `json:"uri"`
			Diagnostics []struct {
				Range    lspRange        `json:"range"`
				Severity int             `json:"severity"`
				Code     json.RawMessage `json:"code,omitempty"`
				Source   string          `json:"source,omitempty"`
				Message  string          `json:"message"`
			} `json:"diagnostics"`
		}
		if json.Unmarshal(msg.Params, &p) != nil || p.URI == "" {
			return
		}
		diags := make([]LSPDiagnostic, 0, len(p.Diagnostics))
		for _, d := range p.Diagnostics {
			sev := d.Severity
			if sev <= 0 {
				sev = 1
			}
			codeStr := strings.Trim(string(d.Code), `" `)
			if codeStr == "null" {
				codeStr = ""
			}
			diags = append(diags, LSPDiagnostic{
				Line:     d.Range.Start.Line + 1,
				EndLine:  d.Range.End.Line + 1,
				Col:      d.Range.Start.Character,
				EndCol:   d.Range.End.Character,
				Severity: sev,
				Message:  d.Message,
				Source:   d.Source,
				Code:     codeStr,
			})
		}
		c.mu.Lock()
		if c.diagnostics == nil {
			c.diagnostics = map[string][]LSPDiagnostic{}
		}
		if len(diags) == 0 {
			delete(c.diagnostics, p.URI)
		} else {
			c.diagnostics[p.URI] = diags
		}
		c.mu.Unlock()
	}
}

func (c *lspClient) busy() bool {
	return c.indexing.Load() > 0
}

// readFrame parses one Content-Length-framed JSON-RPC message.
//
// Header parsing is allocation-free: header lines are sliced out of the
// reader's buffer and matched/parsed in place.
//
// The body buffer is intentionally NOT pooled or reused. rpcMessage carries
// Params/Result/ID as json.RawMessage slices aliasing that buffer, and those
// outlive readFrame (responses are handed to waiting callers, notifications
// are unmarshalled after routing), so reusing the buffer would corrupt
// in-flight messages.
func readFrame(r *bufio.Reader) (rpcMessage, error) {
	var length int
	for {
		line, err := readHeaderLine(r)
		if err != nil {
			return rpcMessage{}, err
		}
		line = trimCRLF(line)
		if len(line) == 0 {
			break
		}
		if name, val, ok := cutHeaderField(line); ok && isContentLength(name) {
			if n, ok := parseHeaderInt(val); ok {
				length = n
			}
		}
	}
	if length <= 0 || length > 64<<20 {
		return rpcMessage{}, fmt.Errorf("bad content length %d", length)
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(r, buf); err != nil {
		return rpcMessage{}, err
	}
	var msg rpcMessage
	if err := json.Unmarshal(buf, &msg); err != nil {
		return rpcMessage{}, err
	}
	return msg, nil
}

// readHeaderLine reads one \n-terminated header line without allocating in the
// common case. The returned slice aliases the reader's buffer and is only
// valid until the next read — callers must consume it immediately.
func readHeaderLine(r *bufio.Reader) ([]byte, error) {
	line, err := r.ReadSlice('\n')
	if err == nil {
		return line, nil
	}
	if err != bufio.ErrBufferFull {
		return nil, err
	}
	// Pathological header line longer than the buffer: accumulate the chunks.
	var buf []byte
	buf = append(buf, line...)
	for {
		chunk, err := r.ReadSlice('\n')
		buf = append(buf, chunk...)
		if err == nil {
			return buf, nil
		}
		if err != bufio.ErrBufferFull {
			return nil, err
		}
	}
}

func trimCRLF(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}

// cutHeaderField splits a "Name: value" header line on the first colon,
// trimming OWS (spaces/tabs) around both parts without allocating.
func cutHeaderField(line []byte) (name, val []byte, ok bool) {
	for i, ch := range line {
		if ch == ':' {
			return trimHeaderSpace(line[:i]), trimHeaderSpace(line[i+1:]), true
		}
	}
	return nil, nil, false
}

func trimHeaderSpace(b []byte) []byte {
	for len(b) > 0 && (b[0] == ' ' || b[0] == '\t') {
		b = b[1:]
	}
	for len(b) > 0 && (b[len(b)-1] == ' ' || b[len(b)-1] == '\t') {
		b = b[:len(b)-1]
	}
	return b
}

// isContentLength reports whether a header name is "Content-Length",
// case-insensitively (header names are ASCII by spec).
func isContentLength(name []byte) bool {
	const want = "content-length"
	if len(name) != len(want) {
		return false
	}
	for i := 0; i < len(want); i++ {
		ch := name[i]
		if 'A' <= ch && ch <= 'Z' {
			ch += 'a' - 'A'
		}
		if ch != want[i] {
			return false
		}
	}
	return true
}

// parseHeaderInt parses a decimal header value without allocating. It rejects
// empty/non-numeric input and values beyond the frame size cap, mirroring the
// bounds check readFrame applies afterwards.
func parseHeaderInt(b []byte) (int, bool) {
	if len(b) == 0 {
		return 0, false
	}
	n := 0
	for _, ch := range b {
		if ch < '0' || ch > '9' {
			return 0, false
		}
		n = n*10 + int(ch-'0')
		if n > 64<<20 {
			return 0, false
		}
	}
	return n, true
}

// enqueue hands one framed message to the dedicated writer and returns the
// channel the writer will deliver the flush result on. It blocks only on
// enqueueing (backpressure when the queue is full), never on pipe I/O.
// Callers choose whether to wait for the flush.
func (c *lspClient) enqueue(body []byte) (<-chan error, error) {
	c.mu.RLock()
	dead := c.dead
	ch := c.writeCh
	done := c.deadCh
	started := c.writeLoopStarted
	c.mu.RUnlock()
	if dead != nil {
		return nil, dead
	}
	if ch == nil {
		return nil, fmt.Errorf("lsp client not started")
	}
	if !started {
		c.beginWriteLoop() // idempotent; no-op if a concurrent write beat us
	}
	req := writeReq{body: body, res: make(chan error, 1)}
	select {
	case ch <- req:
		return req.res, nil
	case <-done:
		return nil, c.connLost()
	}
}

// write enqueues one framed message for the dedicated writer and waits for it
// to hit the pipe. It never holds c.mu across I/O: the handle and the dead
// flag are snapshotted under a read lock, then everything blocks on channels.
// If the client dies mid-enqueue, the sender is released via deadCh instead of
// hanging on a queue nobody drains.
//
// The writer is started lazily on first write as a backstop: start() always
// starts it, but a client wired by hand (tests, embedders) may not have.
// Without this, write() would block forever on a queue nobody drains.
func (c *lspClient) write(body []byte) error {
	res, err := c.enqueue(body)
	if err != nil {
		return err
	}
	select {
	case err := <-res:
		return err
	case <-c.deadCh:
		return c.connLost()
	}
}

// writeAsync enqueues one framed message without waiting for the flush. It
// exists for server→client request replies issued from the readLoop: a
// synchronous write there can deadlock — if the server is blocked writing to
// us while its stdin pipe is full, we would wait for a flush the server can't
// drain because it's waiting for us to read. The reply still goes out in FIFO
// order; we just don't wait for it.
func (c *lspClient) writeAsync(body []byte) error {
	_, err := c.enqueue(body)
	return err
}

func (c *lspClient) connLost() error {
	if err := c.alive(); err != nil {
		return err
	}
	return fmt.Errorf("%s: connection lost", c.def.Name)
}

func (c *lspClient) notify(method string, params any) error {
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
	if err != nil {
		return err
	}
	return c.write(body)
}

// notifyAsync enqueues a notification without waiting for the flush. For
// shutdown-time messages where the process kill is the real guarantee and a
// hung server must not trap the caller.
func (c *lspClient) notifyAsync(method string, params any) {
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
	if err != nil {
		return
	}
	_ = c.writeAsync(body)
}

func (c *lspClient) call(ctx context.Context, method string, params any, out any) error {
	c.lastUsed.Store(time.Now().UnixNano())
	c.mu.Lock()
	if c.dead != nil {
		err := c.dead
		c.mu.Unlock()
		return err
	}
	c.nextID++
	id := c.nextID
	ch := make(chan rpcMessage, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	// forget drops the pending slot. It runs on every path where the request
	// never reached (or will never reach) the server, so a marshal or write
	// failure can't leak the entry — and its channel — forever.
	forget := func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}

	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": id, "method": method, "params": params,
	})
	if err != nil {
		forget()
		return err
	}
	// Enqueue without waiting for the flush: the flush wait below joins the
	// same select as ctx.Done(), so a server that stops reading its stdin
	// can't trap the caller past its deadline even when the pipe is full.
	flushed, err := c.enqueue(body)
	if err != nil {
		forget()
		return err
	}
	select {
	case err := <-flushed:
		if err != nil {
			forget()
			return err
		}
	case <-ctx.Done():
		// The request may still be flushed later; dropping the pending slot
		// means a late response is ignored instead of delivered nowhere.
		// Send a cancel in case it does get flushed — best-effort, and async
		// so a wedged server can't trap us here either.
		forget()
		c.notifyAsync("$/cancelRequest", map[string]any{"id": id})
		return ctx.Err()
	}

	select {
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		// Tell the server to stop working on something nobody is waiting for.
		// Async: the server may already be wedged, and a synchronous notify
		// here would block with no timeout.
		c.notifyAsync("$/cancelRequest", map[string]any{"id": id})
		return ctx.Err()
	case msg, ok := <-ch:
		if !ok {
			return fmt.Errorf("%s: connection lost", c.def.Name)
		}
		if msg.Error != nil {
			return msg.Error
		}
		if out == nil || len(msg.Result) == 0 || string(msg.Result) == "null" {
			return nil
		}
		return json.Unmarshal(msg.Result, out)
	}
}

func (c *lspClient) initialize(ctx context.Context) error {
	params := map[string]any{
		"processId": os.Getpid(),
		"rootUri":   pathToURI(c.root),
		"clientInfo": map[string]string{
			"name": "px0", "version": version,
		},
		"workspaceFolders": []any{
			map[string]string{"uri": pathToURI(c.root), "name": filepath.Base(c.root)},
		},
		"capabilities": map[string]any{
			"general": map[string]any{
				// Ask for byte offsets so we can skip UTF-16 conversion where
				// the server is willing; we handle either answer.
				"positionEncodings": []string{"utf-8", "utf-16"},
			},
			"workspace": map[string]any{
				"workspaceFolders": true,
				"configuration":    true,
				"symbol":           map[string]any{"dynamicRegistration": false},
			},
			"textDocument": map[string]any{
				"synchronization": map[string]any{"didSave": true, "dynamicRegistration": false},
				"definition":      map[string]any{"linkSupport": true},
				"typeDefinition":  map[string]any{"linkSupport": true},
				"implementation":  map[string]any{"linkSupport": true},
				"references":      map[string]any{"dynamicRegistration": false},
				"callHierarchy":   map[string]any{"dynamicRegistration": false},
				"documentSymbol": map[string]any{
					"hierarchicalDocumentSymbolSupport": true,
					"dynamicRegistration":               false,
				},
				// Order matters: servers pick the first format they support,
				// and markdown is what carries the fenced signature block.
				"hover":              map[string]any{"contentFormat": []string{"markdown", "plaintext"}},
				"publishDiagnostics": map[string]any{"relatedInformation": false},
			},
			"window": map[string]any{"workDoneProgress": true},
		},
		"initializationOptions": c.def.InitOptions,
	}

	var res struct {
		Capabilities struct {
			PositionEncoding string          `json:"positionEncoding"`
			TextDocumentSync json.RawMessage `json:"textDocumentSync"`
		} `json:"capabilities"`
	}
	if err := c.call(ctx, "initialize", params, &res); err != nil {
		return err
	}
	if res.Capabilities.PositionEncoding != "" {
		c.encoding = res.Capabilities.PositionEncoding
	}
	// Set under the mutex: the client is published to the manager (and thus
	// to other goroutines) as soon as start returns.
	c.mu.Lock()
	c.syncIncremental = parseIncrementalSync(res.Capabilities.TextDocumentSync)
	c.mu.Unlock()
	if err := c.notify("initialized", map[string]any{}); err != nil {
		return err
	}
	c.once.Do(func() { close(c.readyCh) })
	return nil
}

// parseIncrementalSync reports whether an initialize result's textDocumentSync
// capability asks for incremental updates. The capability is either a number
// (0=none, 1=full, 2=incremental) or an object with a numeric "change" field.
// Anything unrecognized conservatively means full sync: sending a ranged
// change to a full-sync server would corrupt its copy of the document.
func parseIncrementalSync(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var n float64
	if err := json.Unmarshal(raw, &n); err == nil {
		return n == 2
	}
	var obj struct {
		Change *float64 `json:"change"`
	}
	if err := json.Unmarshal(raw, &obj); err == nil && obj.Change != nil {
		return *obj.Change == 2
	}
	return false
}

func (c *lspClient) shutdown() {
	c.shutdownOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		// The shutdown request respects ctx (see call): a hung server can't
		// trap us past the deadline.
		c.call(ctx, "shutdown", nil, nil)
		// Deliver the exit notification if the server is healthy enough to
		// take it, but don't wait forever: 200ms is plenty for a live
		// server to flush, and a hung one can't trap shutdownOnce.Do (which
		// would block every other caller). Wedged servers die on stdin EOF
		// or the kill timer below.
		if body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "exit"}); err == nil {
			if flushed, err := c.enqueue(body); err == nil {
				select {
				case <-flushed:
				case <-time.After(200 * time.Millisecond):
				}
			}
		}
		if c.in != nil {
			c.in.Close()
		}
		if c.cmd != nil && c.cmd.Process != nil {
			time.AfterFunc(time.Second, func() { c.cmd.Process.Kill() })
		}
	})
}

// ensureOpen tells the server about a file. Most servers refuse to answer
// questions about a document they were never handed.
//
// The file is read before taking docMu: holding the document lock across file
// I/O would serialize every other document's sync behind a slow disk. The
// check → didOpen → record sequence still runs atomically under docMu, so two
// concurrent callers can't both see "not open" and send didOpen twice for the
// same URI. If the file changed between the read and the didOpen, the next
// syncDoc repairs it.
func (c *lspClient) ensureOpen(abs, rel string) error {
	c.lastUsed.Store(time.Now().UnixNano())
	uri := pathToURI(abs)
	// Fail fast on a dead client instead of doing file I/O for a didOpen
	// that can never be sent.
	if err := c.alive(); err != nil {
		return err
	}
	// Fast path: skip disk I/O if the document was already opened.
	c.mu.RLock()
	_, already := c.opened[uri]
	c.mu.RUnlock()
	if already {
		return nil
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return err
	}
	text := string(data)

	c.docMu.Lock()
	defer c.docMu.Unlock()
	// Re-check under docMu: the client may have died while we were reading,
	// or another goroutine may have opened the document first.
	if err := c.alive(); err != nil {
		return err
	}
	c.mu.RLock()
	_, already = c.opened[uri]
	c.mu.RUnlock()
	if already {
		return nil
	}
	if err := c.notify("textDocument/didOpen", map[string]any{
		"textDocument": map[string]any{
			"uri":        uri,
			"languageId": c.def.LanguageID(rel),
			"version":    1,
			"text":       text,
		},
	}); err != nil {
		return err
	}
	c.mu.Lock()
	c.opened[uri] = 1
	c.openedText[uri] = text
	c.mu.Unlock()
	return nil
}

// syncDoc re-reads an already opened file from disk and notifies the server of
// changes, e.g. after a reindex or external edit.
//
// The version read → increment → store sequence runs under docMu, so it is
// atomic: concurrent syncDocs can no longer read the same version and send
// duplicate version numbers with different content (an LSP violation —
// versions must increase monotonically per document). The file read itself
// happens before docMu is taken, so a slow disk doesn't serialize other
// documents' syncs; the didChange notify still goes out under docMu to keep
// versions and sends in order.
//
// If the file is byte-identical to what was last sent, the didChange/didSave
// round trip is skipped entirely: reindex sweeps call syncDoc over every open
// file, and resending identical full text is pure IPC/CPU waste.
//
// When the server advertised incremental textDocumentSync during initialize,
// only the changed range is sent (a prefix/suffix diff); otherwise the full
// text goes, exactly like didOpen. The diff is conservative: anything it can't
// express safely falls back to the full text.
//
// The version and text are recorded only after the didChange was actually
// sent, so a failed notify leaves the old state in place and the next syncDoc
// retries cleanly instead of skipping a change the server never saw.
func (c *lspClient) syncDoc(abs, rel string) error {
	uri := pathToURI(abs)
	if err := c.alive(); err != nil {
		return err
	}
	// Skip the file read entirely for documents we never opened.
	c.mu.RLock()
	_, opened := c.opened[uri]
	c.mu.RUnlock()
	if !opened {
		return nil
	}
	// Read the file before taking docMu, as in ensureOpen. The generation
	// counter below keeps concurrent syncDocs applying in read order (not
	// lock order): a syncDoc whose read is older than one that already
	// applied is discarded, so a stale read can't overwrite newer text.
	gen := c.syncGenNext.Add(1)
	data, readErr := os.ReadFile(abs)

	c.docMu.Lock()
	defer c.docMu.Unlock()

	if readErr != nil {
		c.closeDocLocked(abs)
		return readErr
	}
	if err := c.alive(); err != nil {
		return err
	}
	c.mu.RLock()
	v, ok := c.opened[uri]
	oldText := c.openedText[uri]
	incremental := c.syncIncremental
	lastGen := c.syncGen[uri]
	c.mu.RUnlock()
	if !ok {
		return nil
	}
	if gen < lastGen {
		// A syncDoc with a newer read already won the lock; our data is
		// stale. Returning here (not an error) keeps the newer text.
		return nil
	}
	newText := string(data)
	if newText == oldText {
		// Record the generation even though nothing is sent: our read is
		// the latest, so a concurrent syncDoc with an older read must be
		// discarded rather than overwriting with stale text.
		c.mu.Lock()
		c.syncGen[uri] = gen
		c.mu.Unlock()
		return nil
	}
	v++

	contentChanges := fullTextChange(newText)
	if incremental {
		if ranged, ok := incrementalChange(c, oldText, newText); ok {
			contentChanges = ranged
		}
	}

	if err := c.notify("textDocument/didChange", map[string]any{
		"textDocument": map[string]any{
			"uri":     uri,
			"version": v,
		},
		"contentChanges": contentChanges,
	}); err != nil {
		return err
	}
	_ = c.notify("textDocument/didSave", map[string]any{
		"textDocument": map[string]any{
			"uri": uri,
		},
	})
	c.mu.Lock()
	c.opened[uri] = v
	c.openedText[uri] = newText
	c.syncGen[uri] = gen
	c.mu.Unlock()
	return nil
}

// fullTextChange is the didChange payload for full-sync servers: a single
// TextDocumentContentChangeEvent carrying the whole document.
func fullTextChange(text string) []map[string]any {
	return []map[string]any{{"text": text}}
}

// incrementalChange builds a single ranged TextDocumentContentChangeEvent that
// turns oldText into newText. It returns ok=false when the change can't be
// expressed safely as a range, in which case the caller sends the full text.
//
// Safety rules: positions are computed on '\n'-split lines (the same line
// model fromLSP uses, so the server and client agree on what a line is),
// change boundaries are backed up to UTF-8 rune edges so no character is
// split, and any '\r' in either text forces a full-text fallback — servers
// disagree on whether '\r' belongs to the line, and a one-off position there
// would corrupt the server's copy of the document.
func incrementalChange(c *lspClient, oldText, newText string) ([]map[string]any, bool) {
	if strings.IndexByte(oldText, '\r') >= 0 || strings.IndexByte(newText, '\r') >= 0 {
		return nil, false
	}
	start, end, repl, ok := diffRange(oldText, newText)
	if !ok {
		return nil, false
	}
	return []map[string]any{{
		"range": map[string]any{
			"start": c.offsetToPosition(oldText, start),
			"end":   c.offsetToPosition(oldText, end),
		},
		"text": repl,
	}}, true
}

// diffRange finds the smallest single byte range [start, end) in oldText that,
// replaced by repl, yields newText. Boundaries are on UTF-8 rune edges so the
// position conversion can't split a character. ok=false when the texts are
// identical (callers skip those before calling).
func diffRange(oldText, newText string) (start, end int, repl string, ok bool) {
	// Common prefix, in bytes.
	p := 0
	for p < len(oldText) && p < len(newText) && oldText[p] == newText[p] {
		p++
	}
	// Back up to a rune boundary: a shared partial rune must be replaced
	// whole on both sides, never split down the middle.
	for p > 0 && p < len(oldText) && !utf8.RuneStart(oldText[p]) {
		p--
	}
	// Common suffix, in bytes, not overlapping the prefix.
	s := 0
	for s < len(oldText)-p && s < len(newText)-p &&
		oldText[len(oldText)-1-s] == newText[len(newText)-1-s] {
		s++
	}
	// Same backup for the suffix cut: shrink the shared suffix until the cut
	// sits on a rune boundary (or vanishes).
	for s > 0 && len(oldText)-s > p && !utf8.RuneStart(oldText[len(oldText)-s]) {
		s--
	}
	if p == len(oldText) && p == len(newText) {
		return 0, 0, "", false
	}
	return p, len(oldText) - s, newText[p : len(newText)-s], true
}

// offsetToPosition converts a byte offset in text to an lspPosition in the
// negotiated encoding. The caller guarantees no '\r' in text and a
// rune-boundary offset (both ensured by incrementalChange).
func (c *lspClient) offsetToPosition(text string, off int) lspPosition {
	line := 1
	lineStart := 0
	for i := 0; i < off; i++ {
		if text[i] == '\n' {
			line++
			lineStart = i + 1
		}
	}
	lineEnd := len(text)
	if i := strings.IndexByte(text[lineStart:], '\n'); i >= 0 {
		lineEnd = lineStart + i
	}
	return c.toLSP(text[lineStart:lineEnd], line, off-lineStart)
}

// closeDoc notifies the server that the file was closed, allowing the server
// to free ASTs and file memory.
func (c *lspClient) closeDoc(abs string) {
	c.docMu.Lock()
	defer c.docMu.Unlock()
	c.closeDocLocked(abs)
}

// closeDocLocked is the docMu-held half of closeDoc; syncDoc's error path uses
// it directly to avoid re-locking.
func (c *lspClient) closeDocLocked(abs string) {
	uri := pathToURI(abs)
	c.mu.Lock()
	_, already := c.opened[uri]
	if already {
		delete(c.opened, uri)
		delete(c.openedText, uri)
		delete(c.syncGen, uri)
	}
	c.mu.Unlock()
	if !already {
		return
	}

	c.notify("textDocument/didClose", map[string]any{
		"textDocument": map[string]any{
			"uri": uri,
		},
	})
}

// ---------------------------------------------------------------- positions

// toLSP converts a 1-based line and 0-based byte column into the offsets the
// server expects. The spec counts UTF-16 code units by default, which is not
// what Go gives us.
func (c *lspClient) toLSP(lineText string, line, byteCol int) lspPosition {
	if byteCol < 0 {
		byteCol = 0
	}
	if byteCol > len(lineText) {
		byteCol = len(lineText)
	}
	prefix := lineText[:byteCol]
	var ch int
	switch c.encoding {
	case "utf-8":
		ch = byteCol
	case "utf-32":
		ch = utf8.RuneCountInString(prefix)
	default:
		// Count UTF-16 code units without allocating: one unit per rune,
		// plus one more for astral runes (surrogate pairs). Ranging over the
		// string yields the same runes []rune(prefix) would — including
		// U+FFFD per invalid byte — so this matches utf16.Encode exactly.
		for _, r := range prefix {
			if r >= 0x10000 {
				ch += 2
			} else {
				ch++
			}
		}
	}
	return lspPosition{Line: line - 1, Character: ch}
}

// fromLSP converts a server position back into a 1-based line and 0-based byte
// column against the file we have on disk.
func (c *lspClient) fromLSP(lines []string, p lspPosition) (int, int) {
	line := p.Line + 1
	if p.Line < 0 || p.Line >= len(lines) {
		return line, 0
	}
	return c.fromLSPLine(lines[p.Line], p)
}

func (c *lspClient) fromLSPLine(text string, p lspPosition) (int, int) {
	line := p.Line + 1
	if p.Character <= 0 {
		return line, 0
	}
	switch c.encoding {
	case "utf-8":
		if p.Character > len(text) {
			return line, len(text)
		}
		return line, p.Character
	case "utf-32":
		// Sum rune widths directly instead of materializing []rune and
		// re-encoding the prefix to a throwaway string.
		n, bytes := 0, 0
		for _, r := range text {
			if n >= p.Character {
				break
			}
			n++
			bytes += utf8.RuneLen(r)
		}
		return line, bytes
	default:
		return line, utf16ToByte(text, p.Character)
	}
}

// ---------------------------------------------------------------- uris

func pathToURI(p string) string {
	p = filepath.ToSlash(p)
	if runtime.GOOS == "windows" {
		if !strings.HasPrefix(p, "/") {
			p = "/" + p
		}
	}
	u := url.URL{Scheme: "file", Path: p}
	return u.String()
}

func uriToPath(uri string) (string, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return "", err
	}
	if u.Scheme != "file" && u.Scheme != "" {
		return "", fmt.Errorf("not a file uri: %s", uri)
	}
	p := u.Path
	if runtime.GOOS == "windows" {
		p = strings.TrimPrefix(p, "/")
	}
	return filepath.FromSlash(p), nil
}

// ---------------------------------------------------------------- navigation

// NavHit is one navigation result, shaped exactly like a search Match so the
// UI renders LSP answers and regex answers with the same code.
type NavHit struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Pre  string `json:"pre"`
	Mid  string `json:"mid"`
	Post string `json:"post"`
	Def  bool   `json:"def,omitempty"`
	Ext  bool   `json:"ext,omitempty"` // outside the indexed tree, e.g. stdlib
}

// utf16ToByte converts a column measured in UTF-16 code units (what JavaScript
// string indexes count) into a byte offset into the same line.
func utf16ToByte(line string, u16 int) int {
	if u16 <= 0 {
		return 0
	}
	units, bytes := 0, 0
	for _, r := range line {
		if units >= u16 {
			break
		}
		if r >= 0x10000 {
			units += 2
		} else {
			units++
		}
		bytes += utf8.RuneLen(r)
	}
	if bytes > len(line) {
		return len(line)
	}
	return bytes
}

// resolve turns a set of LSP ranges into display-ready hits, reading each target
// file through the same cache the editor uses.
func (m *lspManager) resolve(c *lspClient, locs []lspLocation, markDef bool) []NavHit {
	type fileKey struct {
		abs, rel string
		ext      bool
	}
	byFile := map[fileKey][]lspRange{}
	var order []fileKey

	for _, l := range locs {
		abs, err := uriToPath(l.URI)
		if err != nil {
			continue
		}
		// Outside the indexed tree is still worth jumping to, so treePath
		// allows this exact file to be opened even though the tree guard
		// would refuse it.
		rel, ext := m.treePath(abs, true)
		k := fileKey{abs, rel, ext}
		if _, seen := byFile[k]; !seen {
			order = append(order, k)
		}
		byFile[k] = append(byFile[k], l.Range)
	}

	var out []NavHit
	for _, k := range order {
		d, err := render.Open(k.abs, k.rel)
		ranges := byFile[k]
		sort.Slice(ranges, func(i, j int) bool {
			if ranges[i].Start.Line != ranges[j].Start.Line {
				return ranges[i].Start.Line < ranges[j].Start.Line
			}
			return ranges[i].Start.Character < ranges[j].Start.Character
		})
		seen := map[int]bool{}
		for _, r := range ranges {
			line := r.Start.Line + 1
			if seen[line] {
				continue // one entry per line keeps the list readable
			}
			seen[line] = true

			text := ""
			from := 0
			if err == nil && d != nil && r.Start.Line >= 0 && r.Start.Line < d.Total {
				text = d.Raw(line)
				_, from = c.fromLSPLine(text, r.Start)
			}
			to := from
			if r.End.Line == r.Start.Line && err == nil && d != nil && r.End.Line >= 0 && r.End.Line < d.Total {
				_, to = c.fromLSPLine(text, r.End)
			}
			if to <= from || to > len(text) {
				to = len(text)
				if from > to {
					from = to
				}
			}
			h := snip([]byte(text), from, to)
			out = append(out, NavHit{
				Path: k.rel, Line: line, Pre: h.Pre, Mid: h.Mid, Post: h.Post,
				Def: markDef, Ext: k.ext,
			})
		}
	}
	return out
}

type lspLocationOrLink struct {
	URI                  string    `json:"uri"`
	Range                lspRange  `json:"range"`
	TargetURI            string    `json:"targetUri"`
	TargetRange          *lspRange `json:"targetRange"`
	TargetSelectionRange *lspRange `json:"targetSelectionRange"`
}

func (l lspLocationOrLink) toLocation() (lspLocation, bool) {
	if l.URI != "" {
		return lspLocation{URI: l.URI, Range: l.Range}, true
	}
	if l.TargetURI != "" {
		if l.TargetSelectionRange != nil {
			return lspLocation{URI: l.TargetURI, Range: *l.TargetSelectionRange}, true
		}
		if l.TargetRange != nil {
			return lspLocation{URI: l.TargetURI, Range: *l.TargetRange}, true
		}
		return lspLocation{URI: l.TargetURI}, true
	}
	return lspLocation{}, false
}

// locate runs one position-based request and normalises the two shapes a server
// may answer with (Location or LocationLink).
func (m *lspManager) locate(ctx context.Context, method, abs, rel string, line, u16col int, extra map[string]any) ([]NavHit, error) {
	c, err := m.client(ctx, rel)
	if err != nil {
		return nil, err
	}
	if err := c.ensureOpen(abs, rel); err != nil {
		return nil, err
	}

	d, err := render.Open(abs, rel)
	if err != nil {
		return nil, err
	}
	rawLine := d.Raw(line)
	byteCol := utf16ToByte(rawLine, u16col)

	params := map[string]any{
		"textDocument": map[string]any{"uri": pathToURI(abs)},
		"position":     c.toLSP(rawLine, line, byteCol),
	}
	for k, v := range extra {
		params[k] = v
	}

	var rawMsg json.RawMessage
	if err := c.call(ctx, method, params, &rawMsg); err != nil {
		return nil, err
	}
	trimmed := bytes.TrimSpace(rawMsg)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return []NavHit{}, nil
	}
	if trimmed[0] == '{' {
		var one lspLocationOrLink
		if err := json.Unmarshal(trimmed, &one); err != nil {
			return nil, err
		}
		if loc, ok := one.toLocation(); ok {
			return m.resolve(c, []lspLocation{loc}, method != "textDocument/references"), nil
		}
		return []NavHit{}, nil
	}

	var items []lspLocationOrLink
	if err := json.Unmarshal(trimmed, &items); err != nil {
		return nil, err
	}
	locs := make([]lspLocation, 0, len(items))
	for _, item := range items {
		if loc, ok := item.toLocation(); ok {
			locs = append(locs, loc)
		}
	}
	return m.resolve(c, locs, method != "textDocument/references"), nil
}

// Definition queries the language server for the definition site(s) of the symbol at the given position.
// Line is 1-based and col is the UTF-16 code unit offset from the frontend.
func (m *lspManager) Definition(ctx context.Context, abs, rel string, line, col int) ([]NavHit, error) {
	return m.locate(ctx, "textDocument/definition", abs, rel, line, col, nil)
}

// References queries the language server for all reference locations of the symbol at the given position,
// including its declaration site.
func (m *lspManager) References(ctx context.Context, abs, rel string, line, col int) ([]NavHit, error) {
	return m.locate(ctx, "textDocument/references", abs, rel, line, col,
		map[string]any{"context": map[string]any{"includeDeclaration": true}})
}

// Symbols returns the document outline, flattened with indentation that mirrors
// the server's nesting.
func (m *lspManager) Symbols(ctx context.Context, abs, rel string) ([]symbols.Symbol, error) {
	c, err := m.client(ctx, rel)
	if err != nil {
		return nil, err
	}
	if err := c.ensureOpen(abs, rel); err != nil {
		return nil, err
	}

	var raw []lspDocumentSymbol
	if err := c.call(ctx, "textDocument/documentSymbol", map[string]any{
		"textDocument": map[string]any{"uri": pathToURI(abs)},
	}, &raw); err != nil {
		return nil, err
	}

	var out []symbols.Symbol
	var walk func(syms []lspDocumentSymbol, depth int)
	walk = func(syms []lspDocumentSymbol, depth int) {
		for _, s := range syms {
			rng := s.SelectionRange
			if s.Location != nil {
				rng = s.Location.Range // symbolInformation form
			}
			kind := lspSymbolKind[s.Kind]
			if kind == "" {
				kind = "sym"
			}
			out = append(out, symbols.Symbol{
				Name: s.Name, Kind: kind, Line: rng.Start.Line + 1, Indent: depth * 2,
			})
			if len(s.Children) > 0 {
				walk(s.Children, depth+1)
			}
		}
	}
	walk(raw, 0)

	// symbolInformation comes back unordered often enough to be worth fixing.
	sort.SliceStable(out, func(i, j int) bool { return out[i].Line < out[j].Line })
	return out, nil
}

// ---------------------------------------------------------------- hover

// HoverInfo is what the UI shows in the hover card: a syntax-highlighted
// signature and whatever documentation the server attached to it.
type HoverInfo struct {
	Signature string `json:"signature"` // pre-highlighted HTML, one line per entry
	Doc       string `json:"doc"`
	Empty     bool   `json:"empty"`
}

// Hover asks the server what a position is, and renders the answer.
func (m *lspManager) Hover(ctx context.Context, abs, rel string, line, col int) (*HoverInfo, error) {
	c, err := m.client(ctx, rel)
	if err != nil {
		return nil, err
	}
	if err := c.ensureOpen(abs, rel); err != nil {
		return nil, err
	}
	d, err := render.Open(abs, rel)
	if err != nil {
		return nil, err
	}
	byteCol := utf16ToByte(d.Raw(line), col)

	var res struct {
		Contents json.RawMessage `json:"contents"`
	}
	err = c.call(ctx, "textDocument/hover", map[string]any{
		"textDocument": map[string]any{"uri": pathToURI(abs)},
		"position":     c.toLSP(d.Raw(line), line, byteCol),
	}, &res)
	if err != nil {
		return nil, err
	}
	if len(res.Contents) == 0 {
		return &HoverInfo{Empty: true}, nil
	}

	code, prose := parseHoverContents(res.Contents)
	if code == "" && prose == "" {
		return &HoverInfo{Empty: true}, nil
	}
	return &HoverInfo{Signature: highlightSnippet(code, rel), Doc: prose}, nil
}

// parseHoverContents flattens the three shapes the spec allows: a MarkupContent
// object, a MarkedString, or an array of MarkedStrings.
func parseHoverContents(raw json.RawMessage) (code, prose string) {
	var markup struct {
		Kind  string `json:"kind"`
		Value string `json:"value"`
	}
	if err := json.Unmarshal(raw, &markup); err == nil && markup.Value != "" {
		return splitMarkdown(markup.Value)
	}

	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		return splitMarkdown(one)
	}

	var many []json.RawMessage
	if err := json.Unmarshal(raw, &many); err == nil {
		var codes, proses []string
		for _, item := range many {
			var s string
			if json.Unmarshal(item, &s) == nil {
				c, p := splitMarkdown(s)
				if c != "" {
					codes = append(codes, c)
				}
				if p != "" {
					proses = append(proses, p)
				}
				continue
			}
			var ms struct {
				Language string `json:"language"`
				Value    string `json:"value"`
			}
			if json.Unmarshal(item, &ms) == nil && ms.Value != "" {
				if ms.Language != "" {
					codes = append(codes, ms.Value)
				} else {
					proses = append(proses, ms.Value)
				}
			}
		}
		return strings.Join(codes, "\n"), strings.Join(proses, "\n\n")
	}
	return "", ""
}

// splitMarkdown pulls fenced code blocks out of a hover body; what is left is
// documentation prose.
func splitMarkdown(s string) (code, prose string) {
	var codes, text []string
	inFence := false
	var fence []string
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			if inFence {
				codes = append(codes, strings.Join(fence, "\n"))
				fence = nil
			}
			inFence = !inFence
			continue
		}
		if inFence {
			fence = append(fence, line)
		} else {
			text = append(text, line)
		}
	}
	if len(fence) > 0 {
		codes = append(codes, strings.Join(fence, "\n"))
	}
	code = strings.TrimSpace(strings.Join(codes, "\n"))
	prose = strings.TrimSpace(stripMarkdown(strings.Join(text, "\n")))

	// A server that only speaks plaintext sends no fences at all. Its first
	// line is still the declaration, so promote it rather than showing nothing.
	if code == "" && prose != "" {
		first, rest, _ := strings.Cut(prose, "\n")
		if looksLikeDeclaration(first) {
			code, prose = first, strings.TrimSpace(rest)
		}
	}
	const maxProse = 900
	if len(prose) > maxProse {
		if cut := strings.LastIndex(prose[:maxProse], " "); cut > 0 {
			prose = prose[:cut]
		} else {
			prose = prose[:maxProse]
		}
		prose += "…"
	}
	return code, prose
}

// looksLikeDeclaration is a deliberately conservative test: short, no trailing
// sentence punctuation, and carrying syntax a prose line would not.
func looksLikeDeclaration(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 200 || strings.HasSuffix(s, ".") {
		return false
	}
	return strings.ContainsAny(s, "(){}[]<>:=*&") || strings.Contains(s, " ")
}

var (
	mdInline = strings.NewReplacer("**", "", "__", "", "`", "")
	mdLink   = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	// No backreference: Go's regexp is RE2, so the three rule forms are spelled out.
	mdRule     = regexp.MustCompile(`^\s*(?:(?:-\s*){3,}|(?:\*\s*){3,}|(?:_\s*){3,})$`)
	mdOnlyLink = regexp.MustCompile(`^\s*\[[^\]]*\]\([^)]*\)\s*$`)
)

// stripMarkdown flattens a hover body into something readable in a tooltip.
// Nothing here tries to be a markdown renderer; it removes the decorations that
// would otherwise show up as literal punctuation.
func stripMarkdown(s string) string {
	var out []string
	blank := 0
	for _, line := range strings.Split(s, "\n") {
		if mdRule.MatchString(line) || mdOnlyLink.MatchString(line) {
			continue // horizontal rules and bare "see also" links are noise
		}
		line = strings.TrimRight(mdInline.Replace(mdLink.ReplaceAllString(line, "$1")), " ")
		if strings.TrimSpace(line) == "" {
			blank++
			if blank > 1 {
				continue
			}
		} else {
			blank = 0
		}
		out = append(out, line)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

const maxSignatureLines = 12

// highlightSnippet runs a hover signature through the same lexer the file uses,
// so the card matches the editor.
func highlightSnippet(code, rel string) string {
	if code == "" {
		return ""
	}
	lines := strings.Split(code, "\n")
	if len(lines) > maxSignatureLines {
		lines = append(lines[:maxSignatureLines], "…")
		code = strings.Join(lines, "\n")
	}
	d := render.NewDoc(code, rel)
	out, _ := d.Lines(0, d.Total)
	return strings.Join(out, "\n")
}

// ---------------------------------------------------------------- call trails

// CallNode is one function in a call trail.
type CallNode struct {
	Name     string `json:"name"`
	Detail   string `json:"detail,omitempty"`
	Kind     string `json:"kind"`
	Path     string `json:"path"` // where the function is declared
	Line     int    `json:"line"`
	Ext      bool   `json:"ext,omitempty"`
	SitePath string `json:"sitePath,omitempty"` // file holding the call expressions
	Sites    []int  `json:"sites,omitempty"`    // lines of those calls, ascending
	Item     string `json:"item"`               // CallHierarchyItem, returned as-is to expand
}

// lspCallItem represents the LSP wire format for CallHierarchyItem
// defined in the Language Server Protocol specification.
type lspCallItem struct {
	Name           string   `json:"name"`
	Kind           int      `json:"kind"`
	Detail         string   `json:"detail"`
	URI            string   `json:"uri"`
	SelectionRange lspRange `json:"selectionRange"`
}

// treePath maps an absolute path to what the UI shows and opens: relative
// inside the indexed tree, absolute outside it. Only paths a language server
// named may be allowlisted for opening; paths that arrived from the browser
// must pass allow=false.
func (m *lspManager) treePath(abs string, allow bool) (rel string, ext bool) {
	rel, err := filepath.Rel(m.root, abs)
	if err != nil || strings.HasPrefix(rel, "..") {
		// The standard library, or a dependency in the module cache.
		if allow {
			m.allow(abs)
		}
		return filepath.ToSlash(abs), true
	}
	return filepath.ToSlash(rel), false
}

func (m *lspManager) callNode(raw json.RawMessage, allow bool) (CallNode, lspCallItem, error) {
	var it lspCallItem
	if err := json.Unmarshal(raw, &it); err != nil || it.URI == "" {
		return CallNode{}, it, fmt.Errorf("bad call hierarchy item")
	}
	abs, err := uriToPath(it.URI)
	if err != nil {
		return CallNode{}, it, err
	}
	rel, ext := m.treePath(abs, allow)
	kind := lspSymbolKind[it.Kind]
	if kind == "" {
		kind = "sym"
	}
	return CallNode{
		Name: it.Name, Detail: it.Detail, Kind: kind,
		Path: rel, Line: it.SelectionRange.Start.Line + 1, Ext: ext,
		Item: string(raw),
	}, it, nil
}

// PrepareCalls resolves the function at a position into the root(s) of a trail.
func (m *lspManager) PrepareCalls(ctx context.Context, abs, rel string, line, u16col int) ([]CallNode, error) {
	c, err := m.client(ctx, rel)
	if err != nil {
		return nil, err
	}
	if err := c.ensureOpen(abs, rel); err != nil {
		return nil, err
	}
	d, err := render.Open(abs, rel)
	if err != nil {
		return nil, err
	}
	byteCol := utf16ToByte(d.Raw(line), u16col)

	var raw []json.RawMessage
	if err := c.call(ctx, "textDocument/prepareCallHierarchy", map[string]any{
		"textDocument": map[string]any{"uri": pathToURI(abs)},
		"position":     c.toLSP(d.Raw(line), line, byteCol),
	}, &raw); err != nil {
		return nil, err
	}
	out := make([]CallNode, 0, len(raw))
	for _, r := range raw {
		if n, _, err := m.callNode(r, true); err == nil {
			out = append(out, n)
		}
	}
	return out, nil
}

// Calls expands one node of a trail: its callers, or with outgoing its callees.
// rel picks the language server; item is a node's Item as the browser sent it.
func (m *lspManager) Calls(ctx context.Context, rel, item string, outgoing bool) ([]CallNode, error) {
	c, err := m.client(ctx, rel)
	if err != nil {
		return nil, err
	}
	_, parent, err := m.callNode(json.RawMessage(item), false)
	if err != nil {
		return nil, err
	}
	// Some servers only answer about documents they were handed. Only files in
	// the tree are read for this: the item came from the browser.
	if pabs, err := uriToPath(parent.URI); err == nil {
		if prel, ext := m.treePath(pabs, false); !ext {
			c.ensureOpen(pabs, prel)
		}
	}

	method := "callHierarchy/incomingCalls"
	if outgoing {
		method = "callHierarchy/outgoingCalls"
	}
	var res []struct {
		From       json.RawMessage `json:"from"`
		To         json.RawMessage `json:"to"`
		FromRanges []lspRange      `json:"fromRanges"`
	}
	if err := c.call(ctx, method, map[string]any{"item": json.RawMessage(item)}, &res); err != nil {
		return nil, err
	}

	out := make([]CallNode, 0, len(res))
	for _, r := range res {
		raw := r.From
		if outgoing {
			raw = r.To
		}
		n, it, err := m.callNode(raw, true)
		if err != nil {
			continue
		}
		// fromRanges sit inside the caller: the returned function for incoming
		// calls, the function being expanded for outgoing ones.
		siteURI := it.URI
		if outgoing {
			siteURI = parent.URI
		}
		if sabs, err := uriToPath(siteURI); err == nil {
			n.SitePath, _ = m.treePath(sabs, false)
		}
		n.Sites = siteLines(r.FromRanges)
		out = append(out, n)
	}

	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if outgoing {
			return firstSite(a) < firstSite(b) // callees in the order they are called
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		return a.Line < b.Line
	})
	return out, nil
}

func siteLines(rs []lspRange) []int {
	seen := map[int]bool{}
	var out []int
	for _, r := range rs {
		l := r.Start.Line + 1
		if !seen[l] {
			seen[l] = true
			out = append(out, l)
		}
	}
	sort.Ints(out)
	return out
}

func firstSite(n CallNode) int {
	if len(n.Sites) == 0 {
		return 0
	}
	return n.Sites[0]
}

// ---------------------------------------------------------------- servers & manager

// lspServerDef describes one language server we know how to drive. Nothing here
// is required for px0 to work; a server is used only if its binary is found on
// PATH or in one of the folders installers commonly use (lspBinDirs).
type lspServerDef struct {
	Name        string
	Lang        string // language name, shown when offering to install the server
	Cmd         []string
	Exts        []string          // file extensions this server handles
	LangIDs     map[string]string // ext -> LSP languageId, when it differs
	DefaultLang string
	InitOptions map[string]any
	Install     []lspInstall // ways to get the binary, best first
}

func (d lspServerDef) LanguageID(rel string) string {
	ext := strings.ToLower(filepath.Ext(rel))
	if id, ok := d.LangIDs[ext]; ok {
		return id
	}
	return d.DefaultLang
}

// lspRegistry is ordered: the first entry whose binary exists wins for a given
// extension, so a more capable server listed earlier takes precedence.
var lspRegistry = []lspServerDef{
	{
		Name: "gopls", Lang: "Go", Cmd: []string{"gopls"},
		Exts: []string{".go"}, DefaultLang: "go",
		Install: []lspInstall{
			{Cmd: []string{"go", "install", "golang.org/x/tools/gopls@latest"}, Auto: true},
			{OS: "darwin", Cmd: []string{"brew", "install", "gopls"}, Auto: true},
		},
	},
	{
		Name: "rust-analyzer", Lang: "Rust", Cmd: []string{"rust-analyzer"},
		Exts: []string{".rs"}, DefaultLang: "rust",
		Install: []lspInstall{
			{Cmd: []string{"rustup", "component", "add", "rust-analyzer"}, Auto: true},
			{OS: "darwin", Cmd: []string{"brew", "install", "rust-analyzer"}, Auto: true},
		},
	},
	{
		Name: "pyright", Lang: "Python", Cmd: []string{"pyright-langserver", "--stdio"},
		Exts: []string{".py", ".pyi"}, DefaultLang: "python",
		Install: []lspInstall{
			{Cmd: []string{"npm", "install", "-g", "pyright"}, Auto: true},
			{OS: "darwin", Cmd: []string{"brew", "install", "pyright"}, Auto: true},
		},
	},
	{
		Name: "pylsp", Lang: "Python", Cmd: []string{"pylsp"},
		Exts: []string{".py", ".pyi"}, DefaultLang: "python",
		Install: []lspInstall{
			{Cmd: []string{"pipx", "install", "python-lsp-server"}, Auto: true},
			{OS: "darwin", Cmd: []string{"brew", "install", "python-lsp-server"}, Auto: true},
		},
	},
	{
		// Not offered for install: it lints, but answers no call hierarchy.
		Name: "ruff", Lang: "Python", Cmd: []string{"ruff", "server"},
		Exts: []string{".py"}, DefaultLang: "python",
	},
	{
		Name: "typescript", Lang: "TypeScript and JavaScript", Cmd: []string{"typescript-language-server", "--stdio"},
		Exts:        []string{".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs"},
		LangIDs:     map[string]string{".ts": "typescript", ".tsx": "typescriptreact", ".jsx": "javascriptreact"},
		DefaultLang: "javascript",
		Install: []lspInstall{
			{Cmd: []string{"npm", "install", "-g", "typescript-language-server", "typescript"}, Auto: true},
			{OS: "darwin", Cmd: []string{"brew", "install", "typescript-language-server"}, Auto: true},
		},
	},
	{
		Name: "clangd", Lang: "C and C++", Cmd: []string{"clangd", "--background-index"},
		Exts:        []string{".c", ".h", ".cc", ".cpp", ".cxx", ".hpp", ".hh", ".m", ".mm"},
		LangIDs:     map[string]string{".c": "c", ".h": "c"},
		DefaultLang: "cpp",
		Install: []lspInstall{
			{OS: "darwin", Cmd: []string{"brew", "install", "llvm"}, Auto: true},
			// System package managers want an administrator: shown, not run.
			{OS: "linux", Cmd: []string{"sudo", "apt", "install", "clangd"}},
			{OS: "windows", Cmd: []string{"winget", "install", "LLVM.LLVM"}},
		},
	},
	{
		Name: "zls", Lang: "Zig", Cmd: []string{"zls"},
		Exts: []string{".zig"}, DefaultLang: "zig",
		Install: []lspInstall{
			{OS: "darwin", Cmd: []string{"brew", "install", "zls"}, Auto: true},
		},
	},
	{
		Name: "lua", Lang: "Lua", Cmd: []string{"lua-language-server"},
		Exts: []string{".lua"}, DefaultLang: "lua",
		Install: []lspInstall{
			{OS: "darwin", Cmd: []string{"brew", "install", "lua-language-server"}, Auto: true},
		},
	},
	{
		Name: "solargraph", Lang: "Ruby", Cmd: []string{"solargraph", "stdio"},
		Exts: []string{".rb"}, DefaultLang: "ruby",
		Install: []lspInstall{
			{Cmd: []string{"gem", "install", "solargraph"}, Auto: true},
			{OS: "darwin", Cmd: []string{"brew", "install", "solargraph"}, Auto: true},
		},
	},
	{
		Name: "jdtls", Lang: "Java", Cmd: []string{"jdtls"},
		Exts: []string{".java"}, DefaultLang: "java",
		Install: []lspInstall{
			{OS: "darwin", Cmd: []string{"brew", "install", "jdtls"}, Auto: true},
		},
	},
	{
		Name: "omnisharp", Lang: "C#", Cmd: []string{"omnisharp", "-lsp"},
		Exts: []string{".cs"}, DefaultLang: "csharp",
	},
	{
		Name: "texlab", Lang: "LaTeX", Cmd: []string{"texlab"},
		Exts: []string{".tex"}, DefaultLang: "latex",
		Install: []lspInstall{
			{OS: "darwin", Cmd: []string{"brew", "install", "texlab"}, Auto: true},
		},
	},
}

type lspState string

const (
	lspOff      lspState = "off"      // disabled, or no server installed for this type
	lspStarting lspState = "starting" // process spawned, handshake in flight
	lspIndexing lspState = "indexing" // up, but still chewing through the workspace
	lspReady    lspState = "ready"
	lspFailed   lspState = "failed"
)

// maxLSPRestarts bounds how often one crashed language server is respawned.
const maxLSPRestarts = 3

// lspManager coordinates background language servers across file types and extensions.
// It manages on-demand server lazy starting, discovery, restart on crash, installation jobs,
// and path allowlisting for external references (such as standard library files).
type lspManager struct {
	root    string
	enabled bool

	// mu guards the discovery/registry tables below. It is an RWMutex: the
	// read-heavy paths (defFor, State, Available, ServerStatus) take the read
	// lock, and only spawn/discover/Stop mutate. No I/O happens under it.
	mu        sync.RWMutex
	byExt     map[string]*lspServerDef // resolved by discover(), again on Rescan
	clients   map[string]*lspClient    // server name -> client
	starting  map[string]chan struct{}
	failed    map[string]string
	available []string
	restarts  map[string]int // crashes recovered from, per server name
	stopped   map[string]bool

	discovered bool // the first discover() has finished

	jobMu sync.Mutex
	jobs  map[string]*lspJob // install runs, by server name

	// external holds absolute paths outside the indexed tree that a language
	// server pointed us at. Only these are openable beyond the root, so a
	// jump into the standard library works without opening up the filesystem.
	extMu    sync.RWMutex
	external map[string]bool
}

func (m *lspManager) allow(abs string) {
	m.extMu.Lock()
	if m.external == nil {
		m.external = map[string]bool{}
	}
	if len(m.external) < 20000 {
		m.external[abs] = true
	}
	m.extMu.Unlock()
}

// Allowed reports whether a language server has named this exact file.
func (m *lspManager) Allowed(abs string) bool {
	m.extMu.RLock()
	defer m.extMu.RUnlock()
	return m.external[abs]
}

// newLSPManager creates a new language server manager for root.
// If enabled is true, server discovery begins in the background without blocking startup.
func newLSPManager(root string, enabled bool) *lspManager {
	m := &lspManager{
		root: root, enabled: enabled,
		byExt:    map[string]*lspServerDef{},
		clients:  map[string]*lspClient{},
		starting: map[string]chan struct{}{},
		failed:   map[string]string{},
		stopped:  map[string]bool{},
	}
	if !enabled {
		return m
	}
	// Discover available language servers in background so server startup is instantaneous.
	go m.discover()
	return m
}

// discover resolves which registry servers are installed. It can run again
// (Rescan) after something is installed; the result replaces the previous one
// in a single swap, so readers never see a half-built table.
func (m *lspManager) discover() {
	// Fresh eyes: an install or rescan may have changed what's on disk, so
	// drop memoized binary resolutions before re-resolving.
	invalidateBinaryCache()
	byExt := map[string]*lspServerDef{}
	var available []string
	for i := range lspRegistry {
		def := lspRegistry[i] // a copy: Cmd[0] becomes the resolved path
		bin, ok := cachedLookPath(def.Cmd[0])
		if !ok {
			continue
		}
		def.Cmd = append([]string{bin}, def.Cmd[1:]...)
		claimed := false
		for _, ext := range def.Exts {
			if byExt[ext] == nil {
				byExt[ext] = &def
				claimed = true
			}
		}
		if claimed {
			available = append(available, def.Name)
		}
	}
	m.mu.Lock()
	m.byExt, m.available, m.discovered = byExt, available, true
	m.mu.Unlock()
}

func (m *lspManager) isDiscovered() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.discovered
}

// lspBinDirs lists folders installers put binaries in that are often missing
// from PATH, so a server installed from px0, or by hand after px0 started, is
// found without restarting the shell px0 was launched from.
func lspBinDirs() []string {
	var dirs []string
	add := func(elem ...string) { dirs = append(dirs, filepath.Join(elem...)) }
	if v := os.Getenv("GOBIN"); v != "" {
		add(v)
	}
	for _, p := range filepath.SplitList(os.Getenv("GOPATH")) {
		if p != "" {
			add(p, "bin")
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		add(home, "go", "bin")     // go install, default GOPATH
		add(home, ".cargo", "bin") // rustup, cargo install
		add(home, ".local", "bin") // pipx
		add(home, ".opencode", "bin")
		add(home, ".codex", "bin")
	}
	// npm puts global packages beside its own executable unless the prefix was moved.
	if npm, err := exec.LookPath("npm"); err == nil {
		add(filepath.Dir(npm))
	}
	switch runtime.GOOS {
	case "darwin":
		add("/opt/homebrew/bin")
		add("/usr/local/bin")
		// Homebrew's llvm is keg-only: clangd is installed but never linked.
		add("/opt/homebrew/opt/llvm/bin")
		add("/usr/local/opt/llvm/bin")
	case "windows":
		if v := os.Getenv("APPDATA"); v != "" {
			add(v, "npm")
		}
		if v := os.Getenv("ProgramFiles"); v != "" {
			add(v, "LLVM", "bin")
		}
	}
	return dirs
}

const binDirCacheTTL = 30 * time.Second

type binResolve struct {
	path string
	ok   bool
}

var binDirCache struct {
	sync.RWMutex
	dirs []string
	bins map[string]binResolve // binary name -> resolution, against dirs
	at   time.Time
}

func cachedLookPath(name string) (string, bool) {
	binDirCache.RLock()
	if time.Since(binDirCache.at) < binDirCacheTTL && binDirCache.dirs != nil {
		if r, ok := binDirCache.bins[name]; ok {
			binDirCache.RUnlock()
			return r.path, r.ok
		}
	}
	binDirCache.RUnlock()

	binDirCache.Lock()
	defer binDirCache.Unlock()
	if time.Since(binDirCache.at) >= binDirCacheTTL || binDirCache.dirs == nil {
		binDirCache.dirs = lspBinDirs()
		binDirCache.bins = map[string]binResolve{}
		binDirCache.at = time.Now()
	}
	if r, ok := binDirCache.bins[name]; ok {
		return r.path, r.ok
	}
	p, ok := lookPathIn(name, binDirCache.dirs)
	binDirCache.bins[name] = binResolve{path: p, ok: ok}
	return p, ok
}

func invalidateBinaryCache() {
	binDirCache.Lock()
	binDirCache.dirs = nil
	binDirCache.bins = nil
	binDirCache.at = time.Time{}
	binDirCache.Unlock()
}

func lookPathIn(name string, dirs []string) (string, bool) {
	if p, err := exec.LookPath(name); err == nil {
		return p, true
	}
	for _, d := range dirs {
		if p, err := exec.LookPath(filepath.Join(d, name)); err == nil {
			return p, true
		}
	}
	return "", false
}

func (m *lspManager) Available() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.available == nil {
		return []string{}
	}
	cp := make([]string, len(m.available))
	copy(cp, m.available)
	return cp
}

func (m *lspManager) Enabled() bool { return m.enabled }

func (m *lspManager) memBytes() uint64 {
	m.mu.RLock()
	pids := make([]int, 0, len(m.clients))
	for _, c := range m.clients {
		if c.cmd != nil && c.cmd.Process != nil && c.alive() == nil {
			pids = append(pids, c.cmd.Process.Pid)
		}
	}
	m.mu.RUnlock()

	var total uint64
	for _, pid := range pids {
		total += metrics.ReadRSSForPID(pid)
	}
	return total
}

func (m *lspManager) defFor(rel string) *lspServerDef {
	if !m.enabled {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.byExt[strings.ToLower(filepath.Ext(rel))]
}

func (m *lspManager) State(rel string) (lspState, string) {
	ext := strings.ToLower(filepath.Ext(rel))
	m.mu.RLock()
	stoppedName := ""
	if m.stopped != nil {
		for _, reg := range registryFor(rel) {
			if m.stopped[reg.Name] {
				stoppedName = reg.Name
				break
			}
		}
	}
	enabled := m.enabled
	var def *lspServerDef
	if enabled {
		def = m.byExt[ext] // defFor without the extra lock round-trip
	}
	discovered := m.discovered
	var c *lspClient
	var why string
	var bad, pending, stopped bool
	if def != nil {
		stopped = m.stopped != nil && m.stopped[def.Name]
		c = m.clients[def.Name]
		why, bad = m.failed[def.Name]
		_, pending = m.starting[def.Name]
	}
	m.mu.RUnlock()

	if stoppedName != "" {
		return lspOff, stoppedName
	}
	if def == nil {
		if enabled && !discovered && len(registryFor(rel)) > 0 {
			return lspStarting, ""
		}
		return lspOff, ""
	}
	if stopped {
		return lspOff, def.Name
	}
	switch {
	case bad:
		return lspFailed, why
	case pending:
		return lspStarting, def.Name
	case c == nil:
		return lspStarting, def.Name // not spawned yet; the next call will
	case c.alive() != nil:
		return lspFailed, def.Name
	case c.busy():
		return lspIndexing, def.Name
	}
	return lspReady, def.Name
}

func (m *lspManager) client(ctx context.Context, rel string) (*lspClient, error) {
	def := m.defFor(rel)
	if def == nil {
		return nil, errNoServer
	}
	m.mu.RLock()
	stopped := m.stopped != nil && m.stopped[def.Name]
	m.mu.RUnlock()
	if stopped {
		return nil, errStopped
	}
	for {
		m.mu.RLock()
		why, bad := m.failed[def.Name]
		c, ok := m.clients[def.Name]
		wait, starting := m.starting[def.Name]
		var aliveErr error
		if ok {
			aliveErr = c.alive()
		}
		m.mu.RUnlock()

		if bad {
			return nil, errFailed{why}
		}
		if ok && aliveErr == nil {
			return c, nil
		}
		if starting {
			select {
			case <-wait:
				continue // loop back and pick up the result
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}

		m.mu.Lock()
		if why, bad := m.failed[def.Name]; bad {
			m.mu.Unlock()
			return nil, errFailed{why}
		}
		if c, ok := m.clients[def.Name]; ok {
			err := c.alive()
			if err == nil {
				m.mu.Unlock()
				return c, nil
			}
			if m.restarts == nil {
				m.restarts = map[string]int{}
			}
			if m.restarts[def.Name] >= maxLSPRestarts {
				m.mu.Unlock()
				return nil, err
			}
			m.restarts[def.Name]++
			delete(m.clients, def.Name)
			m.mu.Unlock()
			go c.shutdown()
			continue
		}
		if wait, ok := m.starting[def.Name]; ok {
			m.mu.Unlock()
			select {
			case <-wait:
				continue // loop back and pick up the result
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		done := make(chan struct{})
		m.starting[def.Name] = done
		m.mu.Unlock()

		go m.spawn(def, done)

		select {
		case <-done:
			continue
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (m *lspManager) spawn(def *lspServerDef, done chan struct{}) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	c := newLSPClient(*def, m.root)
	err := c.start(ctx)

	m.mu.Lock()
	if err != nil {
		m.failed[def.Name] = err.Error()
	} else {
		m.clients[def.Name] = c
	}
	delete(m.starting, def.Name)
	m.mu.Unlock()
	close(done)
}

func (m *lspManager) CloseDoc(abs, rel string) {
	def := m.defFor(rel)
	if def == nil {
		return
	}
	m.mu.RLock()
	c := m.clients[def.Name]
	m.mu.RUnlock()
	if c != nil {
		c.closeDoc(abs)
	}
}

func (m *lspManager) EnsureOpen(ctx context.Context, abs, rel string) error {
	c, err := m.client(ctx, rel)
	if err != nil {
		return err
	}
	return c.ensureOpen(abs, rel)
}

func (m *lspManager) RefreshOpenDocs() {
	m.mu.RLock()
	clients := make([]*lspClient, 0, len(m.clients))
	for _, c := range m.clients {
		clients = append(clients, c)
	}
	m.mu.RUnlock()

	for _, c := range clients {
		c.mu.RLock()
		opened := make([]string, 0, len(c.opened))
		for uri := range c.opened {
			opened = append(opened, uri)
		}
		c.mu.RUnlock()

		for _, uri := range opened {
			abs, err := uriToPath(uri)
			if err != nil {
				continue
			}
			if _, err := os.Stat(abs); os.IsNotExist(err) {
				c.closeDoc(abs)
				continue
			}
			rel, err := filepath.Rel(m.root, abs)
			if err != nil {
				rel = abs
			}
			rel = filepath.ToSlash(rel)
			_ = c.syncDoc(abs, rel)
		}
	}
}

func (m *lspManager) Close() {
	m.mu.Lock()
	clients := make([]*lspClient, 0, len(m.clients))
	for _, c := range m.clients {
		clients = append(clients, c)
	}
	m.clients = map[string]*lspClient{}
	m.mu.Unlock()
	var wg sync.WaitGroup
	for _, c := range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.shutdown()
		}()
	}
	wg.Wait()
}

func (m *lspManager) ReapIdle(maxIdle time.Duration) {
	if maxIdle <= 0 {
		return
	}
	now := time.Now()
	var idle []*lspClient
	m.mu.Lock()
	for name, c := range m.clients {
		if c == nil || c.busy() {
			continue
		}
		last := time.Unix(0, c.lastUsed.Load())
		if now.Sub(last) >= maxIdle {
			idle = append(idle, c)
			delete(m.clients, name)
		}
	}
	m.mu.Unlock()
	for _, c := range idle {
		go c.shutdown()
	}
}

func (m *lspManager) Diagnostics(abs, rel string) []LSPDiagnostic {
	def := m.defFor(rel)
	if def == nil {
		return []LSPDiagnostic{}
	}
	m.mu.RLock()
	c := m.clients[def.Name]
	m.mu.RUnlock()
	if c == nil || c.alive() != nil {
		return []LSPDiagnostic{}
	}
	uri := pathToURI(abs)
	c.mu.RLock()
	list := c.diagnostics[uri]
	out := make([]LSPDiagnostic, len(list))
	copy(out, list)
	c.mu.RUnlock()
	return out
}

type errFailed struct{ why string }

func (e errFailed) Error() string { return e.why }

var (
	errNoServer = errFailed{"no language server for this file type"}
	errStopped  = errFailed{"language server stopped"}
)

func (m *lspManager) Stop(name string) {
	m.mu.Lock()
	if m.stopped == nil {
		m.stopped = map[string]bool{}
	}
	if name == "" || name == "all" {
		for _, def := range lspRegistry {
			m.stopped[def.Name] = true
		}
		clients := make([]*lspClient, 0, len(m.clients))
		for _, c := range m.clients {
			clients = append(clients, c)
		}
		m.clients = map[string]*lspClient{}
		m.mu.Unlock()
		for _, c := range clients {
			c.shutdown()
		}
		return
	}

	m.stopped[name] = true
	c := m.clients[name]
	delete(m.clients, name)
	m.mu.Unlock()
	if c != nil {
		c.shutdown()
	}
}

func (m *lspManager) StartServer(name string) error {
	if !m.enabled {
		return fmt.Errorf("lsp disabled")
	}
	m.mu.Lock()
	if m.stopped != nil {
		delete(m.stopped, name)
	}
	if m.failed != nil {
		delete(m.failed, name)
	}
	m.mu.Unlock()

	var targetDef *lspServerDef
	m.mu.RLock()
	for _, def := range m.byExt {
		if def.Name == name {
			targetDef = def
			break
		}
	}
	m.mu.RUnlock()

	if targetDef == nil {
		for i := range lspRegistry {
			if lspRegistry[i].Name == name {
				def := lspRegistry[i]
				if bin, ok := cachedLookPath(def.Cmd[0]); ok {
					def.Cmd = append([]string{bin}, def.Cmd[1:]...)
					targetDef = &def
				}
				break
			}
		}
	}

	if targetDef == nil {
		return fmt.Errorf("language server %q is not installed", name)
	}

	m.mu.Lock()
	if c, ok := m.clients[name]; ok && c.alive() == nil {
		m.mu.Unlock()
		return nil
	}
	if _, ok := m.starting[name]; ok {
		m.mu.Unlock()
		return nil
	}
	done := make(chan struct{})
	m.starting[name] = done
	m.mu.Unlock()

	go m.spawn(targetDef, done)
	return nil
}

func (m *lspManager) AnyRunning() bool {
	m.mu.RLock()
	clients := make([]*lspClient, 0, len(m.clients))
	for _, c := range m.clients {
		clients = append(clients, c)
	}
	starting := len(m.starting) > 0
	m.mu.RUnlock()

	for _, c := range clients {
		if c.alive() == nil {
			return true
		}
	}
	return starting
}

func shortLangName(lang string) string {
	switch lang {
	case "TypeScript and JavaScript":
		return "TS / JS"
	case "TypeScript":
		return "TS"
	case "JavaScript":
		return "JS"
	case "C and C++":
		return "C / C++"
	default:
		return lang
	}
}

func (m *lspManager) ServerStatus(def *lspServerDef) map[string]any {
	m.mu.RLock()
	c, ok := m.clients[def.Name]
	_, starting := m.starting[def.Name]
	why, failed := m.failed[def.Name]
	isStopped := m.stopped != nil && m.stopped[def.Name]
	m.mu.RUnlock()

	_, installed := cachedLookPath(def.Cmd[0])

	var opts []map[string]any
	autoIndex := -1
	for i, in := range def.installsFor(runtime.GOOS) {
		_, has := cachedLookPath(in.Cmd[0])
		if in.Auto && has && autoIndex == -1 {
			autoIndex = i
		}
		opts = append(opts, map[string]any{
			"index":   i,
			"cmd":     strings.Join(in.Cmd, " "),
			"auto":    in.Auto,
			"tool":    in.Cmd[0],
			"hasTool": has,
		})
	}

	res := map[string]any{
		"name":      def.Name,
		"lang":      shortLangName(def.Lang),
		"fullLang":  def.Lang,
		"exts":      def.Exts,
		"installed": installed,
		"running":   false,
		"state":     "stopped",
		"options":   opts,
		"autoOpt":   autoIndex,
	}

	if j := m.job(def.Name); j != nil {
		res["job"] = map[string]any{
			"running": j.Running,
			"cmd":     j.Cmd,
			"error":   j.Error,
		}
		if j.Running {
			res["state"] = "installing"
			return res
		}
		if j.Error != "" && !installed {
			res["state"] = "missing"
			res["error"] = j.Error
			return res
		}
	}

	if !installed {
		res["state"] = "missing"
		return res
	}

	if isStopped {
		res["state"] = "stopped"
		return res
	}

	if failed {
		res["state"] = "failed"
		res["error"] = why
		return res
	}

	if starting {
		res["state"] = "starting"
		res["running"] = true
		return res
	}

	if ok && c != nil && c.alive() == nil {
		res["running"] = true
		if c.busy() {
			res["state"] = "indexing"
		} else {
			res["state"] = "ready"
		}
		if c.cmd != nil && c.cmd.Process != nil {
			pid := c.cmd.Process.Pid
			res["pid"] = pid
			res["memBytes"] = metrics.ReadRSSForPID(pid)
		}
		return res
	}

	res["state"] = "stopped"
	return res
}

// ---------------------------------------------------------------- setup & install

// lspInstall is one way to get a language server's binary.
type lspInstall struct {
	OS   string   // "darwin", "linux" or "windows"; empty for any
	Cmd  []string // Cmd[0] is the installer, which must be present to use this
	Auto bool     // px0 may run it: user-level and non-interactive. Otherwise only shown.
}

func (d lspServerDef) installsFor(goos string) []lspInstall {
	var out []lspInstall
	for _, in := range d.Install {
		if in.OS == "" || in.OS == goos {
			out = append(out, in)
		}
	}
	return out
}

func registryFor(rel string) []*lspServerDef {
	ext := strings.ToLower(filepath.Ext(rel))
	var out []*lspServerDef
	for i := range lspRegistry {
		if slices.Contains(lspRegistry[i].Exts, ext) {
			out = append(out, &lspRegistry[i])
		}
	}
	return out
}

func (m *lspManager) MissingLang(rel string) string {
	if !m.enabled || !m.isDiscovered() || m.defFor(rel) != nil {
		return ""
	}
	if defs := registryFor(rel); len(defs) > 0 {
		return defs[0].Lang
	}
	return ""
}

type lspSetupOption struct {
	Cmd     string `json:"cmd"`
	Auto    bool   `json:"auto"`
	Tool    string `json:"tool"`
	HasTool bool   `json:"hasTool"`
}

type lspSetupServer struct {
	Name    string           `json:"name"`
	Options []lspSetupOption `json:"options"`
	Job     *lspJob          `json:"job,omitempty"`
}

type lspSetup struct {
	Enabled bool             `json:"enabled"`
	Lang    string           `json:"lang"`
	State   string           `json:"state"`
	Server  string           `json:"server"`
	Reason  string           `json:"reason,omitempty"` // why a server failed to start
	Servers []lspSetupServer `json:"servers"`
}

func (m *lspManager) Setup(rel string) lspSetup {
	st, srv := m.State(rel)
	s := lspSetup{Enabled: m.enabled, State: string(st), Server: srv, Servers: []lspSetupServer{}}
	if st == lspFailed {
		s.Reason = srv
		if def := m.defFor(rel); def != nil {
			s.Server = def.Name
		}
	}
	dirs := lspBinDirs()
	for _, def := range registryFor(rel) {
		if s.Lang == "" {
			s.Lang = def.Lang
		}
		ss := lspSetupServer{Name: def.Name, Options: []lspSetupOption{}, Job: m.job(def.Name)}
		for _, in := range def.installsFor(runtime.GOOS) {
			_, has := lookPathIn(in.Cmd[0], dirs)
			ss.Options = append(ss.Options, lspSetupOption{
				Cmd: strings.Join(in.Cmd, " "), Auto: in.Auto, Tool: in.Cmd[0], HasTool: has,
			})
		}
		s.Servers = append(s.Servers, ss)
	}
	return s
}

func (m *lspManager) Rescan() {
	if !m.enabled {
		return
	}
	m.discover()
	m.mu.Lock()
	m.failed = map[string]string{}
	m.restarts = nil
	m.mu.Unlock()
}

const lspInstallTimeout = 15 * time.Minute

type lspJob struct {
	Server  string `json:"server"`
	Cmd     string `json:"cmd"`
	Running bool   `json:"running"`
	Error   string `json:"error,omitempty"`
	Log     string `json:"log"`

	out *tailBuffer
}

func (m *lspManager) job(name string) *lspJob {
	m.jobMu.Lock()
	defer m.jobMu.Unlock()
	j := m.jobs[name]
	if j == nil {
		return nil
	}
	cp := *j
	cp.Log = j.out.String()
	return &cp
}

func (m *lspManager) Install(name string, option int) (*lspJob, error) {
	if !m.enabled {
		return nil, errors.New("language servers are turned off (-no-lsp)")
	}
	var def *lspServerDef
	for i := range lspRegistry {
		if lspRegistry[i].Name == name {
			def = &lspRegistry[i]
			break
		}
	}
	if def == nil {
		return nil, fmt.Errorf("unknown language server %q", name)
	}
	opts := def.installsFor(runtime.GOOS)
	if option < 0 || option >= len(opts) {
		return nil, fmt.Errorf("%s has no install option %d on %s", name, option, runtime.GOOS)
	}
	in := opts[option]
	if !in.Auto {
		return nil, fmt.Errorf("px0 does not run %q; run it in a terminal", strings.Join(in.Cmd, " "))
	}
	tool, ok := lookPathIn(in.Cmd[0], lspBinDirs())
	if !ok {
		return nil, fmt.Errorf("%s is not installed", in.Cmd[0])
	}

	m.jobMu.Lock()
	if j := m.jobs[name]; j != nil && j.Running {
		m.jobMu.Unlock()
		return m.job(name), nil
	}
	j := &lspJob{Server: name, Cmd: strings.Join(in.Cmd, " "), Running: true, out: &tailBuffer{max: 16 << 10}}
	if m.jobs == nil {
		m.jobs = map[string]*lspJob{}
	}
	m.jobs[name] = j
	m.jobMu.Unlock()

	go m.runInstall(def, j, tool, in.Cmd[1:])
	return m.job(name), nil
}

func (m *lspManager) runInstall(def *lspServerDef, j *lspJob, tool string, args []string) {
	ctx, cancel := context.WithTimeout(context.Background(), lspInstallTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, tool, args...)
	if home, err := os.UserHomeDir(); err == nil {
		cmd.Dir = home
	}
	cmd.Stdout, cmd.Stderr = j.out, j.out

	err := cmd.Run()
	switch {
	case ctx.Err() != nil:
		err = fmt.Errorf("gave up after %s", lspInstallTimeout)
	case err == nil:
		m.Rescan()
		invalidateBinaryCache()
		if _, found := lookPathIn(def.Cmd[0], lspBinDirs()); !found {
			err = fmt.Errorf("installed, but %s is not on PATH or in the usual install folders", def.Cmd[0])
		}
	}

	m.jobMu.Lock()
	j.Running = false
	if err != nil {
		j.Error = err.Error()
	}
	m.jobMu.Unlock()
}

type tailBuffer struct {
	mu  sync.Mutex
	b   []byte
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if len(t.b) > t.max {
		t.b = append([]byte(nil), t.b[len(t.b)-t.max:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.b)
}
