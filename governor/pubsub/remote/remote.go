package remote

import (
	"sync"
	"time"

	"github.com/kevmo314/fedtorch/governor/pubsub/local"

	gpupb "github.com/kevmo314/fedtorch/governor/api/go/gpu"
)

type Allocator struct {
	requests  <-chan *gpupb.Request
	ambient   <-chan *gpupb.Fulfillment
	responses chan<- *gpupb.Fulfillment
	returns   chan string

	l         sync.Mutex
	fulfilled map[string]time.Time

	local *local.Allocator

	wait time.Duration
}

func (a *Allocator) fulfiller() {
	for r := range a.requests {
		time.Sleep(a.wait)

		if l := func() *gpupb.Lease {
			a.l.Lock()
			defer a.l.Unlock()

			if _, ok := a.fulfilled[r.GetToken()]; ok {
				return nil
			}

			l, err := a.local.Reserve(r.GetLease().AsDuration())
			if err != nil {
				return nil
			}

			return l
		}(); l != nil {
			a.responses <- &gpupb.Fulfillment{
				Requestor: r.GetRequestor(),

				Gpu:   a.local.Get(l.GetId()),
				Lease: l,
			}

			go func(l *gpupb.Lease) {
				time.Sleep(time.Until(l.GetExpiration().AsTime()))
				a.returns <- l.GetToken()
			}(l)
		}
	}
}

func (a *Allocator) listener() {
	for r := range a.ambient {
		a.l.Lock()

		a.fulfilled[r.GetToken()] = r.GetLease().GetExpiration().AsTime()

		a.l.Unlock()

		go func(r *gpupb.Fulfillment) {
			time.Sleep(time.Until(r.GetLease().GetExpiration().AsTime()))
			a.returns <- r.GetToken()
		}(r)
	}
}

func (a *Allocator) cleaner() {
	for x := range a.returns {
		a.l.Lock()

		// Do not check expiration time.
		delete(a.fulfilled, x)

		a.l.Unlock()
	}
}
