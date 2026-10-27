package raft

import (
	"fmt"
	"math/rand"
	"sort"

	"github.com/eskhakha/vecdb/internal/cluster"
)

// NodeID names a Raft peer.
type NodeID = cluster.NodeID

// State is a node's role.
type State int

const (
	Follower State = iota
	// PreCandidate campaigns WITHOUT incrementing its term.
	//
	// Raft as published does not guarantee liveness under a partial network
	// partition: a node that can receive but not send (or one isolated from a
	// quorum) times out, increments its term, and forces every node it can
	// reach to step down -- repeatedly, forever, preventing any stable leader.
	// PreVote asks "would you vote for me?" before disturbing anyone's term,
	// so a node that cannot win never disrupts the cluster. It is not in the
	// original paper's core protocol but is near-universal in production Raft.
	PreCandidate
	Candidate
	Leader
)

func (s State) String() string {
	switch s {
	case Follower:
		return "follower"
	case PreCandidate:
		return "pre-candidate"
	case Candidate:
		return "candidate"
	case Leader:
		return "leader"
	default:
		return fmt.Sprintf("state(%d)", int(s))
	}
}

// MsgType identifies a message.
type MsgType int

const (
	MsgPreVoteReq MsgType = iota
	MsgPreVoteResp
	MsgVoteReq
	MsgVoteResp
	MsgAppendReq
	MsgAppendResp
)

func (m MsgType) String() string {
	return [...]string{"prevote_req", "prevote_resp", "vote_req", "vote_resp", "append_req", "append_resp"}[m]
}

// Message is one RPC or reply. Everything the protocol does travels as one of
// these, which is what lets the simulation drop, delay and reorder at will.
type Message struct {
	Type MsgType
	From NodeID
	To   NodeID
	Term Term

	// Vote fields.
	LastLogIndex Index
	LastLogTerm  Term
	Granted      bool

	// Append fields.
	PrevLogIndex Index
	PrevLogTerm  Term
	Entries      []Entry
	LeaderCommit Index

	// Append response fields.
	Success    bool
	MatchIndex Index
	// Hint is the follower's last index, letting the leader skip a whole run of
	// probes instead of decrementing nextIndex one at a time.
	Hint Index
}

func (m Message) String() string {
	return fmt.Sprintf("%s %s->%s term=%d", m.Type, m.From, m.To, m.Term)
}

// Config configures a node.
type Config struct {
	ID    NodeID
	Peers []NodeID // includes ID
	// ElectionTick is the base election timeout in ticks. The real timeout is
	// randomized in [ElectionTick, 2*ElectionTick) so that nodes do not all
	// campaign at the same instant and split the vote forever.
	ElectionTick int
	// HeartbeatTick is how often a leader sends empty AppendEntries. It must be
	// well below ElectionTick or followers will time out on a healthy leader.
	HeartbeatTick int
	// Rand seeds the election-timeout randomization. Injectable so a failing
	// simulation run reproduces exactly from its seed.
	Rand *rand.Rand
	// CheckQuorum makes a leader step down if it has not heard from a majority
	// within an election timeout. Without it, a leader isolated from the
	// cluster keeps believing it leads and keeps serving, indefinitely.
	CheckQuorum bool
}

func (c *Config) setDefaults() {
	if c.ElectionTick <= 0 {
		c.ElectionTick = 10
	}
	if c.HeartbeatTick <= 0 {
		c.HeartbeatTick = 1
	}
	if c.Rand == nil {
		c.Rand = rand.New(rand.NewSource(1))
	}
}

// Node is a Raft peer implemented as a pure state machine.
//
// No goroutines, no locks, no clock. Tick() and Step() return the messages to
// send; the caller decides when and whether they arrive. That is exactly what
// makes an entire cluster runnable inside one deterministic test.
type Node struct {
	cfg   Config
	id    NodeID
	peers []NodeID

	state       State
	currentTerm Term
	votedFor    NodeID
	leader      NodeID
	log         *Log

	// Leader state, reinitialized on election.
	nextIndex  map[NodeID]Index
	matchIndex map[NodeID]Index
	// recentActive tracks who has answered since the last quorum check.
	recentActive map[NodeID]bool

	votes map[NodeID]bool

	electionElapsed   int
	heartbeatElapsed  int
	randomizedTimeout int

	// Applied holds commands in commit order, which is the replicated state
	// machine's input. Tests assert on it directly.
	Applied [][]byte
}

// New returns a node in follower state with an empty log.
func New(cfg Config) *Node {
	cfg.setDefaults()
	n := &Node{
		cfg: cfg, id: cfg.ID, peers: append([]NodeID(nil), cfg.Peers...),
		state: Follower, log: NewLog(),
		nextIndex: map[NodeID]Index{}, matchIndex: map[NodeID]Index{},
		recentActive: map[NodeID]bool{}, votes: map[NodeID]bool{},
	}
	sort.Slice(n.peers, func(i, j int) bool { return n.peers[i] < n.peers[j] })
	n.resetTimeout()
	return n
}

// ID returns this node's identity.
func (n *Node) ID() NodeID { return n.id }

// State returns the current role.
func (n *Node) State() State { return n.state }

// Term returns the current term.
func (n *Node) Term() Term { return n.currentTerm }

// Leader returns who this node believes leads, if anyone.
func (n *Node) Leader() NodeID { return n.leader }

// Log exposes the replicated log for assertions.
func (n *Node) Log() *Log { return n.log }

// IsLeader reports whether this node believes it is the leader.
func (n *Node) IsLeader() bool { return n.state == Leader }

func (n *Node) quorum() int { return len(n.peers)/2 + 1 }

func (n *Node) resetTimeout() {
	n.electionElapsed = 0
	n.randomizedTimeout = n.cfg.ElectionTick + n.cfg.Rand.Intn(n.cfg.ElectionTick)
}

// becomeFollower steps down. Any message carrying a higher term forces this,
// which is how a stale leader learns it has been replaced.
func (n *Node) becomeFollower(term Term, leader NodeID) {
	if term > n.currentTerm {
		n.currentTerm = term
		n.votedFor = ""
	}
	n.state = Follower
	n.leader = leader
	n.resetTimeout()
}

func (n *Node) becomeLeader() {
	n.state = Leader
	n.leader = n.id
	n.heartbeatElapsed = 0
	for _, p := range n.peers {
		n.nextIndex[p] = n.log.LastIndex() + 1
		n.matchIndex[p] = 0
		n.recentActive[p] = false
	}
	n.matchIndex[n.id] = n.log.LastIndex()
	n.recentActive[n.id] = true
	// Deliberately NO no-op entry on election. This implementation instead
	// enforces the current-term commit rule directly (see maybeCommit), which
	// is the same protection stated as an invariant rather than a workaround.
}

// Propose appends a command to the leader's log. It returns false on a
// non-leader: proposals must be routed to the leader, never accepted locally.
func (n *Node) Propose(data []byte) ([]Message, bool) {
	if n.state != Leader {
		return nil, false
	}
	n.log.Append(n.currentTerm, data)
	n.matchIndex[n.id] = n.log.LastIndex()
	n.maybeCommit()
	return n.broadcastAppend(), true
}

// Tick advances one logical time step.
func (n *Node) Tick() []Message {
	switch n.state {
	case Leader:
		n.heartbeatElapsed++
		n.electionElapsed++
		var out []Message
		if n.heartbeatElapsed >= n.cfg.HeartbeatTick {
			n.heartbeatElapsed = 0
			out = n.broadcastAppend()
		}
		if n.cfg.CheckQuorum && n.electionElapsed >= n.randomizedTimeout {
			n.electionElapsed = 0
			active := 0
			for _, p := range n.peers {
				if n.recentActive[p] {
					active++
				}
				n.recentActive[p] = false
			}
			n.recentActive[n.id] = true
			if active < n.quorum() {
				// This leader cannot reach a majority. Stepping down is what
				// stops an isolated leader serving indefinitely while a new
				// leader exists on the other side of the partition.
				n.becomeFollower(n.currentTerm, "")
				return nil
			}
		}
		return out
	default:
		n.electionElapsed++
		if n.electionElapsed >= n.randomizedTimeout {
			return n.campaignPre()
		}
	}
	return nil
}

// campaignPre starts a pre-vote round. The term is NOT incremented.
func (n *Node) campaignPre() []Message {
	n.state = PreCandidate
	// Clearing the remembered leader is load-bearing, not tidiness.
	//
	// The CheckQuorum pre-vote guard below refuses a pre-vote from a node that
	// still believes it has a live leader, which stops a briefly-partitioned
	// node unseating a healthy one on rejoin. But campaigning also resets the
	// election timer -- so without this line, every node that times out becomes
	// a pre-candidate with a fresh timer AND a stale leader pointer, and
	// therefore refuses every peer's pre-vote. All five nodes campaign, all five
	// refuse each other, and the cluster deadlocks at pre-candidate forever
	// with no leader and no error.
	//
	// Deciding to campaign IS deciding the leader is gone. Say so.
	n.leader = ""
	n.resetTimeout()
	n.votes = map[NodeID]bool{n.id: true}
	if len(n.peers) == 1 {
		return n.campaign()
	}
	var out []Message
	for _, p := range n.peers {
		if p == n.id {
			continue
		}
		out = append(out, Message{
			Type: MsgPreVoteReq, From: n.id, To: p,
			// The pre-vote asks about the term we WOULD use, without adopting it.
			Term:         n.currentTerm + 1,
			LastLogIndex: n.log.LastIndex(), LastLogTerm: n.log.LastTerm(),
		})
	}
	return out
}

// campaign starts a real election, incrementing the term.
func (n *Node) campaign() []Message {
	n.state = Candidate
	n.currentTerm++
	n.votedFor = n.id
	n.leader = ""
	n.resetTimeout()
	n.votes = map[NodeID]bool{n.id: true}
	if len(n.votes) >= n.quorum() {
		n.becomeLeader()
		return n.broadcastAppend()
	}
	var out []Message
	for _, p := range n.peers {
		if p == n.id {
			continue
		}
		out = append(out, Message{
			Type: MsgVoteReq, From: n.id, To: p, Term: n.currentTerm,
			LastLogIndex: n.log.LastIndex(), LastLogTerm: n.log.LastTerm(),
		})
	}
	return out
}

// Step processes one message and returns the replies to send.
func (n *Node) Step(m Message) []Message {
	// Rule that dominates everything: a higher term always wins. Whoever sees
	// it steps down, because a higher term means an election happened that this
	// node did not participate in.
	//
	// Pre-vote messages are exempt: they carry a speculative term the sender
	// has not adopted, so reacting to it would be exactly the disruption
	// pre-vote exists to prevent.
	if m.Term > n.currentTerm && m.Type != MsgPreVoteReq && m.Type != MsgPreVoteResp {
		lead := m.From
		if m.Type == MsgVoteReq {
			lead = ""
		}
		n.becomeFollower(m.Term, lead)
	}

	switch m.Type {
	case MsgPreVoteReq:
		return n.handlePreVoteReq(m)
	case MsgPreVoteResp:
		return n.handlePreVoteResp(m)
	case MsgVoteReq:
		return n.handleVoteReq(m)
	case MsgVoteResp:
		return n.handleVoteResp(m)
	case MsgAppendReq:
		return n.handleAppendReq(m)
	case MsgAppendResp:
		return n.handleAppendResp(m)
	}
	return nil
}

func (n *Node) handlePreVoteReq(m Message) []Message {
	// Grant only if the speculative term is ahead AND the candidate's log is at
	// least as up to date. No state is mutated: a pre-vote never changes our
	// term and never records a vote.
	grant := m.Term > n.currentTerm && n.log.UpToDate(m.LastLogIndex, m.LastLogTerm)
	// CheckQuorum refinement: if we have heard from a leader recently, refuse.
	// Otherwise a node that was briefly partitioned can unseat a healthy leader
	// the moment it returns.
	if n.cfg.CheckQuorum && n.leader != "" && n.electionElapsed < n.randomizedTimeout {
		grant = false
	}
	return []Message{{
		Type: MsgPreVoteResp, From: n.id, To: m.From,
		Term: n.currentTerm, Granted: grant,
	}}
}

func (n *Node) handlePreVoteResp(m Message) []Message {
	if n.state != PreCandidate {
		return nil
	}
	if m.Granted {
		n.votes[m.From] = true
	}
	if len(n.votes) >= n.quorum() {
		// A majority would vote for us, so campaigning for real will not
		// disrupt anyone for nothing.
		return n.campaign()
	}
	return nil
}

func (n *Node) handleVoteReq(m Message) []Message {
	grant := false
	switch {
	case m.Term < n.currentTerm:
		// Stale candidate.
	case n.votedFor != "" && n.votedFor != m.From:
		// One vote per node per term. This is half of why two leaders cannot
		// coexist in one term; the other half is that a majority is required.
	case !n.log.UpToDate(m.LastLogIndex, m.LastLogTerm):
		// The election restriction. A candidate missing a committed entry
		// cannot win, which is what makes Leader Completeness hold.
	default:
		grant = true
		n.votedFor = m.From
		n.resetTimeout()
	}
	return []Message{{
		Type: MsgVoteResp, From: n.id, To: m.From,
		Term: n.currentTerm, Granted: grant,
	}}
}

func (n *Node) handleVoteResp(m Message) []Message {
	if n.state != Candidate || m.Term != n.currentTerm {
		return nil
	}
	if m.Granted {
		n.votes[m.From] = true
	}
	if len(n.votes) >= n.quorum() {
		n.becomeLeader()
		return n.broadcastAppend()
	}
	return nil
}

func (n *Node) handleAppendReq(m Message) []Message {
	reply := Message{Type: MsgAppendResp, From: n.id, To: m.From, Term: n.currentTerm}
	if m.Term < n.currentTerm {
		// A deposed leader. Refusing is the fence.
		reply.Hint = n.log.LastIndex()
		return []Message{reply}
	}
	n.leader = m.From
	n.state = Follower
	n.resetTimeout()

	// The Log Matching consistency check. If we do not have prevLogIndex at
	// prevLogTerm, our logs have diverged before this point and appending would
	// create a log that is not a prefix-extension of the leader's.
	if !n.log.MatchesAt(m.PrevLogIndex, m.PrevLogTerm) {
		reply.Success = false
		reply.Hint = n.log.LastIndex()
		return []Message{reply}
	}

	n.log.AppendFrom(m.PrevLogIndex, m.Entries)
	if m.LeaderCommit > n.log.Commit() {
		last := m.PrevLogIndex + Index(len(m.Entries))
		if m.LeaderCommit < last {
			last = m.LeaderCommit
		}
		n.log.SetCommit(last)
		n.applyCommitted()
	}
	reply.Success = true
	reply.MatchIndex = m.PrevLogIndex + Index(len(m.Entries))
	return []Message{reply}
}

func (n *Node) handleAppendResp(m Message) []Message {
	if n.state != Leader || m.Term != n.currentTerm {
		return nil
	}
	n.recentActive[m.From] = true
	if m.Success {
		if m.MatchIndex > n.matchIndex[m.From] {
			n.matchIndex[m.From] = m.MatchIndex
		}
		n.nextIndex[m.From] = n.matchIndex[m.From] + 1
		if n.maybeCommit() {
			return n.broadcastAppend()
		}
		return nil
	}
	// Rejected: back up and retry. The follower's hint lets us skip a whole run
	// rather than decrementing one index per round trip.
	next := n.nextIndex[m.From]
	if m.Hint+1 < next {
		next = m.Hint + 1
	} else if next > 1 {
		next--
	}
	if next < 1 {
		next = 1
	}
	n.nextIndex[m.From] = next
	return []Message{n.appendTo(m.From)}
}

// maybeCommit advances the commit index and reports whether it moved.
func (n *Node) maybeCommit() bool {
	if n.state != Leader {
		return false
	}
	matches := make([]Index, 0, len(n.peers))
	for _, p := range n.peers {
		matches = append(matches, n.matchIndex[p])
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i] > matches[j] })
	// The highest index replicated on a majority.
	candidate := matches[n.quorum()-1]
	if candidate <= n.log.Commit() {
		return false
	}
	// THE FIGURE 8 RULE. A leader may only commit an entry from its OWN term by
	// counting replicas. An entry from a previous term that happens to sit on a
	// majority is NOT safe to commit that way: a later leader with a different
	// history could still overwrite it, and committing it would mean an
	// acknowledged entry later disappeared. It becomes committed indirectly,
	// once an entry from the current term above it commits.
	//
	// This is the subtlest rule in Raft and the one most often omitted.
	if t, ok := n.log.TermAt(candidate); !ok || t != n.currentTerm {
		return false
	}
	n.log.SetCommit(candidate)
	n.applyCommitted()
	return true
}

// applyCommitted feeds newly committed entries to the state machine.
func (n *Node) applyCommitted() {
	for i := Index(len(n.Applied)) + 1; i <= n.log.Commit(); i++ {
		e, ok := n.log.EntryAt(i)
		if !ok {
			break
		}
		n.Applied = append(n.Applied, e.Data)
	}
}

func (n *Node) broadcastAppend() []Message {
	var out []Message
	for _, p := range n.peers {
		if p == n.id {
			continue
		}
		out = append(out, n.appendTo(p))
	}
	return out
}

func (n *Node) appendTo(p NodeID) Message {
	next := n.nextIndex[p]
	if next < 1 {
		next = 1
	}
	prev := next - 1
	prevTerm, _ := n.log.TermAt(prev)
	return Message{
		Type: MsgAppendReq, From: n.id, To: p, Term: n.currentTerm,
		PrevLogIndex: prev, PrevLogTerm: prevTerm,
		Entries:      n.log.From(next),
		LeaderCommit: n.log.Commit(),
	}
}
