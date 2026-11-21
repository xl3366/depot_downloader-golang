package steam

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const (
	ProtoMask uint32 = 0x80000000

	EMsgMulti                       uint32 = 1
	EMsgServiceMethodResponse       uint32 = 147
	EMsgServiceMethodCallFromClient uint32 = 151
	EMsgServiceMethodSendToClient   uint32 = 152
	EMsgClientHeartBeat             uint32 = 703
	EMsgClientLogOnResponse         uint32 = 751
	EMsgClientLogon                 uint32 = 5514
)

// ProtoBufHeader represents CMsgProtoBufHeader.
type ProtoBufHeader struct {
	SteamID         uint64
	ClientSessionID int32
	JobIDSource     uint64
	JobIDTarget     uint64
	TargetJobName   string
	EResult         int32
	ErrorMessage    string
}

func (h *ProtoBufHeader) Marshal() []byte {
	w := &protoWriter{}
	if h.SteamID != 0 {
		w.writeFixed64Field(1, h.SteamID)
	}
	if h.ClientSessionID != 0 {
		w.writeVarintField(2, uint64(h.ClientSessionID))
	}
	if h.JobIDSource != 0 {
		w.writeFixed64Field(10, h.JobIDSource)
	}
	if h.JobIDTarget != 0 {
		w.writeFixed64Field(11, h.JobIDTarget)
	}
	if h.TargetJobName != "" {
		w.writeStringField(12, h.TargetJobName)
	}
	if h.EResult != 0 {
		w.writeVarintField(13, uint64(h.EResult))
	}
	if h.ErrorMessage != "" {
		w.writeStringField(14, h.ErrorMessage)
	}
	return w.bytes()
}

func unmarshalProtoBufHeader(data []byte) (*ProtoBufHeader, error) {
	h := &ProtoBufHeader{
		EResult: 1, // Default to OK (1)
	}
	r := &protoReader{buf: data}
	for r.hasMore() {
		fn, wt, err := r.readTag()
		if err != nil {
			return nil, err
		}
		switch fn {
		case 1:
			if wt == 1 {
				v, _ := r.readFixed64()
				h.SteamID = v
			} else {
				v, _ := r.readVarint()
				h.SteamID = v
			}
		case 2:
			v, _ := r.readVarint()
			h.ClientSessionID = int32(v)
		case 10:
			if wt == 1 {
				v, _ := r.readFixed64()
				h.JobIDSource = v
			} else {
				v, _ := r.readVarint()
				h.JobIDSource = v
			}
		case 11:
			if wt == 1 {
				v, _ := r.readFixed64()
				h.JobIDTarget = v
			} else {
				v, _ := r.readVarint()
				h.JobIDTarget = v
			}
		case 12:
			s, _ := r.readString()
			h.TargetJobName = s
		case 13:
			v, _ := r.readVarint()
			h.EResult = int32(v)
		case 14:
			s, _ := r.readString()
			h.ErrorMessage = s
		default:
			_ = r.skip(wt)
		}
	}
	return h, nil
}

// CMsgMulti represents Steam bundled/compressed message payload.
type CMsgMulti struct {
	SizeUnzipped uint32
	MessageBody  []byte
}

func unmarshalCMsgMulti(data []byte) (*CMsgMulti, error) {
	m := &CMsgMulti{}
	r := &protoReader{buf: data}
	for r.hasMore() {
		fn, wt, err := r.readTag()
		if err != nil {
			return nil, err
		}
		switch fn {
		case 1:
			v, _ := r.readVarint()
			m.SizeUnzipped = uint32(v)
		case 2:
			b, err := r.readBytes()
			if err != nil {
				return nil, err
			}
			m.MessageBody = b
		default:
			_ = r.skip(wt)
		}
	}
	return m, nil
}

// ClientLogonMessage represents CMsgClientLogon for anonymous login.
type ClientLogonMessage struct {
	ProtocolVersion      uint32
	ClientOsType         uint32
	CellID               uint32
	ClientPackageVersion uint32
}

func (m *ClientLogonMessage) Marshal() []byte {
	w := &protoWriter{}
	if m.ProtocolVersion != 0 {
		w.writeVarintField(1, uint64(m.ProtocolVersion))
	}
	if m.ClientOsType != 0 {
		w.writeVarintField(4, uint64(m.ClientOsType))
	}
	if m.ClientPackageVersion != 0 {
		w.writeVarintField(5, uint64(m.ClientPackageVersion))
	}
	if m.CellID != 0 {
		w.writeVarintField(11, uint64(m.CellID))
	}
	return w.bytes()
}

// ClientLogOnResponse represents CMsgClientLogOnResponse.
type ClientLogOnResponse struct {
	EResult                  int32
	OutOfGameHeartbeatSec    int32
	ClientSuppliedSteamID    uint64
}

func unmarshalClientLogOnResponse(data []byte) (*ClientLogOnResponse, error) {
	resp := &ClientLogOnResponse{}
	r := &protoReader{buf: data}
	for r.hasMore() {
		fn, wt, err := r.readTag()
		if err != nil {
			return nil, err
		}
		switch fn {
		case 1:
			v, _ := r.readVarint()
			resp.EResult = int32(v)
		case 2:
			v, _ := r.readVarint()
			resp.OutOfGameHeartbeatSec = int32(v)
		case 14:
			v, _ := r.readVarint()
			resp.ClientSuppliedSteamID = v
		default:
			_ = r.skip(wt)
		}
	}
	return resp, nil
}

// CDNAuthTokenRequest represents CContentServerDirectory_GetCDNAuthToken_Request.
type CDNAuthTokenRequest struct {
	DepotID  uint32
	HostName string
	AppID    uint32
}

func (r *CDNAuthTokenRequest) Marshal() []byte {
	w := &protoWriter{}
	if r.DepotID != 0 {
		w.writeVarintField(1, uint64(r.DepotID))
	}
	if r.HostName != "" {
		w.writeStringField(2, r.HostName)
	}
	if r.AppID != 0 {
		w.writeVarintField(3, uint64(r.AppID))
	}
	return w.bytes()
}

// CDNAuthTokenResponse represents CContentServerDirectory_GetCDNAuthToken_Response.
type CDNAuthTokenResponse struct {
	Token          string
	ExpirationTime uint32
}

func unmarshalCDNAuthTokenResponse(data []byte) (*CDNAuthTokenResponse, error) {
	resp := &CDNAuthTokenResponse{}
	r := &protoReader{buf: data}
	for r.hasMore() {
		fn, wt, err := r.readTag()
		if err != nil {
			return nil, err
		}
		switch fn {
		case 1:
			s, _ := r.readString()
			resp.Token = s
		case 2:
			v, _ := r.readVarint()
			resp.ExpirationTime = uint32(v)
		default:
			_ = r.skip(wt)
		}
	}
	return resp, nil
}

// EncodeSteamMessage packs an EMsg, header, and body into a steam network packet.
func EncodeSteamMessage(emsg uint32, hdr *ProtoBufHeader, body []byte) []byte {
	hdrBytes := hdr.Marshal()
	packet := make([]byte, 8+len(hdrBytes)+len(body))

	binary.LittleEndian.PutUint32(packet[0:4], emsg|ProtoMask)
	binary.LittleEndian.PutUint32(packet[4:8], uint32(len(hdrBytes)))

	copy(packet[8:], hdrBytes)
	copy(packet[8+len(hdrBytes):], body)
	return packet
}

// DecodeSteamMessage unpacks a packet into EMsg, header, and body.
func DecodeSteamMessage(data []byte) (uint32, *ProtoBufHeader, []byte, error) {
	if len(data) < 8 {
		return 0, nil, nil, errors.New("steam packet too short")
	}

	rawEMsg := binary.LittleEndian.Uint32(data[0:4])
	emsg := rawEMsg & (^ProtoMask)
	isProto := (rawEMsg & ProtoMask) != 0

	if !isProto {
		// Non-protobuf message
		return emsg, &ProtoBufHeader{}, data[8:], nil
	}

	hdrLen := binary.LittleEndian.Uint32(data[4:8])
	if 8+int(hdrLen) > len(data) {
		return 0, nil, nil, errors.New("steam packet header length overflow")
	}

	hdr, err := unmarshalProtoBufHeader(data[8 : 8+hdrLen])
	if err != nil {
		return 0, nil, nil, fmt.Errorf("failed to unmarshal steam proto header: %w", err)
	}

	body := data[8+hdrLen:]
	return emsg, hdr, body, nil
}

// protoWriter helper for serializing protobuf messages
type protoWriter struct {
	buf []byte
}

func (w *protoWriter) bytes() []byte {
	return w.buf
}

func (w *protoWriter) writeVarint(v uint64) {
	for v >= 0x80 {
		w.buf = append(w.buf, byte(v)|0x80)
		v >>= 7
	}
	w.buf = append(w.buf, byte(v))
}

func (w *protoWriter) writeVarintField(fieldNum int, v uint64) {
	w.writeVarint(uint64((fieldNum << 3) | 0))
	w.writeVarint(v)
}

func (w *protoWriter) writeFixed64Field(fieldNum int, v uint64) {
	w.writeVarint(uint64((fieldNum << 3) | 1))
	var buf [8]byte
	binary.LittleEndian.PutUint64(buf[:], v)
	w.buf = append(w.buf, buf[:]...)
}

func (w *protoWriter) writeStringField(fieldNum int, s string) {
	b := []byte(s)
	w.writeVarint(uint64((fieldNum << 3) | 2))
	w.writeVarint(uint64(len(b)))
	w.buf = append(w.buf, b...)
}

type protoReader struct {
	buf []byte
	pos int
}

func (p *protoReader) hasMore() bool {
	return p.pos < len(p.buf)
}

func (p *protoReader) readTag() (int, int, error) {
	v, err := p.readVarint()
	if err != nil {
		return 0, 0, err
	}
	return int(v >> 3), int(v & 7), nil
}

func (p *protoReader) readVarint() (uint64, error) {
	var val uint64
	var shift uint
	for {
		if p.pos >= len(p.buf) {
			return 0, io.ErrUnexpectedEOF
		}
		b := p.buf[p.pos]
		p.pos++
		val |= uint64(b&0x7F) << shift
		if b&0x80 == 0 {
			break
		}
		shift += 7
		if shift >= 64 {
			return 0, errors.New("varint overflow")
		}
	}
	return val, nil
}

func (p *protoReader) readFixed64() (uint64, error) {
	if p.pos+8 > len(p.buf) {
		return 0, io.ErrUnexpectedEOF
	}
	v := binary.LittleEndian.Uint64(p.buf[p.pos : p.pos+8])
	p.pos += 8
	return v, nil
}

func (p *protoReader) readBytes() ([]byte, error) {
	length, err := p.readVarint()
	if err != nil {
		return nil, err
	}
	l := int(length)
	if p.pos+l > len(p.buf) {
		return nil, io.ErrUnexpectedEOF
	}
	data := p.buf[p.pos : p.pos+l]
	p.pos += l
	return data, nil
}

func (p *protoReader) readString() (string, error) {
	b, err := p.readBytes()
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (p *protoReader) skip(wireType int) error {
	switch wireType {
	case 0:
		_, err := p.readVarint()
		return err
	case 1:
		p.pos += 8
		return nil
	case 2:
		_, err := p.readBytes()
		return err
	case 5:
		p.pos += 4
		return nil
	default:
		return fmt.Errorf("unsupported wire type: %d", wireType)
	}
}
