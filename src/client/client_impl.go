package client

import (
    "crypto/sha1"
    "errors"
    "math/rand"
    "net"
    "net/http"
    "net/rpc"
    "os"
    "time"

    "client/clientproto"
    "tracker/trackerproto"
    "torrent"
    "torrent/torrentproto"
)

// The client's representation of a request to get a chunk.
type Get struct {
    Args *clientproto.GetArgs
    Reply chan *clientproto.GetReply
}

// The client's representation of a request to close the client.
type Close struct {
    // The client passes back any error involved with closing on this channel.
    Reply chan error
}

// The client's representation of a request to offer a file to a Tracker.
type Offer struct {
    // A Torrent for the file being offered.
    Torrent torrentproto.Torrent

    // The local path to the file being offered.
    Path string

    // The client passes back any error involved with offering on this channel.
    Reply chan error
}

// The client's representation of a request to download a file.
type Download struct {
    // A Torrent for the file to download.
    Torrent torrentproto.Torrent

    // The local path to the location to which the file should download.
    Path string
    
    // The client passes back any error involved with downloading on this channel.
    Reply chan error
}

// A ByteTorrent Client implementation.
type client struct {
    // A map from Torrent IDs to associated local file states
    localFiles map[torrentproto.ID]*clientproto.LocalFile

    // Requests to get chunks from this client.
    gets chan *Get

    // Push to this channel to request that the client close.
    closes chan *Close

    // Push to this channel to request that the client downloads files.
    downloads chan *Download

    // Push to this channel to request that the client offer a file.
    offers chan *Offer

    // Go routines pass the IDs of successfully downloaded chunks to the
    // eventHandler via this channel.
    downloadedChunks chan torrentproto.ChunkID

    // This client's hostport.
    hostPort string

    // A listener which the Client will update when it changes local file.
    lfl LocalFileListener
}

// New creates and starts a new ByteTorrent Client.
func NewClient(localFiles map[torrentproto.ID]*clientproto.LocalFile, lfl LocalFileListener, hostPort string) (Client, error) {
    c := & client {
        localFiles: localFiles,
        lfl: lfl,
        gets: make(chan *Get),
        closes: make(chan *Close),
        offers: make(chan *Offer),
        downloads: make(chan *Download),
        downloadedChunks: make(chan torrentproto.ChunkID),
        hostPort: hostPort}

    // Configure this Client to receive RPCs on RemoteClient at hostPort.
    if ln, err := net.Listen("tcp", hostPort); err != nil {
        // Failed to listen on the given host:port.
        return nil, err
    } else if err := rpc.RegisterName("RemoteClient", Wrap(c)); err != nil {
        // Failed to register this Client for RPCs as a RemoteClient.
        return nil, err
    } else {
        // Successfully registered to receive RPCs.
        // Handle these RPCs and other Client events.
        // Return the started Client.
        rpc.HandleHTTP()
        go http.Serve(ln, nil)
        go c.eventHandler()
        return c, nil
    }
}

func (c *client) GetChunk(args *clientproto.GetArgs, reply *clientproto.GetReply) error {
    replyChan := make(chan *clientproto.GetReply)
    get := &Get{
        Args: args,
        Reply: replyChan}
    c.gets <- get
    *reply = *(<-replyChan)
    return nil
}

func (c *client) OfferFile(t torrentproto.Torrent, path string) error {
    replyChan := make(chan error)
    offer := & Offer {
        Torrent: t,
        Path: path,
        Reply: replyChan}
    c.offers <- offer
    return <- replyChan
}

func (c *client) DownloadFile(t torrentproto.Torrent, path string) error {
    replyChan := make(chan error)
    download := & Download {
        Torrent: t,
        Path: path,
        Reply: replyChan}
    c.downloads <- download
    return <-replyChan
}

func (c *client) Close() error {
    replyChan := make(chan error)
    cl := & Close {
        Reply: replyChan}
    c.closes <- cl
    return <-replyChan
}

// eventHandler synchronizes all events on this Client.
func (c *client) eventHandler() {
    for {
        select {

        // The user has supplied a torrent and requested a download.
        // Service the download asynchronously, and respond to the user
        // when done.
        // The IDs of successfully downloaded chunks will be passed back to
        // the eventHandler as they arrive.
        case download := <- c.downloads:
            // Create an entry for this torrent ID.
            localFile := & clientproto.LocalFile {
                Torrent: download.Torrent,
                Path: download.Path,
                Chunks: make(map[int]struct{})}
            c.localFiles[download.Torrent.ID] = localFile

            // Inform this Client's LocalFileListener that local files have
            // been added.
            c.lfl.OnChange(& clientproto.LocalFileChange {
                LocalFile: localFile,
                Operation: clientproto.LocalFileAdd})

            // Asynchronously download chunks of the file for this torrent.
            go c.downloadFile(download)

        // Another Client has requested a chunk.
        case get := <- c.gets:
            torrentID, chunkNum := get.Args.ChunkID.ID, get.Args.ChunkID.ChunkNum
            if localFile, ok := c.localFiles[torrentID]; !ok {
                // This Client does not know about a local file which
                // corresponds to the requested Torrent ID.
                get.Reply <- & clientproto.GetReply {
                    Status: clientproto.ChunkNotFound,
                    Chunk: nil}
            } else if _, ok := localFile.Chunks[chunkNum]; !ok {
                // This Client knows about the requested file,
                // but does not have the requested chunk.
                get.Reply <- & clientproto.GetReply {
                    Status: clientproto.ChunkNotFound,
                    Chunk: nil}
            } else if file, err := os.Open(localFile.Path); err != nil {
                // The Client thought that it had the requested chunk,
                // but cannot open the file containing the chunk.
                get.Reply <- & clientproto.GetReply {
                    Status: clientproto.ChunkNotFound,
                    Chunk: nil}
            } else if chunk, err := torrent.ReadChunk(localFile.Torrent, file, chunkNum); err != nil {
                // The Client could not get the requested chunk from the file.
                get.Reply <- & clientproto.GetReply {
                    Status: clientproto.ChunkNotFound,
                    Chunk: nil}
            } else {
                // Got the requested chunk. Send it back to the requesting
                // client.
                get.Reply <- & clientproto.GetReply {
                    Status: clientproto.OK,
                    Chunk: chunk}
            }

        // Close the client.
        case cl := <- c.closes:
            cl.Reply <- nil
            return

        // The user wants to offer a file to a Tracker.
        // Record on the Client that this file is available.
        // Then, inform the relevant Tracker.
        case offer := <- c.offers:
            // Record that this client has these chunks.
            // Note that we do not check a chunk's hash here to see if it
            // is valid. This is a task for the Client receiving the chunk.
            localFile := & clientproto.LocalFile {
                Torrent: offer.Torrent,
                Path: offer.Path,
                Chunks: make(map[int]struct{})}
            c.localFiles[offer.Torrent.ID] = localFile
            for chunkNum := 0; chunkNum < torrent.NumChunks(offer.Torrent); chunkNum++ {
                localFile.Chunks[chunkNum] = struct{}{}
            }

            // Inform this Client's LocalFileListener that local files have
            // been updated.
            c.lfl.OnChange(& clientproto.LocalFileChange {
                LocalFile: localFile,
                Operation: clientproto.LocalFileUpdate})

            // Offer this file to a Tracker.
            if trackerConn, err := getResponsiveTrackerNode(offer.Torrent); err != nil {
                // Unable to get a responsive Tracker node.
                offer.Reply <- nil
                return
            } else {
                // Confirm to the Tracker that this client has all chunks associated with
                // the Torrent.
                for chunkNum := 0; chunkNum < torrent.NumChunks(offer.Torrent); chunkNum++ {
                    args := & trackerproto.ConfirmArgs{
                        Chunk: torrentproto.ChunkID {
                            ID: offer.Torrent.ID,
                            ChunkNum: chunkNum},
                        HostPort: c.hostPort}
                    reply := & trackerproto.UpdateReply{}
                    if err := trackerConn.Call("RemoteTracker.ConfirmChunk", args, reply); err != nil {
                        // Previously responsive Tracker has failed.
                        offer.Reply <- err
                        return
                    }
                    if reply.Status == trackerproto.FileNotFound {
                        // Torrent refers to a file which does not exist on the Tracker.
                        offer.Reply <- errors.New("Tried to offer file which does not exist on Tracker")
                        return
                    }
                }
            }

            // Inform the user that this offer completed without error.
            offer.Reply <- nil

        // Record that this client has this chunk.
        // Note that we do not check the chunk's hash here to see if it
        // is valid. This is a task for the Client receiving the chunk.
        case chunkID := <- c.downloadedChunks:
            // Record that this client has this chunk.
            if localFile, ok := c.localFiles[chunkID.ID]; !ok {
                // There is no entry for this file.
                // It must have been removed. Do nothing.
            } else {
                localFile.Chunks[chunkID.ChunkNum] = struct{}{}

                // Inform this Client's LocalFileListener that local files have
                // been updated.
                c.lfl.OnChange(& clientproto.LocalFileChange {
                    LocalFile: localFile,
                    Operation: clientproto.LocalFileUpdate})
            }
        }
    }
}

// getResponsiveTrackerNode gets a live connection to a Tracker node.
// However, there is no guarantee that this connection won't die immediately.
func getResponsiveTrackerNode(t torrentproto.Torrent) (*rpc.Client, error) {
    for _, trackerNode := range t.TrackerNodes {
        if conn, err := rpc.DialHTTP("tcp", trackerNode.HostPort); err == nil {
            // Found a live node.
            return conn, nil;
        }
    }

    // Didn't find any live nodes on one pass.
    return nil, errors.New("Could not find a responsive Tracker")
}

// downloadFile gets all chunks of a file from Clients which have them.
// If the chunk is not available, sends a non-nil error to the user.
// As the chunks are downloaded, it informs the Client that they have arrived
// and offers them to the Tracker.
func (c *client) downloadFile(download *Download) {
    // Create a file to hold this chunk.
    if file, err := os.Create(download.Path); err != nil {
        // Failed to create file at given path.
        download.Reply <- err
        return
    } else if trackerConn, err := getResponsiveTrackerNode(download.Torrent); err != nil {
        // Could not contact a tracker.
        download.Reply <- err
        return
    } else {
        // Create a new random number generator to help provide load-balancing
        // for this download.
        r := rand.New(rand.NewSource(time.Now().UnixNano()))

        // Download the chunks for this file in a random order.
        for _, chunkNum := range r.Perm(torrent.NumChunks(download.Torrent)) {
            chunkID := torrentproto.ChunkID {
                ID: download.Torrent.ID,
                ChunkNum: chunkNum}
            trackerArgs := & trackerproto.RequestArgs {Chunk: chunkID}
            trackerReply := & trackerproto.RequestReply {}
            if err := trackerConn.Call("RemoteTracker.RequestChunk", trackerArgs, trackerReply); err != nil {
                // Failed to make RPC.
                download.Reply <- err
                return
            } else if trackerReply.ChunkHash != download.Torrent.ChunkHashes[chunkNum] {
                // This torrent is fake or corrupted.
                // The hash in the torrent for this chunkNum and torrent ID
                // (i.e. this ChunkID) does not match the hash for this ChunkID
                // on the Tracker.
                // Since the Tracker associates exactly one hash with each
                // chunkNum and torrentID when a torrent is first registered,
                // we will get this error if and only if the torrent contains
                // a bad hash for this chunk.
                download.Reply <- errors.New("Bad torrent file")
                return
            } else if err := downloadChunk(download, file, chunkNum, trackerReply.Peers, r); err != nil {
                // Failed to download this chunk.
                download.Reply <- err
                return
            } else {
                // Successfully downloaded and wrote this chunk.
                // Inform the Client.
                c.downloadedChunks <- chunkID
            }
        }
    }

    // Successfully downloaded and wrote all chunks.
    download.Reply <- nil
}

// downloadChunk attemps to download and locally write one chunk.
// If it fails, it returns a non-nil error.
func downloadChunk(download *Download, file *os.File, chunkNum int, peers []string, r *rand.Rand) error {
    // Try peers until one responds with chunk.
    // Randomize order to help balance load across peers.
    peerArgs := & clientproto.GetArgs{
        ChunkID: torrentproto.ChunkID {
            ID: download.Torrent.ID,
            ChunkNum: chunkNum}}
    peerReply := & clientproto.GetReply{}
    h := sha1.New()
    for _, peerNum := range r.Perm(len(peers)) {
        hostPort := peers[peerNum]
        if peer, err := rpc.DialHTTP("tcp", hostPort); err != nil {
            // Failed to connect.
            continue
        } else if err := peer.Call("RemoteClient.GetChunk", peerArgs, peerReply); err != nil {
            // Failed to make RPC.
            continue
        }

        chunk := peerReply.Chunk
        h.Reset()
        h.Write(chunk)
        if string(h.Sum(nil)) != download.Torrent.ChunkHashes[chunkNum] {
            // Chunk had bad hash.
            continue
        } else if err := torrent.WriteChunk(download.Torrent, file, chunkNum, chunk); err != nil {
            // Failed to write chunk locally.
            continue
        } else {
            // Successfully downloaded and wrote chunk.
            return nil
        }
    }

    // Failed to get the chunk from a peer.
    return errors.New("No peers responded with chunk")
}
