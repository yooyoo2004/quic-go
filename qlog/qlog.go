package qlog

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/lucas-clemente/quic-go/internal/congestion"
	"github.com/lucas-clemente/quic-go/internal/protocol"
	"github.com/lucas-clemente/quic-go/internal/wire"

	"github.com/francoispqt/gojay"
)

const eventChanSize = 50

// A Tracer records events to be exported to a qlog.
type Tracer interface {
	Export() error
	StartedConnection(local, remote net.Addr, version protocol.VersionNumber, srcConnID, destConnID protocol.ConnectionID)
	SentTransportParameters(*wire.TransportParameters)
	ReceivedTransportParameters(*wire.TransportParameters)
	SentPacket(hdr *wire.ExtendedHeader, packetSize protocol.ByteCount, ack *wire.AckFrame, frames []wire.Frame)
	ReceivedRetry(*wire.Header)
	ReceivedPacket(hdr *wire.ExtendedHeader, packetSize protocol.ByteCount, frames []wire.Frame)
	ReceivedStatelessReset(token *[16]byte)
	BufferedPacket(PacketType)
	DroppedPacket(PacketType, protocol.ByteCount, PacketDropReason)
	UpdatedMetrics(rttStats *congestion.RTTStats, cwnd protocol.ByteCount, bytesInFLight protocol.ByteCount, packetsInFlight int)
	LostPacket(protocol.EncryptionLevel, protocol.PacketNumber, PacketLossReason)
	UpdatedPTOCount(value uint32)
	UpdatedKeyFromTLS(protocol.EncryptionLevel, protocol.Perspective)
	UpdatedKey(generation protocol.KeyPhase, remote bool)
	DroppedEncryptionLevel(protocol.EncryptionLevel)
}

type tracer struct {
	w           io.WriteCloser
	odcid       protocol.ConnectionID
	perspective protocol.Perspective

	suffix     []byte
	events     chan event
	encodeErr  error
	runStopped chan struct{}

	lastMetrics *metrics
}

var _ Tracer = &tracer{}

// NewTracer creates a new tracer to record a qlog.
func NewTracer(w io.WriteCloser, p protocol.Perspective, odcid protocol.ConnectionID) Tracer {
	t := &tracer{
		w:           w,
		perspective: p,
		odcid:       odcid,
		runStopped:  make(chan struct{}),
		events:      make(chan event, eventChanSize),
	}
	go t.run()
	return t
}

func (t *tracer) run() {
	defer close(t.runStopped)
	buf := &bytes.Buffer{}
	enc := gojay.NewEncoder(buf)
	tl := &topLevel{
		traces: traces{
			{
				VantagePoint: vantagePoint{Type: t.perspective},
				CommonFields: commonFields{ODCID: connectionID(t.odcid), GroupID: connectionID(t.odcid)},
				EventFields:  eventFields[:],
			},
		}}
	if err := enc.Encode(tl); err != nil {
		panic(fmt.Sprintf("qlog encoding into a bytes.Buffer failed: %s", err))
	}
	data := buf.Bytes()
	t.suffix = data[buf.Len()-4:]
	if _, err := t.w.Write(data[:buf.Len()-4]); err != nil {
		t.encodeErr = err
	}
	enc = gojay.NewEncoder(t.w)
	isFirst := true
	for ev := range t.events {
		if t.encodeErr != nil { // if encoding failed, just continue draining the event channel
			continue
		}
		if !isFirst {
			t.w.Write([]byte(","))
		}
		if err := enc.Encode(ev); err != nil {
			t.encodeErr = err
		}
		isFirst = false
	}
}

// Export writes a qlog.
func (t *tracer) Export() error {
	close(t.events)
	<-t.runStopped
	if t.encodeErr != nil {
		return t.encodeErr
	}
	if _, err := t.w.Write(t.suffix); err != nil {
		return err
	}
	return t.w.Close()
}

func (t *tracer) recordEvent(details eventDetails) {
	t.events <- event{
		Time:         time.Now(),
		eventDetails: details,
	}
}

func (t *tracer) StartedConnection(local, remote net.Addr, version protocol.VersionNumber, srcConnID, destConnID protocol.ConnectionID) {
	// ignore this event if we're not dealing with UDP addresses here
	localAddr, ok := local.(*net.UDPAddr)
	if !ok {
		return
	}
	remoteAddr, ok := remote.(*net.UDPAddr)
	if !ok {
		return
	}
	t.recordEvent(&eventConnectionStarted{
		SrcAddr:          localAddr,
		DestAddr:         remoteAddr,
		Version:          version,
		SrcConnectionID:  srcConnID,
		DestConnectionID: destConnID,
	})
}

func (t *tracer) SentTransportParameters(tp *wire.TransportParameters) {
	t.recordTransportParameters(ownerLocal, tp)
}

func (t *tracer) ReceivedTransportParameters(tp *wire.TransportParameters) {
	t.recordTransportParameters(ownerRemote, tp)
}

func (t *tracer) recordTransportParameters(owner owner, tp *wire.TransportParameters) {
	t.recordEvent(&eventTransportParameters{
		Owner:                          owner,
		OriginalConnectionID:           tp.OriginalConnectionID,
		StatelessResetToken:            tp.StatelessResetToken,
		DisableActiveMigration:         tp.DisableActiveMigration,
		MaxIdleTimeout:                 tp.MaxIdleTimeout,
		MaxUDPPayloadSize:              tp.MaxUDPPayloadSize,
		AckDelayExponent:               tp.AckDelayExponent,
		MaxAckDelay:                    tp.MaxAckDelay,
		ActiveConnectionIDLimit:        tp.ActiveConnectionIDLimit,
		InitialMaxData:                 tp.InitialMaxData,
		InitialMaxStreamDataBidiLocal:  tp.InitialMaxStreamDataBidiLocal,
		InitialMaxStreamDataBidiRemote: tp.InitialMaxStreamDataBidiRemote,
		InitialMaxStreamDataUni:        tp.InitialMaxStreamDataUni,
		InitialMaxStreamsBidi:          int64(tp.MaxBidiStreamNum),
		InitialMaxStreamsUni:           int64(tp.MaxUniStreamNum),
	})
}

func (t *tracer) SentPacket(hdr *wire.ExtendedHeader, packetSize protocol.ByteCount, ack *wire.AckFrame, frames []wire.Frame) {
	numFrames := len(frames)
	if ack != nil {
		numFrames++
	}
	fs := make([]frame, 0, numFrames)
	if ack != nil {
		fs = append(fs, *transformFrame(ack))
	}
	for _, f := range frames {
		fs = append(fs, *transformFrame(f))
	}
	header := *transformExtendedHeader(hdr)
	header.PacketSize = packetSize
	t.recordEvent(&eventPacketSent{
		PacketType: PacketTypeFromHeader(&hdr.Header),
		Header:     header,
		Frames:     fs,
	})
}

func (t *tracer) ReceivedPacket(hdr *wire.ExtendedHeader, packetSize protocol.ByteCount, frames []wire.Frame) {
	fs := make([]frame, len(frames))
	for i, f := range frames {
		fs[i] = *transformFrame(f)
	}
	header := *transformExtendedHeader(hdr)
	header.PacketSize = packetSize
	t.recordEvent(&eventPacketReceived{
		PacketType: PacketTypeFromHeader(&hdr.Header),
		Header:     header,
		Frames:     fs,
	})
}

func (t *tracer) ReceivedRetry(hdr *wire.Header) {
	t.recordEvent(&eventRetryReceived{
		Header: *transformHeader(hdr),
	})
}

func (t *tracer) ReceivedStatelessReset(token *[16]byte) {
	t.recordEvent(&eventStatelessResetReceived{
		Token: token,
	})
}

func (t *tracer) BufferedPacket(packetType PacketType) {
	t.recordEvent(&eventPacketBuffered{PacketType: packetType})
}

func (t *tracer) DroppedPacket(packetType PacketType, size protocol.ByteCount, dropReason PacketDropReason) {
	t.recordEvent(&eventPacketDropped{
		PacketType: packetType,
		PacketSize: size,
		Trigger:    dropReason,
	})
}

func (t *tracer) UpdatedMetrics(rttStats *congestion.RTTStats, cwnd, bytesInFlight protocol.ByteCount, packetsInFlight int) {
	m := &metrics{
		MinRTT:           rttStats.MinRTT(),
		SmoothedRTT:      rttStats.SmoothedRTT(),
		LatestRTT:        rttStats.LatestRTT(),
		RTTVariance:      rttStats.MeanDeviation(),
		CongestionWindow: cwnd,
		BytesInFlight:    bytesInFlight,
		PacketsInFlight:  packetsInFlight,
	}
	t.recordEvent(&eventMetricsUpdated{
		Last:    t.lastMetrics,
		Current: m,
	})
	t.lastMetrics = m
}

func (t *tracer) LostPacket(encLevel protocol.EncryptionLevel, pn protocol.PacketNumber, lossReason PacketLossReason) {
	t.recordEvent(&eventPacketLost{
		PacketType:   getPacketTypeFromEncryptionLevel(encLevel),
		PacketNumber: pn,
		Trigger:      lossReason,
	})
}

func (t *tracer) UpdatedPTOCount(value uint32) {
	t.recordEvent(&eventUpdatedPTO{Value: value})
}

func (t *tracer) UpdatedKeyFromTLS(encLevel protocol.EncryptionLevel, pers protocol.Perspective) {
	t.recordEvent(&eventKeyUpdated{
		Trigger: keyUpdateTLS,
		KeyType: encLevelToKeyType(encLevel, pers),
	})
}

func (t *tracer) UpdatedKey(generation protocol.KeyPhase, remote bool) {
	trigger := keyUpdateLocal
	if remote {
		trigger = keyUpdateRemote
	}
	t.recordEvent(&eventKeyUpdated{
		Trigger:    trigger,
		KeyType:    keyTypeClient1RTT,
		Generation: generation,
	})
	t.recordEvent(&eventKeyUpdated{
		Trigger:    trigger,
		KeyType:    keyTypeServer1RTT,
		Generation: generation,
	})
}

func (t *tracer) DroppedEncryptionLevel(encLevel protocol.EncryptionLevel) {
	t.recordEvent(&eventKeyRetired{KeyType: encLevelToKeyType(encLevel, protocol.PerspectiveServer)})
	t.recordEvent(&eventKeyRetired{KeyType: encLevelToKeyType(encLevel, protocol.PerspectiveClient)})
}
