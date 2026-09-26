package helps

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"

	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const codexWebsocketLaneLimit = 32

var ErrCodexWebsocketConnectionRequired = errors.New("codex websocket continuation requires an existing connection")

// CodexWebsocketMuxError fails only the affected request. In particular, a
// routing failure must never trigger credential failover or implicit replay.
type CodexWebsocketMuxError struct{ Message string }

func (e *CodexWebsocketMuxError) Error() string         { return e.Message }
func (e *CodexWebsocketMuxError) IsRequestScoped() bool { return true }

// CodexWebsocketRegistry owns physical connections, independently of execution
// sessions and executor instances. Entries are not evicted: only actual network
// failure permits replacement of a live connection.
type CodexWebsocketRegistry struct {
	mu     sync.Mutex
	owners map[string]*codexWebsocketOwner
}

type CodexWebsocketDial func(context.Context) (*websocket.Conn, *http.Response, error)

type CodexWebsocketTarget struct {
	Credential string
	Owner      string
	URL        string
	// Affinity selects an idle logical lane without changing physical ownership.
	Affinity string
	// ExistingOnly prohibits reconnection for a continuation requiring old state.
	ExistingOnly bool
	Connected    func(*websocket.Conn)
	Disconnected func(*websocket.Conn, error)
}

type codexWebsocketOwner struct {
	mu       sync.Mutex
	writeMu  sync.Mutex
	dialing  chan struct{}
	physical *codexWebsocketPhysical
}

type codexWebsocketPhysical struct {
	conn         *websocket.Conn
	done         chan struct{}
	target       CodexWebsocketTarget
	fault        error
	protocolErr  error
	lanes        [codexWebsocketLaneLimit]codexWebsocketSlot
	responseLane map[string]*CodexWebsocketLane
	clock        uint64
}

type codexWebsocketSlot struct {
	affinity string
	epoch    uint64
	lastUsed uint64
	lease    *CodexWebsocketLane
}

// CodexWebsocketLane is a logical subscriber, never a physical connection owner.
// Releasing it abandons delivery while the reader drains outstanding responses.
// Slow or cancelled subscribers cannot block other streams or close the socket.
type CodexWebsocketLane struct {
	owner              *codexWebsocketOwner
	physical           *codexWebsocketPhysical
	slot               int
	epoch              uint64
	events             chan []byte
	done               chan struct{}
	err                error
	released           bool
	closed             bool
	pending            int
	active             map[string]struct{}
	steerPending       map[string]int
	steerAccepted      map[string]string
	queuedBytes        int
	downstreamStreamID string
	written            bool
}

func (r *CodexWebsocketRegistry) Acquire(ctx context.Context, target CodexWebsocketTarget, dial CodexWebsocketDial) (*CodexWebsocketLane, *http.Response, error) {
	if target.Credential == "" {
		return nil, nil, &CodexWebsocketMuxError{"codex websocket credential identity is missing"}
	}
	r.mu.Lock()
	if r.owners == nil {
		r.owners = make(map[string]*codexWebsocketOwner)
	}
	o := r.owners[target.Credential]
	if o == nil {
		o = &codexWebsocketOwner{}
		r.owners[target.Credential] = o
	}
	r.mu.Unlock()
	for {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		o.mu.Lock()
		if p := o.physical; p != nil {
			if p.fault != nil {
				done := p.done
				o.mu.Unlock()
				select {
				case <-ctx.Done():
					return nil, nil, ctx.Err()
				case <-done:
					continue
				}
			}
			lane, err := o.acquireLocked(p, target)
			o.mu.Unlock()
			return lane, nil, err
		}
		if target.ExistingOnly {
			o.mu.Unlock()
			return nil, nil, ErrCodexWebsocketConnectionRequired
		}
		if pending := o.dialing; pending != nil {
			o.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			case <-pending:
				continue
			}
		}
		o.dialing = make(chan struct{})
		o.mu.Unlock()
		conn, response, errDial := dial(ctx)
		o.mu.Lock()
		if errDial != nil || conn == nil {
			close(o.dialing)
			o.dialing = nil
			o.mu.Unlock()
			if errDial == nil {
				errDial = errors.New("codex websocket dial returned no connection")
			}
			return nil, response, errDial
		}
		p := &codexWebsocketPhysical{conn: conn, target: target, done: make(chan struct{}), responseLane: make(map[string]*CodexWebsocketLane)}
		o.physical = p
		lane, err := o.acquireLocked(p, target)
		close(o.dialing)
		o.dialing = nil
		o.mu.Unlock()
		// No request context, idle timer, or configuration fingerprint owns this
		// socket. The reader remains alive even when there are no subscribers.
		if target.Connected != nil {
			target.Connected(conn)
		}
		go o.read(p)
		return lane, response, err
	}
}

func (o *codexWebsocketOwner) acquireLocked(p *codexWebsocketPhysical, target CodexWebsocketTarget) (*CodexWebsocketLane, error) {
	if p.target.Owner != target.Owner || p.target.URL != target.URL {
		return nil, &CodexWebsocketMuxError{"codex credential already owns a live websocket for a different upstream identity; retaining the existing connection"}
	}
	if p.protocolErr != nil {
		return nil, p.protocolErr
	}
	index := -1
	for i := range p.lanes {
		slot := &p.lanes[i]
		if slot.lease != nil {
			continue
		}
		if target.Affinity != "" && slot.affinity == target.Affinity {
			index = i
			break
		}
		if index < 0 || slot.lastUsed < p.lanes[index].lastUsed {
			index = i
		}
	}
	if index < 0 {
		return nil, &CodexWebsocketMuxError{"codex websocket has 32 occupied logical streams; a second connection is prohibited"}
	}
	slot := &p.lanes[index]
	if slot.affinity != target.Affinity || target.Affinity == "" {
		slot.epoch++
	}
	p.clock++
	slot.lastUsed, slot.affinity = p.clock, target.Affinity
	lane := &CodexWebsocketLane{owner: o, physical: p, slot: index, epoch: slot.epoch, events: make(chan []byte, 256), done: make(chan struct{}), active: make(map[string]struct{}), steerPending: make(map[string]int), steerAccepted: make(map[string]string)}
	slot.lease = lane
	return lane, nil
}

func (l *CodexWebsocketLane) Conn() *websocket.Conn           { return l.physical.conn }
func (l *CodexWebsocketLane) StreamID() string                { return "cpa_" + strconv.Itoa(l.slot) }
func (l *CodexWebsocketLane) Epoch() uint64                   { return l.epoch }
func (l *CodexWebsocketLane) ConnectionDone() <-chan struct{} { return l.physical.done }

func (l *CodexWebsocketLane) ConnectionError() error {
	l.owner.mu.Lock()
	defer l.owner.mu.Unlock()
	return l.physical.fault
}

func (l *CodexWebsocketLane) Available() bool {
	l.owner.mu.Lock()
	defer l.owner.mu.Unlock()
	return !l.released && !l.closed && l.owner.physical == l.physical && l.physical.fault == nil
}

// Write serializes only the frame write. It never waits for a response to finish.
// The callback retains the executor's existing chunking and write instrumentation.
func (l *CodexWebsocketLane) Write(payload []byte, write func([]byte) error) error {
	o, p := l.owner, l.physical
	o.writeMu.Lock()
	defer o.writeMu.Unlock()
	o.mu.Lock()
	if l.released || l.closed || o.physical != p || p.fault != nil || p.protocolErr != nil {
		err := l.err
		if err == nil {
			err = &CodexWebsocketMuxError{"codex websocket logical stream is no longer available"}
		}
		o.mu.Unlock()
		return err
	}
	kind := gjson.GetBytes(payload, "type").String()
	if kind == "response.create" {
		downstreamStreamID := gjson.GetBytes(payload, "stream_id").String()
		if l.written && downstreamStreamID != l.downstreamStreamID {
			o.mu.Unlock()
			return &CodexWebsocketMuxError{"a downstream websocket execution session must keep one logical stream_id"}
		}
		l.downstreamStreamID, l.written = downstreamStreamID, true
		var err error
		payload, err = sjson.SetBytes(payload, "stream_id", l.StreamID())
		if err != nil {
			o.mu.Unlock()
			return &CodexWebsocketMuxError{"cannot encode codex websocket stream_id"}
		}
		l.pending++
	} else if kind == "response.steer" {
		parent := gjson.GetBytes(payload, "previous_response_id").String()
		if p.responseLane[parent] != l {
			o.mu.Unlock()
			return &CodexWebsocketMuxError{"steering target does not belong to this logical stream"}
		}
		l.steerPending[parent]++
	}
	o.mu.Unlock()
	if err := write(payload); err != nil {
		o.networkFailure(p, err)
		return err
	}
	return nil
}

func (l *CodexWebsocketLane) Read(ctx context.Context) ([]byte, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case payload := <-l.events:
		return l.consume(payload), nil
	case <-l.done:
		// Deliver already received terminal events before a subsequent peer close.
		select {
		case payload := <-l.events:
			return l.consume(payload), nil
		default:
		}
		l.owner.mu.Lock()
		err := l.err
		l.owner.mu.Unlock()
		return nil, err
	}
}

func (l *CodexWebsocketLane) consume(payload []byte) []byte {
	l.owner.mu.Lock()
	l.queuedBytes -= len(payload)
	l.owner.mu.Unlock()
	return payload
}

func (l *CodexWebsocketLane) Release() {
	l.owner.mu.Lock()
	defer l.owner.mu.Unlock()
	l.released = true
	l.finishLocked(context.Canceled)
	l.recycleLocked()
}

func (l *CodexWebsocketLane) finishLocked(err error) {
	if !l.closed {
		l.closed, l.err = true, err
		close(l.done)
	}
}

func (l *CodexWebsocketLane) recycleLocked() {
	if !l.released || l.pending != 0 || len(l.active) != 0 || len(l.steerPending) != 0 || len(l.steerAccepted) != 0 {
		return
	}
	if slot := &l.physical.lanes[l.slot]; slot.lease == l {
		slot.lease = nil
	}
	for id, lane := range l.physical.responseLane {
		if lane == l {
			delete(l.physical.responseLane, id)
		}
	}
}

func (o *codexWebsocketOwner) read(p *codexWebsocketPhysical) {
	for {
		kind, payload, err := p.conn.ReadMessage()
		if err != nil {
			o.networkFailure(p, err)
			break
		}
		if kind != websocket.TextMessage {
			o.mu.Lock()
			o.protocolFailureLocked(p, "upstream sent a non-text response event")
			o.mu.Unlock()
			continue
		}
		o.mu.Lock()
		o.routeLocked(p, payload)
		o.mu.Unlock()
	}
	// Close has completed and the sole reader has exited before another dial
	// can observe an empty credential slot. There is no background reconnect.
	o.mu.Lock()
	err := p.fault
	o.mu.Unlock()
	if p.target.Disconnected != nil {
		p.target.Disconnected(p.conn, err)
	}
	o.mu.Lock()
	if o.physical == p {
		o.physical = nil
	}
	close(p.done)
	o.mu.Unlock()
}

func (o *codexWebsocketOwner) networkFailure(p *codexWebsocketPhysical, err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if p.fault != nil {
		return
	}
	p.fault = err
	// This is the only physical Close in the registry. Callers cannot use it
	// for cancellation, eviction, overload, hot reload, or a protocol error.
	_ = p.conn.Close()
	for i := range p.lanes {
		if lane := p.lanes[i].lease; lane != nil {
			lane.finishLocked(err)
		}
	}
}

func (o *codexWebsocketOwner) protocolFailureLocked(p *codexWebsocketPhysical, message string) {
	if p.protocolErr != nil {
		return
	}
	p.protocolErr = &CodexWebsocketMuxError{fmt.Sprintf("codex websocket multiplexing unavailable: %s; retaining the physical connection without replay or a second connection", message)}
	for i := range p.lanes {
		if lane := p.lanes[i].lease; lane != nil {
			lane.finishLocked(p.protocolErr)
		}
	}
}

func (o *codexWebsocketOwner) routeLocked(p *codexWebsocketPhysical, payload []byte) {
	if p.protocolErr != nil || p.fault != nil {
		return
	}
	kind := gjson.GetBytes(payload, "type").String()
	stream := gjson.GetBytes(payload, "stream_id").String()
	var lane *CodexWebsocketLane
	if stream != "" {
		for i := range p.lanes {
			if stream == "cpa_"+strconv.Itoa(i) {
				lane = p.lanes[i].lease
				break
			}
		}
	} else if kind == "codex.rate_limits" {
		// Quota snapshots belong to the credential, not a particular turn.
		for i := range p.lanes {
			if subscriber := p.lanes[i].lease; subscriber != nil {
				subscriber.enqueueLocked(payload)
				subscriber.recycleLocked()
			}
		}
		return
	} else if kind == "responsesapi.websocket_timing" {
		// Unlabelled diagnostics cannot safely be attributed to a request.
		return
	} else if parent := gjson.GetBytes(payload, "steer.previous_response_id").String(); parent != "" {
		lane = p.responseLane[parent]
	}
	if lane == nil {
		o.protocolFailureLocked(p, "upstream event cannot be associated with a named stream")
		return
	}
	id := gjson.GetBytes(payload, "response.id").String()
	if id == "" {
		id = gjson.GetBytes(payload, "response_id").String()
	}
	steerParent := gjson.GetBytes(payload, "steer.previous_response_id").String()
	steerID := gjson.GetBytes(payload, "steer.id").String()
	if kind == "response.steer.accepted" || kind == "response.steer.failed" {
		if lane.steerPending[steerParent] > 1 {
			lane.steerPending[steerParent]--
		} else {
			delete(lane.steerPending, steerParent)
		}
		if kind == "response.steer.accepted" {
			lane.steerAccepted[steerID] = steerParent
		} else {
			delete(lane.steerAccepted, steerID)
		}
	}
	if kind == "response.created" && id != "" {
		parent := gjson.GetBytes(payload, "response.previous_response_id").String()
		if parent != "" {
			for steer, pendingParent := range lane.steerAccepted {
				if pendingParent == parent {
					delete(lane.steerAccepted, steer)
				}
			}
		}
		if lane.pending > 0 {
			lane.pending--
		}
		lane.active[id] = struct{}{}
		p.responseLane[id] = lane
		// Keep only live response / steering targets plus the latest response.
		// Conversation continuation itself uses previous_response_id upstream.
		for previous, owner := range p.responseLane {
			if owner != lane || previous == id {
				continue
			}
			if _, active := lane.active[previous]; active || lane.steerPending[previous] > 0 {
				continue
			}
			retained := false
			for _, parent := range lane.steerAccepted {
				if parent == previous {
					retained = true
					break
				}
			}
			if !retained {
				delete(p.responseLane, previous)
			}
		}
	}
	switch kind {
	case "response.completed", "response.done", "response.failed", "response.incomplete", "error":
		if _, active := lane.active[id]; active {
			delete(lane.active, id)
		} else if lane.pending > 0 {
			lane.pending--
		} else if id == "" && len(lane.active) == 1 {
			clear(lane.active)
		}
	}
	lane.enqueueLocked(payload)
	lane.recycleLocked()
}

func (l *CodexWebsocketLane) enqueueLocked(payload []byte) {
	if !l.closed {
		// stream_id is CPA's private routing label, not a downstream field.
		if l.downstreamStreamID == "" {
			payload, _ = sjson.DeleteBytes(payload, "stream_id")
		} else {
			payload, _ = sjson.SetBytes(payload, "stream_id", l.downstreamStreamID)
		}
		if l.queuedBytes+len(payload) > 8<<20 {
			l.released = true
			l.finishLocked(&CodexWebsocketMuxError{"codex websocket subscriber exceeded its response buffer"})
		} else {
			select {
			case l.events <- payload:
				l.queuedBytes += len(payload)
			default:
				l.released = true
				l.finishLocked(&CodexWebsocketMuxError{"codex websocket subscriber is not consuming events"})
			}
		}
	}
}
