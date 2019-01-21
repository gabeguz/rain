// Package session provides a BitTorrent client implementation that is capable of downlaoding multiple torrents in parallel.
package session

import (
	"bytes"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/boltdb/bolt"
	"github.com/cenkalti/rain/internal/logger"
	"github.com/cenkalti/rain/internal/torrent"
	"github.com/cenkalti/rain/internal/torrent/bitfield"
	"github.com/cenkalti/rain/internal/torrent/magnet"
	"github.com/cenkalti/rain/internal/torrent/metainfo"
	"github.com/cenkalti/rain/internal/torrent/resumer"
	"github.com/cenkalti/rain/internal/torrent/resumer/boltdbresumer"
	"github.com/cenkalti/rain/internal/torrent/storage/filestorage"
	"github.com/cenkalti/rain/internal/tracker"
	"github.com/cenkalti/rain/session/internal/blocklist"
	"github.com/cenkalti/rain/session/internal/trackermanager"
	"github.com/mitchellh/go-homedir"
	"github.com/nictuku/dht"
)

var mainBucket = []byte("torrents")

type Session struct {
	config         Config
	db             *bolt.DB
	log            logger.Logger
	dht            *dht.DHT
	blocklist      *blocklist.Blocklist
	trackerManager *trackermanager.TrackerManager
	closeC         chan struct{}

	mPeerRequests   sync.Mutex
	dhtPeerRequests map[dht.InfoHash]struct{}

	m                  sync.RWMutex
	torrents           map[uint64]*Torrent
	torrentsByInfoHash map[dht.InfoHash][]*Torrent

	mPorts         sync.Mutex
	availablePorts map[uint16]struct{}
}

// New returns a pointer to new Rain BitTorrent client.
func New(cfg Config) (*Session, error) {
	if cfg.PortBegin >= cfg.PortEnd {
		return nil, errors.New("invalid port range")
	}
	rLimit := syscall.Rlimit{
		Cur: cfg.MaxOpenFiles,
		Max: cfg.MaxOpenFiles,
	}
	err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &rLimit)
	if err != nil {
		return nil, err
	}
	cfg.Database, err = homedir.Expand(cfg.Database)
	if err != nil {
		return nil, err
	}
	cfg.DataDir, err = homedir.Expand(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	err = os.MkdirAll(filepath.Dir(cfg.Database), 0750)
	if err != nil {
		return nil, err
	}
	l := logger.New("session")
	db, err := bolt.Open(cfg.Database, 0640, &bolt.Options{Timeout: time.Second})
	if err == bolt.ErrTimeout {
		return nil, errors.New("resume database is locked by another process")
	} else if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			db.Close()
		}
	}()
	var ids []uint64
	err = db.Update(func(tx *bolt.Tx) error {
		b, err2 := tx.CreateBucketIfNotExists(mainBucket)
		if err2 != nil {
			return err2
		}
		return b.ForEach(func(k, _ []byte) error {
			id, err3 := strconv.ParseUint(string(k), 10, 64)
			if err3 != nil {
				l.Error(err3)
				return nil
			}
			ids = append(ids, id)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	dhtConfig := dht.NewConfig()
	dhtConfig.Address = cfg.DHTAddress
	dhtConfig.Port = int(cfg.DHTPort)
	dhtNode, err := dht.New(dhtConfig)
	if err != nil {
		return nil, err
	}
	err = dhtNode.Start()
	if err != nil {
		return nil, err
	}
	ports := make(map[uint16]struct{})
	for p := cfg.PortBegin; p < cfg.PortEnd; p++ {
		ports[p] = struct{}{}
	}
	bl := blocklist.New()
	c := &Session{
		config:             cfg,
		db:                 db,
		blocklist:          bl,
		trackerManager:     trackermanager.New(bl),
		log:                l,
		torrents:           make(map[uint64]*Torrent),
		torrentsByInfoHash: make(map[dht.InfoHash][]*Torrent),
		availablePorts:     ports,
		dht:                dhtNode,
		dhtPeerRequests:    make(map[dht.InfoHash]struct{}),
		closeC:             make(chan struct{}),
	}
	err = c.ReloadBlocklist()
	if err != nil {
		return nil, err
	}
	err = c.loadExistingTorrents(ids)
	if err != nil {
		return nil, err
	}
	go c.processDHTResults()
	return c, nil
}

func (s *Session) processDHTResults() {
	dhtLimiter := time.NewTicker(time.Second)
	defer dhtLimiter.Stop()
	for {
		select {
		case <-dhtLimiter.C:
			s.handleDHTtick()
		case res := <-s.dht.PeersRequestResults:
			for ih, peers := range res {
				torrents, ok := s.torrentsByInfoHash[ih]
				if !ok {
					continue
				}
				addrs := parseDHTPeers(peers)
				for _, t := range torrents {
					select {
					case t.dhtAnnouncer.peersC <- addrs:
					case <-t.removed:
					}
				}
			}
		case <-s.closeC:
			return
		}
	}
}

func (s *Session) handleDHTtick() {
	s.mPeerRequests.Lock()
	defer s.mPeerRequests.Unlock()
	if len(s.dhtPeerRequests) == 0 {
		return
	}
	var ih dht.InfoHash
	found := false
	for ih = range s.dhtPeerRequests {
		found = true
		break
	}
	if !found {
		return
	}
	s.dht.PeersRequest(string(ih), true)
	delete(s.dhtPeerRequests, ih)
}

func parseDHTPeers(peers []string) []*net.TCPAddr {
	var addrs []*net.TCPAddr
	for _, peer := range peers {
		if len(peer) != 6 {
			// only IPv4 is supported for now
			continue
		}
		addr := &net.TCPAddr{
			IP:   net.IP(peer[:4]),
			Port: int((uint16(peer[4]) << 8) | uint16(peer[5])),
		}
		addrs = append(addrs, addr)
	}
	return addrs
}

func (s *Session) parseTrackers(trackers []string) []tracker.Tracker {
	var ret []tracker.Tracker
	for _, tr := range trackers {
		t, err := s.trackerManager.Get(tr, s.config.Torrent.HTTPTrackerTimeout, s.config.Torrent.HTTPTrackerUserAgent)
		if err != nil {
			s.log.Warningln("cannot parse tracker url:", err)
			continue
		}
		ret = append(ret, t)
	}
	return ret
}

func (s *Session) loadExistingTorrents(ids []uint64) error {
	var loaded int
	var started []*Torrent
	for _, id := range ids {
		res, err := boltdbresumer.New(s.db, mainBucket, []byte(strconv.FormatUint(id, 10)))
		if err != nil {
			s.log.Error(err)
			continue
		}
		hasStarted, err := s.hasStarted(id)
		if err != nil {
			s.log.Error(err)
			continue
		}
		spec, err := res.Read()
		if err != nil {
			s.log.Error(err)
			continue
		}
		opt := torrent.Options{
			Name:      spec.Name,
			Port:      spec.Port,
			Trackers:  s.parseTrackers(spec.Trackers),
			Resumer:   res,
			Blocklist: s.blocklist,
			Config:    &s.config.Torrent,
			Stats: resumer.Stats{
				BytesDownloaded: spec.BytesDownloaded,
				BytesUploaded:   spec.BytesUploaded,
				BytesWasted:     spec.BytesWasted,
			},
		}
		var private bool
		var ann *dhtAnnouncer
		if len(spec.Info) > 0 {
			info, err2 := metainfo.NewInfo(spec.Info)
			if err2 != nil {
				s.log.Error(err2)
				continue
			}
			opt.Info = info
			private = info.Private == 1
			if len(spec.Bitfield) > 0 {
				bf, err3 := bitfield.NewBytes(spec.Bitfield, info.NumPieces)
				if err3 != nil {
					s.log.Error(err3)
					continue
				}
				opt.Bitfield = bf
			}
		}
		if !private {
			ann = newDHTAnnouncer(s.dht, spec.InfoHash, spec.Port)
			opt.DHT = ann
		}
		sto, err := filestorage.New(spec.Dest)
		if err != nil {
			s.log.Error(err)
			continue
		}
		t, err := opt.NewTorrent(spec.InfoHash, sto)
		if err != nil {
			s.log.Error(err)
			continue
		}
		delete(s.availablePorts, uint16(spec.Port))

		t2 := s.newTorrent(t, id, uint16(spec.Port), ann)
		s.log.Debugf("loaded existing torrent: #%d %s", id, t.Name())
		loaded++
		if hasStarted {
			started = append(started, t2)
		}
	}
	s.log.Infof("loaded %d existing torrents", loaded)
	for _, t := range started {
		t.Start()
	}
	return nil
}

func (s *Session) hasStarted(id uint64) (bool, error) {
	subBucket := strconv.FormatUint(id, 10)
	started := false
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(mainBucket).Bucket([]byte(subBucket))
		val := b.Get([]byte("started"))
		if bytes.Equal(val, []byte("1")) {
			started = true
		}
		return nil
	})
	return started, err
}

func (s *Session) Close() error {
	s.dht.Stop()

	var wg sync.WaitGroup
	s.m.Lock()
	wg.Add(len(s.torrents))
	for _, t := range s.torrents {
		go func(t *Torrent) {
			t.torrent.Close()
			wg.Done()
		}(t)
	}
	wg.Wait()
	s.torrents = nil
	s.m.Unlock()

	return s.db.Close()
}

func (s *Session) ListTorrents() []*Torrent {
	s.m.RLock()
	defer s.m.RUnlock()
	torrents := make([]*Torrent, 0, len(s.torrents))
	for _, t := range s.torrents {
		torrents = append(torrents, t)
	}
	return torrents
}

func (s *Session) AddTorrent(r io.Reader) (*Torrent, error) {
	mi, err := metainfo.New(r)
	if err != nil {
		return nil, err
	}
	opt, sto, id, err := s.add()
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			s.releasePort(uint16(opt.Port))
		}
	}()
	opt.Name = mi.Info.Name
	opt.Trackers = s.parseTrackers(mi.GetTrackers())
	opt.Info = mi.Info
	var ann *dhtAnnouncer
	if mi.Info.Private != 1 {
		ann = newDHTAnnouncer(s.dht, mi.Info.Hash[:], opt.Port)
		opt.DHT = ann
	}
	t, err := opt.NewTorrent(mi.Info.Hash[:], sto)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			t.Close()
		}
	}()
	rspec := &boltdbresumer.Spec{
		InfoHash: t.InfoHashBytes(),
		Dest:     sto.Dest(),
		Port:     opt.Port,
		Name:     opt.Name,
		Trackers: mi.GetTrackers(),
		Info:     opt.Info.Bytes,
	}
	if opt.Bitfield != nil {
		rspec.Bitfield = opt.Bitfield.Bytes()
	}
	err = opt.Resumer.(*boltdbresumer.Resumer).Write(rspec)
	if err != nil {
		return nil, err
	}
	t2 := s.newTorrent(t, id, uint16(opt.Port), ann)
	return t2, t2.Start()
}

func (s *Session) AddMagnet(link string) (*Torrent, error) {
	ma, err := magnet.New(link)
	if err != nil {
		return nil, err
	}
	opt, sto, id, err := s.add()
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			s.releasePort(uint16(opt.Port))
		}
	}()
	opt.Name = ma.Name
	opt.Trackers = s.parseTrackers(ma.Trackers)
	ann := newDHTAnnouncer(s.dht, ma.InfoHash[:], opt.Port)
	opt.DHT = ann
	t, err := opt.NewTorrent(ma.InfoHash[:], sto)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			t.Close()
		}
	}()
	rspec := &boltdbresumer.Spec{
		InfoHash: ma.InfoHash[:],
		Dest:     sto.Dest(),
		Port:     opt.Port,
		Name:     opt.Name,
		Trackers: ma.Trackers,
	}
	err = opt.Resumer.(*boltdbresumer.Resumer).Write(rspec)
	if err != nil {
		return nil, err
	}
	t2 := s.newTorrent(t, id, uint16(opt.Port), ann)
	return t2, t2.Start()
}

func (s *Session) add() (*torrent.Options, *filestorage.FileStorage, uint64, error) {
	port, err := s.getPort()
	if err != nil {
		return nil, nil, 0, err
	}
	defer func() {
		if err != nil {
			s.releasePort(port)
		}
	}()
	var id uint64
	err = s.db.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket(mainBucket)
		id, err = bucket.NextSequence()
		return err
	})
	if err != nil {
		return nil, nil, 0, err
	}
	res, err := boltdbresumer.New(s.db, mainBucket, []byte(strconv.FormatUint(id, 10)))
	if err != nil {
		return nil, nil, 0, err
	}
	dest := filepath.Join(s.config.DataDir, strconv.FormatUint(id, 10))
	sto, err := filestorage.New(dest)
	if err != nil {
		return nil, nil, 0, err
	}
	return &torrent.Options{
		Port:      int(port),
		Resumer:   res,
		Blocklist: s.blocklist,
		Config:    &s.config.Torrent,
	}, sto, id, nil
}

func (s *Session) newTorrent(t *torrent.Torrent, id uint64, port uint16, ann *dhtAnnouncer) *Torrent {
	t2 := &Torrent{
		session:      s,
		torrent:      t,
		id:           id,
		port:         port,
		dhtAnnouncer: ann,
		removed:      make(chan struct{}),
	}
	s.m.Lock()
	defer s.m.Unlock()
	s.torrents[id] = t2
	ih := dht.InfoHash(t.InfoHashBytes())
	s.torrentsByInfoHash[ih] = append(s.torrentsByInfoHash[ih], t2)
	return t2
}

func (s *Session) getPort() (uint16, error) {
	s.mPorts.Lock()
	defer s.mPorts.Unlock()
	for p := range s.availablePorts {
		delete(s.availablePorts, p)
		return p, nil
	}
	return 0, errors.New("no free port")
}

func (s *Session) releasePort(port uint16) {
	s.mPorts.Lock()
	defer s.mPorts.Unlock()
	s.availablePorts[port] = struct{}{}
}

func (s *Session) GetTorrent(id uint64) *Torrent {
	s.m.RLock()
	defer s.m.RUnlock()
	return s.torrents[id]
}

func (s *Session) RemoveTorrent(id uint64) error {
	s.m.Lock()
	defer s.m.Unlock()
	t, ok := s.torrents[id]
	if !ok {
		return nil
	}
	close(t.removed)
	t.torrent.Close()
	delete(s.torrents, id)
	delete(s.torrentsByInfoHash, dht.InfoHash(t.torrent.InfoHashBytes()))
	s.releasePort(t.port)
	subBucket := strconv.FormatUint(id, 10)
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(mainBucket).DeleteBucket([]byte(subBucket))
	})
}

func (s *Session) ReloadBlocklist() error {
	if s.config.Blocklist == "" {
		return nil
	}
	f, err := os.Open(s.config.Blocklist)
	if err != nil {
		return err
	}
	defer f.Close()
	n, err := s.blocklist.Reload(f)
	if err != nil {
		return err
	}
	s.log.Infof("Loaded %d rules from blocklist.", n)
	return nil
}