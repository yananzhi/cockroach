// Copyright 2015 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License. See the AUTHORS file
// for names of contributors.
//
// Author: Ben Darnell

package multiraft

import (
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/cockroachdb/cockroach/util"
	"github.com/cockroachdb/cockroach/util/log"
	"github.com/coreos/etcd/raft"
	"github.com/coreos/etcd/raft/raftpb"
	"golang.org/x/net/context"
)

// NodeID is a type alias for a raft node ID. Note that a raft node corresponds
// to a cockroach node+store combination.
type NodeID uint64

// Format implements the fmt.Formatter interface.
func (n NodeID) Format(f fmt.State, verb rune) {
	// Note: this implementation doesn't handle the width and precision
	// specifiers such as "%20.10s".
	fmt.Fprint(f, strconv.FormatInt(int64(n), 16))
}

// Config contains the parameters necessary to construct a MultiRaft object.
type Config struct {
	Storage   Storage
	Transport Transport
	// Ticker may be nil to use real time and TickInterval.
	Ticker Ticker
	// StateMachine may be nil if the state machine is transient and always starts from
	// a blank slate.
	StateMachine StateMachine

	// A new election is called if the ElectionTimeout elapses with no contact from the leader.
	// The actual ElectionTimeout is chosen randomly from the range [ElectionTimeoutMin,
	// ElectionTimeoutMax) to minimize the chances of several servers trying to become leaders
	// simultaneously. The Raft paper suggests a range of 150-300ms for local networks;
	// geographically distributed installations should use higher values to account for the
	// increased round trip time.
	ElectionTimeoutTicks   int
	HeartbeatIntervalTicks int
	TickInterval           time.Duration

	// If Strict is true, some warnings become fatal panics and additional (possibly expensive)
	// sanity checks will be done.
	Strict bool

	EntryFormatter raft.EntryFormatter
}

// validate returns an error if any required elements of the Config are missing or invalid.
// Called automatically by NewMultiRaft.
func (c *Config) validate() error {
	if c.Transport == nil {
		return util.Error("Transport is required")
	}
	if c.ElectionTimeoutTicks == 0 {
		return util.Error("ElectionTimeoutTicks must be non-zero")
	}
	if c.HeartbeatIntervalTicks == 0 {
		return util.Error("HeartbeatIntervalTicks must be non-zero")
	}
	if c.TickInterval == 0 {
		return util.Error("TickInterval must be non-zero")
	}
	return nil
}

// MultiRaft represents a local node in a raft cluster. The owner is responsible for consuming
// the Events channel in a timely manner.
type MultiRaft struct {
	Config
	multiNode       raft.MultiNode
	Events          chan interface{}
	nodeID          NodeID
	reqChan         chan *RaftMessageRequest
	createGroupChan chan *createGroupOp
	removeGroupChan chan *removeGroupOp
	proposalChan    chan *proposal
	// callbackChan is a generic hook to run a callback in the raft thread.
	callbackChan chan func()
}

// multiraftServer is a type alias to separate RPC methods
// (which net/rpc finds via reflection) from others.
type multiraftServer MultiRaft

// NewMultiRaft creates a MultiRaft object.
func NewMultiRaft(nodeID NodeID, config *Config) (*MultiRaft, error) {
	if nodeID == 0 {
		return nil, util.Error("Invalid NodeID")
	}
	err := config.validate()
	if err != nil {
		return nil, err
	}

	if config.Ticker == nil {
		config.Ticker = newTicker(config.TickInterval)
	}

	if config.EntryFormatter != nil {
		// Wrap the EntryFormatter to strip off the command id.
		ef := config.EntryFormatter
		config.EntryFormatter = func(data []byte) string {
			if len(data) == 0 {
				return "[empty]"
			}
			id, cmd := decodeCommand(data)
			formatted := ef(cmd)
			return fmt.Sprintf("%x: %s", id, formatted)
		}
	}

	m := &MultiRaft{
		Config:          *config,
		multiNode:       raft.StartMultiNode(uint64(nodeID)),
		nodeID:          nodeID,
		Events:          make(chan interface{}, 1000),
		reqChan:         make(chan *RaftMessageRequest, 100),
		createGroupChan: make(chan *createGroupOp, 100),
		removeGroupChan: make(chan *removeGroupOp, 100),
		proposalChan:    make(chan *proposal, 100),
		callbackChan:    make(chan func(), 100),
	}

	err = m.Transport.Listen(nodeID, (*multiraftServer)(m))
	if err != nil {
		return nil, err
	}

	return m, nil
}

// Start runs the raft algorithm in a background goroutine.
func (m *MultiRaft) Start(stopper *util.Stopper) error {
	s := newState(m)
	s.start(stopper)
	return nil
}

// RaftMessage implements ServerInterface; this method is called by net/rpc
// when we receive a message.
func (ms *multiraftServer) RaftMessage(req *RaftMessageRequest,
	resp *RaftMessageResponse) error {
	ms.reqChan <- req
	return nil
}

// strictErrorLog panics in strict mode and logs an error otherwise. Arguments are printf-style
// and will be passed directly to either log.Errorf or log.Fatalf.
func (m *MultiRaft) strictErrorLog(format string, args ...interface{}) {
	if m.Strict {
		log.Fatalf(format, args...)
	} else {
		log.Errorf(format, args...)
	}
}

func (m *MultiRaft) sendEvent(event interface{}) {
	select {
	case m.Events <- event:
		return
	default:
		// TODO(bdarnell): how should we handle filling up the Event queue?
		// Is there any place to apply backpressure?
		panic("MultiRaft.Events backlog reached limit")
	}
}

// fanoutHeartbeat sends the given heartbeat to all groups which believe that
// their leader resides on the sending node.
func (s *state) fanoutHeartbeat(req *RaftMessageRequest) {
	// A heartbeat message is expanded into a heartbeat for each group
	// that the remote node is a part of.
	fromID := NodeID(req.Message.From)
	originNode, ok := s.nodes[fromID]
	if !ok {
		// When a leader considers a follower to be down, it doesn't begin recovery
		// until the follower has successfully responded to a heartbeat. If we get a
		// heartbeat from a node we don't know, it must think we are a follower of
		// some group, so we need to respond so it can activate the recovery process.
		log.Warningf("node %v: not fanning out heartbeat from unknown node %v (but responding anyway)",
			s.nodeID, fromID)
		err := s.Transport.Send(fromID, &RaftMessageRequest{
			GroupID: math.MaxUint64,
			Message: raftpb.Message{
				From: uint64(s.nodeID),
				Type: raftpb.MsgHeartbeatResp,
			},
		})
		if err != nil {
			log.Errorf("node %v: error sending heartbeat response to %v", s.nodeID, fromID)
		}
		return
	}
	cnt := 0
	for groupID := range originNode.groupIDs {
		// If we don't think that the sending node is leading that group, don't
		// propagate.
		if s.groups[groupID].leader != fromID || fromID == s.nodeID {
			log.V(8).Infof("node %v: not fanning out heartbeat to %v, msg is from %d and leader is %d",
				s.nodeID, req.Message.To, fromID, s.groups[groupID].leader)
			continue
		}
		if err := s.multiNode.Step(context.Background(), groupID, req.Message); err != nil {
			log.V(4).Infof("node %v: coalesced heartbeat step failed for message %s", s.nodeID, groupID,
				raft.DescribeMessage(req.Message, s.EntryFormatter))
		}
		cnt++
	}
	log.V(7).Infof("node %v: received coalesced heartbeat from node %v; "+
		"fanned out to %d followers in %d overlapping groups",
		s.nodeID, fromID, cnt, len(originNode.groupIDs))
}

// fanoutHeartbeatResponse sends the given heartbeat response to all groups
// which overlap with the sender's groups and consider themselves leader.
func (s *state) fanoutHeartbeatResponse(req *RaftMessageRequest) {
	fromID := NodeID(req.Message.From)
	originNode, ok := s.nodes[fromID]
	if !ok {
		log.Warningf("node %v: not fanning out heartbeat response from unknown node %v",
			s.nodeID, fromID)
		return
	}
	cnt := 0
	for groupID := range originNode.groupIDs {
		// If we don't think that the local node is leader, don't propagate.
		if s.groups[groupID].leader != s.nodeID || fromID == s.nodeID {
			log.V(8).Infof("node %v: not fanning out heartbeat response to %v, msg is from %d and leader is %d",
				s.nodeID, req.Message.To, fromID, s.groups[groupID].leader)
			continue
		}
		if err := s.multiNode.Step(context.Background(), groupID, req.Message); err != nil {
			log.V(4).Infof("node %v: coalesced heartbeat response step failed for message %s", s.nodeID, groupID,
				raft.DescribeMessage(req.Message, s.EntryFormatter))
		}
		cnt++
	}
	log.V(7).Infof("node %v: received coalesced heartbeat response from node %v; "+
		"fanned out to %d leaders in %d overlapping groups",
		s.nodeID, fromID, cnt, len(originNode.groupIDs))
}

// CreateGroup creates a new consensus group and joins it. The initial membership of this
// group is determined by the InitialState method of the group's Storage object.
func (m *MultiRaft) CreateGroup(groupID uint64) error {
	op := &createGroupOp{
		groupID: groupID,
		ch:      make(chan error, 1),
	}
	m.createGroupChan <- op
	return <-op.ch
}

// RemoveGroup destroys the consensus group with the given ID.
// No events for this group will be emitted after this method returns
// (but some events may still be in the channel buffer).
func (m *MultiRaft) RemoveGroup(groupID uint64) error {
	op := &removeGroupOp{
		groupID: groupID,
		ch:      make(chan error, 1),
	}
	m.removeGroupChan <- op
	return <-op.ch
}

// SubmitCommand sends a command (a binary blob) to the cluster. This method returns
// when the command has been successfully sent, not when it has been committed.
// An error or nil will be written to the returned channel when the command has
// been committed or aborted.
func (m *MultiRaft) SubmitCommand(groupID uint64, commandID string, command []byte) <-chan error {
	log.V(6).Infof("node %v submitting command to group %v", m.nodeID, groupID)
	ch := make(chan error, 1)
	m.proposalChan <- &proposal{
		groupID:   groupID,
		commandID: commandID,
		fn: func() {
			err := m.multiNode.Propose(context.Background(), uint64(groupID),
				encodeCommand(commandID, command))
			if err != nil {
				log.Errorf("node %v: error proposing command to group %v: %s", m.nodeID, groupID, err)
			}
		},
		ch: ch,
	}
	return ch
}

// ChangeGroupMembership submits a proposed membership change to the cluster.
// Payload is an opaque blob that will be returned in EventMembershipChangeCommitted.
func (m *MultiRaft) ChangeGroupMembership(groupID uint64, commandID string,
	changeType raftpb.ConfChangeType, nodeID NodeID, payload []byte) <-chan error {
	log.V(6).Infof("node %v proposing membership change to group %v", m.nodeID, groupID)
	ch := make(chan error, 1)
	m.proposalChan <- &proposal{
		groupID:   groupID,
		commandID: commandID,
		fn: func() {
			err := m.multiNode.ProposeConfChange(context.Background(), uint64(groupID),
				raftpb.ConfChange{
					Type:    changeType,
					NodeID:  uint64(nodeID),
					Context: encodeCommand(commandID, payload),
				})
			if err != nil {
				log.Errorf("node %v: error proposing membership change to node %v: %s", m.nodeID,
					groupID, err)
			}

		},
		ch: ch,
	}
	return ch
}

type proposal struct {
	groupID   uint64
	commandID string
	fn        func()
	ch        chan<- error
}

// group represents the state of a consensus group.
type group struct {
	// committedTerm is the term of the most recently committed entry.
	committedTerm uint64

	// leader is the node ID of the last known leader for this group, or
	// 0 if an election is in progress.
	leader NodeID

	// pending contains all commands that have been proposed but not yet
	// committed in the current term. When a proposal is committed, nil
	// is written to proposal.ch and it is removed from this
	// map.
	pending map[string]*proposal
}

type createGroupOp struct {
	groupID uint64
	ch      chan error
}

type removeGroupOp struct {
	groupID uint64
	ch      chan error
}

// node represents a connection to a remote node.
type node struct {
	nodeID   NodeID
	refCount int
	groupIDs map[uint64]struct{}
}

func (n *node) registerGroup(groupID uint64) {
	n.groupIDs[groupID] = struct{}{}
}

func (n *node) unregisterGroup(groupID uint64) {
	delete(n.groupIDs, groupID)
}

// state represents the internal state of a MultiRaft object. All variables here
// are accessible only from the state.start goroutine so they can be accessed without
// synchronization.
type state struct {
	*MultiRaft
	groups        map[uint64]*group
	nodes         map[NodeID]*node
	electionTimer *time.Timer
	writeTask     *writeTask
	stopper       *util.Stopper
}

func newState(m *MultiRaft) *state {
	return &state{
		MultiRaft: m,
		groups:    make(map[uint64]*group),
		nodes:     make(map[NodeID]*node),
		writeTask: newWriteTask(m.Storage),
	}
}

func (s *state) start(stopper *util.Stopper) {
	s.stopper = stopper
	stopper.RunWorker(func() {
		log.V(1).Infof("node %v starting", s.nodeID)
		s.writeTask.start(s.stopper)
		// These maps form a kind of state machine: We don't want to read from the
		// ready channel until the groups we got from the last read have made their
		// way through the rest of the pipeline.
		var readyGroups map[uint64]raft.Ready
		var writingGroups map[uint64]raft.Ready
		// Counts up to heartbeat interval and is then reset.
		ticks := 0
		for {
			// raftReady signals that the Raft state machine has pending
			// work. That work is supplied over the raftReady channel as a map
			// from group ID to raft.Ready struct.
			var raftReady <-chan map[uint64]raft.Ready
			// writeReady is set to the write task's ready channel, which
			// receives when the write task is prepared to persist ready data
			// from the Raft state machine.
			var writeReady chan struct{}

			// The order of operations in this loop structure is as follows:
			// start by setting raftReady to the multiNode's Ready()
			// channel. Once a new raftReady has been consumed from the
			// channel, set writeReady to the write task's ready channel and
			// set raftReady back to nil. This advances our read-from-raft /
			// write-to-storage state machine to the next step: wait for the
			// write task to be ready to persist the new data.
			if readyGroups != nil {
				writeReady = s.writeTask.ready
			} else if writingGroups == nil {
				raftReady = s.multiNode.Ready()
			}

			log.V(8).Infof("node %v: selecting", s.nodeID)
			select {
			case <-s.stopper.ShouldStop():
				log.V(6).Infof("node %v: stopping", s.nodeID)
				s.stop()
				return

			case req := <-s.reqChan:
				log.V(5).Infof("node %v: group %v got message %.200s", s.nodeID, req.GroupID,
					raft.DescribeMessage(req.Message, s.EntryFormatter))
				switch req.Message.Type {
				case raftpb.MsgHeartbeat:
					s.fanoutHeartbeat(req)
				case raftpb.MsgHeartbeatResp:
					s.fanoutHeartbeatResponse(req)
				default:
					// We only want to lazily create the group if it's not heartbeat-related;
					// our heartbeats are coalesced and contain a dummy GroupID.
					// TODO(tschottdorf) still shouldn't hurt to move this part outside,
					// but suddenly tests will start failing. Should investigate.
					if _, ok := s.groups[req.GroupID]; !ok {
						log.Infof("node %v: got message for unknown group %d; creating it", s.nodeID, req.GroupID)
						if err := s.createGroup(req.GroupID); err != nil {
							log.Warningf("Error creating group %d: %s", req.GroupID, err)
							break
						}
					}

					if err := s.multiNode.Step(context.Background(), req.GroupID, req.Message); err != nil {
						log.V(4).Infof("node %v: multinode step failed for message %s", s.nodeID, req.GroupID,
							raft.DescribeMessage(req.Message, s.EntryFormatter))
					}
				}
			case op := <-s.createGroupChan:
				log.V(6).Infof("node %v: got op %#v", s.nodeID, op)
				op.ch <- s.createGroup(op.groupID)

			case op := <-s.removeGroupChan:
				log.V(6).Infof("node %v: got op %#v", s.nodeID, op)
				s.removeGroup(op)

			case prop := <-s.proposalChan:
				s.propose(prop)

			case readyGroups = <-raftReady:
				// readyGroups are saved in a local variable until they can be sent to
				// the write task (and then the real work happens after the write is
				// complete). All we do for now is log them.
				s.logRaftReady(readyGroups)

			case writeReady <- struct{}{}:
				s.handleWriteReady(readyGroups)
				writingGroups = readyGroups
				readyGroups = nil

			case resp := <-s.writeTask.out:
				s.handleWriteResponse(resp, writingGroups)
				s.multiNode.Advance(writingGroups)
				writingGroups = nil

			case <-s.Ticker.Chan():
				log.V(8).Infof("node %v: got tick", s.nodeID)
				s.multiNode.Tick()
				ticks++
				if ticks >= s.HeartbeatIntervalTicks {
					ticks = 0
					s.coalescedHeartbeat()
				}

			case cb := <-s.callbackChan:
				cb()
			}
		}
	})
}

func (s *state) coalescedHeartbeat() {
	// TODO(Tobias): We don't need to send heartbeats to nodes that have
	// no group following one of our local groups. But that's unlikely
	// to be the case for many of our nodes. It could make sense though
	// to space out the heartbeats over the heartbeat interval so that
	// we don't try to send for all nodes at once.
	for nodeID := range s.nodes {
		// Don't heartbeat yourself.
		if nodeID == s.nodeID {
			continue
		}
		log.V(6).Infof("node %v: triggering coalesced heartbeat to node %v", s.nodeID, nodeID)
		msg := raftpb.Message{
			From: uint64(s.nodeID),
			To:   uint64(nodeID),
			Type: raftpb.MsgHeartbeat,
		}
		err := s.Transport.Send(nodeID,
			&RaftMessageRequest{
				GroupID: math.MaxUint64, // irrelevant
				Message: msg,
			})
		if err != nil {
			log.Errorf("node %v: error sending coalesced heartbeat to %v: %s", s.nodeID, nodeID, err)
		}
	}
}

func (s *state) stop() {
	log.V(6).Infof("node %v stopping", s.nodeID)
	s.MultiRaft.Transport.Stop(s.nodeID)

	// Drain the create/remove group channels because other threads may be blocking
	// on these operations.
	done := false
	for !done {
		select {
		case op := <-s.createGroupChan:
			op.ch <- util.Errorf("shutting down")
		case op := <-s.removeGroupChan:
			op.ch <- util.Errorf("shutting down")
		default:
			done = true
		}
	}
	s.MultiRaft.multiNode.Stop()
}

// addNode creates a node and registers the given groupIDs for that
// node. If the node already exists and possible some of the groups
// are already registered, only the missing groups will be added in.
func (s *state) addNode(nodeID NodeID, groupIDs ...uint64) error {
	for _, groupID := range groupIDs {
		if _, ok := s.groups[groupID]; !ok {
			return util.Errorf("can not add invalid group %d to node %d",
				groupID, nodeID)
		}
	}
	newNode, ok := s.nodes[nodeID]
	if !ok {
		s.nodes[nodeID] = &node{
			nodeID:   nodeID,
			refCount: 1,
			groupIDs: make(map[uint64]struct{}),
		}
		newNode = s.nodes[nodeID]
	}
	for _, groupID := range groupIDs {
		newNode.registerGroup(groupID)
	}
	return nil
}

func (s *state) createGroup(groupID uint64) error {
	if _, ok := s.groups[groupID]; ok {
		return nil
	}
	log.V(6).Infof("node %v creating group %v", s.nodeID, groupID)

	gs := s.Storage.GroupStorage(groupID)
	_, cs, err := gs.InitialState()
	if err != nil {
		return err
	}

	var appliedIndex uint64
	if s.StateMachine != nil {
		appliedIndex, err = s.StateMachine.AppliedIndex(groupID)
		if err != nil {
			return err
		}
	}

	raftCfg := &raft.Config{
		Applied:       appliedIndex,
		ElectionTick:  s.ElectionTimeoutTicks,
		HeartbeatTick: s.HeartbeatIntervalTicks,
		Storage:       gs,
		// TODO(bdarnell): make these configurable; evaluate defaults.
		MaxSizePerMsg:   1024 * 1024,
		MaxInflightMsgs: 256,
	}
	if err := s.multiNode.CreateGroup(groupID, raftCfg, nil); err != nil {
		return err
	}
	s.groups[groupID] = &group{
		pending: map[string]*proposal{},
	}

	for _, nodeID := range cs.Nodes {
		if err := s.addNode(NodeID(nodeID), groupID); err != nil {
			return err
		}
	}

	// Automatically campaign and elect a leader for this group if there's
	// exactly one known node for this group.
	//
	// A grey area for this being correct happens in the case when we're
	// currently in the progress of adding a second node to the group,
	// with the change committed but not applied.
	// Upon restarting, the node would immediately elect itself and only
	// then apply the config change, where really it should be applying
	// first and then waiting for the majority (which would now require
	// two votes, not only its own).
	// However, in that special case, the second node has no chance to
	// be elected master while this node restarts (as it's aware of the
	// configuration and knows it needs two votes), so the worst that
	// could happen is both nodes ending up in candidate state, timing
	// out and then voting again. This is expected to be an extremely
	// rare event.
	if len(cs.Nodes) == 1 && s.MultiRaft.nodeID == NodeID(cs.Nodes[0]) {
		s.multiNode.Campaign(context.Background(), groupID)
	}
	return nil
}

func (s *state) removeGroup(op *removeGroupOp) {
	// Group creation is lazy and idempotent; so is removal.
	if _, ok := s.groups[op.groupID]; !ok {
		op.ch <- nil
		return
	}
	if err := s.multiNode.RemoveGroup(op.groupID); err != nil {
		op.ch <- err
		return
	}
	gs := s.Storage.GroupStorage(op.groupID)
	_, cs, err := gs.InitialState()
	if err != nil {
		op.ch <- err
	}
	for _, nodeID := range cs.Nodes {
		s.nodes[NodeID(nodeID)].unregisterGroup(op.groupID)
	}
	delete(s.groups, op.groupID)
	op.ch <- nil
}

func (s *state) propose(p *proposal) {
	g, ok := s.groups[p.groupID]
	if !ok {
		if p.ch != nil {
			// p.ch could be nil if this command was re-proposed due to leadership change
			// but finished before we processed it from the proposal queue.
			p.ch <- util.Errorf("group %d not found", p.groupID)
		}
		return
	}
	g.pending[p.commandID] = p
	p.fn()
}

func (s *state) logRaftReady(readyGroups map[uint64]raft.Ready) {
	for groupID, ready := range readyGroups {
		if log.V(5) {
			log.Infof("node %v: group %v raft ready", s.nodeID, groupID)
			if ready.SoftState != nil {
				log.Infof("SoftState updated: %+v", *ready.SoftState)
			}
			if !raft.IsEmptyHardState(ready.HardState) {
				log.Infof("HardState updated: %+v", ready.HardState)
			}
			for i, e := range ready.Entries {
				log.Infof("New Entry[%d]: %.200s", i, raft.DescribeEntry(e, s.EntryFormatter))
			}
			for i, e := range ready.CommittedEntries {
				log.Infof("Committed Entry[%d]: %.200s", i, raft.DescribeEntry(e, s.EntryFormatter))
			}
			if !raft.IsEmptySnap(ready.Snapshot) {
				log.Infof("Snapshot updated: %.200s", ready.Snapshot.String())
			}
			for i, m := range ready.Messages {
				log.Infof("Outgoing Message[%d]: %.200s", i, raft.DescribeMessage(m, s.EntryFormatter))
			}
		}
	}
}

// handleWriteReady converts a set of raft.Ready structs into a writeRequest
// to be persisted, and sends it to the writeTask.
func (s *state) handleWriteReady(readyGroups map[uint64]raft.Ready) {
	log.V(6).Infof("node %v write ready, preparing request", s.nodeID)
	writeRequest := newWriteRequest()
	for groupID, ready := range readyGroups {
		gwr := &groupWriteRequest{}
		if !raft.IsEmptyHardState(ready.HardState) {
			gwr.state = ready.HardState
		}
		if !raft.IsEmptySnap(ready.Snapshot) {
			gwr.snapshot = ready.Snapshot
		}
		if len(ready.Entries) > 0 {
			gwr.entries = ready.Entries
		}
		writeRequest.groups[groupID] = gwr
	}
	s.writeTask.in <- writeRequest
}

// processCommittedEntry tells the application that a command was committed.
// Returns the commandID, or an empty string if the given entry was not a command.
func (s *state) processCommittedEntry(groupID uint64, g *group, entry raftpb.Entry) string {
	var commandID string
	switch entry.Type {
	case raftpb.EntryNormal:
		// etcd raft occasionally adds a nil entry (e.g. upon election); ignore these.
		if entry.Data != nil {
			var command []byte
			commandID, command = decodeCommand(entry.Data)
			s.sendEvent(&EventCommandCommitted{
				GroupID:   groupID,
				CommandID: commandID,
				Command:   command,
				Index:     entry.Index,
			})
		}

	case raftpb.EntryConfChange:
		cc := raftpb.ConfChange{}
		err := cc.Unmarshal(entry.Data)
		if err != nil {
			log.Fatalf("invalid ConfChange data: %s", err)
		}
		var payload []byte
		if len(cc.Context) > 0 {
			commandID, payload = decodeCommand(cc.Context)
		}
		s.sendEvent(&EventMembershipChangeCommitted{
			GroupID:    groupID,
			CommandID:  commandID,
			Index:      entry.Index,
			NodeID:     NodeID(cc.NodeID),
			ChangeType: cc.Type,
			Payload:    payload,
			Callback: func(err error) {
				s.callbackChan <- func() {
					if err == nil {
						log.V(3).Infof("node %v applying configuration change %v", s.nodeID, cc)
						// TODO(bdarnell): dedupe by keeping a record of recently-applied commandIDs
						switch cc.Type {
						case raftpb.ConfChangeAddNode:
							err = s.addNode(NodeID(cc.NodeID), groupID)
						case raftpb.ConfChangeRemoveNode:
							// TODO(bdarnell): support removing nodes; fix double-application of initial entries
						case raftpb.ConfChangeUpdateNode:
							// Updates don't concern multiraft, they are simply passed through.
						}
						if err != nil {
							log.Errorf("error applying configuration change %v: %s", cc, err)
						}
						s.multiNode.ApplyConfChange(groupID, cc)
					} else {
						log.Warningf("aborting configuration change: %s", err)
						s.multiNode.ApplyConfChange(groupID,
							raftpb.ConfChange{})
					}

					// Re-submit all pending proposals, in case any of them were config changes
					// that were dropped due to the one-at-a-time rule. This is a little
					// redundant since most pending proposals won't benefit from this but
					// config changes should be rare enough (and the size of the pending queue
					// small enough) that it doesn't really matter.
					for _, prop := range g.pending {
						s.proposalChan <- prop
					}
				}
			},
		})
	}
	return commandID
}

// sendMessage sends a raft message on the given group.
func (s *state) sendMessage(groupID uint64, msg raftpb.Message) {
	log.V(6).Infof("node %v sending message %.200s to %v", s.nodeID,
		raft.DescribeMessage(msg, s.EntryFormatter), msg.To)
	nodeID := NodeID(msg.To)
	if _, ok := s.nodes[nodeID]; !ok {
		log.V(4).Infof("node %v: connecting to new node %v", s.nodeID, nodeID)
		if err := s.addNode(nodeID, groupID); err != nil {
			log.Errorf("node %v: error adding node %v", s.nodeID, nodeID)
		}
	}
	err := s.Transport.Send(NodeID(msg.To), &RaftMessageRequest{groupID, msg})
	snapStatus := raft.SnapshotFinish
	if err != nil {
		log.Warningf("node %v failed to send message to %v: %s", s.nodeID, nodeID, err)
		s.multiNode.ReportUnreachable(msg.To, groupID)
		snapStatus = raft.SnapshotFailure
	}
	if msg.Type == raftpb.MsgSnap {
		// TODO(bdarnell): add an ack for snapshots and don't report status until
		// ack, error, or timeout.
		s.multiNode.ReportSnapshot(msg.To, groupID, snapStatus)
	}
}

// maybeSendLeaderEvent processes a raft.Ready to send events in response to leadership
// changes (this includes both sending an event to the app and retrying any pending
// proposals).
func (s *state) maybeSendLeaderEvent(groupID uint64, g *group, ready *raft.Ready) {
	term := g.committedTerm
	if ready.SoftState != nil {
		// Always save the leader whenever we get a SoftState.
		g.leader = NodeID(ready.SoftState.Lead)
	}
	if len(ready.CommittedEntries) > 0 {
		term = ready.CommittedEntries[len(ready.CommittedEntries)-1].Term
	}
	if term != g.committedTerm && g.leader != 0 {
		// Whenever the committed term has advanced and we know our leader,
		// emit an event.
		g.committedTerm = term
		s.sendEvent(&EventLeaderElection{
			GroupID: groupID,
			NodeID:  NodeID(g.leader),
			Term:    g.committedTerm,
		})

		// Re-submit all pending proposals
		for _, prop := range g.pending {
			s.proposalChan <- prop
		}
	}
}

// handleWriteResponse updates the state machine and sends messages for a raft Ready batch.
func (s *state) handleWriteResponse(response *writeResponse, readyGroups map[uint64]raft.Ready) {
	log.V(6).Infof("node %v got write response: %#v", s.nodeID, *response)
	// Everything has been written to disk; now we can apply updates to the state machine
	// and send outgoing messages.
	for groupID, ready := range readyGroups {
		g, ok := s.groups[groupID]
		if !ok {
			log.V(4).Infof("dropping stale write to group %v", groupID)
			continue
		}

		// Process committed entries.
		for _, entry := range ready.CommittedEntries {
			commandID := s.processCommittedEntry(groupID, g, entry)
			if p, ok := g.pending[commandID]; ok {
				// TODO(bdarnell): the command is now committed, but not applied until the
				// application consumes EventCommandCommitted. Is returning via the channel
				// at this point useful or do we need to wait for the command to be
				// applied too?
				// This could be done with a Callback as in EventMembershipChangeCommitted
				// or perhaps we should move away from a channel to a callback-based system.
				if p.ch != nil {
					// Because of the way we re-queue proposals during leadership
					// changes, we may finish the same proposal object twice.
					p.ch <- nil
					p.ch = nil
				}
				delete(g.pending, commandID)
			}
		}

		// Process SoftState and leader changes.
		s.maybeSendLeaderEvent(groupID, g, &ready)

		// Send all messages.
		noMoreHeartbeats := make(map[uint64]struct{})
		for _, msg := range ready.Messages {
			switch msg.Type {
			case raftpb.MsgHeartbeat:
				log.V(7).Infof("node %v dropped individual heartbeat to node %v",
					s.nodeID, msg.To)
				continue
			case raftpb.MsgHeartbeatResp:
				if _, ok := noMoreHeartbeats[msg.To]; ok {
					log.V(7).Infof("node %v dropped redundant heartbeat response to node %v",
						s.nodeID, msg.To)
					continue
				}
				noMoreHeartbeats[msg.To] = struct{}{}
			}

			s.sendMessage(groupID, msg)
		}
	}
}
