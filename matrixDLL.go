package matrixdll

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
    _ "golang.org/x/mobile/bind"
	
	"github.com/google/uuid"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media"
	"github.com/pion/webrtc/v3/pkg/media/samplebuilder"
	"layeh.com/gopus"
)

const (
	sampleRate   = 48000
	channels     = 1
	frameSize    = 960
	opusBufSize  = 4000
	syncRetryGap = time.Second
)

// HTTP helper
func postJSON(url string, payload interface{}, out interface{}) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	resp, err := http.Post(url, "application/json", bytes.NewReader(data))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Matrix API paths
const (
	loginPath     = "/_matrix/client/r0/login"
	sendEventPath = "/_matrix/client/r0/rooms/%s/send/%s/%s"
)

type loginReq struct {
	Type       string                 `json:"type"`
	Identifier map[string]interface{} `json:"identifier"`
	Password   string                 `json:"password"`
}
type loginResp struct {
	UserID      string `json:"user_id"`
	AccessToken string `json:"access_token"`
}

// Client for Matrix call
type Client struct {
	homeserver  string
	accessToken string
	userID      string

	roomID string
	pc     *webrtc.PeerConnection

	currentCallID string
	myPartyID     string

	dataCh   chan []int16
	decodeCh chan []int16
}

// NewClient logs in and prepares WebRTC PeerConnection
func NewClient(homeserver, username, password, roomID string) (*Client, error) {
	// Login
	url := homeserver + loginPath
	req := loginReq{
		Type:       "m.login.password",
		Identifier: map[string]interface{}{"type": "m.id.user", "user": username},
		Password:   password,
	}
	var resp loginResp
	if err := postJSON(url, req, &resp); err != nil {
		return nil, fmt.Errorf("login error: %w", err)
	}

	// Prepare PeerConnection
	conf := webrtc.Configuration{
		ICETransportPolicy: webrtc.ICETransportPolicyAll,
		ICEServers: []webrtc.ICEServer{
			{URLs: []string{"stun:stun.l.google.com:19302"}},
			{URLs: []string{"turn:webqalqan.com:3478"}, Username: "turnuser", Credential: "turnpass"},
		},
	}
	pc, err := webrtc.NewPeerConnection(conf)
	if err != nil {
		return nil, fmt.Errorf("NewPeerConnection: %w", err)
	}

	c := &Client{
		homeserver:    homeserver,
		accessToken:   resp.AccessToken,
		userID:        resp.UserID,
		roomID:        roomID,
		pc:            pc,
		myPartyID:     uuid.NewString(),
		dataCh:        make(chan []int16, 50),
		decodeCh:      make(chan []int16, 50),
	}

	// ICE candidate handler
	pc.OnICECandidate(func(cand *webrtc.ICECandidate) {
		if cand == nil {
			return
		}
		ice := cand.ToJSON()
		payload := map[string]interface{}{
			"call_id":   c.currentCallID,
			"party_id":  c.myPartyID,
			"version":   "1",
			"candidates": []interface{}{map[string]interface{}{
				"candidate":     ice.Candidate,
				"sdpMid":        ice.SDPMid,
				"sdpMLineIndex": ice.SDPMLineIndex,
			}},
		}
		txn := uuid.NewString()
		url := fmt.Sprintf(c.homeserver+sendEventPath, c.roomID, "m.call.candidates", txn)
		if err := postJSON(url, payload, &map[string]interface{}{}); err != nil {
			log.Println("Send candidates error:", err)
		}
	})

	// Track handler
	pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		if track.Kind() != webrtc.RTPCodecTypeAudio {
			return
		}
		go func() {
			sb := samplebuilder.New(10, &codecs.OpusPacket{}, track.Codec().ClockRate)
			dec, _ := gopus.NewDecoder(sampleRate, channels)
			for {
				pkt, _, err := track.ReadRTP()
				if err != nil {
					return
				}
				sb.Push(pkt)
				for s := sb.Pop(); s != nil; s = sb.Pop() {
					pcm, _ := dec.Decode(s.Data, frameSize, false)
					select {
					case c.decodeCh <- pcm:
					default:
					}
				}
			}
		}()
	})

	return c, nil
}

// StartCall creates offer and sends invite
func (c *Client) StartCall() error {
	// Add audio track
	enc, _ := gopus.NewEncoder(sampleRate, channels, gopus.Voip)
	sendTrack, _ := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: sampleRate, Channels: channels},
		"matrix-send", "audio",
	)
	c.pc.AddTrack(sendTrack)

	// Connection state
	c.pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if state == webrtc.PeerConnectionStateConnected {
			go func() {
				for pcm := range c.dataCh {
					data, _ := enc.Encode(pcm, frameSize, opusBufSize)
					sendTrack.WriteSample(media.Sample{Data: data, Duration: 20 * time.Millisecond})
				}
			}()
		}
	})

	// Sync placeholder: implement your own sync if needed

	// Create offer
	off, _ := c.pc.CreateOffer(nil)
	c.pc.SetLocalDescription(off)
	<-webrtc.GatheringCompletePromise(c.pc)
	c.currentCallID = fmt.Sprintf("call-%d", time.Now().Unix())
	invite := map[string]interface{}{ "call_id": c.currentCallID, "party_id": c.myPartyID, "lifetime": 60000, "offer": map[string]interface{}{"type": "offer", "sdp": off.SDP}, "version": "1" }
	txn := uuid.NewString()
	url := fmt.Sprintf(c.homeserver+sendEventPath, c.roomID, "m.call.invite", txn)
	if err := postJSON(url, invite, &map[string]interface{}{}); err != nil {
		return err
	}
	return nil
}

// SendAudio queues PCM bytes for sending
func (c *Client) SendAudio(data []byte) {
	n := len(data)/2
	samples := make([]int16, n)
	for i := 0; i < n; i++ {
		samples[i] = int16(data[2*i]) | int16(data[2*i+1])<<8
	}
	select {
	case c.dataCh <- samples:
	default:
	}
}

// ReceiveAudio returns next PCM frame
func (c *Client) ReceiveAudio() []byte {
	pcm := <-c.decodeCh
	out := make([]byte, len(pcm)*2)
	for i, v := range pcm {
		out[2*i] = byte(v)
		out[2*i+1] = byte(v >> 8)
	}
	return out
}
