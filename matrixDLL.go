package matrixdll

import (
	"context"
	"fmt"
	"log"
	"time"

	 _ "golang.org/x/mobile/bind"
	"github.com/google/uuid"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media"
	"github.com/pion/webrtc/v3/pkg/media/samplebuilder"
	"layeh.com/gopus"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

const (
	sampleRate   = 48000
	channels     = 1
	frameSize    = 960
	opusBufSize  = 4000
	syncRetryGap = time.Second
)

type Client struct {
	homeserver    string
	username      string
	password      string
	roomID        string
	mautrixClient *mautrix.Client
	pc            *webrtc.PeerConnection

	currentCallID string
	myPartyID     string
	myUserID      id.UserID

	dataCh   chan []int16
	decodeCh chan []int16
}

func NewClient(homeserver, username, password, roomID string) (*Client, error) {
	c := &Client{
		homeserver: homeserver,
		username:   username,
		password:   password,
		roomID:     roomID,
		dataCh:     make(chan []int16, 50),
		decodeCh:   make(chan []int16, 50),
	}

	mcl, err := mautrix.NewClient(c.homeserver, "", "")
	if err != nil {
		return nil, fmt.Errorf("mautrix NewClient: %w", err)
	}
	resp, err := mcl.Login(context.Background(), &mautrix.ReqLogin{
		Type:       "m.login.password",
		Identifier: mautrix.UserIdentifier{Type: "m.id.user", User: c.username},
		Password:   c.password,
	})
	if err != nil {
		return nil, fmt.Errorf("login error: %w", err)
	}
	mcl.SetCredentials(resp.UserID, resp.AccessToken)
	c.mautrixClient = mcl
	c.myUserID = resp.UserID
	c.myPartyID = uuid.NewString()

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

	pc.OnICECandidate(func(cand *webrtc.ICECandidate) {
		if cand == nil {
			return
		}
		ice := cand.ToJSON()
		payload := map[string]interface{}{
			"call_id":  c.currentCallID,
			"party_id": c.myPartyID,
			"version":  "1",
			"candidates": []interface{}{
				map[string]interface{}{
					"candidate":     ice.Candidate,
					"sdpMid":        ice.SDPMid,
					"sdpMLineIndex": ice.SDPMLineIndex,
				},
			},
		}
		if _, err := c.mautrixClient.SendMessageEvent(
			context.Background(),
			id.RoomID(c.roomID),
			event.CallCandidates,
			payload,
		); err != nil {
			log.Println("Send ICE candidate error:", err)
		}
	})

	// Log state changes
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Println("PeerConnection state:", state)
	})
	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		log.Println("ICE connection state:", state)
	})

	// Incoming audio handler
	pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		if track.Kind() != webrtc.RTPCodecTypeAudio {
			return
		}
		go func() {
			sb := samplebuilder.New(10, &codecs.OpusPacket{}, track.Codec().ClockRate)
			dec, err := gopus.NewDecoder(sampleRate, channels)
			if err != nil {
				log.Println("Decoder init error:", err)
				return
			}
			for {
				pkt, _, err := track.ReadRTP()
				if err != nil {
					return
				}
				sb.Push(pkt)
				for s := sb.Pop(); s != nil; s = sb.Pop() {
					pcm, err := dec.Decode(s.Data, frameSize, false)
					if err != nil {
						log.Println("Decode error:", err)
						break
					}
					select {
					case c.decodeCh <- pcm:
					default:
					}
				}
			}
		}()
	})

	c.pc = pc
	return c, nil
}

func (c *Client) StartCall() error {
	enc, err := gopus.NewEncoder(sampleRate, channels, gopus.Voip)
	if err != nil {
		return fmt.Errorf("gopus encoder: %w", err)
	}
	sendTrack, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: sampleRate, Channels: channels},
		"matrix-send", "audio",
	)
	if err != nil {
		return fmt.Errorf("create send track: %w", err)
	}
	if _, err := c.pc.AddTrack(sendTrack); err != nil {
		return fmt.Errorf("add send track: %w", err)
	}
	c.pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if state == webrtc.PeerConnectionStateConnected {
			go func() {
				for pcm := range c.dataCh {
					pkt, err := enc.Encode(pcm, frameSize, opusBufSize)
					if err != nil {
						log.Println("encode error:", err)
						continue
					}
					if err := sendTrack.WriteSample(media.Sample{Data: pkt, Duration: 20 * time.Millisecond}); err != nil {
						log.Println("WriteSample error:", err)
					}
				}
			}()
		}
	})

	go func() {
		for {
			if err := c.mautrixClient.Sync(); err != nil {
				log.Println("Sync error:", err)
				time.Sleep(syncRetryGap)
			}
		}
	}()

	c.currentCallID = fmt.Sprintf("call-%d", time.Now().Unix())
	sdp, err := BuildOfferSDP(c.pc)
	if err != nil {
		return fmt.Errorf("BuildOfferSDP error: %w", err)
	}
	invite := map[string]interface{}{
		"call_id":  c.currentCallID,
		"lifetime": 60000,
		"offer":    map[string]interface{}{"type": "offer", "sdp": sdp},
		"version":  "1",
		"party_id": c.myPartyID,
	}
	if _, err := c.mautrixClient.SendMessageEvent(
		context.Background(),
		id.RoomID(c.roomID),
		event.CallInvite,
		invite,
	); err != nil {
		return fmt.Errorf("send invite: %w", err)
	}
	return nil
}

func BuildOfferSDP(pc *webrtc.PeerConnection) (string, error) {
	off, err := pc.CreateOffer(nil)
	if err != nil {
		return "", fmt.Errorf("CreateOffer error: %w", err)
	}
	if err := pc.SetLocalDescription(off); err != nil {
		return "", fmt.Errorf("SetLocalDescription error: %w", err)
	}
	<-webrtc.GatheringCompletePromise(pc)
	return off.SDP, nil
}

func (c *Client) SendAudio(data []byte) error {
	if c.dataCh == nil {
		return fmt.Errorf("client not initialized")
	}
	n := len(data) / 2
	samples := make([]int16, n)
	for i := 0; i < n; i++ {
		samples[i] = int16(data[2*i]) | int16(data[2*i+1])<<8
	}
	select {
	case c.dataCh <- samples:
	default:
	}
	return nil
}

func (c *Client) ReceiveAudio() ([]byte, error) {
	if c.decodeCh == nil {
		return nil, fmt.Errorf("client not initialized")
	}
	pcm := <-c.decodeCh
	out := make([]byte, len(pcm)*2)
	for i, v := range pcm {
		out[2*i] = byte(v)
		out[2*i+1] = byte(v >> 8)
	}
	return out, nil
}
