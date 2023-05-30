package p2p

import (
	"context"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p/core/event"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/net/conngater"
)

const (
	// defaultScore specifies the score for newly connected peers.
	defaultScore float32 = 1
	// maxTrackerSize specifies the max amount of peers that can be added to the PeerTracker.
	maxPeerTrackerSize = 100
)

var (
	// maxAwaitingTime specifies the duration that gives to the disconnected peer to be back online,
	// otherwise it will be removed on the next GC cycle.
	maxAwaitingTime = time.Hour
	// gcCycle defines the duration after which the PeerTracker starts removing peers.
	gcCycleDefault = time.Minute * 1
)

type PeerTracker struct {
	host      host.Host
	connGater *conngater.BasicConnectionGater

	peerLk sync.RWMutex
	// trackedPeers contains active peers that we can request to.
	// we cache the peer once they disconnect,
	// so we can guarantee that peerQueue will only contain active peers
	trackedPeers map[peer.ID]*peerStat
	// disconnectedPeers contains disconnected peers. In case if peer does not return
	// online until pruneDeadline, it will be removed and its score will be lost.
	disconnectedPeers map[peer.ID]*peerStat

	// pidstore is used to store peers periodically.
	pidstore PeerIDStore

	ctx    context.Context
	cancel context.CancelFunc
	// done is used to gracefully stop the PeerTracker.
	// It allows to wait until track() and gc() will be stopped.
	done chan struct{}
}

func NewPeerTracker(
	h host.Host,
	connGater *conngater.BasicConnectionGater,
	pidstore PeerIDStore,
) *PeerTracker {
	ctx, cancel := context.WithCancel(context.Background())
	return &PeerTracker{
		host:              h,
		connGater:         connGater,
		disconnectedPeers: make(map[peer.ID]*peerStat),
		trackedPeers:      make(map[peer.ID]*peerStat),
		pidstore:          pidstore,
		ctx:               ctx,
		cancel:            cancel,
		done:              make(chan struct{}, 2),
	}
}

func (p *PeerTracker) track() {
	defer func() {
		p.done <- struct{}{}
	}()

	// store peers that have been already connected
	for _, c := range p.host.Network().Conns() {
		p.connected(c.RemotePeer())
	}

	subs, err := p.host.EventBus().Subscribe(&event.EvtPeerConnectednessChanged{})
	if err != nil {
		log.Errorw("subscribing to EvtPeerConnectednessChanged", "err", err)
		return
	}

	for {
		select {
		case <-p.ctx.Done():
			err = subs.Close()
			if err != nil {
				log.Errorw("closing subscription", "err", err)
			}
			return
		case subscription := <-subs.Out():
			ev := subscription.(event.EvtPeerConnectednessChanged)
			switch ev.Connectedness {
			case network.Connected:
				p.connected(ev.Peer)
			case network.NotConnected:
				p.disconnected(ev.Peer)
			}
		}
	}
}

func (p *PeerTracker) getTrackedPeers() map[peer.ID]*peerStat {
	p.peerLk.RLock()
	defer p.peerLk.RUnlock()
	return p.trackedPeers
}

func (p *PeerTracker) getDisconnectedPeers() map[peer.ID]*peerStat {
	p.peerLk.RLock()
	defer p.peerLk.RUnlock()
	return p.disconnectedPeers
}

func (p *PeerTracker) connected(pID peer.ID) {
	if p.host.ID() == pID {
		return
	}

	for _, c := range p.host.Network().ConnsToPeer(pID) {
		// check if connection is short-termed and skip this peer
		if c.Stat().Transient {
			return
		}
	}

	p.peerLk.Lock()
	defer p.peerLk.Unlock()
	// skip adding the peer to avoid overfilling of the PeerTracker with unused peers if:
	// PeerTracker reaches the maxTrackerSize and there are more connected peers
	// than disconnected peers.
	if len(p.trackedPeers)+len(p.disconnectedPeers) > maxPeerTrackerSize &&
		len(p.trackedPeers) > len(p.disconnectedPeers) {
		return
	}

	// additional check in p.trackedPeers should be done,
	// because libp2p does not emit multiple Connected events per 1 peer
	stats, ok := p.disconnectedPeers[pID]
	if !ok {
		stats = &peerStat{peerID: pID, peerScore: defaultScore}
	} else {
		delete(p.disconnectedPeers, pID)
	}
	p.trackedPeers[pID] = stats
}

func (p *PeerTracker) disconnected(pID peer.ID) {
	p.peerLk.Lock()
	defer p.peerLk.Unlock()
	stats, ok := p.trackedPeers[pID]
	if !ok {
		return
	}
	stats.pruneDeadline = time.Now().Add(maxAwaitingTime)
	p.disconnectedPeers[pID] = stats
	delete(p.trackedPeers, pID)
}

func (p *PeerTracker) peers() []*peerStat {
	p.peerLk.RLock()
	defer p.peerLk.RUnlock()
	peers := make([]*peerStat, 0, len(p.trackedPeers))
	for _, stat := range p.trackedPeers {
		peers = append(peers, stat)
	}
	return peers
}

// gc goes through connected and disconnected peers once every gcPeriod
// and removes:
// * disconnected peers which have been disconnected for more than maxAwaitingTime;
// * connected peers whose scores are less than or equal than defaultScore;
func (p *PeerTracker) gc() {
	ticker := time.NewTicker(gcCycleDefault)
	for {
		select {
		case <-p.ctx.Done():
			p.done <- struct{}{}
			return
		case <-ticker.C:
			p.peerLk.Lock()
			now := time.Now()
			// prune expired disconnected peers
			for id, peer := range p.disconnectedPeers {
				if peer.pruneDeadline.Before(now) {
					delete(p.disconnectedPeers, id)
				}
			}
			// prune peers with scores below the default and copy remaining
			// peers for persisted storage if available
			trackedPeers := make([]peer.ID, 0, len(p.trackedPeers))
			for id, peer := range p.trackedPeers {
				if peer.peerScore <= defaultScore {
					delete(p.trackedPeers, id)
				} else {
					trackedPeers = append(trackedPeers, id)
				}
			}
			p.peerLk.Unlock()

			p.persistPeers(trackedPeers)
		}
	}
}

func (p *PeerTracker) persistPeers(trackedPeers []peer.ID) {
	if p.pidstore == nil {
		return
	}

	err := p.pidstore.Put(p.ctx, trackedPeers)
	if err != nil {
		log.Errorw("persisting updated peer list", "err", err)
	}
}

// stop waits until all background routines will be finished.
func (p *PeerTracker) stop(ctx context.Context) error {
	p.cancel()

	for i := 0; i < cap(p.done); i++ {
		select {
		case <-p.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	return nil
}

// blockPeer blocks a peer on the networking level and removes it from the local cache.
func (p *PeerTracker) blockPeer(pID peer.ID, reason error) {
	// add peer to the blacklist, so we can't connect to it in the future.
	err := p.connGater.BlockPeer(pID)
	if err != nil {
		log.Errorw("header/p2p: blocking peer failed", "pID", pID, "err", err)
	}
	// close connections to peer.
	err = p.host.Network().ClosePeer(pID)
	if err != nil {
		log.Errorw("header/p2p: closing connection with peer failed", "pID", pID, "err", err)
	}

	log.Warnw("header/p2p: blocked peer", "pID", pID, "reason", reason)

	p.peerLk.Lock()
	defer p.peerLk.Unlock()
	// remove peer from cache.
	delete(p.trackedPeers, pID)
	delete(p.disconnectedPeers, pID)
}
