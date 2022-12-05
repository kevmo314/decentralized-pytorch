package pubsub

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/peer"
	"google.golang.org/protobuf/proto"
	"gorgonia.org/cu"

	gpupb "github.com/kevmo314/fedtorch/governor/api/go/gpu"
	dpb "google.golang.org/protobuf/types/known/durationpb"
	tpb "google.golang.org/protobuf/types/known/timestamppb"
)

const (
	GPURequestTopic     = "GPU_REQUEST"
	GPUFulfillmentTopic = "GPU_FULFILLMENT"
)

type GPUAllocator struct {
	// id is the host ID as provided by the p2plib host.Host.ID(). The
	// allocator does not track the host, and therefore this data needs to
	// be passed in explicitly.
	id peer.ID

	pubsub      *pubsub.PubSub
	request     *pubsub.Topic
	fulfillment *pubsub.Topic

	requestSub     *pubsub.Subscription
	fulfillmentSub *pubsub.Subscription

	l      sync.Mutex
	gpus   []*gpupb.GPU
	leases []*gpupb.Lease
}

// New constructs a GPUAllocator daemon.
//
// The input pubsub instance is assumed to have already been started, as is the
// governor.
//
// The local governer will track this GPUAllocator instance, and estimate how
// much resources will need to be reserved given an incoming workload. For each
// GPU which needs to be called, the governor will call
//
//	gpu, err := a.Reserve(ctx)
//
// A non-nil error will be returned if there was a problem reserving a GPU. The
// governor may call Reserver() multiple times concurrently. The returned GPU
// may be remote. The governor should negotiate with the remote governor on how
// exactly to use the reserved GPU.
func New(ctx context.Context, governorAddress string, id peer.ID, ps *pubsub.PubSub) *GPUAllocator {
	request, err := ps.Join(GPURequestTopic)
	if err != nil {
		panic(fmt.Sprintf("could not join GPU request topic %v: %v", GPURequestTopic, err))
	}
	fulfillment, err := ps.Join(GPUFulfillmentTopic)
	if err != nil {
		panic(fmt.Sprintf("could not join GPU fulfillment topic %v: %v", GPUFulfillmentTopic, err))
	}

	requestSub, err := request.Subscribe(pubsub.WithBufferSize(0))
	if err != nil {
		panic(fmt.Sprintf("could not subscribe to the GPU request topic %v: %v", GPURequestTopic, err))
	}

	fulfillmentSub, err := fulfillment.Subscribe(pubsub.WithBufferSize(0))
	if err != nil {
		panic(fmt.Sprintf("could not subscribe to the GPU fulfillment response topic %v: %v", GPUFulfillmentTopic, err))
	}

	a := &GPUAllocator{
		id:     id,
		pubsub: ps,

		request:     request,
		fulfillment: fulfillment,

		requestSub:     requestSub,
		fulfillmentSub: fulfillmentSub,

		gpus: get(governorAddress),
	}

	fulfillmentMonitorSub, err := fulfillment.Subscribe(pubsub.WithBufferSize(0))
	if err != nil {
		panic(fmt.Sprintf("could not subscribe to the GPU fulfillment monitoring topic %v: %v", GPUFulfillmentTopic, err))
	}

	go a.fulfillmentDaemon(ctx, fulfillmentMonitorSub)

	return a
}

func get(addr string) []*gpupb.GPU {
	n, err := cu.NumDevices()
	if err != nil {
		return nil
	}

	var devices []*gpupb.GPU
	for d := 0; d < n; d++ {
		dev := cu.Device(d)
		name, _ := dev.Name()
		cr, _ := dev.Attribute(cu.ClockRate)
		mem, _ := dev.TotalMem()
		g := &gpupb.GPU{
			Addr:      addr,
			Id:        int32(d),
			Name:      name,
			ClockRate: int32(cr),
			Memory:    mem,
		}
		devices = append(devices, g)
	}

	return devices
}

// fulfillmentDaemon listens on the network for remote nodes that need GPUs.
// Because this is a distributed system, it is possible for multiple nodes to
// receive the same remote node request. In order to limit the amount of
// duplicated reservations, we will do a random backoff.
//
// TODO(minkezhang): Use a Allocate / Reserve / Free model instead in v2 in
// which the gRPC server explicitly checks out a GPU for some period of time;
// in the meantime, temporarily lease out the GPU for ~5min.
func (a *GPUAllocator) fulfillmentDaemon(ctx context.Context, monitor *pubsub.Subscription) {
	for {
		msg, err := a.requestSub.Next(ctx)
		if err != nil {
			continue
		}

		req := &gpupb.Request{}
		if err := proto.Unmarshal(msg.Data, req); err != nil {
			continue
		}

		// Wait some time and see if another fulfillment has already
		// happened.
		n := int(rand.Int31n(10) + 10)
		waitCtx, cancel := context.WithDeadline(ctx, time.Now().Add(time.Duration(n*int(time.Second))))
		if !func(ctx context.Context) bool {
			defer cancel()

			for {
				msg, err := monitor.Next(ctx)
				if err != nil {
					continue
				}

				f := &gpupb.Fulfillment{}
				if err := proto.Unmarshal(msg.Data, f); err != nil {
					continue
				}

				if peer.ID(f.GetRequestor()) == peer.ID(req.GetRequestor()) {
					if f.GetToken() == req.GetToken() {
						return false
					}
				}
			}

			// No fulfillment messages have occurred yet.
			return true
		}(waitCtx) {
			continue
		}

		// Local reservation requests are not published.
		if peer.ID(req.GetRequestor()) != a.id {
			g, err := a.reserveLocal(req.GetLease().AsDuration())
			if err == nil {
				f, err := proto.Marshal(&gpupb.Fulfillment{
					Requestor: req.GetRequestor(),
					Token:     req.GetToken(),
					Gpu:       g,
				})
				if err != nil {
					continue
				}
				a.fulfillment.Publish(ctx, f)
			}
		}
	}
}

func (a *GPUAllocator) isAllocatedUnsafe(id int) bool {
	for _, l := range a.leases {
		if id == int(l.GetId()) && time.Now().Before(
			l.GetExpiration().AsTime(),
		) {
			return true
		}
	}
	return false
}

func (a *GPUAllocator) reserveLocal(lease time.Duration) (*gpupb.GPU, error) {
	expiration := time.Now().Add(lease)
	g, err := func() (*gpupb.GPU, error) {
		a.l.Lock()
		defer a.l.Unlock()

		for _, g := range a.gpus {
			if !a.isAllocatedUnsafe(int(g.GetId())) {
				a.leases = append(a.leases, &gpupb.Lease{
					Id:         g.GetId(),
					Expiration: tpb.New(expiration),
				})
				return g, nil
			}
		}
		return nil, fmt.Errorf("no local GPU available")
	}()
	if err != nil {
		return nil, err
	}

	go func() {
		<-time.After(time.Until(expiration))
		a.dropLocal(int(g.GetId()))
	}()

	return g, nil
}

func (a *GPUAllocator) reserveRemote(ctx context.Context, lease time.Duration) (*gpupb.GPU, error) {
	token := "random-string"

	req, err := proto.Marshal(&gpupb.Request{
		Requestor: string(a.id),
		Lease:     dpb.New(lease),
		Token:     token,
	})
	if err != nil {
		return nil, err
	}
	err = nil

	ctx, cancel := context.WithDeadline(ctx, time.Now().Add(time.Minute))

	var g *gpupb.GPU
	go func(ctx context.Context) {
		// cancelling here means the next request.Publish call may be
		// cut off early. This is probably okay, but will incur some
		// non-zero reservation penalty (i.e. a useless reservation on
		// the network).
		//
		// TODO(minkezhang): Rewrite the network code to be more
		// stateful.
		defer cancel()
		for {
			msg, terr := a.fulfillmentSub.Next(ctx)
			if terr != nil {
				err = terr
				return
			}

			if msg.ReceivedFrom == a.id {
				continue
			}

			f := &gpupb.Fulfillment{}
			if terr := proto.Unmarshal(msg.Data, f); terr != nil {
				continue
			}

			// A fulfillment request has been made on behalf of the
			// local node.
			if peer.ID(f.GetRequestor()) == a.id && f.GetToken() == token {
				g = f.GetGpu()
				return
			}
		}
	}(ctx)

	a.request.Publish(ctx, req)

	<-ctx.Done()

	return g, err
}

// Reserve allocates some GPU on a local or remote host for the specified amount
// of time. This is called by the governor when planning for a workload.
func (a *GPUAllocator) Reserve(ctx context.Context, lease time.Duration) (*gpupb.GPU, error) {
	if g, err := a.reserveLocal(lease); err == nil {
		return g, nil
	}
	return a.reserveRemote(ctx, lease)
}

func (a *GPUAllocator) dropLocal(id int) error {
	a.l.Lock()
	defer a.l.Unlock()

	n := len(a.leases)
	for i := 0; i < n; i++ {
		l := a.leases[i]
		if id == int(l.GetId()) {
			a.leases[i] = a.leases[n-1]
			a.leases = a.leases[:n-1]
			return nil
		}
	}

	return fmt.Errorf("cannot find GPU %v", id)
}
