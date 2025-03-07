package sfu

import (
	"math"
	"sync"
	"time"

	"github.com/livekit/protocol/logger"
)

const (
	defaultRtt           = 70
	ignoreRetransmission = 100 // Ignore packet retransmission after ignoreRetransmission milliseconds
)

func btoi(b bool) int {
	if b {
		return 1
	}

	return 0
}

func itob(i int) bool {
	return i != 0
}

type packetMeta struct {
	// Original sequence number from stream.
	// The original sequence number is used to find the original
	// packet from publisher
	sourceSeqNo uint16
	// Modified sequence number after offset.
	// This sequence number is used for the associated
	// down track, is modified according the offsets, and
	// must not be shared
	targetSeqNo uint16
	// Modified timestamp for current associated
	// down track.
	timestamp uint32
	// The last time this packet was nack requested.
	// Sometimes clients request the same packet more than once, so keep
	// track of the requested packets helps to avoid writing multiple times
	// the same packet.
	// The resolution is 1 ms counting after the sequencer start time.
	lastNack uint32
	// number of NACKs this packet has received
	nacked uint8
	// Spatial layer of packet
	layer int8
	// Information that differs depending on the codec
	codecBytes []byte
	// Dependency Descriptor of packet
	ddBytes []byte
}

// Sequencer stores the packet sequence received by the down track
type sequencer struct {
	sync.Mutex
	init         bool
	max          int
	seq          []*packetMeta
	meta         []packetMeta
	metaWritePtr int
	step         int
	headSN       uint16
	startTime    int64
	rtt          uint32
	logger       logger.Logger
}

func newSequencer(maxTrack int, maxPadding int, logger logger.Logger) *sequencer {
	return &sequencer{
		startTime:    time.Now().UnixNano() / 1e6,
		max:          maxTrack + maxPadding,
		seq:          make([]*packetMeta, maxTrack+maxPadding),
		meta:         make([]packetMeta, maxTrack),
		metaWritePtr: 0,
		rtt:          defaultRtt,
		logger:       logger,
	}
}

func (s *sequencer) setRTT(rtt uint32) {
	s.Lock()
	defer s.Unlock()

	if rtt == 0 {
		s.rtt = defaultRtt
	} else {
		s.rtt = rtt
	}
}

func (s *sequencer) push(sn, offSn uint16, timeStamp uint32, layer int8) *packetMeta {
	s.Lock()
	defer s.Unlock()

	slot, isValid := s.getSlot(offSn)
	if !isValid {
		return nil
	}

	s.meta[s.metaWritePtr] = packetMeta{
		sourceSeqNo: sn,
		targetSeqNo: offSn,
		timestamp:   timeStamp,
		layer:       layer,
	}

	s.seq[slot] = &s.meta[s.metaWritePtr]

	s.metaWritePtr++
	if s.metaWritePtr >= len(s.meta) {
		s.metaWritePtr -= len(s.meta)
	}

	return s.seq[slot]
}

func (s *sequencer) pushPadding(offSn uint16) {
	s.Lock()
	defer s.Unlock()

	slot, isValid := s.getSlot(offSn)
	if !isValid {
		return
	}

	s.seq[slot] = nil
}

func (s *sequencer) getSlot(offSn uint16) (int, bool) {
	if !s.init {
		s.headSN = offSn - 1
		s.init = true
	}

	diff := offSn - s.headSN
	if diff == 0 {
		// duplicate
		return 0, false
	}

	slot := 0
	if diff > (1 << 15) {
		// out-of-order
		back := int(s.headSN - offSn)
		if back >= s.max {
			s.logger.Debugw("old packet, can not be sequenced", "head", s.headSN, "received", offSn)
			return 0, false
		}
		slot = s.step - back - 1
	} else {
		s.headSN = offSn

		// invalidate intervening slots
		for idx := 0; idx < int(diff)-1; idx++ {
			s.seq[s.wrap(s.step+idx)] = nil
		}

		slot = s.step + int(diff) - 1

		// for next packet
		s.step = s.wrap(s.step + int(diff))
	}

	return s.wrap(slot), true
}

func (s *sequencer) getPacketsMeta(seqNo []uint16) []*packetMeta {
	s.Lock()
	defer s.Unlock()

	meta := make([]*packetMeta, 0, len(seqNo))
	refTime := uint32(time.Now().UnixNano()/1e6 - s.startTime)
	for _, sn := range seqNo {
		diff := s.headSN - sn
		if diff > (1<<15) || int(diff) >= s.max {
			// out-of-order from head (should not happen) or too old
			continue
		}

		slot := s.wrap(s.step - int(diff) - 1)
		seq := s.seq[slot]
		if seq == nil || seq.targetSeqNo != sn {
			continue
		}

		if seq.lastNack == 0 || refTime-seq.lastNack > uint32(math.Min(float64(ignoreRetransmission), float64(2*s.rtt))) {
			seq.nacked++
			seq.lastNack = refTime
			meta = append(meta, seq)
		}
	}

	return meta
}

func (s *sequencer) wrap(slot int) int {
	for slot < 0 {
		slot += s.max
	}

	for slot >= s.max {
		slot -= s.max
	}

	return slot
}
