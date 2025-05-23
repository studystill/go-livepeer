package server

import (
	"container/heap"
	"context"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/livepeer/go-livepeer/core"
	"github.com/livepeer/go-livepeer/net"
	"github.com/stretchr/testify/require"

	ethcommon "github.com/ethereum/go-ethereum/common"
	"github.com/livepeer/go-livepeer/common"
	"github.com/stretchr/testify/assert"
)

type stubOrchestratorStore struct {
	orchs []*common.DBOrch
	err   error
}

func (s *stubOrchestratorStore) OrchCount(filter *common.DBOrchFilter) (int, error) { return 0, nil }
func (s *stubOrchestratorStore) UpdateOrch(orch *common.DBOrch) error               { return nil }
func (s *stubOrchestratorStore) SelectOrchs(filter *common.DBOrchFilter) ([]*common.DBOrch, error) {
	if s.err != nil {
		return nil, s.err
	}
	return s.orchs, nil
}

func TestStoreStakeReader(t *testing.T) {
	assert := assert.New(t)

	store := &stubOrchestratorStore{}
	rdr := &storeStakeReader{store: store}

	store.err = errors.New("SelectOrchs error")
	_, err := rdr.Stakes(nil)
	assert.EqualError(err, store.err.Error())

	// Test when we receive results for only some addresses
	store.err = nil
	store.orchs = []*common.DBOrch{{EthereumAddr: "foo", Stake: 77}}
	stakes, err := rdr.Stakes([]ethcommon.Address{{}, {}})
	assert.Nil(err)
	assert.Len(stakes, 1)
	assert.Equal(stakes[ethcommon.HexToAddress("foo")], int64(77))

	// Test when we receive results for all addresses
	store.orchs = []*common.DBOrch{
		{EthereumAddr: "foo", Stake: 77},
		{EthereumAddr: "bar", Stake: 88},
	}
	stakes, err = rdr.Stakes([]ethcommon.Address{{}, {}})
	assert.Nil(err)

	for _, orch := range store.orchs {
		addr := ethcommon.HexToAddress(orch.EthereumAddr)
		assert.Contains(stakes, addr)
		assert.Equal(stakes[addr], orch.Stake)
	}
}

type stubStakeReader struct {
	stakes map[ethcommon.Address]int64
	err    error
}

func newStubStakeReader() *stubStakeReader {
	return &stubStakeReader{stakes: make(map[ethcommon.Address]int64)}
}

func (r *stubStakeReader) Stakes(addrs []ethcommon.Address) (map[ethcommon.Address]int64, error) {
	if r.err != nil {
		return nil, r.err
	}

	stakes := make(map[ethcommon.Address]int64)
	for _, addr := range addrs {
		stakes[addr] = r.stakes[addr]
	}

	return stakes, nil
}

func (r *stubStakeReader) SetStakes(stakes map[ethcommon.Address]int64) {
	r.stakes = stakes
}

type stubSelectionAlgorithm struct{}

func (sa stubSelectionAlgorithm) Select(ctx context.Context, addrs []ethcommon.Address, stakes map[ethcommon.Address]int64, maxPrice *big.Rat, prices map[ethcommon.Address]*big.Rat, perfScores map[ethcommon.Address]float64) ethcommon.Address {
	if len(addrs) == 0 {
		return ethcommon.Address{}
	}
	addr := addrs[0]
	if len(prices) > 0 {
		// select lowest price
		lowest := prices[addr]
		for _, a := range addrs {
			if prices[a].Cmp(lowest) < 0 {
				addr = a
				lowest = prices[a]
			}
		}
	} else if len(perfScores) > 0 {
		// select highest performance score
		highest := perfScores[addr]
		for _, a := range addrs {
			if perfScores[a] > highest {
				addr = a
				highest = perfScores[a]
			}
		}
	} else if len(stakes) > 0 {
		// select highest stake
		highest := stakes[addr]
		for _, a := range addrs {
			if stakes[a] > highest {
				addr = a
				highest = stakes[a]
			}
		}
	}
	return addr
}

func TestSessHeap(t *testing.T) {
	assert := assert.New(t)

	h := &sessHeap{}
	heap.Init(h)
	assert.Zero(h.Len())
	// Return nil for empty heap
	assert.Nil(h.Peek())

	sess1 := &BroadcastSession{LatencyScore: 1.0}
	heap.Push(h, sess1)
	assert.Equal(h.Len(), 1)
	assert.Equal(h.Peek().(*BroadcastSession), sess1)

	sess2 := &BroadcastSession{LatencyScore: 1.1}
	heap.Push(h, sess2)
	assert.Equal(h.Len(), 2)
	assert.Equal(h.Peek().(*BroadcastSession), sess1)

	sess3 := &BroadcastSession{LatencyScore: .9}
	heap.Push(h, sess3)
	assert.Equal(h.Len(), 3)
	assert.Equal(h.Peek().(*BroadcastSession), sess3)

	assert.Equal(heap.Pop(h).(*BroadcastSession), sess3)
	assert.Equal(heap.Pop(h).(*BroadcastSession), sess1)
	assert.Equal(heap.Pop(h).(*BroadcastSession), sess2)
	assert.Zero(h.Len())
}

func TestSelector_Select(t *testing.T) {
	assert := assert.New(t)

	// given
	sel := NewSelector(nil, stubSelectionAlgorithm{}, nil, nil)
	sessions := []*BroadcastSession{
		{PMSessionID: "session-1", InitialLatency: 400 * time.Millisecond},
		{PMSessionID: "session-2", InitialLatency: 200 * time.Millisecond},
		{PMSessionID: "session-3", InitialLatency: 600 * time.Millisecond},
	}
	sel.Add(sessions)

	// when
	sess1 := sel.Select(context.Background())
	sess2 := sel.Select(context.Background())
	sess3 := sel.Select(context.Background())

	// then
	assert.Equal("session-2", sess1.PMSessionID)
	assert.Equal("session-1", sess2.PMSessionID)
	assert.Equal("session-3", sess3.PMSessionID)
}

func TestSelector_CompleteAndSelect(t *testing.T) {
	assert := assert.New(t)

	// given
	sel := NewSelector(nil, stubSelectionAlgorithm{}, nil, nil)
	sessions := []*BroadcastSession{
		{PMSessionID: "session-1", InitialLatency: 400 * time.Millisecond},
		{PMSessionID: "session-2", InitialLatency: 200 * time.Millisecond},
		{PMSessionID: "session-3", InitialLatency: 600 * time.Millisecond},
	}
	sel.Add(sessions)

	// when
	sess1 := sel.Select(context.Background())
	sel.Complete(sess1)
	sess2 := sel.Select(context.Background())
	sess3 := sel.Select(context.Background())
	sel.Complete(sess3)
	sel.Complete(sess2)
	sess4 := sel.Select(context.Background())

	// then
	assert.Equal("session-2", sess1.PMSessionID)
	assert.Equal("session-2", sess2.PMSessionID)
	assert.Equal("session-1", sess3.PMSessionID)
	assert.Equal("session-2", sess4.PMSessionID)
}

func TestSelector_Size(t *testing.T) {
	assert := assert.New(t)

	// given
	sel := NewSelector(nil, stubSelectionAlgorithm{}, nil, nil)
	sessions := []*BroadcastSession{
		{PMSessionID: "session-1", InitialLatency: 400 * time.Millisecond},
		{PMSessionID: "session-2", InitialLatency: 200 * time.Millisecond},
		{PMSessionID: "session-3", InitialLatency: 600 * time.Millisecond},
	}
	sel.Add(sessions)

	// when & then
	assert.Equal(3, sel.Size())
	sess1 := sel.Select(context.Background())
	assert.Equal(2, sel.Size())
	sel.Complete(sess1)
	assert.Equal(3, sel.Size())
	sess2 := sel.Select(context.Background())
	sess3 := sel.Select(context.Background())
	assert.Equal(1, sel.Size())
	sel.Complete(sess3)
	sel.Complete(sess2)
	assert.Equal(3, sel.Size())
	sel.Remove(sess2)
	assert.Equal(2, sel.Size())
	sel.Clear()
	assert.Equal(0, sel.Size())
	assert.Nil(sel.Select(context.Background()))
}

func TestSelector_SortByInitialLatency(t *testing.T) {
	assert := assert.New(t)

	sel := NewSelector(nil, stubSelectionAlgorithm{}, nil, nil)
	sessions := []*BroadcastSession{
		{PMSessionID: "session-1", InitialLatency: 400 * time.Millisecond},
		{PMSessionID: "session-2", InitialLatency: 200 * time.Millisecond},
		{PMSessionID: "session-3", InitialLatency: 600 * time.Millisecond},
	}
	sel.Add(sessions)

	assert.Equal("session-2", sel.sessions[0].PMSessionID)
	assert.Equal("session-1", sel.sessions[1].PMSessionID)
	assert.Equal("session-3", sel.sessions[2].PMSessionID)
}

func TestSelector_SortByLatencyScore(t *testing.T) {
	assert := assert.New(t)

	sel := NewSelectorOrderByLatencyScore(nil, stubSelectionAlgorithm{}, nil, nil)
	sessions := []*BroadcastSession{
		{PMSessionID: "session-1", InitialLatency: 400 * time.Millisecond, LatencyScore: 0.001},
		{PMSessionID: "session-2", InitialLatency: 200 * time.Millisecond, LatencyScore: 0.01},
		{PMSessionID: "session-3", InitialLatency: 600 * time.Millisecond, LatencyScore: 0.08},
	}
	sel.Add(sessions)
	assert.Equal("session-1", sel.sessions[0].PMSessionID)
	assert.Equal("session-2", sel.sessions[1].PMSessionID)
	assert.Equal("session-3", sel.sessions[2].PMSessionID)
}

func TestMinLSSelector(t *testing.T) {
	assert := assert.New(t)

	sel := NewMinLSSelector(nil, 1.0, stubSelectionAlgorithm{}, nil, nil)
	assert.Zero(sel.Size())

	oneSess := &BroadcastSession{}
	sessions := []*BroadcastSession{
		{},
		oneSess,
		{},
	}

	// Return nil when there are no sessions
	assert.Nil(sel.Select(context.TODO()))

	sel.Add(sessions)
	assert.Equal(sel.Size(), 3)
	for _, sess := range sessions {
		assert.Contains(sel.sessions, sess)
	}

	// Remove session
	sel.Remove(oneSess)
	assert.Equal(2, sel.Size())
	sel.Add([]*BroadcastSession{oneSess})

	// Select from sessions
	sess1 := sel.Select(context.TODO())
	assert.Equal(sel.Size(), 2)
	assert.Equal(len(sel.sessions), 2)

	// Set sess1.LatencyScore to not be good enough
	sess1.LatencyScore = 1.1
	sel.Complete(sess1)
	assert.Equal(sel.Size(), 3)
	assert.Equal(len(sel.sessions), 2)
	assert.Equal(sel.knownSessions.Len(), 1)

	// Select from sessions
	sess2 := sel.Select(context.TODO())
	assert.Equal(sel.Size(), 2)
	assert.Equal(len(sel.sessions), 1)
	assert.Equal(sel.knownSessions.Len(), 1)

	// Set sess2.LatencyScore to be good enough
	sess2.LatencyScore = .9
	sel.Complete(sess2)
	assert.Equal(sel.Size(), 3)
	assert.Equal(len(sel.sessions), 1)
	assert.Equal(sel.knownSessions.Len(), 2)

	// Select from knownSessions
	knownSess := sel.Select(context.TODO())
	assert.Equal(sel.Size(), 2)
	assert.Equal(len(sel.sessions), 1)
	assert.Equal(sel.knownSessions.Len(), 1)
	assert.Equal(knownSess, sess2)

	// Set knownSess.LatencyScore to not be good enough
	knownSess.LatencyScore = 1.1
	sel.Complete(knownSess)
	// Clear sessions
	sess := sel.Select(context.TODO())
	sess.LatencyScore = 2.1
	sel.Complete(sess)
	assert.Equal(len(sel.sessions), 0)
	assert.Equal(sel.knownSessions.Len(), 3)

	// Select from knownSessions
	knownSess = sel.Select(context.TODO())
	assert.Equal(sel.Size(), 2)
	assert.Equal(len(sel.sessions), 0)
	assert.Equal(sel.knownSessions.Len(), 2)

	sel.Clear()
	assert.Zero(sel.Size())
	assert.Nil(sel.sessions)
	assert.Zero(sel.knownSessions.Len())
	assert.Nil(sel.stakeRdr)
}

func TestMinLSSelector_RemoveUnknownSession(t *testing.T) {
	assert := assert.New(t)

	sel := NewMinLSSelector(nil, 1.0, stubSelectionAlgorithm{}, nil, nil)

	// Use ManifestID to identify each session
	sessions := []*BroadcastSession{
		{Params: &core.StreamParameters{ManifestID: "foo"}},
		{Params: &core.StreamParameters{ManifestID: "bar"}},
		{Params: &core.StreamParameters{ManifestID: "baz"}},
	}

	resetsessions := func() {
		// Make a copy of the original slice so we can reset sessions to the original slice
		sel.sessions = make([]*BroadcastSession, len(sessions))
		copy(sel.sessions, sessions)
	}

	// Test remove from front of list
	resetsessions()
	sel.removeUnknownSession(0)
	assert.Len(sel.sessions, 2)
	assert.Equal("bar", string(sel.sessions[0].Params.ManifestID))
	assert.Equal("baz", string(sel.sessions[1].Params.ManifestID))

	// Test remove from middle of list
	resetsessions()
	sel.removeUnknownSession(1)
	assert.Len(sel.sessions, 2)
	assert.Equal("foo", string(sel.sessions[0].Params.ManifestID))
	assert.Equal("baz", string(sel.sessions[1].Params.ManifestID))

	// Test remove from back of list
	resetsessions()
	sel.removeUnknownSession(2)
	assert.Len(sel.sessions, 2)
	assert.Equal("foo", string(sel.sessions[0].Params.ManifestID))
	assert.Equal("bar", string(sel.sessions[1].Params.ManifestID))

	// Test remove when list length = 1
	sel.sessions = []*BroadcastSession{{}}
	sel.removeUnknownSession(0)
	assert.Empty(sel.sessions)
}

func TestMinLSSelector_SelectUnknownSession(t *testing.T) {

	tests := []struct {
		name       string
		sessions   []*BroadcastSession
		stakes     map[ethcommon.Address]int64
		perfScores map[ethcommon.Address]float64
		want       *BroadcastSession
	}{
		{
			name:     "No unknown sessions",
			sessions: []*BroadcastSession{},
			want:     nil,
		},
		{
			name: "Select lowest price",
			sessions: []*BroadcastSession{
				sessionWithPrice("0x0000000000000000000000000000000000000001", 1000, 1),
				sessionWithPrice("0x0000000000000000000000000000000000000002", 500, 1),
			},
			want: sessionWithPrice("0x0000000000000000000000000000000000000002", 500, 1),
		},
		{
			name: "Select highest stake",
			sessions: []*BroadcastSession{
				session("0x0000000000000000000000000000000000000001"),
				session("0x0000000000000000000000000000000000000002"),
			},
			stakes: map[ethcommon.Address]int64{
				ethcommon.HexToAddress("0x0000000000000000000000000000000000000001"): 1000,
				ethcommon.HexToAddress("0x0000000000000000000000000000000000000002"): 2000,
			},
			want: session("0x0000000000000000000000000000000000000002"),
		},
		{
			name: "Select highest performance score",
			sessions: []*BroadcastSession{
				session("0x0000000000000000000000000000000000000001"),
				session("0x0000000000000000000000000000000000000002"),
			},
			perfScores: map[ethcommon.Address]float64{
				ethcommon.HexToAddress("0x0000000000000000000000000000000000000001"): 0.4,
				ethcommon.HexToAddress("0x0000000000000000000000000000000000000002"): 0.6,
			},
			want: session("0x0000000000000000000000000000000000000002"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stakeRdr := newStubStakeReader()
			if tt.stakes != nil {
				stakeRdr.SetStakes(tt.stakes)
			}
			var perfScore *common.PerfScore
			selAlg := stubSelectionAlgorithm{}
			if tt.perfScores != nil {
				perfScore = &common.PerfScore{Scores: tt.perfScores}
			}
			sel := NewMinLSSelector(stakeRdr, 1.0, selAlg, perfScore, nil)
			sel.Add(tt.sessions)

			sess := sel.selectUnknownSession(context.TODO())

			require.Equal(t, tt.want, sess)
		})
	}

}

func sessionWithPrice(recipientAddr string, pricePerUnit, pixelsPerUnit int64) *BroadcastSession {
	sess := session(recipientAddr)
	sess.OrchestratorInfo.PriceInfo = &net.PriceInfo{
		PricePerUnit:  pricePerUnit,
		PixelsPerUnit: pixelsPerUnit,
	}
	return sess
}

func session(recipientAddr string) *BroadcastSession {
	return &BroadcastSession{
		OrchestratorInfo: &net.OrchestratorInfo{
			TicketParams: &net.TicketParams{
				Recipient: ethcommon.HexToAddress(recipientAddr).Bytes(),
			},
		},
	}
}

func TestMinLSSelector_SelectUnknownSession_NilStakeReader(t *testing.T) {
	sel := NewMinLSSelector(nil, 1.0, stubSelectionAlgorithm{}, nil, nil)

	sessions := make([]*BroadcastSession, 10)
	for i := 0; i < 10; i++ {
		sessions[i] = &BroadcastSession{}
	}

	sel.Add(sessions)

	i := 0
	// Check that we select sessions based on the order of sessions and that the size of
	// sessions decreases with each selection
	for sel.Size() > 0 {
		sess := sel.selectUnknownSession(context.TODO())
		assert.Same(t, sess, sessions[i])
		i++
	}
}
