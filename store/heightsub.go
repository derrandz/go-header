package store

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/celestiaorg/go-header"
)

// errElapsedHeight is thrown when a requested height was already provided to heightSub.
var errElapsedHeight = errors.New("elapsed height")

// heightSub provides a minimalistic mechanism to wait till header for a height becomes available.
type heightSub[H header.Header] struct {
	// height refers to the latest locally available header height
	// that has been fully verified and inserted into the subjective chain
	height       atomic.Uint64
	heightReqsLk sync.Mutex
	heightReqs   map[uint64][]chan H
}

// newHeightSub instantiates new heightSub.
func newHeightSub[H header.Header]() *heightSub[H] {
	return &heightSub[H]{
		heightReqs: make(map[uint64][]chan H),
	}
}

// Height reports current height.
func (hs *heightSub[H]) Height() uint64 {
	return hs.height.Load()
}

// SetHeight sets the new head height for heightSub.
func (hs *heightSub[H]) SetHeight(height uint64) {
	hs.height.Store(height)
}

// Sub subscribes for a header of a given height.
// It can return errElapsedHeight, which means a requested header was already provided
// and caller should get it elsewhere.
func (hs *heightSub[H]) Sub(ctx context.Context, height uint64) (H, error) {
	var zero H
	if hs.Height() >= height {
		return zero, errElapsedHeight
	}

	hs.heightReqsLk.Lock()
	if hs.Height() >= height {
		// This is a rare case we have to account for.
		// The lock above can park a goroutine long enough for hs.height to change for a requested height,
		// leaving the request never fulfilled and the goroutine deadlocked.
		hs.heightReqsLk.Unlock()
		return zero, errElapsedHeight
	}
	resp := make(chan H, 1)
	hs.heightReqs[height] = append(hs.heightReqs[height], resp)
	hs.heightReqsLk.Unlock()

	select {
	case resp := <-resp:
		return resp, nil
	case <-ctx.Done():
		// no need to keep the request, if the op is canceled
		hs.heightReqsLk.Lock()
		delete(hs.heightReqs, height)
		hs.heightReqsLk.Unlock()
		return zero, ctx.Err()
	}
}

// Pub processes all the outstanding subscriptions matching the given headers.
// Pub is only safe when called from one goroutine.
// For Pub to work correctly, heightSub has to be initialized with SetHeight
// so that given headers are contiguous to the height on heightSub.
func (hs *heightSub[H]) Pub(headers ...H) {
	ln := len(headers)
	if ln == 0 {
		return
	}

	height := hs.Height()
	from, to := uint64(headers[0].Height()), uint64(headers[ln-1].Height())
	if height+1 != from {
		log.Fatal("PLEASE FILE A BUG REPORT: headers given to the heightSub are in the wrong order")
		return
	}
	hs.SetHeight(to)

	hs.heightReqsLk.Lock()
	defer hs.heightReqsLk.Unlock()

	// there is a common case where we Pub only header
	// in this case, we shouldn't loop over each heightReqs
	// and instead read from the map directly
	if ln == 1 {
		reqs, ok := hs.heightReqs[from]
		if ok {
			for _, req := range reqs {
				req <- headers[0] // reqs must always be buffered, so this won't block
			}
			delete(hs.heightReqs, from)
		}
		return
	}

	// instead of looping over each header in 'headers', we can loop over each request
	// which will drastically decrease idle iterations, as there will be less requests than headers
	for height, reqs := range hs.heightReqs {
		// then we look if any of the requests match the given range of headers
		if height >= from && height <= to {
			// and if so, calculate its position and fulfill requests
			h := headers[height-from]
			for _, req := range reqs {
				req <- h // reqs must always be buffered, so this won't block
			}
			delete(hs.heightReqs, height)
		}
	}
}
