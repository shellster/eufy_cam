package mpegts

import (
	"fmt"
	"io"

	mpeg2 "github.com/yapingcat/gomedia/go-mpeg2"
)

type Codec int

const (
	CodecH264 Codec = iota
	CodecH265
)

type Muxer struct {
	muxer *mpeg2.TSMuxer
	pid   uint16
	w     io.Writer
}

func NewMuxer(w io.Writer, codec Codec) *Muxer {
	m := &Muxer{
		muxer: mpeg2.NewTSMuxer(),
		w:     w,
	}

	m.muxer.OnPacket = func(pkg []byte) {
	_, _ = m.w.Write(pkg)
	}

	var streamType mpeg2.TS_STREAM_TYPE
	switch codec {
	case CodecH264:
		streamType = mpeg2.TS_STREAM_H264
	case CodecH265:
		streamType = mpeg2.TS_STREAM_H265
	default:
		streamType = mpeg2.TS_STREAM_H264
	}

	m.pid = m.muxer.AddStream(streamType)

	return m
}

func (m *Muxer) WriteFrame(nalData []byte, ptsMs uint64) error {
	if m.muxer == nil {
		return fmt.Errorf("muxer closed")
	}

	err := m.muxer.Write(m.pid, nalData, ptsMs, ptsMs)
	if err != nil {
		return fmt.Errorf("ts mux write: %w", err)
	}

	return nil
}

func (m *Muxer) Close() {
	if m.muxer != nil {
		m.muxer = nil
	}
}

func CodecFromStreamType(streamType byte) Codec {
	if streamType == 2 {
		return CodecH265
	}
	return CodecH264
}

func DetectCodecFromData(data []byte) Codec {
	if len(data) < 5 {
		return CodecH264
	}
	nalType := data[4] & 0x1F
	if nalType >= 32 {
		return CodecH265
	}
	return CodecH264
}

