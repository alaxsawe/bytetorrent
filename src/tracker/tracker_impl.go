package tracker

/* Implementation of a Tracker Server:
 *
 * Data Storage:
 *   torrents   map[torrentproto.ID]torrentproto.Torrent
 *     - Maps the torrentID to the torrent's information
 *   peers      map[torrentproto.ChunkID](map[string](struct{}))
 *     - Maps the chunkID (which includes torrentID and chunkNum)
 *       to a map whose keys are the clients that own that torrent
 *
 * Goroutines:
 *   eventHandler
 *     - rpc's send the args to the eventHandler to deal with
 *   paxosHandler
 *     - eventHandler sends paxosHandler any pending operations
 *     - broadcasts the paxos messages to the other nodes in the cluster
 *
 * Other Notes:
 * - paxosHandler uses exponential back-off to deal with dualing leaders
 * - Upon receiving a paxos message for a "future" seqNum,
 *   the tracker pings the other nodes, asking for any committed actions
 *   that it missed.
 * - Paxos Cluster is initialized using the master/slave model
 *   (as in storage server)
 */

import (
	"container/list"
	"net"
	"net/http"
	"net/rpc"
	"runtime"
	"strconv"
	"sync"
	"time"

	"torrent"
	"torrent/torrentproto"
	"tracker/trackerproto"
)

// The time between RegisterServer calls from a slave server, in seconds
const REGISTER_PERIOD = 1

type PaxosType int

const (
	PaxosPrepare PaxosType = iota
	PaxosAccept
	PaxosCommit
)

type Register struct {
	Args  *trackerproto.RegisterArgs
	Reply chan *trackerproto.RegisterReply
}

type Get struct {
	Args  *trackerproto.GetArgs
	Reply chan *trackerproto.GetReply
}

type Prepare struct {
	Args  *trackerproto.PrepareArgs
	Reply chan *trackerproto.PrepareReply
}

type Accept struct {
	Args  *trackerproto.AcceptArgs
	Reply chan *trackerproto.AcceptReply
}

type Commit struct {
	Args  *trackerproto.CommitArgs
	Reply chan *trackerproto.CommitReply
}

type Request struct {
	Args  *trackerproto.RequestArgs
	Reply chan *trackerproto.RequestReply
}

type Confirm struct {
	Args  *trackerproto.ConfirmArgs
	Reply chan *trackerproto.UpdateReply
}

type Report struct {
	Args  *trackerproto.ReportArgs
	Reply chan *trackerproto.UpdateReply
}

type Create struct {
	Args  *trackerproto.CreateArgs
	Reply chan *trackerproto.UpdateReply
}

type GetTrackers struct {
	Args  *trackerproto.TrackersArgs
	Reply chan *trackerproto.TrackersReply
}

type Pending struct {
	Value trackerproto.Operation
	Reply chan *trackerproto.UpdateReply
}

type PaxosReply struct {
	Status    trackerproto.Status
	ReqPaxNum int
	PaxNum    int
	Value     trackerproto.Operation
	SeqNum    int
}

type PaxosBroadcast struct {
	MyN    int
	Type   PaxosType
	Value  trackerproto.Operation
	SeqNum int
	Reply  chan *PaxosReply
}

type trackerServer struct {
	// Cluster Set-up
	nodes                []trackerproto.Node
	numNodes             int
	masterServerHostPort string
	port                 int
	registers            chan *Register
	nodeID               int
	trackers             []*rpc.Client

	// Channels for rpc calls
	prepares    chan *Prepare
	accepts     chan *Accept
	commits     chan *Commit
	gets        chan *Get
	requests    chan *Request
	confirms    chan *Confirm
	reports     chan *Report
	creates     chan *Create
	getTrackers chan *GetTrackers
	pending     chan *Pending
	outOfDate   chan int

	// Paxos Stuff
	myN      int
	highestN int
	accN     int
	accV     trackerproto.Operation

	// Sequencing / Logging
	seqNum int
	log    map[int]trackerproto.Operation

	// Actual data storage
	torrents   map[torrentproto.ID]torrentproto.Torrent         // Map the torrentID to the Torrent information
	peers      map[torrentproto.ChunkID](map[string](struct{})) // Maps chunk info -> list of host:port with that chunk
	pendingOps *list.List
	pendingMut *sync.Mutex

	// Used for debugging
	dbclose    chan struct{}
	dbstall    chan int
	dbstallall chan struct{}
	dbcontinue chan struct{}
}

// If masterServerHostPort is "", then this assumes that it is the master server
// numNodes tells us how many nodes are in the Paxos Cluster
// nodeID is this node's position in the cluster (each node should have a different id, 0 <= nodeID < numNodes)
// port is the port to start this server on
func NewTrackerServer(masterServerHostPort string, numNodes, port, nodeID int) (Tracker, error) {
	t := &trackerServer{
		masterServerHostPort: masterServerHostPort,
		nodeID:               nodeID,
		nodes:                nil,
		numNodes:             numNodes,
		port:                 port,
		accepts:              make(chan *Accept),
		commits:              make(chan *Commit),
		confirms:             make(chan *Confirm),
		gets:                 make(chan *Get),
		prepares:             make(chan *Prepare),
		registers:            make(chan *Register),
		reports:              make(chan *Report),
		requests:             make(chan *Request),
		creates:              make(chan *Create),
		getTrackers:          make(chan *GetTrackers),
		pending:              make(chan *Pending),
		myN:                  nodeID,
		highestN:             0,
		accV:                 trackerproto.Operation{OpType: trackerproto.None},
		seqNum:               0,
		log:                  make(map[int]trackerproto.Operation),
		torrents:             make(map[torrentproto.ID]torrentproto.Torrent),
		peers:                make(map[torrentproto.ChunkID](map[string](struct{}))),
		trackers:             make([]*rpc.Client, numNodes),
		outOfDate:            make(chan int, 1),
		pendingOps:           list.New(),
		pendingMut:           &sync.Mutex{},
		dbclose:              make(chan struct{}),
		dbstall:              make(chan int),
		dbstallall:           make(chan struct{})}

	// Configure this TrackerServer to receive RPCs over HTTP on a
	// trackerproto.Tracker interface.
	if regErr := rpc.RegisterName("RemoteTracker", WrapRemote(t)); regErr != nil {
		return nil, regErr
	}

	// New configure this TrackerServer to receive RPCs over HTTP on a
	// trackerproto.Paxos interface
	if regErr := rpc.RegisterName("PaxosTracker", WrapPaxos(t)); regErr != nil {
		return nil, regErr
	}
	rpc.HandleHTTP()

	// Attempt to service connections on the given port.
	ln, lnErr := net.Listen("tcp", net.JoinHostPort("localhost", strconv.Itoa(port)))
	if lnErr != nil {
		return nil, lnErr
	}

	go http.Serve(ln, nil)

	// Wait for all TrackerServers to join the ring.
	var joinErr error
	if masterServerHostPort == "" {
		// This is the master StorageServer.
		joinErr = t.masterAwaitJoin()

		// Yield to reduce the chance that the master returns before the slaves.
		runtime.Gosched()
	} else {
		// This is a slave TrackerServer.
		joinErr = t.slaveAwaitJoin()
	}

	if joinErr != nil {
		return nil, joinErr
	} else {
		// We've registered with the master, and gotten a list of all servers.
		// We need to connect to all of them over rpc,
		// then add these data points to t.trackers
		for _, node := range t.nodes {
			trackerproto, err := rpc.DialHTTP("tcp", node.HostPort)
			if err != nil {
				return nil, err
			}
			t.trackers[node.NodeID] = trackerproto
		}
	}

	// Start this TrackerServer's eventHandler, which will respond to RPCs,
	// and return it.
	go t.eventHandler()

	// Spawn a goroutine to talk to the other Paxos Nodes
	go t.paxosHandler()

	return t, nil
}

func (t *trackerServer) RegisterServer(args *trackerproto.RegisterArgs, reply *trackerproto.RegisterReply) error {
	replyChan := make(chan *trackerproto.RegisterReply)
	register := &Register{
		Args:  args,
		Reply: replyChan}
	t.registers <- register
	*reply = *(<-replyChan)
	return nil
}

func (t *trackerServer) GetOp(args *trackerproto.GetArgs, reply *trackerproto.GetReply) error {
	replyChan := make(chan *trackerproto.GetReply)
	get := &Get{
		Args:  args,
		Reply: replyChan}
	t.gets <- get
	*reply = *(<-replyChan)
	return nil
}

func (t *trackerServer) Prepare(args *trackerproto.PrepareArgs, reply *trackerproto.PrepareReply) error {
	replyChan := make(chan *trackerproto.PrepareReply)
	prepare := &Prepare{
		Args:  args,
		Reply: replyChan}
	t.prepares <- prepare
	*reply = *(<-replyChan)
	return nil
}

func (t *trackerServer) Accept(args *trackerproto.AcceptArgs, reply *trackerproto.AcceptReply) error {
	replyChan := make(chan *trackerproto.AcceptReply)
	accept := &Accept{
		Args:  args,
		Reply: replyChan}
	t.accepts <- accept
	*reply = *(<-replyChan)
	return nil
}

func (t *trackerServer) Commit(args *trackerproto.CommitArgs, reply *trackerproto.CommitReply) error {
	replyChan := make(chan *trackerproto.CommitReply)
	commit := &Commit{
		Args:  args,
		Reply: replyChan}
	t.commits <- commit
	*reply = *(<-replyChan)
	return nil
}

func (t *trackerServer) ReportMissing(args *trackerproto.ReportArgs, reply *trackerproto.UpdateReply) error {
	replyChan := make(chan *trackerproto.UpdateReply)
	report := &Report{
		Args:  args,
		Reply: replyChan}
	t.reports <- report
	*reply = *(<-replyChan)
	return nil
}

func (t *trackerServer) ConfirmChunk(args *trackerproto.ConfirmArgs, reply *trackerproto.UpdateReply) error {
	replyChan := make(chan *trackerproto.UpdateReply)
	confirm := &Confirm{
		Args:  args,
		Reply: replyChan}
	t.confirms <- confirm
	*reply = *(<-replyChan)
	return nil
}

func (t *trackerServer) CreateEntry(args *trackerproto.CreateArgs, reply *trackerproto.UpdateReply) error {
	replyChan := make(chan *trackerproto.UpdateReply)
	create := &Create{
		Args:  args,
		Reply: replyChan}
	t.creates <- create
	*reply = *(<-replyChan)
	return nil
}

func (t *trackerServer) RequestChunk(args *trackerproto.RequestArgs, reply *trackerproto.RequestReply) error {
	replyChan := make(chan *trackerproto.RequestReply)
	request := &Request{
		Args:  args,
		Reply: replyChan}
	t.requests <- request
	*reply = *(<-replyChan)
	return nil
}

func (t *trackerServer) GetTrackers(args *trackerproto.TrackersArgs, reply *trackerproto.TrackersReply) error {
	replyChan := make(chan *trackerproto.TrackersReply)
	trackers := &GetTrackers{
		Args:  args,
		Reply: replyChan}
	t.getTrackers <- trackers
	*reply = *(<-replyChan)
	return nil
}

// Waits for all slave trackerServers to call the master's RegisterServer RPC.
func (t *trackerServer) masterAwaitJoin() error {
	// Initialize the array of Nodes, and create a map of all slaves that have
	// registered (and another of those who have received an OK) for fast
	// lookup.
	nodeIDs := make(map[int]struct{})
	okIDs := make(map[int]struct{})
	t.nodes = make([]trackerproto.Node, 0)

	// Count the master server.
	//
	// NOTE: We always list the host as localhost.
	nodeIDs[t.nodeID] = struct{}{}
	okIDs[t.nodeID] = struct{}{}
	t.nodes = append(t.nodes, trackerproto.Node{
		HostPort: net.JoinHostPort("localhost", strconv.Itoa(t.port)),
		NodeID:   t.nodeID})

	// Loop until we've heard from (and replied to) all nodes.
	for len(okIDs) != t.numNodes {
		// A node wants to register.
		register := <-t.registers
		node := register.Args.TrackerInfo
		if _, ok := nodeIDs[node.NodeID]; !ok {
			// This is a new nodeId.
			nodeIDs[node.NodeID] = struct{}{}
			t.nodes = append(t.nodes, node)
		}

		// Determine the status to return.
		var status trackerproto.Status
		if len(t.nodes) == t.numNodes {
			status = trackerproto.OK

			// If this node will receive an OK, record this.
			okIDs[node.NodeID] = struct{}{}
		} else {
			status = trackerproto.NotReady
		}

		// Reply to the node.
		nodes := make([]trackerproto.Node, len(t.nodes))
		copy(nodes, t.nodes)
		register.Reply <- &trackerproto.RegisterReply{
			Status:   status,
			Trackers: nodes}
	}

	// We've heard from all nodes.
	return nil
}

// Waits for the master storageServer to accept a slave's RegisterServer RPC
// and confirm that all other slaves have joined.
func (t *trackerServer) slaveAwaitJoin() error {
	// Connect to the master trackerServer, retrying until we succeed.
	var conn *rpc.Client
	for conn == nil {
		if conn, _ = rpc.DialHTTP("tcp", t.masterServerHostPort); conn == nil {
			// Sleep, and try again later.
			time.Sleep(time.Second * time.Duration(REGISTER_PERIOD))
		}
	}

	// Make RegisterServer RPCs to the master trackerServer until it confirms
	// that the ring is complete.
	args := &trackerproto.RegisterArgs{
		TrackerInfo: trackerproto.Node{
			HostPort: net.JoinHostPort("localhost", strconv.Itoa(t.port)),
			NodeID:   t.nodeID}}
	reply := &trackerproto.RegisterReply{}

	for {
		if callErr := conn.Call("PaxosTracker.RegisterServer", args, reply); callErr != nil {
			// Failed to make RPC to storageServer.
			return callErr
		}

		if reply.Status == trackerproto.OK {
			// The ring is ready.
			break
		}

		// Wait for a set period before trying again.
		time.Sleep(time.Second * time.Duration(REGISTER_PERIOD))
	}

	// Record which nodes are in the ring, and return.
	t.nodes = make([]trackerproto.Node, len(reply.Trackers))
	copy(t.nodes, reply.Trackers)
	return nil
}

func (t *trackerServer) eventHandler() {
	for {
		select {
		case <-t.dbclose:
			// Closing (for debugging / testing reasons)
			return
		case <-t.dbstallall:
			// Stalling (for debugging / testing reasons)
			// Wait until we receive a signal on t.dbcontinue,
			// then keep going
			<-t.dbcontinue
		case s := <-t.dbstall:
			// Someone requested that we stall for s seconds.
			t.dbcontinue = make(chan struct{})
			close(t.dbstallall)
			wait := time.Duration(s) * time.Second
			time.AfterFunc(wait,
				func() {
					t.dbstallall = make(chan struct{})
					close(t.dbcontinue)
				})
		case seqNum := <-t.outOfDate:
			// t is out of date
			// Needs to catch up to seqNum
			t.catchUp(seqNum)
		case prep := <-t.prepares:
			// Handle prepare messages
			reply := &trackerproto.PrepareReply{
				PaxNum: t.accN,
				Value:  t.accV,
				SeqNum: t.seqNum}
			if prep.Args.SeqNum != t.seqNum {
				if prep.Args.SeqNum < t.seqNum {
					// Other guy is out of date,
					// Let him know and send the correct value
					reply.Status = trackerproto.OutOfDate
					reply.Value = t.log[prep.Args.SeqNum]
					prep.Reply <- reply
				} else {
					// This tracker is out of date
					// It should not be validating updates
					reply.Status = trackerproto.Reject
					prep.Reply <- reply
					// We spawn a goroutine, because we don't want the eventHandler to wait for itself
					go func() { t.outOfDate <- prep.Args.SeqNum }()
				}
			} else if prep.Args.PaxNum < t.highestN {
				reply.Status = trackerproto.Reject
				prep.Reply <- reply
			} else {
				t.highestN = prep.Args.PaxNum
				reply.Status = trackerproto.OK
				prep.Reply <- reply
			}
		case acc := <-t.accepts:
			// Handle accept messages
			var status trackerproto.Status
			if acc.Args.SeqNum < t.seqNum {
				status = trackerproto.OutOfDate
			} else if acc.Args.SeqNum > t.seqNum {
				// Spawn a goroutine, lest the eventhandler wait for itself
				go func() { t.outOfDate <- acc.Args.SeqNum }()
			} else if acc.Args.PaxNum < t.highestN {
				status = trackerproto.Reject
			} else {
				status = trackerproto.OK
				t.highestN = acc.Args.PaxNum
				t.accN = acc.Args.PaxNum
				t.accV = acc.Args.Value
			}
			acc.Reply <- &trackerproto.AcceptReply{Status: status}
		case com := <-t.commits:
			// Handle commit messages
			v := com.Args.Value
			if com.Args.SeqNum == t.seqNum {
				t.logOp(t.seqNum, v)
				t.commitOp(v)
			} else {
				t.logOp(com.Args.SeqNum, v)
			}
			com.Reply <- &trackerproto.CommitReply{}
		case get := <-t.gets:
			// Another tracker has requested a previously commited op
			s := get.Args.SeqNum
			if s >= t.seqNum {
				get.Reply <- &trackerproto.GetReply{Status: trackerproto.OutOfDate}
			} else {
				get.Reply <- &trackerproto.GetReply{
					Status: trackerproto.OK,
					Value:  t.log[s]}
			}
		case rep := <-t.reports:
			// A client has reported that it does not have a chunk
			tor, ok := t.torrents[rep.Args.Chunk.ID]
			if !ok {
				// File does not exist
				rep.Reply <- &trackerproto.UpdateReply{Status: trackerproto.FileNotFound}
			} else if rep.Args.Chunk.ChunkNum < 0 || rep.Args.Chunk.ChunkNum >= torrent.NumChunks(tor) {
				// ChunkNum is not right for this file
				rep.Reply <- &trackerproto.UpdateReply{Status: trackerproto.OutOfRange}
			} else {
				// Put the operation in the pending list
				op := trackerproto.Operation{
					OpType:     trackerproto.Delete,
					Chunk:      rep.Args.Chunk,
					ClientAddr: rep.Args.HostPort}
				// Spawn a goroutine, because we don't want the eventHandler to wait for anyone
				go func() { t.pending <- &Pending{Value: op, Reply: rep.Reply} }()
			}
		case conf := <-t.confirms:
			// A client has confirmed that it has a chunk
			tor, ok := t.torrents[conf.Args.Chunk.ID]
			if !ok {
				// File does not exist
				conf.Reply <- &trackerproto.UpdateReply{Status: trackerproto.FileNotFound}
			} else if conf.Args.Chunk.ChunkNum < 0 || conf.Args.Chunk.ChunkNum >= torrent.NumChunks(tor) {
				// ChunkNum is not right for this file
				conf.Reply <- &trackerproto.UpdateReply{Status: trackerproto.OutOfRange}
			} else {
				// Put the operation in the pending list
				op := trackerproto.Operation{
					OpType:     trackerproto.Add,
					Chunk:      conf.Args.Chunk,
					ClientAddr: conf.Args.HostPort}
				// Spawn a goroutine, because the event handler waits for no-man!
				go func() { t.pending <- &Pending{Value: op, Reply: conf.Reply} }()
			}
		case cre := <-t.creates:
			// First check that all of the suggested nodes are in the cluster
			correctTrackers := true
			for _, tortrack := range cre.Args.Torrent.TrackerNodes {
				inCluster := false
				for _, tracker := range t.nodes {
					if tracker.HostPort == tortrack.HostPort {
						inCluster = true
					}
				}
				correctTrackers = correctTrackers && inCluster
			}

			// Now make sure that all of the cluster's nodes appear in the list
			for _, tracker := range t.nodes {
				inCluster := false
				for _, tortrack := range cre.Args.Torrent.TrackerNodes {
					if tracker.HostPort == tortrack.HostPort {
						inCluster = true
					}
				}
				correctTrackers = correctTrackers && inCluster
			}

			if !correctTrackers {
				cre.Reply <- &trackerproto.UpdateReply{Status: trackerproto.InvalidTrackers}
			}

			// A client has requested to create a new file
			_, ok := t.torrents[cre.Args.Torrent.ID]
			if !ok {
				// ID not in use,
				// So make the pending request for this
				op := trackerproto.Operation{
					OpType:  trackerproto.Create,
					Torrent: cre.Args.Torrent}
				// Spawn a goroutine, because we don't want the eventhandler to block
				go func() { t.pending <- &Pending{Value: op, Reply: cre.Reply} }()
			} else {
				// File already exists, so tell the client that this ID is invalid
				cre.Reply <- &trackerproto.UpdateReply{Status: trackerproto.InvalidID}
			}
		case req := <-t.requests:
			// A client has requested a list of users with a certain chunk
			tor, ok := t.torrents[req.Args.Chunk.ID]
			if !ok {
				// File does not exist
				req.Reply <- &trackerproto.RequestReply{Status: trackerproto.FileNotFound}
			} else if req.Args.Chunk.ChunkNum < 0 || req.Args.Chunk.ChunkNum >= torrent.NumChunks(tor) {
				// ChunkNum is not right for this file
				req.Reply <- &trackerproto.RequestReply{Status: trackerproto.OutOfRange}
			} else {
				// Get a list of all peers, then respond
				peers := make([]string, 0)
				for k, _ := range t.peers[req.Args.Chunk] {
					peers = append(peers, k)
				}
				req.Reply <- &trackerproto.RequestReply{
					Status:    trackerproto.OK,
					Peers:     peers,
					ChunkHash: tor.ChunkHashes[req.Args.Chunk.ChunkNum]}
			}
		case gt := <-t.getTrackers:
			// A client has requested a list of users with a certain chunk
			hostPorts := make([]string, t.numNodes)
			for i, node := range t.nodes {
				hostPorts[i] = node.HostPort
			}
			gt.Reply <- &trackerproto.TrackersReply{
				Status:    trackerproto.OK,
				HostPorts: hostPorts}
		}
	}
}

// Logs the operation at the given seqNum
func (t *trackerServer) logOp(seqNum int, v trackerproto.Operation) {
	t.log[seqNum] = v
}

// t commits the operation to memory, and logs it
func (t *trackerServer) commitOp(v trackerproto.Operation) {
	t.seqNum++
	t.accN = 0
	t.accV = trackerproto.Operation{OpType: trackerproto.None}

	// Now make the change
	key := v.Chunk
	m, ok := t.peers[key]
	if !ok {
		t.peers[key] = make(map[string](struct{}))
		m = t.peers[key]
	}

	if v.OpType == trackerproto.Add {
		m[v.ClientAddr] = struct{}{}
	} else if v.OpType == trackerproto.Delete {
		delete(m, v.ClientAddr)
	} else if v.OpType == trackerproto.Create {
		t.torrents[v.Torrent.ID] = v.Torrent
	}

	// Go through the list of ops that we have pending
	// If this is one of those, then respond
	t.pendingMut.Lock()
	for e := t.pendingOps.Front(); e != nil; e = e.Next() {
		pen := e.Value.(*Pending).Value
		if pen.OpType == v.OpType && pen.Chunk == v.Chunk && pen.ClientAddr == v.ClientAddr {
			t.pendingOps.Remove(e)
			e.Value.(*Pending).Reply <- &trackerproto.UpdateReply{Status: trackerproto.OK}
		}
	}
	t.pendingMut.Unlock()

	// Check if the next thing is in the log already
	// If it is, then commit it.
	if _, ok := t.log[t.seqNum]; ok {
		t.commitOp(t.log[t.seqNum])
	}
}

// t contacts other servers in an attempt to catch-up
// with missed changes
func (t *trackerServer) catchUp(target int) {
	current := (t.nodeID + 1) % t.numNodes
	for t.seqNum < target {
		args := &trackerproto.GetArgs{SeqNum: t.seqNum}
		reply := &trackerproto.GetReply{}
		if err := t.trackers[current].Call("PaxosTracker.GetOp", args, reply); err != nil {
			// there was an issue, so let's try another server
			current = (current + 1) % t.numNodes
			// If we've looped around the entire way and we're not done,
			// then the given target was probably too ambitious
			if current == t.nodeID {
				target = t.seqNum
			}
		} else {
			if reply.Status == trackerproto.OK {
				// This increments t.seqNum
				t.commitOp(reply.Value)
			} else {
				// Server didn't have operation, so let's try another server
				current = (current + 1) % t.numNodes
				// If we've looped around the entire way and we're not done,
				// then the given target was probably too ambitious
				if current == t.nodeID {
					target = t.seqNum
				}
			}
		}
	}
}

// Send mess to the paxos server with the given id
func (t *trackerServer) sendMess(id int, mess *PaxosBroadcast) {
	reqPaxNum := mess.MyN
	if mess.Type == PaxosPrepare {
		args := &trackerproto.PrepareArgs{
			PaxNum: reqPaxNum,
			SeqNum: mess.SeqNum}
		reply := &trackerproto.PrepareReply{}
		if err := t.trackers[id].Call("PaxosTracker.Prepare", args, reply); err != nil {
			// Error: Tell the paxosHandler that we were "rejected"
			mess.Reply <- &PaxosReply{Status: trackerproto.Reject}
		} else {
			// Pass the data back to the PaxosHandler
			mess.Reply <- &PaxosReply{
				Status:    reply.Status,
				ReqPaxNum: reqPaxNum,
				PaxNum:    reply.PaxNum,
				Value:     reply.Value,
				SeqNum:    reply.SeqNum}
		}
	} else if mess.Type == PaxosAccept {
		args := &trackerproto.AcceptArgs{
			PaxNum: reqPaxNum,
			SeqNum: mess.SeqNum,
			Value:  mess.Value}
		reply := &trackerproto.AcceptReply{}
		if err := t.trackers[id].Call("PaxosTracker.Accept", args, reply); err != nil {
			// Error: Tell the paxosHandler that we were "rejected"
			mess.Reply <- &PaxosReply{Status: trackerproto.Reject}
		} else {
			mess.Reply <- &PaxosReply{
				Status:    reply.Status,
				ReqPaxNum: reqPaxNum,
				SeqNum:    mess.SeqNum}
		}
	} else if mess.Type == PaxosCommit {
		args := &trackerproto.CommitArgs{
			SeqNum: mess.SeqNum,
			Value:  mess.Value}
		reply := &trackerproto.CommitReply{}
		t.trackers[id].Call("PaxosTracker.Commit", args, reply)

		// This tells the paxosHandler when this tracker has commited the result
		if id == t.nodeID {
			mess.Reply <- &PaxosReply{
				Status: trackerproto.OK}
		}
	}
}

// This is the function that broadcasts paxos messages and collects replies
// Most of the paxos-leader logic takes place here
func (t *trackerServer) paxosHandler() {
	initPaxos := make(chan struct{}, t.numNodes)

	// reply channels
	prepareReply := make(chan *PaxosReply)
	acceptReply := make(chan *PaxosReply)
	comReply := make(chan *PaxosReply)

	// Keep track of the current phase in the paxos-round
	prepPhase := false
	accPhase := false
	inPaxos := false

	// The accepted value
	accN := 0
	accV := trackerproto.Operation{OpType: trackerproto.None}

	backoff := 2
	oks := 0
	var T *time.Timer
	for {
		select {
		case <-t.dbclose:
			// Closing (for debugging / testing reasons)
			return
		case <-t.dbstallall:
			// Stalling (for debugging / testing reasons)
			// Wait until we receive a signal on t.dbcontinue,
			// then keep going
			<-t.dbcontinue
		case <-initPaxos:
			// Initialize values
			inPaxos = true
			accV = trackerproto.Operation{OpType: trackerproto.None}
			t.myN = (t.highestN - (t.highestN % t.numNodes)) + (t.numNodes + t.nodeID)
			oks = 0
			prepPhase = true
			accPhase = false

			// Set a timer to tell us when to restart the paxos round
			backoff = 2 * (backoff + t.nodeID)
			wait := time.Second * time.Duration(backoff)
			T = time.AfterFunc(wait, func() { initPaxos <- struct{}{} })

			// Broadcast the prepare message
			for id := 0; id < t.numNodes; id++ {
				mess := &PaxosBroadcast{
					MyN:    t.myN,
					Type:   PaxosPrepare,
					Reply:  prepareReply,
					SeqNum: t.seqNum}
				go t.sendMess(id, mess)
			}
		case op := <-t.pending:
			t.pendingMut.Lock()
			t.pendingOps.PushBack(op)
			t.pendingMut.Unlock()
			if !inPaxos {
				// We don't want to worry about the paxosHandler waiting for itself
				go func() { initPaxos <- struct{}{} }()
			}
		case prep := <-prepareReply:
			// First check that this is a response to the current PaxosMessage
			if prep.ReqPaxNum == t.myN && prepPhase {
				if prep.Status == trackerproto.OK {
					oks++
					if prep.Value.OpType != trackerproto.None {
						if prep.PaxNum > accN {
							accN = prep.PaxNum
							accV = prep.Value
						}
					}
				} else if prep.Status == trackerproto.OutOfDate {
					// We spawn a goroutine for this,
					// because we don't want the paxosHandler to block
					// waiting for the eventHandler
					go func() { t.outOfDate <- prep.SeqNum }()
				}

				if oks > (t.numNodes / 2) {
					T.Stop() // Stop the timer that would tell us to restart Paxos
					if accV.OpType == trackerproto.None {
						// If no node had accepted a value,
						// check that there's something in our pending list
						t.pendingMut.Lock()
						if t.pendingOps.Len() > 0 {
							e := t.pendingOps.Front()
							accV = e.Value.(*Pending).Value
						}
						t.pendingMut.Unlock()
					}

					if accV.OpType != trackerproto.None {
						// Prepare variables for next phase of paxos
						oks = 0
						prepPhase = false
						accPhase = true

						// Reset timer
						wait := time.Second * time.Duration(backoff)
						T = time.AfterFunc(wait, func() { initPaxos <- struct{}{} })

						// Broadcast accept message
						for id := 0; id < t.numNodes; id++ {
							mess := &PaxosBroadcast{
								MyN:    t.myN,
								Type:   PaxosAccept,
								Reply:  acceptReply,
								SeqNum: t.seqNum,
								Value:  accV}
							go t.sendMess(id, mess)
						}
					} else {
						inPaxos = false
					}
				}
			}
		case acc := <-acceptReply:
			// Received the reply to an accept message
			if acc.ReqPaxNum == t.myN && accPhase {
				if acc.Status == trackerproto.OK {
					oks++
				}

				if oks > (t.numNodes / 2) {
					T.Stop() // Stop the timer
					accPhase = false
					backoff = 2
					comReply = make(chan *PaxosReply)

					// Broadcast the commit message
					for id := 0; id < t.numNodes; id++ {
						mess := &PaxosBroadcast{
							MyN:    t.myN,
							Type:   PaxosCommit,
							Reply:  comReply,
							SeqNum: t.seqNum,
							Value:  accV}
						go t.sendMess(id, mess)
					}
				}
			}
		case com := <-comReply:
			// This line says:
			//  "wait until this tracker has committed before continuing"
			if com.Status == trackerproto.OK {
				t.pendingMut.Lock()
				if t.pendingOps.Len() > 0 {
					initPaxos <- struct{}{}
				} else {
					accV = trackerproto.Operation{OpType: trackerproto.None}
					inPaxos = false
				}
				t.pendingMut.Unlock()
			}
		}
	}
}

// DebugClose is used only in debugging.
// Lets you tell the tracker to stop doing things for stall-many seconds
// If stall <= 0, then it just shuts down.
func (t *trackerServer) DebugStall(stall int) {
	if stall <= 0 {
		close(t.dbclose)
	} else {
		t.dbstall <- stall
	}
}
