package torrent

import (
	"errors"
	"io"
	"io/ioutil"
	"log"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/lkslts64/charo-torrent/metainfo"
	"github.com/lkslts64/charo-torrent/peer_wire"
	"github.com/lkslts64/charo-torrent/tracker"
)

const clientID = "CH"
const version = "0001"

var reserved [8]byte

//Client manages multiple torrents
type Client struct {
	config    *Config
	peerID    [20]byte
	logger    *log.Logger
	logWriter io.Writer
	//torrents []Torrent
	//conns    []net.Conn
	torrents map[[20]byte]*Torrent
	//torrents map[[20]byte]Torrent
	//a set of info_hashes that clients is
	//responsible for - easy access
	//this will be set in initialaztion
	//no  mutex needed
	//TODO:it is dupicate
	infoHashes map[[20]byte]struct{}
	//extensionsSupported map[string]int
	listener net.Listener
	//when this channel closes, all Torrents and conns that the client is managing will close.
	//close                   chan chan struct{}
	announcer               *trackerAnnouncer
	trackerAnnouncerCloseCh chan chan struct{}
	port                    int16
}

type Config struct {
	MaxOnFlightReqs int //max outstanding requests we allowed for a peer to have
	MaxConns        int
	DisableTrackers bool
	//directory to store the data
	BaseDir string
}

func NewClient(cfg *Config) (*Client, error) {
	var err error
	if cfg == nil {
		cfg, err = defaultConfig()
		if err != nil {
			return nil, err
		}
	}
	//TODO: maybe overwrite log file instead of creating a new one
	//logFile, err := ioutil.TempFile("", "charo.log")
	logFile, err := os.Create(os.TempDir() + "/charo.log")
	if err != nil {
		return nil, err
	}
	cl := &Client{
		peerID:     newPeerID(),
		config:     cfg,
		logWriter:  logFile,
		logger:     log.New(logFile, "client", log.LstdFlags),
		infoHashes: make(map[[20]byte]struct{}),
		torrents:   make(map[[20]byte]*Torrent),
	}
	if err = cl.listen(); err != nil {
		return nil, err
	}
	go cl.accept()
	cl.announcer = &trackerAnnouncer{
		cl:                            cl,
		trackerAnnouncerSubmitEventCh: make(chan trackerAnnouncerEvent, 5),
		trackers:                      make(map[string]tracker.TrackerURL),
	}
	if !cl.config.DisableTrackers {
		go cl.announcer.run()
	}
	return cl, nil
}

//drops all torrents that client manages
func (cl *Client) Close() {
	chanArr := []chan struct{}{}
	for range cl.torrents {
		chanArr = append(chanArr, make(chan struct{}, 1))
	}
	//signal all torents to close
	for i, t := range cl.Torrents() {
		t.close <- chanArr[i]
	}
	//wait until all torrents actually close
	for i := 0; i < len(cl.torrents); i++ {
		<-chanArr[i]
	}
}

func defaultConfig() (*Config, error) {
	tdir, err := ioutil.TempDir(os.TempDir(), "")
	if err != nil {
		return nil, err
	}
	return &Config{
		MaxOnFlightReqs: 250,
		MaxConns:        55,
		BaseDir:         tdir,
	}, nil
}

func DefaultConfig() *Config {
	return &Config{
		MaxOnFlightReqs: 250,
		MaxConns:        55,
		BaseDir:         "./",
	}
}

//NewTorrentFromFile creates a torrent based on a .torrent file
func (cl *Client) NewTorrentFromFile(filename string) (*Torrent, error) {
	t := newTorrent(cl)
	var err error
	if t.mi, err = metainfo.LoadMetainfoFile(filename); err != nil {
		return nil, err
	}
	cl.infoHashes[t.mi.Info.Hash] = struct{}{}
	cl.torrents[t.mi.Info.Hash] = t
	//TODO: find another way of getting the info bytes, it is expensive
	//to read and decode the file twice
	if t.infoRaw, err = t.mi.Info.Bytes(filename); err != nil {
		return nil, err
	}
	t.gotInfo()
	//go t.mainLoop()
	return t, nil
}

func (cl *Client) Torrents() []*Torrent {
	ts := []*Torrent{}
	for _, t := range cl.torrents {
		ts = append(ts, t)
	}
	return ts
}

/*func NewClient(sources []string) (*Client, error) {
	var err error
	var peerID [20]byte
	rand.Read(peerID[:])
	torrents := make([]*Torrent, len(sources))
	for i, s := range sources {
		torrents[i], err = NewTorrent(s)
		if err != nil {
			return nil, err
		}
	}
	infoHashes := make(map[[20]byte]struct{})
	for _, t := range torrents {
		infoHashes[t.tm.mi.Info.Hash] = struct{}{}
	}
	return &Client{
		peerID:     peerID,
		torrents:   torrents,
		infoHashes: infoHashes,
	}, nil

}
*/

//6881-6889
func (cl *Client) listen() error {
	var i int16
	var err error
	//var ln net.Listener
	for i = 6881; i < 6890; i++ {
		//we dont support IPv6
		cl.listener, err = net.Listen("tcp4", ":"+strconv.Itoa(int(i)))
		if err == nil {
			cl.port = i
			return nil
		}
	}
	//if none of the BT ports was avaialable, try other ones.
	if cl.listener, err = net.Listen("tcp4", ":"); err != nil {
		return errors.New("could not find port to listen")
	}
	return nil
}

func (cl *Client) accept() error {
	for {
		conn, err := cl.listener.Accept()
		if err != nil {
			//TODO: handle different
			cl.logger.Printf("error accepting conn")
			continue
		}
		go cl.handleConn(conn)
	}
}

func (cl *Client) handleConn(tcpConn net.Conn) {
	hs := &peer_wire.HandShake{
		Reserved: reserved,
		PeerID:   cl.peerID,
	}
	err := cl.handshake(tcpConn, hs)
	if err != nil {
		return
	}
	var (
		t  *Torrent
		ok bool
	)
	if t, ok = cl.torrents[hs.InfoHash]; !ok {
		panic("we checked that we have this torrent")
	}
	err = newConn(t, tcpConn, cl.peerID[:]).mainLoop()
	if err != nil {
		cl.logger.Println(err)
	}
}

//TODO: store the remote addr and pop when finish
func (cl *Client) connectToPeer(address string, t *Torrent) {
	tcpConn, err := net.Dial("tcp", address)
	if err != nil {
		cl.logger.Printf("cannot dial peer: %s", err)
		return
	}
	defer tcpConn.Close()
	err = cl.handshake(tcpConn, &peer_wire.HandShake{
		Reserved: reserved,
		PeerID:   cl.peerID,
		InfoHash: t.mi.Info.Hash,
	})
	if err != nil {
		cl.logger.Println(err)
		return
	}
	err = newConn(t, tcpConn, cl.peerID[:]).mainLoop()
	if err != nil {
		cl.logger.Println(err)
	}
}

func (cl *Client) connectToPeers(t *Torrent, addresses ...string) {
	for _, addr := range addresses {
		go cl.connectToPeer(addr, t)
	}
}

func (cl *Client) addr() string {
	return cl.listener.Addr().String()
}

func (cl *Client) handshake(tcpConn net.Conn, hs *peer_wire.HandShake) error {
	//dont wait more than 5 secs for handshake
	tcpConn.SetDeadline(time.Now().Add(5 * time.Second))
	defer tcpConn.SetDeadline(time.Time{})
	err := hs.Do(tcpConn, cl.infoHashes)
	if err != nil {
		cl.logger.Println(err)
	}
	return err
}
