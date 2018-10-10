package outgoinghandshaker

import (
	"io"
	"net"

	"github.com/cenkalti/rain/internal/logger"
	"github.com/cenkalti/rain/torrent/internal/bitfield"
	"github.com/cenkalti/rain/torrent/internal/btconn"
	"github.com/cenkalti/rain/torrent/internal/peerconn"
)

type OutgoingHandshaker struct {
	addr     net.Addr
	peerID   [20]byte
	infoHash [20]byte
	resultC  chan Result
	closeC   chan struct{}
	doneC    chan struct{}
	log      logger.Logger
}

type Result struct {
	Peer  *peerconn.Conn
	Addr  net.Addr
	Error error
}

func NewOutgoing(addr net.Addr, peerID, infoHash [20]byte, resultC chan Result, l logger.Logger) *OutgoingHandshaker {
	return &OutgoingHandshaker{
		addr:     addr,
		peerID:   peerID,
		infoHash: infoHash,
		resultC:  resultC,
		closeC:   make(chan struct{}),
		doneC:    make(chan struct{}),
		log:      l,
	}
}

func (h *OutgoingHandshaker) Close() {
	close(h.closeC)
	<-h.doneC
}

func (h *OutgoingHandshaker) Run() {
	defer close(h.doneC)
	log := logger.New("peer -> " + h.addr.String())

	// TODO get this from config
	encryptionDisableOutgoing := false
	encryptionForceOutgoing := false

	// TODO get supported extensions from common place
	var ourExtensions [8]byte
	ourbf := bitfield.NewBytes(ourExtensions[:], 64)
	ourbf.Set(61) // Fast Extension (BEP 6)
	ourbf.Set(43) // Extension Protocol (BEP 10)

	// TODO separate dial and handshake
	conn, cipher, peerExtensions, peerID, err := btconn.Dial(h.addr, !encryptionDisableOutgoing, encryptionForceOutgoing, ourExtensions, h.infoHash, h.peerID, h.closeC)
	if err != nil {
		if err == io.EOF {
			log.Debug("peer has closed the connection: EOF")
		} else if err == io.ErrUnexpectedEOF {
			log.Debug("peer has closed the connection: Unexpected EOF")
		} else if _, ok := err.(*net.OpError); ok {
			log.Debugln("net operation error:", err)
		} else {
			log.Errorln("cannot complete outgoing handshake:", err)
		}
		select {
		case h.resultC <- Result{Addr: h.addr, Error: err}:
		case <-h.closeC:
		}
		return
	}
	log.Debugf("Connected to peer. (cipher=%s extensions=%x client=%q)", cipher, peerExtensions, peerID[:8])

	peerbf := bitfield.NewBytes(peerExtensions[:], 64)
	extensions := ourbf.And(peerbf)

	p := peerconn.New(conn, peerID, extensions, log)
	select {
	case h.resultC <- Result{Addr: h.addr, Peer: p}:
	case <-h.closeC:
		conn.Close()
	}
}