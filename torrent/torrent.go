package torrent

import (
	"bytes"
	"crypto/sha1"
	"errors"
	"fmt"
	"log"
	"math"
	"math/rand"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/anacrolix/dht/v2"
	"github.com/anacrolix/missinggo/bitmap"
	"github.com/dustin/go-humanize"
	"github.com/lkslts64/charo-torrent/bencode"
	"github.com/lkslts64/charo-torrent/metainfo"
	"github.com/lkslts64/charo-torrent/peer_wire"
	"github.com/lkslts64/charo-torrent/torrent/storage"
	"github.com/lkslts64/charo-torrent/tracker"
)

var maxEstablishedConnsDefault = 55

var maxRequestBlockSz = 1 << 14

const metadataPieceSz = 1 << 14

//Torrent represents a torrent and maintains state about it.Multiple goroutines may
//invoke methods on a Torrent simultaneously.
type Torrent struct {
	cl     *Client
	logger *log.Logger
	//channel we receive messages from conns
	recvC       chan msgWithConn
	openStorage storage.Open
	storage     storage.Storage
	//These are active connections
	conns                     []*connInfo
	halfOpenmu                sync.Mutex
	halfOpen                  map[string]Peer
	maxHalfOpenConns          int
	maxEstablishedConnections int
	//we should make effort to obtain new peers if they are below this threshold
	wantPeersThreshold int
	peers              []Peer
	newConnC           chan *connInfo
	pieces             *pieces
	choker             *choker
	//the number of outstanding request messages we support
	//without dropping any. The default in in libtorrent is 250.
	reqq                         int
	blockRequestSize             int
	trackerAnnouncerTimer        *time.Timer
	canAnnounceTracker           bool
	trackerAnnouncerResponseC    chan trackerAnnouncerResponse
	trackerAnnouncerSubmitEventC chan trackerAnnouncerEvent
	lastAnnounceResp             *tracker.AnnounceResp
	numAnnounces                 int
	numTrackerAnnouncesSend      int
	//
	dhtAnnounceResp  *dht.Announce
	dhtAnnounceTimer *time.Timer
	canAnnounceDht   bool
	numDhtAnnounces  int
	//fires when an exported method wants to be invoked
	userC chan chan interface{}
	//these bools are set true when we should actively download/upload the torrent's data.
	//the download of the info is not controled with this variable
	uploadEnabled   bool
	downloadEnabled bool
	//closes when the Torrent closes
	ClosedC  chan struct{}
	isClosed bool
	//when this closes it signals all conns to exit
	dropC chan struct{}
	//closes when all pieces have been downloaded
	DownloadedDataC chan struct{}
	//fires when we get the info dictionary
	InfoC chan error
	//channel to send requests to piece hasher goroutine
	pieceQueuedHashingC chan int
	//response channel of piece hasher
	pieceHashedC          chan pieceHashed
	queuedForVerification map[int]struct{}
	//Info field of `mi` is nil if we dont have it.
	//Restrict access to metainfo before we get the
	//whole mi.Info part.
	mi       *metainfo.MetaInfo
	infoSize int64
	//we serve metadata only if we have it all.
	//lock only when writing
	infoMu sync.Mutex
	//used only when we dont have infoDict part of metaInfo
	infoBytes []byte
	//TODO: change to bitmap
	ownedInfoBlocks []bool
	//frequency map of infoSizes we have received
	infoSizeFreq freqMap
	//length of data to be downloaded
	length         int
	stats          Stats
	connMsgsRecv   int
	msgsSentToConn int
}

func newTorrent(cl *Client) *Torrent {
	t := &Torrent{
		cl:                        cl,
		openStorage:               cl.config.OpenStorage,
		reqq:                      250, //libtorent also has this default
		recvC:                     make(chan msgWithConn, maxEstablishedConnsDefault*sendCSize),
		newConnC:                  make(chan *connInfo, maxEstablishedConnsDefault),
		halfOpen:                  make(map[string]Peer),
		userC:                     make(chan chan interface{}),
		maxEstablishedConnections: cl.config.MaxEstablishedConns,
		maxHalfOpenConns:          55,
		wantPeersThreshold:        100,
		dropC:                     make(chan struct{}),
		DownloadedDataC:           make(chan struct{}),
		InfoC:                     make(chan error),
		ClosedC:                   make(chan struct{}),
		trackerAnnouncerResponseC: make(chan trackerAnnouncerResponse, 1),
		trackerAnnouncerTimer:     newExpiredTimer(),
		dhtAnnounceTimer:          newExpiredTimer(),
		dhtAnnounceResp:           new(dht.Announce),
		queuedForVerification:     make(map[int]struct{}),
		infoSizeFreq:              newFreqMap(),
		logger:                    log.New(cl.logger.Writer(), "torrent", log.LstdFlags),
		canAnnounceDht:            true,
		canAnnounceTracker:        true,
	}
	if t.cl.trackerAnnouncer != nil {
		t.trackerAnnouncerSubmitEventC = cl.trackerAnnouncer.trackerAnnouncerSubmitEventCh
	}
	t.choker = newChoker(t)
	return t
}

//close closes all connections with peers that were associated with this Torrent.
func (t *Torrent) close() {
	if t.isClosed {
		panic("attempt to close torrent but is already closed")
	}
	defer func() {
		close(t.ClosedC)
		t.isClosed = true
	}()
	t.dropAllConns()
	t.choker.ticker.Stop()
	t.trackerAnnouncerTimer.Stop()
	t.dhtAnnounceTimer.Stop()
	t.choker = nil
	t.trackerAnnouncerResponseC = nil
	t.recvC = nil
	t.newConnC = nil
	t.pieces = nil
	t.infoBytes = nil
	t.peers = nil
	//t.logger = nil
	//TODO: clear struct fields
}

func (t *Torrent) dropAllConns() {
	t.closeDhtAnnounce()
	//signal conns to close and wait until all conns actually close
	//maybe we don't want to wait?
	close(t.dropC)
	for _, c := range t.conns {
		<-c.droppedC
	}
	t.conns = nil
}

func (t *Torrent) mainLoop() {
	defer func() {
		if r := recover(); r != nil {
			t.logger.Fatal(r)
		}
	}()
	t.tryAnnounceAll()
	t.choker.startTicker()
	for {
		select {
		case e := <-t.recvC:
			t.onConnMsg(e)
			t.connMsgsRecv++
		case res := <-t.pieceHashedC:
			t.pieceHashed(res.pieceIndex, res.ok)
			if t.pieces.haveAll() {
				t.sendAnnounceToTracker(tracker.Completed)
				t.downloadedAll()
			}
		case ci := <-t.newConnC: //we established a new connection
			t.establishedConnection(ci)
		case <-t.choker.ticker.C:
			t.choker.reviewUnchokedPeers()
		case tresp := <-t.trackerAnnouncerResponseC:
			t.trackerAnnounced(tresp)
		case <-t.trackerAnnouncerTimer.C:
			t.canAnnounceTracker = true
			t.tryAnnounceAll()
		case pvs, ok := <-t.dhtAnnounceResp.Peers:
			if !ok {
				t.dhtAnnounceResp.Peers = nil
			}
			t.dhtAnnounced(pvs)
		case <-t.dhtAnnounceTimer.C:
			t.canAnnounceDht = true
			//close the previous one and try announce again (kind of weird but I think anacrolix does it that way)
			t.closeDhtAnnounce()
			t.tryAnnounceAll()
		//an exported method wants to be invoked
		case userDone := <-t.userC:
			<-userDone
			if t.isClosed {
				return
			}
		}
	}
}

func (t *Torrent) onConnMsg(e msgWithConn) {
	switch v := e.val.(type) {
	case *peer_wire.Msg:
		switch v.Kind {
		case peer_wire.Interested, peer_wire.NotInterested:
			e.conn.peerInterestChanged()
		case peer_wire.Choke, peer_wire.Unchoke:
			e.conn.peerChokeChanged()
		case peer_wire.Have:
			e.conn.peerBf.Set(int(v.Index), true)
			e.conn.reviewInterestsOnHave(int(v.Index))
		}
	case downloadedBlock:
		t.blockDownloaded(e.conn, block(v))
	case uploadedBlock:
		t.blockUploaded(e.conn, block(v))
	case metainfoSize:
	case bitmap.Bitmap:
		e.conn.peerBf = v
		e.conn.reviewInterestsOnBitfield()
	case connDroped:
		t.droppedConn(e.conn)
	case discardedRequests:
		t.broadcastToConns(requestsAvailable{})
	}
}

//true if we would like to initiate new connections
func (t *Torrent) wantConns() bool {
	return len(t.conns) < t.maxEstablishedConnections && t.dataTransferAllowed()
}

func (t *Torrent) wantPeers() bool {
	wantPeersThreshold := t.wantPeersThreshold
	if len(t.conns) > 0 {
		//if we have many active conns increase the wantPeersThreshold.
		fullfilledConnSlotsRatio := float64(t.maxEstablishedConnections / len(t.conns))
		if fullfilledConnSlotsRatio < 10/9 {
			wantPeersThreshold = int(float64(t.wantPeersThreshold) + (1/fullfilledConnSlotsRatio)*10)
		}
	}
	return len(t.peers) < wantPeersThreshold && t.dataTransferAllowed()
}

func (t *Torrent) seeding() bool {
	return t.haveAll() && t.uploadEnabled
}

func (t *Torrent) haveAll() bool {
	if !t.haveInfo() {
		return false
	}
	return t.pieces.haveAll()
}

//true if we are allowed to download/upload torrent data or we need info
func (t *Torrent) dataTransferAllowed() bool {
	return !t.haveInfo() || t.uploadEnabled || t.downloadEnabled
}

func (t *Torrent) sendAnnounceToTracker(event tracker.Event) {
	if t.cl.config.DisableTrackers || t.cl.trackerAnnouncer == nil || t.mi.Announce == "" {
		return
	}
	t.trackerAnnouncerSubmitEventC <- trackerAnnouncerEvent{t, event, t.stats}
	t.numTrackerAnnouncesSend++
	t.canAnnounceTracker = false
}

func (t *Torrent) trackerAnnounced(tresp trackerAnnouncerResponse) {
	t.numAnnounces++
	if tresp.err != nil {
		t.logger.Printf("tracker error: %s\n", tresp.err)
		//announce after one minute if tracker send error
		t.resetNextTrackerAnnounce(60)
		return
	}
	t.resetNextTrackerAnnounce(tresp.resp.Interval)
	t.lastAnnounceResp = tresp.resp
	peers := make([]Peer, len(tresp.resp.Peers))
	for i := 0; i < len(peers); i++ {
		peers[i] = Peer{
			P:      tresp.resp.Peers[i],
			Source: SourceTracker,
		}
	}
	t.gotPeers(peers)
}

func (t *Torrent) addFilteredPeers(peers []Peer, f func(peer Peer) bool) {
	for _, peer := range peers {
		if f(peer) {
			t.peers = append(t.peers, peer)
		}
	}
}

func (t *Torrent) resetNextTrackerAnnounce(interval int32) {
	nextAnnounce := time.Duration(interval) * time.Second
	if !t.trackerAnnouncerTimer.Stop() {
		select {
		//rare case - only when we announced with event Complete or Started
		case <-t.trackerAnnouncerTimer.C:
		default:
		}
	}
	t.trackerAnnouncerTimer.Reset(nextAnnounce)
}

func (t *Torrent) announceDht() {
	if t.cl.config.DisableDHT || t.cl.dhtServer == nil {
		return
	}
	ann, err := t.cl.dhtServer.Announce(t.mi.Info.Hash, int(t.cl.port), true)
	if err != nil {
		t.logger.Printf("dht error: %s", err)
	}
	t.dhtAnnounceResp = ann
	if !t.dhtAnnounceTimer.Stop() {
		select {
		//TODO:shouldn't happen
		case <-t.dhtAnnounceTimer.C:
		default:
		}
	}
	t.dhtAnnounceTimer.Reset(5 * time.Minute)
	t.canAnnounceDht = false
	t.numDhtAnnounces++
}

func (t *Torrent) dhtAnnounced(pvs dht.PeersValues) {
	peers := []Peer{}
	for _, peer := range pvs.Peers {
		if peer.Port == 0 {
			continue
		}
		peers = append(peers, Peer{
			P: tracker.Peer{
				IP:   peer.IP,
				Port: uint16(peer.Port),
			},
			Source: SourceDHT,
		})
	}
	t.gotPeers(peers)
}

func (t *Torrent) closeDhtAnnounce() {
	if t.cl.dhtServer == nil || t.dhtAnnounceResp.Peers == nil {
		return
	}
	t.dhtAnnounceResp.Close()
	//invalidate the channel
	t.dhtAnnounceResp.Peers = nil
}

func (t *Torrent) gotPeers(peers []Peer) {
	t.cl.mu.Lock()
	t.addFilteredPeers(peers, func(peer Peer) bool {
		for _, ip := range t.cl.blackList {
			if ip.Equal(peer.P.IP) {
				return false
			}
		}
		return true
	})
	t.cl.mu.Unlock()
	t.dialConns()
}

func (t *Torrent) dialConns() {
	if !t.wantConns() {
		return
	}
	defer t.tryAnnounceAll()
	t.halfOpenmu.Lock()
	defer t.halfOpenmu.Unlock()
	for len(t.peers) > 0 && len(t.halfOpen) < t.maxHalfOpenConns {
		peer := t.popPeer()
		if t.peerInActiveConns(peer) {
			continue
		}
		t.halfOpen[peer.P.String()] = peer
		go t.cl.makeOutgoingConnection(t, peer)
	}
}

//has peer the same addr with any active connection
func (t *Torrent) peerInActiveConns(peer Peer) bool {
	for _, ci := range t.conns {
		if bytes.Equal(ci.peer.P.IP, peer.P.IP) && ci.peer.P.Port == peer.P.Port {
			return true
		}
	}
	return false
}

func (t *Torrent) popPeer() (p Peer) {
	i := rand.Intn(len(t.peers))
	p = t.peers[i]
	t.peers = append(t.peers[:i], t.peers[i+1:]...)
	return
}

func (t *Torrent) swarm() (peers []Peer) {
	for _, c := range t.conns {
		peers = append(peers, c.peer)
	}
	func() {
		t.halfOpenmu.Lock()
		defer t.halfOpenmu.Unlock()
		for _, p := range t.halfOpen {
			peers = append(peers, p)
		}
	}()
	for _, p := range t.peers {
		peers = append(peers, p)
	}
	return
}

func (t *Torrent) writeStatus(b *strings.Builder) {
	if t.haveInfo() {
		b.WriteString(fmt.Sprintf("Name: %s\n", t.mi.Info.Name))
	}
	b.WriteString(fmt.Sprintf("#DhtAnnounces: %d\n", t.numDhtAnnounces))
	b.WriteString("Tracker: " + t.mi.Announce + "\tAnnounce: " + func() string {
		if t.lastAnnounceResp != nil {
			return "OK"
		}
		return "Not Available"
	}() + "\t#AnnouncesSend: " + strconv.Itoa(t.numTrackerAnnouncesSend) + "\n")
	if t.lastAnnounceResp != nil {
		b.WriteString(fmt.Sprintf("Seeders: %d\tLeechers: %d\tInterval: %d(secs)\n", t.lastAnnounceResp.Seeders, t.lastAnnounceResp.Seeders, t.lastAnnounceResp.Interval))
	}
	b.WriteString(fmt.Sprintf("State: %s\n", t.state()))
	b.WriteString(fmt.Sprintf("Downloaded: %s\tUploaded: %s\tRemaining: %s\n", humanize.Bytes(uint64(t.stats.BytesDownloaded)),
		humanize.Bytes(uint64(t.stats.BytesUploaded)), humanize.Bytes(uint64(t.stats.BytesLeft))))
	b.WriteString(fmt.Sprintf("Connected to %d peers\n", len(t.conns)))
	tabWriter := tabwriter.NewWriter(b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tabWriter, "Address\t%\tUp\tDown\t")
	for _, ci := range t.conns {
		fmt.Fprintf(tabWriter, "%s\t%s\t%s\t%s\t\n", ci.peer.P.IP.String(),
			strconv.Itoa(int(float64(ci.peerBf.Len())/float64(t.numPieces())*100))+"%",
			humanize.Bytes(uint64(ci.stats.uploadUsefulBytes)),
			humanize.Bytes(uint64(ci.stats.downloadUsefulBytes)))
	}
	tabWriter.Flush()
}

func (t *Torrent) state() string {
	if t.isClosed {
		return "closed"
	}
	if t.seeding() {
		return "seeding"
	}
	if !t.haveInfo() {
		return "downloading info"
	}
	switch {
	case t.uploadEnabled && t.downloadEnabled:
		return "uploading/downloading"
	case t.uploadEnabled:
		return "uploading only"
	case t.downloadEnabled:
		return "downloading only"
	}
	return "waiting for downloading request"
}

func (t *Torrent) blockDownloaded(c *connInfo, b block) {
	c.stats.onBlockDownload(b.len)
	t.stats.blockDownloaded(b.len)
	t.pieces.setBlockComplete(b.pc, b.off, c)
}

func (t *Torrent) blockUploaded(c *connInfo, b block) {
	c.stats.onBlockUpload(b.len)
	t.stats.blockUploaded(b.len)
}

func (t *Torrent) downloadedAll() {
	close(t.DownloadedDataC)
	for _, c := range t.conns {
		c.notInterested()
	}
}

func (t *Torrent) queuePieceForHashing(i int) {
	if _, ok := t.queuedForVerification[i]; ok || t.pieces.pcs[i].verified {
		//piece is already queued or verified
		return
	}
	t.queuedForVerification[i] = struct{}{}
	select {
	case t.pieceQueuedHashingC <- i:
	default:
		panic("queue piece hash: should not block")
	}
}

func (t *Torrent) pieceHashed(i int, correct bool) {
	delete(t.queuedForVerification, i)
	t.pieces.pieceHashed(i, correct)
	if correct {
		t.onPieceDownload(i)
	} else {
		t.banPeer()
	}
}

//this func is started in its own goroutine.
//when we close eventCh of conn, the goroutine
//exits
func (t *Torrent) aggregateEvents(ci *connInfo) {
	for e := range ci.recvC {
		t.recvC <- msgWithConn{ci, e}
	}
}

//careful when using this, we might send over nil chan
func (t *Torrent) broadcastToConns(cmd interface{}) {
	for _, ci := range t.conns {
		ci.sendMsgToConn(cmd)
	}
}

func (t *Torrent) sendCancels(b block) {
	t.broadcastToConns(b.cancelMsg())
}

//TODO: make iter around t.conns and dont iterate twice over conns
func (t *Torrent) onPieceDownload(i int) {
	t.stats.onPieceDownload(t.pieceLen(uint32(i)))
	t.reviewInterestsOnPieceDownload(i)
	t.sendHaves(i)
}

func (t *Torrent) reviewInterestsOnPieceDownload(i int) {
	if t.haveAll() {
		return
	}
	for _, c := range t.conns {
		if c.peerBf.Get(i) {
			c.numWant--
			if c.numWant <= 0 {
				c.notInterested()
			}
		}
	}
}

func (t *Torrent) sendHaves(i int) {
	for _, c := range t.conns {
		c.have(i)
	}
}

func (t *Torrent) establishedConnection(ci *connInfo) bool {
	if !t.wantConns() {
		ci.sendC <- drop{}
		t.closeDhtAnnounce()
		t.logger.Printf("rejected a connection with peer %v\n", ci.peer.P)
		return false
	}
	defer t.choker.reviewUnchokedPeers()
	t.conns = append(t.conns, ci)
	//notify conn that we have metainfo
	if t.haveInfo() {
		ci.sendMsgToConn(haveInfo{})
	}
	//TODO:minimize sends...
	//
	//TODO:send here extension msg
	//
	//if we have some pieces, we should sent a bitfield
	if t.pieces.ownedPieces.Len() > 0 {
		ci.sendBitfield()
	}
	if ci.reserved.SupportDHT() && t.cl.reserved.SupportDHT() && t.cl.dhtServer != nil {
		ci.sendPort()
	}
	go t.aggregateEvents(ci)
	return true
}

func (t *Torrent) banPeer() {
	max := math.MinInt32
	var toBan *connInfo
	for _, c := range t.conns {
		if m := c.stats.malliciousness(); m > max {
			max = m
			toBan = c
		}
	}
	if toBan == nil {
		return
	}
	t.cl.banIP(toBan.peer.P.IP)
	toBan.sendMsgToConn(drop{})
}

//conn notified us that it was dropped
//returns false if we have already dropped it.
func (t *Torrent) droppedConn(ci *connInfo) bool {
	var (
		i  int
		ok bool
	)
	if i, ok = t.connIndex(ci); !ok {
		return false
	}
	defer t.choker.reviewUnchokedPeers()
	defer t.dialConns()
	t.removeConn(ci, i)
	//If there is a large time gap between the time we download the info and before the user
	//requests to download the data we may lose some connections (seeders will close because
	//we won't request any pieces). So, we may have to store the peers that droped us during
	//that period in order to reconnect.
	if t.infoWasDownloaded() && !t.dataTransferAllowed() {
		t.peers = append(t.peers, ci.peer)
	}
	return true
}

//bool is true if was found
func (t *Torrent) connIndex(ci *connInfo) (int, bool) {
	for i, cn := range t.conns {
		if cn == ci {
			return i, true
		}
	}
	return -1, false
}

func (t *Torrent) connsIter(f func(ci *connInfo) bool) {
	for _, ci := range t.conns {
		if !f(ci) {
			break
		}
	}
}

func (t *Torrent) tryAnnounceAll() {
	if !t.wantPeers() {
		return
	}
	if t.canAnnounceTracker {
		t.sendAnnounceToTracker(tracker.None)
	}
	if t.canAnnounceDht {
		t.announceDht()
	}
}

//clear the fields that included the conn
func (t *Torrent) removeConn(ci *connInfo, index int) {
	t.conns = append(t.conns[:index], t.conns[index+1:]...)
}

func (t *Torrent) removeHalfOpen(addr string) {
	t.halfOpenmu.Lock()
	delete(t.halfOpen, addr)
	t.halfOpenmu.Unlock()
	t.dialConns()
}

//TODO: maybe wrap these at the same func passing as arg the func (read/write)
func (t *Torrent) writeBlock(data []byte, piece, begin int) error {
	off := int64(piece*t.mi.Info.PieceLen + begin)
	_, err := t.storage.WriteBlock(data, off)
	return err
}

func (t *Torrent) readBlock(data []byte, piece, begin int) error {
	off := int64(piece*t.mi.Info.PieceLen + begin)
	n, err := t.storage.ReadBlock(data, off)
	if n != len(data) {
		t.logger.Printf("coudn't read whole block from storage, read only %d bytes\n", n)
	}
	if err != nil {
		t.logger.Printf("storage read err %s\n", err)
	}
	return err
}

func (t *Torrent) uploaders() (uploaders []*connInfo) {
	for _, c := range t.conns {
		if c.state.canDownload() {
			uploaders = append(uploaders, c)
		}
	}
	return
}

func (t *Torrent) numPieces() int {
	return t.mi.Info.NumPieces()
}

func (t *Torrent) downloadMetadata() bool {
	//take the infoSize that we have seen most times from peers
	infoSize := t.infoSizeFreq.max()
	if infoSize == 0 || infoSize > 10000000 { //10MB,anacrolix pulled from his ass
		return false
	}
	t.infoBytes = make([]byte, infoSize)
	isLastSmaller := infoSize%metadataPieceSz != 0
	numPieces := infoSize / metadataPieceSz
	if isLastSmaller {
		numPieces++
	}
	t.ownedInfoBlocks = make([]bool, numPieces)
	//send requests to all conns
	return true
}

func (t *Torrent) downloadedMetadata() bool {
	for _, v := range t.ownedInfoBlocks {
		if !v {
			return false
		}
	}
	return true
}

func (t *Torrent) writeMetadataPiece(b []byte, i int) error {
	//tm.metadataMu.Lock()
	//defer tm.metadataMu.Unlock()
	if t.ownedInfoBlocks[i] {
		return nil
	}
	//TODO:log this
	if i*metadataPieceSz >= len(t.infoBytes) {
		return errors.New("write metadata piece: out of range")
	}
	if len(b) > metadataPieceSz {
		return errors.New("write metadata piece: length of of piece too big")
	}
	if len(b) != metadataPieceSz && i != len(t.ownedInfoBlocks)-1 {
		return errors.New("write metadata piece:piece is not the last and length is not 16KB")
	}
	copy(t.infoBytes[i*metadataPieceSz:], b)
	t.ownedInfoBlocks[i] = true
	if t.downloadedMetadata() {
		//TODO: verify should be done by Torrent goroutine
		switch ok, err := t.verifyInfoDict(); {
		case err != nil:
			t.logger.Println(err)
		case !ok:
			//log
		default:
			t.gotInfo()
		}
	} else {
		//pick the max freq `infoSize`
	}
	return nil
}

func (t *Torrent) readMetadataPiece(b []byte, i int) error {
	if !t.haveInfo() {
		panic("read metadata piece:we dont have info")
	}
	//out of range
	if i*metadataPieceSz >= len(t.infoBytes) {
		return errors.New("read metadata piece: out of range")
	}

	//last piece case
	if (i+1)*metadataPieceSz >= len(t.infoBytes) {
		b = t.infoBytes[i*metadataPieceSz:]
	} else {
		b = t.infoBytes[i*metadataPieceSz : (i+1)*metadataPieceSz]
	}
	return nil
}

func (t *Torrent) verifyInfoDict() (ok bool, err error) {
	if sha1.Sum(t.infoBytes) != t.mi.Info.Hash {
		return false, nil
	}
	if err := bencode.Decode(t.infoBytes, t.mi.Info); err != nil {
		return false, errors.New("cant decode info dict")
	}
	return true, nil
}

func (t *Torrent) gotInfoHash() {
	logPrefix := t.cl.logger.Prefix() + fmt.Sprintf("TR%x", t.mi.Info.Hash[14:])
	t.logger = log.New(t.cl.logger.Writer(), logPrefix, log.LstdFlags)
}

func (t *Torrent) gotInfo() {
	defer close(t.InfoC)
	t.length = t.mi.Info.TotalLength()
	t.stats.BytesLeft = t.length
	t.blockRequestSize = t.blockSize()
	t.pieces = newPieces(t)
	t.pieceQueuedHashingC = make(chan int, t.numPieces())
	t.pieceHashedC = make(chan pieceHashed, t.numPieces())
	var haveAll bool
	t.storage, haveAll = t.openStorage(t.mi, t.cl.config.BaseDir, t.pieces.blocks(), t.logger)
	t.broadcastToConns(haveInfo{})
	if haveAll {
		//mark all bocks completed and do all apropriate things when a piece
		//hashing is succesfull
		for i, p := range t.pieces.pcs {
			p.unrequestedBlocks, p.completeBlocks = p.completeBlocks, p.unrequestedBlocks
			t.pieceHashed(i, true)
		}
		t.downloadedAll()
	} else {
		ph := pieceHasher{t: t}
		go ph.Run()
	}
	//TODO:review interests
}

func (t *Torrent) pieceLen(i uint32) (pieceLen int) {
	numPieces := int(t.mi.Info.NumPieces())
	//last piece case
	if int(i) == numPieces-1 {
		pieceLen = t.length % int(t.mi.Info.PieceLen)
	} else {
		pieceLen = t.mi.Info.PieceLen
	}
	return
}

//call this when we get info
func (t *Torrent) blockSize() int {
	if maxRequestBlockSz > t.mi.Info.PieceLen {
		return t.mi.Info.PieceLen
	}
	return maxRequestBlockSz
}

/*
func (t *Torrent) scrapeTracker() (*tracker.ScrapeResp, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	return t.trackerURL.Scrape(ctx, t.mi.Info.Hash)
}
*/

func (t *Torrent) haveInfo() bool {
	return t.mi.Info != nil
}

//true if we hadn't the info on start up and downloaded/downloadiing it via metadata extension.
func (t *Torrent) infoWasDownloaded() bool {
	return t.infoBytes != nil
}

func (t *Torrent) newLocker() *torrentLocker {
	return &torrentLocker{
		t: t,
	}
}
